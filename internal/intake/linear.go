// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Linear GraphQL API intake provider.
//
// Authentication: API key read from the environment:
//
//	LINEAR_API_KEY — the Linear personal or workspace API key
//
// The value may be a 1Password vault reference (starts with "op://"); when
// detected, the value is resolved via `op read <ref>` before use.
//
// Source format (koryph.project.json):
//
//	"provider": "linear",
//	"source":   "ENG",           // Linear team key
//	"trigger":  "label:triage"   // filter type + value; see parseTrigger
//
// Trigger syntax (case-insensitive prefix):
//
//	""                → no extra filter; all open issues in the team
//	"label:<name>"    → issues that carry the named label
//	"state:<name>"    → issues whose workflow state matches the name
//	"<bare-value>"    → treated as a label name (backward-compatible default)
//
// Provenance key: "linear-<team-key>#<number>"
//
// e.g. "linear-ENG#42" for issue ENG-42.
//
// Priority mapping (Linear → koryph):
//
//	0 (No priority) → 2 (medium, default)
//	1 (Urgent)      → 0
//	2 (High)        → 1
//	3 (Medium)      → 2
//	4 (Low)         → 3
//
// Type mapping: issues carrying a label named "bug" (case-insensitive) are
// filed as bd type "bug".  Idempotency, dry-run, and comment-back semantics
// mirror the GitHub and JIRA providers.
package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/registry"
)

const linearGraphQLEndpoint = "https://api.linear.app/graphql"

// --- Linear GraphQL response types ------------------------------------------

// linearResponse wraps the top-level GraphQL response envelope.
type linearResponse struct {
	Data   linearData     `json:"data"`
	Errors []linearGQLErr `json:"errors"`
}

type linearGQLErr struct {
	Message string `json:"message"`
}

type linearData struct {
	Issues linearIssueConn `json:"issues"`
}

type linearIssueConn struct {
	Nodes []linearIssue `json:"nodes"`
}

type linearIssue struct {
	ID          string          `json:"id"`         // UUID — used for comment mutation
	Identifier  string          `json:"identifier"` // e.g. "ENG-42"
	Title       string          `json:"title"`
	Description string          `json:"description"` // plain Markdown
	Priority    int             `json:"priority"`
	Labels      linearLabelConn `json:"labels"`
	State       linearState     `json:"state"`
	Creator     linearActor     `json:"creator"`
}

type linearLabelConn struct {
	Nodes []linearLabel `json:"nodes"`
}

type linearLabel struct {
	Name string `json:"name"`
}

type linearState struct {
	Name string `json:"name"`
}

type linearActor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// linearCommentResponse wraps the commentCreate mutation response.
type linearCommentResponse struct {
	Data struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	} `json:"data"`
	Errors []linearGQLErr `json:"errors"`
}

// --- client -----------------------------------------------------------------

// linearClient authenticates against Linear and fetches issues via GraphQL.
type linearClient struct {
	apiKey  string
	baseURL string // defaults to linearGraphQLEndpoint; overridable for tests
	http    *http.Client
}

// newLinear creates a linearClient, resolving LINEAR_API_KEY from the
// environment (or a vault reference).
func newLinear() (*linearClient, error) {
	key, err := resolveSecret("LINEAR_API_KEY")
	if err != nil {
		return nil, fmt.Errorf("linear: %w", err)
	}
	if key == "" {
		return nil, fmt.Errorf("linear: LINEAR_API_KEY is not set (required for Linear API authentication)")
	}
	return &linearClient{
		apiKey:  key,
		baseURL: linearGraphQLEndpoint,
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// --- Source interface -------------------------------------------------------

// List implements Source. It queries the Linear GraphQL API for open issues in
// the given team, optionally filtered by label or state.
//
// Source interface parameter mapping for Linear:
//   - owner: unused (Linear workspace is implicit from the API key)
//   - repo:  Linear team key (e.g. "ENG")
//   - label: trigger string — see parseTrigger for syntax
func (l *linearClient) List(ctx context.Context, owner, repo, trigger string, limit int) ([]SourceIssue, error) {
	raw, err := l.listIssues(ctx, repo, trigger, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SourceIssue, 0, len(raw))
	for _, iss := range raw {
		num, nerr := linearIssueNumber(iss.Identifier)
		if nerr != nil {
			return nil, fmt.Errorf("linear: unexpected identifier %q: %w", iss.Identifier, nerr)
		}
		labels := linearLabels(iss)
		out = append(out, SourceIssue{
			Number: num,
			Title:  iss.Title,
			Body:   iss.Description,
			Labels: labels,
			Author: linearAuthor(iss.Creator),
		})
	}
	return out, nil
}

// Comment implements Source. It posts a Markdown comment on the issue whose
// identifier is "<teamKey>-<number>".  It looks up the issue UUID first so the
// mutation does not depend on caller-visible numbering.
func (l *linearClient) Comment(ctx context.Context, owner, repo string, number int, body string) error {
	return l.postComment(ctx, repo, number, body)
}

// Provenance implements Source. Returns "linear-<repo>#<number>" (e.g.
// "linear-ENG#42") so Linear keys never collide with GitHub or JIRA keys.
func (l *linearClient) Provenance(owner, repo string, number int) string {
	return fmt.Sprintf("linear-%s#%d", repo, number)
}

// --- GraphQL helpers --------------------------------------------------------

// graphqlRequest is the JSON body sent to a GraphQL endpoint.
type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// linearIssueFields is the shared field set fetched for every issue.
const linearIssueFields = `{
  id
  identifier
  title
  description
  priority
  labels { nodes { name } }
  state { name }
  creator { name email }
}`

// listIssues calls the Linear GraphQL API and returns raw issue nodes.
func (l *linearClient) listIssues(ctx context.Context, teamKey, trigger string, limit int) ([]linearIssue, error) {
	query, variables := buildLinearQuery(teamKey, trigger, limit)
	body, err := l.gql(ctx, query, variables)
	if err != nil {
		return nil, err
	}
	var resp linearResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("linear: parse issues response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("linear: GraphQL error: %s", resp.Errors[0].Message)
	}
	return resp.Data.Issues.Nodes, nil
}

// buildLinearQuery constructs the GraphQL query and variables for an issue
// list, varying the filter based on the parsed trigger.
func buildLinearQuery(teamKey, trigger string, limit int) (string, map[string]any) {
	filterKind, filterValue := parseTrigger(trigger)

	var queryStr string
	variables := map[string]any{
		"teamKey": teamKey,
		"first":   limit,
	}

	switch filterKind {
	case triggerLabel:
		queryStr = `query($teamKey: String!, $labelName: String!, $first: Int!) {
  issues(first: $first filter: {
    team: { key: { eq: $teamKey } }
    labels: { name: { in: [$labelName] } }
  }) { nodes ` + linearIssueFields + ` }
}`
		variables["labelName"] = filterValue

	case triggerState:
		queryStr = `query($teamKey: String!, $stateName: String!, $first: Int!) {
  issues(first: $first filter: {
    team: { key: { eq: $teamKey } }
    state: { name: { eq: $stateName } }
  }) { nodes ` + linearIssueFields + ` }
}`
		variables["stateName"] = filterValue

	default: // triggerNone
		queryStr = `query($teamKey: String!, $first: Int!) {
  issues(first: $first filter: {
    team: { key: { eq: $teamKey } }
  }) { nodes ` + linearIssueFields + ` }
}`
	}

	return queryStr, variables
}

// postComment posts a comment on the issue identified by "<teamKey>-<number>".
// It first resolves the issue UUID via a small query, then calls commentCreate.
func (l *linearClient) postComment(ctx context.Context, teamKey string, number int, body string) error {
	identifier := fmt.Sprintf("%s-%d", teamKey, number)

	// Resolve issue UUID.
	idQuery := `query($identifier: String!) { issue(id: $identifier) { id } }`
	rawID, err := l.gql(ctx, idQuery, map[string]any{"identifier": identifier})
	if err != nil {
		return fmt.Errorf("linear: resolve issue id for %s: %w", identifier, err)
	}
	var idResp struct {
		Data struct {
			Issue struct {
				ID string `json:"id"`
			} `json:"issue"`
		} `json:"data"`
		Errors []linearGQLErr `json:"errors"`
	}
	if err := json.Unmarshal(rawID, &idResp); err != nil {
		return fmt.Errorf("linear: parse issue id response: %w", err)
	}
	if len(idResp.Errors) > 0 {
		return fmt.Errorf("linear: resolve issue %s: %s", identifier, idResp.Errors[0].Message)
	}
	issueID := idResp.Data.Issue.ID
	if issueID == "" {
		return fmt.Errorf("linear: issue %s not found", identifier)
	}

	// Post comment.
	mutation := `mutation($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId body: $body }) { success }
}`
	rawComment, err := l.gql(ctx, mutation, map[string]any{
		"issueId": issueID,
		"body":    body,
	})
	if err != nil {
		return fmt.Errorf("linear: commentCreate for %s: %w", identifier, err)
	}
	var cr linearCommentResponse
	if err := json.Unmarshal(rawComment, &cr); err != nil {
		return fmt.Errorf("linear: parse commentCreate response: %w", err)
	}
	if len(cr.Errors) > 0 {
		return fmt.Errorf("linear: commentCreate error: %s", cr.Errors[0].Message)
	}
	if !cr.Data.CommentCreate.Success {
		return fmt.Errorf("linear: commentCreate returned success=false for %s", identifier)
	}
	return nil
}

// gql sends a GraphQL request and returns the raw response body.
func (l *linearClient) gql(ctx context.Context, query string, variables map[string]any) ([]byte, error) {
	reqBody, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, fmt.Errorf("linear: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("linear: build request: %w", err)
	}
	req.Header.Set("Authorization", l.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := l.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("linear: request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("linear: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear: HTTP %d: %s", resp.StatusCode, trimMessage(body))
	}
	return body, nil
}

// --- RunLinear --------------------------------------------------------------

// LinearOptions configures one Linear intake run.
type LinearOptions struct {
	// Project is the registry record (used for the bd binary path).
	Project *registry.Record
	// TeamKey is the Linear team key (e.g. "ENG").
	TeamKey string
	// Trigger is the filter applied to issues. See parseTrigger for syntax.
	// When empty, all open issues in the team are polled.
	Trigger string
	// Limit caps the number of issues fetched per run; default 20.
	Limit int
	// DryRun prints intent and mutates nothing.
	DryRun bool
	// CommentBack posts the new bead ID back on each ingested issue.
	CommentBack bool
	// Client allows injecting a pre-built client (used in tests). When nil,
	// newLinear() is called to create one from env credentials.
	Client *linearClient
}

// RunLinear polls a Linear team for issues matching the trigger filter and
// files one planning bead per new issue. Idempotency, dry-run, and
// comment-back semantics are identical to the GitHub and JIRA Run functions.
func RunLinear(ctx context.Context, opts LinearOptions) (*Result, error) {
	if opts.Project == nil {
		return nil, fmt.Errorf("intake/linear: project record is required")
	}
	if opts.TeamKey == "" {
		return nil, fmt.Errorf("intake/linear: team key is required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}

	client := opts.Client
	if client == nil {
		c, cerr := newLinear()
		if cerr != nil {
			return nil, cerr
		}
		client = c
	}

	bd := beads.New(opts.Project.Root)
	if v := os.Getenv("KORYPH_BD_BIN"); v != "" {
		bd.Bin = v
	}
	if !bd.Available() {
		return nil, fmt.Errorf("intake/linear: bd (%q) not found on PATH", bd.Bin)
	}

	issues, err := client.List(ctx, "linear.app", opts.TeamKey, opts.Trigger, limit)
	if err != nil {
		return nil, fmt.Errorf("intake/linear: list issues for team %s: %w", opts.TeamKey, err)
	}

	return ingest(ctx, bd, client, "linear.app", opts.TeamKey, issues, ingestOptions{
		errPrefix:   "intake/linear",
		DryRun:      opts.DryRun,
		CommentBack: opts.CommentBack,
	}, func(iss SourceIssue) string {
		return buildLinearDescription(opts.TeamKey, iss)
	})
}

// --- trigger parsing --------------------------------------------------------

type triggerKind int

const (
	triggerNone  triggerKind = iota // no filter beyond team
	triggerLabel                    // filter by label name
	triggerState                    // filter by state name
)

// parseTrigger splits a trigger string into a kind and value.
//
//	""                → (triggerNone, "")
//	"label:triage"    → (triggerLabel, "triage")
//	"state:Todo"      → (triggerState, "Todo")
//	"triage"          → (triggerLabel, "triage")  — bare = label (default)
func parseTrigger(trigger string) (triggerKind, string) {
	t := strings.TrimSpace(trigger)
	if t == "" {
		return triggerNone, ""
	}
	lower := strings.ToLower(t)
	if _, ok := strings.CutPrefix(lower, "label:"); ok {
		return triggerLabel, strings.TrimSpace(t[len("label:"):])
	}
	if _, ok := strings.CutPrefix(lower, "state:"); ok {
		return triggerState, strings.TrimSpace(t[len("state:"):])
	}
	// Bare value → label (mirrors GitHub intake default behavior).
	return triggerLabel, t
}

// --- helpers ----------------------------------------------------------------

// linearIssueNumber extracts the numeric suffix from a Linear identifier.
// "ENG-42" → 42.
func linearIssueNumber(identifier string) (int, error) {
	idx := strings.LastIndex(identifier, "-")
	if idx < 0 || idx == len(identifier)-1 {
		return 0, fmt.Errorf("cannot parse numeric suffix from %q", identifier)
	}
	n, err := strconv.Atoi(identifier[idx+1:])
	if err != nil {
		return 0, fmt.Errorf("non-numeric suffix in %q: %w", identifier, err)
	}
	return n, nil
}

// linearLabels synthesises a label slice from a Linear issue, mapping the
// Linear priority integer and label names so priorityFor and issueTypeFor work
// unchanged.
func linearLabels(iss linearIssue) []string {
	var labels []string
	// Map Linear priority integer → koryph pN label.
	switch iss.Priority {
	case 1: // Urgent
		labels = append(labels, "p0")
	case 2: // High
		labels = append(labels, "p1")
	case 3: // Medium
		labels = append(labels, "p2")
	case 4: // Low
		labels = append(labels, "p3")
	default: // 0 = No priority
		labels = append(labels, "p2")
	}
	// Include native Linear label names verbatim (enables issueTypeFor "bug").
	for _, l := range iss.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	return labels
}

// linearAuthor returns the best available author string for a Linear actor.
func linearAuthor(a linearActor) string {
	if a.Email != "" {
		return a.Email
	}
	return a.Name
}

// buildLinearDescription assembles a bead description with a Linear provenance
// footer. The body is the raw Markdown description from Linear.
func buildLinearDescription(teamKey string, iss SourceIssue) string {
	identifier := fmt.Sprintf("%s-%d", teamKey, iss.Number)
	footer := fmt.Sprintf(
		"Source: linear.app/team/%s/issue/%s, author %s, ingested by koryph intake",
		teamKey, identifier, iss.Author,
	)
	return withProvenanceFooter(iss.Body, footer)
}
