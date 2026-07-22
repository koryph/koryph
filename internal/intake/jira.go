// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// JIRA Cloud REST v3 intake provider.
//
// Authentication: HTTP Basic Auth (email + API token) read from the environment:
//
//	JIRA_EMAIL — the Atlassian account email address
//	JIRA_TOKEN — the API token (see https://id.atlassian.com/manage-profile/security/api-tokens)
//
// Either value may be a 1Password vault reference (starts with "op://"); when
// detected, the value is resolved via `op read <ref>` before use.
//
// Source format (koryph.project.json):
//
//	"provider": "jira",
//	"source":   "acme.atlassian.net/ENG",   // <host>/<project-key>
//	"trigger":  "status = \"To Do\"",         // JQL — appended to "project = <key> AND"
//
// Provenance key: "jira-<host>/<project-key>#<number>"
// e.g. "jira-acme.atlassian.net/ENG#42" for issue ENG-42.
//
// Idempotency, dry-run, and comment-back semantics mirror the GitHub provider.
package intake

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/registry"
)

// --- JIRA REST v3 response types -------------------------------------------

// jiraSearchResponse is the top-level `GET /rest/api/3/search` response.
type jiraSearchResponse struct {
	Issues []jiraIssue `json:"issues"`
}

// jiraIssue is the subset of a JIRA Cloud REST v3 issue that intake consumes.
type jiraIssue struct {
	ID     string     `json:"id"`
	Key    string     `json:"key"` // e.g. "ENG-42"
	Fields jiraFields `json:"fields"`
}

type jiraFields struct {
	Summary     string       `json:"summary"`
	Description interface{}  `json:"description"` // ADF (Atlassian Document Format) in v3; may be null
	Labels      []string     `json:"labels"`
	Priority    jiraNamed    `json:"priority"`
	IssueType   jiraNamed    `json:"issuetype"`
	Reporter    jiraReporter `json:"reporter"`
}

type jiraNamed struct {
	Name string `json:"name"`
}

type jiraReporter struct {
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

// --- client ----------------------------------------------------------------

// jiraClient authenticates against JIRA Cloud and fetches issues via REST v3.
type jiraClient struct {
	baseURL string // e.g. "https://acme.atlassian.net" (no trailing slash)
	auth    string // base64("<email>:<token>")
	http    *http.Client
}

// newJIRA creates a jiraClient resolving credentials from the environment.
// JIRA_EMAIL and JIRA_TOKEN are read; either may be an "op://" vault reference.
func newJIRA(baseURL string) (*jiraClient, error) {
	email, err := resolveSecret("JIRA_EMAIL")
	if err != nil {
		return nil, fmt.Errorf("jira: %w", err)
	}
	if email == "" {
		return nil, fmt.Errorf("jira: JIRA_EMAIL is not set (required for JIRA Cloud authentication)")
	}
	token, err := resolveSecret("JIRA_TOKEN")
	if err != nil {
		return nil, fmt.Errorf("jira: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("jira: JIRA_TOKEN is not set (required for JIRA Cloud authentication)")
	}
	raw := email + ":" + token
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
	return &jiraClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		auth:    auth,
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// resolveSecret reads an env var and, when its value looks like an op:// vault
// reference, resolves it via `op read`. Returns "" when the env var is unset.
func resolveSecret(envKey string) (string, error) {
	v := os.Getenv(envKey)
	if v == "" {
		return "", nil
	}
	if strings.HasPrefix(v, "op://") {
		return resolveOpRef(v)
	}
	return v, nil
}

// resolveOpRef calls `op read <ref>` and returns the trimmed output.
func resolveOpRef(ref string) (string, error) {
	res, err := execx.MustSucceed(context.Background(), execx.Cmd{
		Name:    "op",
		Args:    []string{"read", ref},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		return "", fmt.Errorf("vault: op read %q: %w", ref, err)
	}
	return strings.TrimRight(res.Stdout, "\n"), nil
}

// --- Source interface -------------------------------------------------------

// List implements Source. It issues a JQL search against the JIRA project and
// maps results to provider-neutral SourceIssues.
//
// The Source interface parameters map to JIRA as follows:
//   - owner: JIRA instance hostname (e.g. "acme.atlassian.net") — unused by
//     List itself since baseURL is already configured; kept for interface parity.
//   - repo:  JIRA project key (e.g. "ENG").
//   - label: JQL predicate to filter issues (e.g. `status = "To Do"`). When
//     non-empty it is AND-combined with "project = <repo>"; when empty the
//     implicit query is "project = <repo>".
func (j *jiraClient) List(ctx context.Context, owner, repo, jql string, limit int) ([]SourceIssue, error) {
	fullJQL := buildJQL(repo, jql)
	raw, err := j.search(ctx, fullJQL, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SourceIssue, 0, len(raw))
	for _, iss := range raw {
		num, err := issueNumberFromKey(iss.Key)
		if err != nil {
			return nil, fmt.Errorf("jira: unexpected issue key %q: %w", iss.Key, err)
		}
		labels := jiraLabels(iss)
		out = append(out, SourceIssue{
			Number: num,
			Title:  iss.Fields.Summary,
			Body:   extractADFText(iss.Fields.Description),
			Labels: labels,
			Author: jiraAuthor(iss.Fields.Reporter),
		})
	}
	return out, nil
}

// Comment implements Source. It posts a plain-text comment on the JIRA issue
// whose key is reconstructed as "<repo>-<number>".
func (j *jiraClient) Comment(ctx context.Context, owner, repo string, number int, body string) error {
	key := repo + "-" + strconv.Itoa(number)
	return j.postComment(ctx, key, body)
}

// Provenance implements Source. Returns "jira-<owner>/<repo>#<number>" (e.g.
// "jira-acme.atlassian.net/ENG#42") so JIRA keys never collide with GitHub or
// cross-instance keys in the bead store.
func (j *jiraClient) Provenance(owner, repo string, number int) string {
	return fmt.Sprintf("jira-%s/%s#%d", owner, repo, number)
}

// --- REST helpers ----------------------------------------------------------

// search calls GET /rest/api/3/search with the given JQL and returns issues.
func (j *jiraClient) search(ctx context.Context, jql string, limit int) ([]jiraIssue, error) {
	u := j.baseURL + "/rest/api/3/search"
	params := url.Values{
		"jql":        {jql},
		"maxResults": {strconv.Itoa(limit)},
		"fields":     {"summary,description,labels,priority,issuetype,reporter"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("jira: build search request: %w", err)
	}
	req.Header.Set("Authorization", j.auth)
	req.Header.Set("Accept", "application/json")

	resp, err := j.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira: search request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("jira: read search response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira: search HTTP %d: %s", resp.StatusCode, trimMessage(body))
	}
	var sr jiraSearchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("jira: parse search response: %w", err)
	}
	return sr.Issues, nil
}

// postComment calls POST /rest/api/3/issue/<key>/comment.
func (j *jiraClient) postComment(ctx context.Context, key, body string) error {
	u := fmt.Sprintf("%s/rest/api/3/issue/%s/comment", j.baseURL, key)
	// JIRA REST v3 requires a simple ADF doc for comment bodies. We wrap the
	// plain text in a minimal paragraph node so the API accepts it.
	payload := map[string]interface{}{
		"body": map[string]interface{}{
			"version": 1,
			"type":    "doc",
			"content": []interface{}{
				map[string]interface{}{
					"type": "paragraph",
					"content": []interface{}{
						map[string]interface{}{
							"type": "text",
							"text": body,
						},
					},
				},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("jira: marshal comment: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("jira: build comment request: %w", err)
	}
	req.Header.Set("Authorization", j.auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := j.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira: post comment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jira: comment HTTP %d: %s", resp.StatusCode, trimMessage(b))
	}
	return nil
}

// --- RunJIRA ---------------------------------------------------------------

// JIRAOptions configures one JIRA intake run.
type JIRAOptions struct {
	// Project is the registry record (used for the bd binary path).
	Project *registry.Record
	// BaseURL is the JIRA Cloud instance URL, e.g. "https://acme.atlassian.net".
	BaseURL string
	// ProjectKey is the JIRA project key, e.g. "ENG". Used for Provenance
	// scoping and as the "repo" equivalent throughout this run.
	ProjectKey string
	// JQL is the trigger query appended to "project = <ProjectKey> AND".
	// When empty only "project = <ProjectKey>" is used.
	JQL string
	// Limit caps the number of open issues fetched per run; default 20.
	Limit int
	// DryRun prints intent and mutates nothing.
	DryRun bool
	// CommentBack posts the new bead ID back on each ingested issue.
	CommentBack bool
	// Client allows injecting a pre-built client (used in tests). When nil,
	// newJIRA(BaseURL) is called to create one from env credentials.
	Client *jiraClient
}

// RunJIRA polls a JIRA project for issues matching the JQL trigger and files
// one planning bead per new issue. Idempotency, dry-run, and comment-back
// semantics are identical to the GitHub Run function.
func RunJIRA(ctx context.Context, opts JIRAOptions) (*Result, error) {
	if opts.Project == nil {
		return nil, fmt.Errorf("intake/jira: project record is required")
	}
	host, err := jiraHost(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("intake/jira: %w", err)
	}
	if opts.ProjectKey == "" {
		return nil, fmt.Errorf("intake/jira: project key is required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}

	client := opts.Client
	if client == nil {
		c, cerr := newJIRA(opts.BaseURL)
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
		return nil, fmt.Errorf("intake/jira: bd (%q) not found on PATH", bd.Bin)
	}

	issues, err := client.List(ctx, host, opts.ProjectKey, opts.JQL, limit)
	if err != nil {
		return nil, fmt.Errorf("intake/jira: list issues %s/%s: %w", host, opts.ProjectKey, err)
	}

	return ingest(ctx, bd, client, host, opts.ProjectKey, issues, ingestOptions{
		errPrefix:   "intake/jira",
		DryRun:      opts.DryRun,
		CommentBack: opts.CommentBack,
	}, func(iss SourceIssue) string {
		return buildJIRADescription(host, opts.ProjectKey, iss)
	})
}

// --- helpers ---------------------------------------------------------------

// buildJQL constructs the full JQL for a JIRA project. When userJQL is non-empty
// it is AND-combined with the project constraint.
func buildJQL(projectKey, userJQL string) string {
	base := fmt.Sprintf("project = %q", projectKey)
	if userJQL == "" {
		return base
	}
	return base + " AND (" + userJQL + ")"
}

// buildJIRADescription builds a bead description with a JIRA provenance footer.
func buildJIRADescription(host, projectKey string, iss SourceIssue) string {
	footer := fmt.Sprintf(
		"Source: %s/browse/%s-%d, author %s, ingested by koryph intake",
		"https://"+host, projectKey, iss.Number, iss.Author,
	)
	return withProvenanceFooter(iss.Body, footer)
}

// jiraHost extracts the hostname from a JIRA base URL or bare hostname.
func jiraHost(baseURL string) (string, error) {
	s := strings.TrimSpace(baseURL)
	if s == "" {
		return "", fmt.Errorf("base URL is empty")
	}
	if !strings.Contains(s, "://") {
		// Bare hostname: "acme.atlassian.net"
		return strings.Trim(s, "/"), nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse base URL %q: %w", baseURL, err)
	}
	return u.Host, nil
}

// issueNumberFromKey parses the numeric suffix from a JIRA issue key.
// "ENG-42" → 42.
func issueNumberFromKey(key string) (int, error) {
	return parseNumericSuffix(key)
}

// jiraLabels synthesises a label slice from a JIRA issue's metadata. JIRA
// priority and issuetype are mapped to p0-p3/bug labels so the provider-neutral
// priorityFor and issueTypeFor helpers work without change.
func jiraLabels(iss jiraIssue) []string {
	var labels []string
	// Map JIRA priority → koryph priority label.
	switch strings.ToLower(strings.TrimSpace(iss.Fields.Priority.Name)) {
	case "highest", "blocker":
		labels = append(labels, "p0")
	case "high", "critical":
		labels = append(labels, "p1")
	case "medium", "":
		labels = append(labels, "p2")
	case "low", "minor":
		labels = append(labels, "p3")
	case "lowest", "trivial":
		labels = append(labels, "p3")
	default:
		labels = append(labels, "p2")
	}
	// Map Bug issuetype → "bug" label.
	if strings.EqualFold(strings.TrimSpace(iss.Fields.IssueType.Name), "bug") {
		labels = append(labels, "bug")
	}
	// Include native JIRA labels verbatim.
	labels = append(labels, iss.Fields.Labels...)
	return labels
}

// jiraAuthor returns the best available author identifier for an issue.
func jiraAuthor(r jiraReporter) string {
	if r.EmailAddress != "" {
		return r.EmailAddress
	}
	return r.DisplayName
}

// extractADFText recursively walks an Atlassian Document Format value and
// concatenates all text nodes into a plain-text string. Returns "" when the
// description is null or of an unrecognised type.
func extractADFText(v interface{}) string {
	if v == nil {
		return ""
	}
	var buf strings.Builder
	walkADF(v, &buf)
	return strings.TrimRight(buf.String(), "\n")
}

// walkADF visits ADF nodes depth-first and appends text to buf.
func walkADF(v interface{}, buf *strings.Builder) {
	switch node := v.(type) {
	case map[string]interface{}:
		// A node with type "text" has a "text" string field.
		if t, ok := node["text"].(string); ok {
			buf.WriteString(t)
		}
		// Recurse into "content" array.
		if content, ok := node["content"].([]interface{}); ok {
			nodeType, _ := node["type"].(string)
			for _, child := range content {
				walkADF(child, buf)
			}
			// Add newline after block-level nodes.
			switch nodeType {
			case "paragraph", "heading", "bulletList", "orderedList", "blockquote", "codeBlock":
				buf.WriteString("\n")
			}
		}
	case []interface{}:
		for _, child := range node {
			walkADF(child, buf)
		}
	case string:
		// Top-level plain string (JIRA Server style, not ADF).
		buf.WriteString(node)
	}
}

// trimMessage returns the first 200 bytes of a JSON error body as a string,
// stripping outer whitespace, for use in error messages.
func trimMessage(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
