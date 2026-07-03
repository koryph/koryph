// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
)

// PRMeta is the pull-request metadata review-pr needs.
type PRMeta struct {
	Number int
	Author string
	URL    string
	Title  string
	State  string
}

// PRHost abstracts the GitHub operations `koryph review-pr` needs so tests run
// without a live GitHub remote. GhHost is the default gh-CLI implementation.
type PRHost interface {
	// Viewer returns the login of the authenticated user (the approver).
	Viewer(ctx context.Context, dir string) (login string, err error)
	// Info returns metadata for the PR selector (number/branch/url).
	Info(ctx context.Context, dir, selector string) (PRMeta, error)
	// Checkout materializes the PR head in an ephemeral worktree for review and
	// returns its path, the head ref, and a cleanup func.
	Checkout(ctx context.Context, dir, selector string) (worktree, ref string, cleanup func(), err error)
	// Approve registers an approving review on the PR as the viewer identity.
	Approve(ctx context.Context, dir, selector, body string) error
}

// PRReviewer runs the reviewer over a checked-out PR. It defaults to
// review.Review; tests inject a fake to avoid spawning an agent.
type PRReviewer func(ctx context.Context, o review.Opts) review.Verdict

// ReviewPROpts configures ReviewPR.
type ReviewPROpts struct {
	Selector string    // PR number, branch, or URL
	Approve  bool      // register an approving review (the operator's explicit instruction)
	Body     string    // optional review/approval body
	Out      io.Writer // human-readable output; nil = silent
}

// ReviewPRResult reports the outcome.
type ReviewPRResult struct {
	Number   int
	Author   string
	URL      string
	Verdict  string // clean|blocking|degraded (analysis) or approved
	Approved bool
	Blocking bool
	Degraded bool
	Findings []review.Finding
}

// ReviewPR analyzes someone else's PR with koryph's reviewer and prints the
// analysis, OR — when o.Approve is set (the operator's explicit instruction
// after reading the analysis) — registers an approving review. Analysis and
// approval are DISTINCT steps: koryph never approves on its own; the operator
// may override the model's verdict and approve anyway. The approving identity
// is the operator's, so it must differ from the PR author (GitHub rejects
// self-approval); that case surfaces as a clear error pointing at direct merge.
func ReviewPR(ctx context.Context, rec *registry.Record, cfg *project.Config, host PRHost, reviewer PRReviewer, o ReviewPROpts) (ReviewPRResult, error) {
	if host == nil {
		host = GhHost{}
	}
	if reviewer == nil {
		reviewer = review.Review
	}
	if o.Selector == "" {
		return ReviewPRResult{}, fmt.Errorf("review-pr: a PR (number/branch/url) is required")
	}

	meta, err := host.Info(ctx, rec.Root, o.Selector)
	if err != nil {
		return ReviewPRResult{}, err
	}
	res := ReviewPRResult{Number: meta.Number, Author: meta.Author, URL: meta.URL}

	if o.Approve {
		if login, verr := host.Viewer(ctx, rec.Root); verr == nil && login != "" && strings.EqualFold(login, meta.Author) {
			return res, fmt.Errorf("cannot approve PR #%d: you (%s) are its author — GitHub rejects self-approval; merge it directly (koryph land / merge_policy auto with a branch-protection bypass) or have another maintainer approve", meta.Number, login)
		}
		if err := host.Approve(ctx, rec.Root, o.Selector, o.Body); err != nil {
			return res, err
		}
		res.Approved = true
		res.Verdict = "approved"
		if o.Out != nil {
			fmt.Fprintf(o.Out, "approved PR #%d (%s)\n", meta.Number, meta.URL)
		}
		return res, nil
	}

	// Analysis: check out the PR head and run the reviewer over its diff.
	wt, ref, cleanup, err := host.Checkout(ctx, rec.Root, o.Selector)
	if err != nil {
		return res, err
	}
	defer cleanup()

	v := reviewer(ctx, review.Opts{
		RepoRoot:  rec.Root,
		Worktree:  wt,
		Branch:    ref,
		Base:      rec.DefaultBranch,
		Persona:   modelroute.PersonaFor(modelroute.StageReview, cfg.Stages),
		Model:     modelroute.TierOpus,
		Profile:   account.Profile{Name: rec.AccountProfile, ConfigDir: rec.ClaudeConfigDir},
		ClaudeBin: os.Getenv(envClaudeBin),
	})
	res.Blocking, res.Degraded, res.Findings = v.Blocking, v.Degraded, v.Findings
	switch {
	case v.Degraded:
		res.Verdict = "degraded"
	case v.Blocking:
		res.Verdict = "blocking"
	default:
		res.Verdict = "clean"
	}
	if o.Out != nil {
		printPRAnalysis(o.Out, meta, v)
	}
	return res, nil
}

// printPRAnalysis renders koryph's analysis for the operator to read before
// deciding whether to approve.
func printPRAnalysis(w io.Writer, meta PRMeta, v review.Verdict) {
	fmt.Fprintf(w, "PR #%d by %s — %s\n%s\n\n", meta.Number, meta.Author, meta.Title, meta.URL)
	if v.Degraded {
		fmt.Fprintf(w, "ANALYSIS DEGRADED: %s\n", v.Reason)
		fmt.Fprintln(w, "koryph could not complete the analysis; do not approve on this basis.")
		return
	}
	if v.Blocking {
		fmt.Fprintln(w, "VERDICT: koryph flagged blocking issues — examine carefully before approving.")
	} else {
		fmt.Fprintln(w, "VERDICT: koryph found no blocking issues.")
	}
	if len(v.Findings) == 0 {
		fmt.Fprintln(w, "  (no specific findings)")
	}
	for _, f := range v.Findings {
		loc := f.File
		if loc == "" {
			loc = "(general)"
		}
		fmt.Fprintf(w, "  [%s] %s: %s\n", f.Severity, loc, f.Summary)
	}
	fmt.Fprintln(w, "\nThis is koryph's analysis, not an approval. Examine the flagged code, then")
	fmt.Fprintln(w, "instruct approval with:  koryph review-pr --project <id> <pr> --approve")
}

// GhHost is the gh-CLI implementation of PRHost.
type GhHost struct{}

// Viewer returns the authenticated gh user's login.
func (GhHost) Viewer(ctx context.Context, dir string) (string, error) {
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: []string{"api", "user", "--jq", ".login"}})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("gh api user: %s", strings.TrimSpace(tailOf(res.Stderr, 300)))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Info reads PR metadata via `gh pr view --json`.
func (GhHost) Info(ctx context.Context, dir, selector string) (PRMeta, error) {
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: []string{
		"pr", "view", selector, "--json", "number,url,title,state,author",
	}})
	if err != nil {
		return PRMeta{}, err
	}
	if res.ExitCode != 0 {
		return PRMeta{}, fmt.Errorf("gh pr view %s: %s", selector, strings.TrimSpace(tailOf(res.Stderr, 300)))
	}
	var v struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &v); err != nil {
		return PRMeta{}, fmt.Errorf("parse gh pr view: %w", err)
	}
	return PRMeta{Number: v.Number, URL: v.URL, Title: v.Title, State: v.State, Author: v.Author.Login}, nil
}

// Checkout fetches the PR head and adds an ephemeral detached worktree at it.
func (GhHost) Checkout(ctx context.Context, dir, selector string) (string, string, func(), error) {
	var meta struct {
		Number     int    `json:"number"`
		HeadRefOid string `json:"headRefOid"`
	}
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: []string{
		"pr", "view", selector, "--json", "number,headRefOid",
	}})
	if err != nil {
		return "", "", nil, err
	}
	if res.ExitCode != 0 {
		return "", "", nil, fmt.Errorf("gh pr view %s: %s", selector, strings.TrimSpace(tailOf(res.Stderr, 300)))
	}
	if err := json.Unmarshal([]byte(res.Stdout), &meta); err != nil {
		return "", "", nil, fmt.Errorf("parse gh pr view: %w", err)
	}

	if r, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "git", Args: []string{
		"fetch", "origin", fmt.Sprintf("pull/%d/head", meta.Number),
	}}); err != nil {
		return "", "", nil, err
	} else if r.ExitCode != 0 {
		return "", "", nil, fmt.Errorf("fetch pull/%d/head: %s", meta.Number, strings.TrimSpace(tailOf(r.Stderr, 300)))
	}

	base, err := os.MkdirTemp("", fmt.Sprintf("koryph-pr-%d-", meta.Number))
	if err != nil {
		return "", "", nil, err
	}
	wt := filepath.Join(base, "wt")
	if r, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "git", Args: []string{
		"worktree", "add", "--detach", wt, meta.HeadRefOid,
	}}); err != nil {
		_ = os.RemoveAll(base)
		return "", "", nil, err
	} else if r.ExitCode != 0 {
		_ = os.RemoveAll(base)
		return "", "", nil, fmt.Errorf("worktree add PR head: %s", strings.TrimSpace(tailOf(r.Stderr, 300)))
	}
	cleanup := func() {
		_, _ = execx.Run(ctx, execx.Cmd{Dir: dir, Name: "git", Args: []string{"worktree", "remove", "--force", wt}})
		_ = os.RemoveAll(base)
	}
	return wt, meta.HeadRefOid, cleanup, nil
}

// Approve registers an approving review as the authenticated (operator) identity.
func (GhHost) Approve(ctx context.Context, dir, selector, body string) error {
	args := []string{"pr", "review", selector, "--approve"}
	if strings.TrimSpace(body) != "" {
		args = append(args, "--body", body)
	}
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: args})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("gh pr review --approve: %s", strings.TrimSpace(tailOf(res.Stderr, 400)))
	}
	return nil
}
