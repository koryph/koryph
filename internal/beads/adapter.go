// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
)

// mergeSlotBackoffCap bounds the exponential backoff between claim attempts.
const mergeSlotBackoffCap = 30 * time.Second

// mergeSlotBackoffBase is the first backoff interval; doubled each retry up to
// the cap. Package-level so tests can shrink it for fast retry coverage.
var mergeSlotBackoffBase = time.Second

// run executes `bd <args...>` in the project root, failing on a non-zero exit.
func (a *Adapter) run(ctx context.Context, args ...string) (execx.Result, error) {
	return execx.MustSucceed(ctx, execx.Cmd{
		Dir:  a.RepoRoot,
		Name: a.Bin,
		Args: args,
	})
}

// runStdin is run with data piped to the command's stdin.
func (a *Adapter) runStdin(ctx context.Context, stdin string, args ...string) (execx.Result, error) {
	return execx.MustSucceed(ctx, execx.Cmd{
		Dir:   a.RepoRoot,
		Name:  a.Bin,
		Args:  args,
		Stdin: stdin,
	})
}

// Version returns the first line of `bd version`, trimmed.
func (a *Adapter) Version(ctx context.Context) (string, error) {
	res, err := a.run(ctx, "version")
	if err != nil {
		return "", err
	}
	line := res.Stdout
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line), nil
}

// Ready returns the ready frontier via `bd ready --json --limit 0`
// (+ `--parent <p>` when scoped), preserving bd's priority order.
func (a *Adapter) Ready(ctx context.Context, opts ReadyOpts) ([]Issue, error) {
	args := []string{"ready", "--json", "--limit", "0"}
	if opts.Parent != "" {
		args = append(args, "--parent", opts.Parent)
	}
	res, err := a.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseIssueList([]byte(res.Stdout))
}

// Show returns one issue via `bd show <id> --json`.
func (a *Adapter) Show(ctx context.Context, id string) (Issue, error) {
	res, err := a.run(ctx, "show", id, "--json")
	if err != nil {
		return Issue{}, err
	}
	return parseIssue([]byte(res.Stdout))
}

// ListChildren returns the children of id via `bd list --parent <id> --json`.
func (a *Adapter) ListChildren(ctx context.Context, id string) ([]Issue, error) {
	res, err := a.run(ctx, "list", "--parent", id, "--json")
	if err != nil {
		return nil, err
	}
	return parseIssueList([]byte(res.Stdout))
}

// ListByLabel returns every open+closed issue carrying the exact label via
// `bd list --label <label> --json --limit 0`. Callers use it for idempotency
// checks (e.g. gh-intake dedupe on a `gh-<number>` provenance label).
func (a *Adapter) ListByLabel(ctx context.Context, label string) ([]Issue, error) {
	res, err := a.run(ctx, "list", "--label", label, "--json", "--limit", "0", "--all")
	if err != nil {
		return nil, err
	}
	return parseIssueList([]byte(res.Stdout))
}

// ListByExternalRef returns every open+closed issue carrying the exact
// external-ref key via `bd list --external-ref <ref> --json --limit 0 --all`.
// Used as the primary idempotency check during intake (see also ListByLabel for
// backward-compat fallback on beads created before external-ref was introduced).
func (a *Adapter) ListByExternalRef(ctx context.Context, ref string) ([]Issue, error) {
	res, err := a.run(ctx, "list", "--external-ref", ref, "--json", "--limit", "0", "--all")
	if err != nil {
		return nil, err
	}
	return parseIssueList([]byte(res.Stdout))
}

// CreateInput describes a single bead to create via `bd create`.
type CreateInput struct {
	Title       string
	Description string   // piped on stdin (--body-file -); may be multi-line
	Labels      []string // comma-joined into --labels
	Priority    int      // 0..4 (0 = highest); passed to --priority
	IssueType   string   // bug|feature|task|epic|chore|decision; "" = bd default
	ExternalRef string   // canonical external-ref key (e.g. "gh-42"); "" = omit
}

// Create creates one bead via `bd create <title> --silent --body-file -` and
// returns the new id. The description is piped on stdin so arbitrarily long,
// multi-line bodies are safe; --silent makes bd emit only the id.
func (a *Adapter) Create(ctx context.Context, in CreateInput) (string, error) {
	args := []string{"create", in.Title, "--silent", "--body-file", "-", "--priority", strconv.Itoa(in.Priority)}
	if len(in.Labels) > 0 {
		args = append(args, "--labels", strings.Join(in.Labels, ","))
	}
	if in.IssueType != "" {
		args = append(args, "--type", in.IssueType)
	}
	if in.ExternalRef != "" {
		args = append(args, "--external-ref", in.ExternalRef)
	}
	res, err := a.runStdin(ctx, in.Description, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Comment posts a comment via `bd comment <id> <text>`.
//
// NOTE: bd's comment verb varies across builds (`bd comment` vs
// `bd comments add`). We standardize on `bd comment <id> <text>`.
func (a *Adapter) Comment(ctx context.Context, id, text string) error {
	_, err := a.run(ctx, "comment", id, text)
	return err
}

// Close closes an issue via `bd close <id> --reason <reason>`.
func (a *Adapter) Close(ctx context.Context, id, reason string) error {
	_, err := a.run(ctx, "close", id, "--reason", reason)
	return err
}

// Claim claims an issue via `bd update <id> --claim`.
func (a *Adapter) Claim(ctx context.Context, id string) error {
	_, err := a.run(ctx, "update", id, "--claim")
	return err
}

// SetStatus sets an issue's status via `bd update <id> --status <status>`.
func (a *Adapter) SetStatus(ctx context.Context, id, status string) error {
	_, err := a.run(ctx, "update", id, "--status", status)
	return err
}

// CreateGraph feeds graphJSON to `bd create --graph` on stdin, adding
// `--dry-run` when dryRun is true, and returns bd's stdout.
func (a *Adapter) CreateGraph(ctx context.Context, graphJSON string, dryRun bool) (string, error) {
	args := []string{"create", "--graph"}
	if dryRun {
		args = append(args, "--dry-run")
	}
	res, err := a.runStdin(ctx, graphJSON, args...)
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// MergeSlotAcquire claims the merge-slot bead, retrying with exponential
// backoff (capped at 30s). retries <= 0 defaults to 3. When bd is not on PATH
// the slot is advisory: this is a logged no-op returning nil.
//
// NOTE: bd exposes no first-class slot verb, so the merge mutex is modeled as
// a claim/unclaim lease over the gt:slot bead (acquire == `bd update --claim`,
// release == `bd update --status open`).
func (a *Adapter) MergeSlotAcquire(ctx context.Context, slotID, owner string, retries int) error {
	if !a.Available() {
		fmt.Fprintf(os.Stderr, "beads: %q not on PATH; merge slot %q acquire (owner %q) is an advisory no-op\n", a.Bin, slotID, owner)
		return nil
	}
	if retries <= 0 {
		retries = 3
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			backoff := mergeSlotBackoffBase << uint(attempt-1)
			if backoff > mergeSlotBackoffCap || backoff <= 0 {
				backoff = mergeSlotBackoffCap
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		if _, err := a.run(ctx, "update", slotID, "--claim"); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("merge slot %q not acquired for owner %q after %d retries: %w", slotID, owner, retries, lastErr)
}

// MergeSlotRelease releases the merge-slot bead via `bd update --status open`.
// A logged no-op returning nil when bd is not on PATH.
func (a *Adapter) MergeSlotRelease(ctx context.Context, slotID string) error {
	if !a.Available() {
		fmt.Fprintf(os.Stderr, "beads: %q not on PATH; merge slot %q release is an advisory no-op\n", a.Bin, slotID)
		return nil
	}
	_, err := a.run(ctx, "update", slotID, "--status", "open")
	return err
}

// Snapshot tars the .beads directory (excluding lock files) to
// <RepoRoot>/.plan-logs/beads-snapshots/<utc-timestamp>.tar.gz and returns the
// archive path. Used as a Dolt-level backup before destructive operations.
func (a *Adapter) Snapshot(ctx context.Context) (string, error) {
	dir := filepath.Join(a.RepoRoot, ".plan-logs", "beads-snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	target := filepath.Join(dir, ts+".tar.gz")
	_, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir:  a.RepoRoot,
		Name: "tar",
		Args: []string{
			"-czf", target,
			"--exclude", "*.lock",
			"--exclude", "embeddeddolt/.lock",
			".beads",
		},
	})
	if err != nil {
		return "", err
	}
	return target, nil
}

// Remember persists a durable note via `bd remember <text>`.
func (a *Adapter) Remember(ctx context.Context, text string) error {
	_, err := a.run(ctx, "remember", text)
	return err
}

// List returns all non-closed open issues via `bd list --json --limit 0`.
// This is the full open corpus, not just the ready frontier. Gate and infra
// beads (gt:*, agent/rig/role/message types) are excluded by bd's default
// filters; closed issues are excluded unless --all is passed (it is not here).
func (a *Adapter) List(ctx context.Context) ([]Issue, error) {
	res, err := a.run(ctx, "list", "--json", "--limit", "0")
	if err != nil {
		return nil, err
	}
	return parseIssueList([]byte(res.Stdout))
}

// DepDigraph returns the project's dependency graph as a map from each issue ID
// to the set of issue IDs it directly depends on (its blockers), parsed from
// `bd list --format digraph`. The digraph format emits one edge per line:
// "A B" meaning A depends on B (A is blocked until B closes). The returned map
// includes only edges present in bd's digraph; missing keys have no
// dependencies. A missing binary is not an error: returns an empty map.
func (a *Adapter) DepDigraph(ctx context.Context) (map[string][]string, error) {
	if !a.Available() {
		return map[string][]string{}, nil
	}
	res, err := a.run(ctx, "list", "--format", "digraph", "--limit", "0")
	if err != nil {
		// A non-zero exit (e.g. no issues in the db) is a graceful empty.
		return map[string][]string{}, nil
	}
	deps := map[string][]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		child, parent := parts[0], parts[1]
		// "child parent" means child depends on parent — skip self-loops
		// (parent-child containment edges produce them occasionally).
		if child == parent {
			continue
		}
		deps[child] = append(deps[child], parent)
	}
	return deps, nil
}

// Available reports whether the bd binary resolves on PATH.
func (a *Adapter) Available() bool {
	return execx.LookPath(a.Bin)
}

// --- tolerant JSON parsing -------------------------------------------------

// wirePriority accepts a JSON number or a "P<n>"/"<n>" string.
type wirePriority int

func (p *wirePriority) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*p = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "P")
		s = strings.TrimPrefix(s, "p")
		if s == "" {
			*p = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("bad priority %q: %w", string(b), err)
		}
		*p = wirePriority(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*p = wirePriority(n)
	return nil
}

// wireIssue mirrors Issue but tolerates bd's looser field shapes.
type wireIssue struct {
	ID              string       `json:"id"`
	Title           string       `json:"title"`
	Description     string       `json:"description"`
	Status          string       `json:"status"`
	Priority        wirePriority `json:"priority"`
	IssueType       string       `json:"issue_type"`
	Labels          []string     `json:"labels"`
	DependencyCount int          `json:"dependency_count"`
	DependentCount  int          `json:"dependent_count"`
	ParentID        string       `json:"parent_id"`
}

func (w wireIssue) toIssue() Issue {
	labels := w.Labels
	if labels == nil {
		labels = []string{}
	}
	return Issue{
		ID:              w.ID,
		Title:           w.Title,
		Description:     w.Description,
		Status:          w.Status,
		Priority:        int(w.Priority),
		IssueType:       w.IssueType,
		Labels:          labels,
		DependencyCount: w.DependencyCount,
		DependentCount:  w.DependentCount,
		ParentID:        w.ParentID,
	}
}

// parseIssueList accepts either a JSON array or {"issues":[...]}.
func parseIssueList(data []byte) ([]Issue, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var wires []wireIssue
	switch data[0] {
	case '[':
		if err := json.Unmarshal(data, &wires); err != nil {
			return nil, fmt.Errorf("parse bd issue array: %w", err)
		}
	case '{':
		var wrap struct {
			Issues []wireIssue `json:"issues"`
		}
		if err := json.Unmarshal(data, &wrap); err != nil {
			return nil, fmt.Errorf("parse bd issue envelope: %w", err)
		}
		wires = wrap.Issues
	default:
		return nil, fmt.Errorf("unexpected bd list json: %.60s", data)
	}
	out := make([]Issue, 0, len(wires))
	for _, w := range wires {
		out = append(out, w.toIssue())
	}
	return out, nil
}

// parseIssue accepts a bare issue object, {"issue":{...}}, or a single-element
// array.
func parseIssue(data []byte) (Issue, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return Issue{}, fmt.Errorf("empty bd show output")
	}
	switch data[0] {
	case '{':
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(data, &probe); err != nil {
			return Issue{}, fmt.Errorf("parse bd issue object: %w", err)
		}
		if inner, ok := probe["issue"]; ok {
			var w wireIssue
			if err := json.Unmarshal(inner, &w); err != nil {
				return Issue{}, fmt.Errorf("parse wrapped bd issue: %w", err)
			}
			return w.toIssue(), nil
		}
		var w wireIssue
		if err := json.Unmarshal(data, &w); err != nil {
			return Issue{}, fmt.Errorf("parse bd issue: %w", err)
		}
		return w.toIssue(), nil
	case '[':
		list, err := parseIssueList(data)
		if err != nil {
			return Issue{}, err
		}
		if len(list) == 0 {
			return Issue{}, fmt.Errorf("bd show returned an empty array")
		}
		return list[0], nil
	default:
		return Issue{}, fmt.Errorf("unexpected bd show json: %.60s", data)
	}
}
