// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package intake polls a project's GitHub issues (via the `gh` CLI — never a
// direct API token) that carry a trigger label and files one PLANNING bead per
// issue. It is idempotent: an issue already ingested (a bead carrying
// --external-ref gh-<owner>/<repo>#<number>) is skipped. When multiple sources
// are configured the owner/repo-qualified key prevents cross-repo collisions;
// a backward-compat search for the pre-v1 key ("gh-<number>") is also performed
// so beads created by older intake runs are not re-ingested.
//
// Every ingested bead carries the label `no-dispatch` so the autonomous wave
// engine never builds it directly — an ingested issue is a planning input that
// a human or planner must triage first. Intake never mutates GitHub state
// except for the opt-in comment-back; it never labels or closes issues in v1.
package intake

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/registry"
)

// Defaults for Options.
const (
	DefaultLabel = "triage"
	DefaultLimit = 20
)

// Mandatory labels applied to every ingested bead. The gh-<number> provenance
// label is prepended per issue.
const (
	labelIntake     = "intake"
	labelNoDispatch = "no-dispatch" // MANDATORY: ingested issues need review before dispatch
)

// Options configures one intake run.
type Options struct {
	Project     *registry.Record // required; Remote must be a GitHub remote
	Source      Source           // issue-tracker provider; nil = GitHub CLI (gh)
	Label       string           // trigger label; default "triage"
	Limit       int              // max issues to poll; default 20
	DryRun      bool             // print intent, mutate nothing
	CommentBack bool             // opt-in: comment the bead id back on the issue
}

// Item is one issue's outcome.
type Item struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	BeadID string `json:"bead_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Result splits the polled issues into those ingested and those skipped.
type Result struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	Ingested []Item `json:"ingested"`
	Skipped  []Item `json:"skipped"`
}

// Run polls the project's labeled issues and files a planning bead per new
// issue. It reads through a Source (default: GitHub CLI) and `bd`; the only
// GitHub mutation is the opt-in comment-back. In DryRun mode it performs reads
// only.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Project == nil {
		return nil, fmt.Errorf("intake: project record is required")
	}
	owner, repo, err := ParseGitHubRemote(opts.Project.Remote)
	if err != nil {
		return nil, err
	}
	label := opts.Label
	if label == "" {
		label = DefaultLabel
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}

	src := opts.Source
	if src == nil {
		gh := newGH(opts.Project.Root)
		if !execx.LookPath(gh.bin) {
			return nil, fmt.Errorf("intake: %q not found on PATH (install the GitHub CLI)", gh.bin)
		}
		src = gh
	}

	bd := beads.New(opts.Project.Root)
	if v := os.Getenv("KORYPH_BD_BIN"); v != "" {
		bd.Bin = v
	}
	if !bd.Available() {
		return nil, fmt.Errorf("intake: bd (%q) not found on PATH", bd.Bin)
	}

	issues, err := src.List(ctx, owner, repo, label, limit)
	if err != nil {
		return nil, fmt.Errorf("intake: list issues %s/%s: %w", owner, repo, err)
	}

	// legacyProvenancer is an optional interface implemented by Sources that
	// can also produce the pre-v1 unqualified key (e.g. "gh-42") for
	// backward-compatible deduplication of beads created before repo
	// qualification was introduced.
	type legacyProvenancer interface {
		legacyProvenance(number int) string
	}

	res := &Result{Owner: owner, Repo: repo}
	for _, iss := range issues {
		provKey := src.Provenance(owner, repo, iss.Number)

		// Idempotency: look up by external-ref (the canonical dedup key). Fall
		// back to the provenance label for beads created before external-ref was
		// introduced, so a first run after an upgrade does not re-ingest them.
		existing, derr := bd.ListByExternalRef(ctx, provKey)
		if derr != nil {
			return nil, fmt.Errorf("intake: dedupe check for #%d: %w", iss.Number, derr)
		}
		if len(existing) == 0 {
			existing, derr = bd.ListByLabel(ctx, provKey)
			if derr != nil {
				return nil, fmt.Errorf("intake: dedupe label fallback for #%d: %w", iss.Number, derr)
			}
		}
		// Backward-compat: also check the pre-v1 unqualified key (gh-<number>)
		// so beads created by older koryph intake runs are not re-ingested.
		if len(existing) == 0 {
			if lp, ok := src.(legacyProvenancer); ok {
				oldKey := lp.legacyProvenance(iss.Number)
				existing, _ = bd.ListByExternalRef(ctx, oldKey)
				if len(existing) == 0 {
					existing, _ = bd.ListByLabel(ctx, oldKey)
				}
			}
		}
		if len(existing) > 0 {
			res.Skipped = append(res.Skipped, Item{
				Number: iss.Number,
				Title:  iss.Title,
				BeadID: existing[0].ID,
				Reason: "already ingested",
			})
			continue
		}

		item := Item{Number: iss.Number, Title: iss.Title}
		if opts.DryRun {
			item.Reason = "would ingest (dry-run)"
			res.Ingested = append(res.Ingested, item)
			continue
		}

		id, cerr := bd.Create(ctx, beads.CreateInput{
			Title:       iss.Title,
			Description: buildDescription(owner, repo, iss),
			Labels:      []string{provKey, labelIntake, labelNoDispatch},
			Priority:    priorityFor(iss),
			IssueType:   issueTypeFor(iss),
			ExternalRef: provKey,
		})
		if cerr != nil {
			return nil, fmt.Errorf("intake: create bead for #%d: %w", iss.Number, cerr)
		}
		item.BeadID = id

		if opts.CommentBack {
			body := fmt.Sprintf("Tracked as bead %s for planning.", id)
			if gerr := src.Comment(ctx, owner, repo, iss.Number, body); gerr != nil {
				// Non-fatal: the bead already exists; record the miss.
				item.Reason = "comment-back failed: " + gerr.Error()
			} else {
				item.Reason = "commented"
			}
		}
		res.Ingested = append(res.Ingested, item)
	}
	return res, nil
}

// priorityFor maps a p0/p1/p2/p3 issue label to that bd priority (0..3),
// defaulting to 2 when none is present.
func priorityFor(iss SourceIssue) int {
	for _, l := range iss.Labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "p0":
			return 0
		case "p1":
			return 1
		case "p2":
			return 2
		case "p3":
			return 3
		}
	}
	return 2
}

// issueTypeFor passes a `bug`-labeled issue through as the bd `bug` type;
// everything else uses bd's default type ("").
func issueTypeFor(iss SourceIssue) string {
	for _, l := range iss.Labels {
		if strings.EqualFold(strings.TrimSpace(l), "bug") {
			return "bug"
		}
	}
	return ""
}

// buildDescription is the issue body plus a provenance footer.
func buildDescription(owner, repo string, iss SourceIssue) string {
	footer := fmt.Sprintf(
		"Source: github.com/%s/%s/issues/%d, author @%s, ingested by koryph intake",
		owner, repo, iss.Number, iss.Author,
	)
	body := strings.TrimRight(iss.Body, "\n")
	if body == "" {
		return footer
	}
	return body + "\n\n---\n" + footer
}

// --- remote parsing --------------------------------------------------------

// ParseGitHubRemote extracts owner/repo from a GitHub remote in either HTTPS
// (`https://github.com/owner/repo.git`) or SSH/scp
// (`git@github.com:owner/repo.git`, `ssh://git@github.com/owner/repo.git`)
// form. It errors on an empty, malformed, or non-GitHub remote.
func ParseGitHubRemote(remote string) (owner, repo string, err error) {
	r := strings.TrimSpace(remote)
	if r == "" {
		return "", "", fmt.Errorf("intake: project has no remote configured")
	}

	var host, path string
	switch {
	case strings.Contains(r, "://"):
		u, perr := url.Parse(r)
		if perr != nil {
			return "", "", fmt.Errorf("intake: parse remote %q: %w", remote, perr)
		}
		host = u.Host
		path = strings.TrimPrefix(u.Path, "/")
	case strings.Contains(r, ":"):
		// scp-like: [user@]host:owner/repo
		hostPath := r
		if at := strings.LastIndex(hostPath, "@"); at >= 0 {
			hostPath = hostPath[at+1:]
		}
		colon := strings.Index(hostPath, ":")
		host = hostPath[:colon]
		path = hostPath[colon+1:]
	default:
		return "", "", fmt.Errorf("intake: unrecognized remote %q", remote)
	}

	// Strip any leftover userinfo and port from the host.
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if colon := strings.IndexByte(host, ':'); colon >= 0 {
		host = host[:colon]
	}
	host = strings.ToLower(host)
	if host != "github.com" && !strings.HasSuffix(host, ".github.com") {
		return "", "", fmt.Errorf("intake: remote %q is not a GitHub remote (host %q)", remote, host)
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[len(parts)-1] == "" {
		return "", "", fmt.Errorf("intake: cannot parse owner/repo from remote %q", remote)
	}
	// owner is the first segment; repo is the last (handles enterprise
	// path prefixes defensively).
	return parts[0], parts[len(parts)-1], nil
}
