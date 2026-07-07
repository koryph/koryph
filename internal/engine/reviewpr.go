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
	"strconv"
	"strings"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
)

// PRMeta is the pull-request metadata review-pr needs.
type PRMeta struct {
	Number  int
	Author  string
	URL     string
	Title   string
	State   string
	Draft   bool
	HeadSHA string // PR head commit; anchors inline review comments
}

// LineComment is one inline PR review comment anchored to a file line.
type LineComment struct {
	Path string
	Line int
	Body string
}

// PRHost abstracts the GitHub operations `koryph review-pr` needs so tests run
// without a live GitHub remote. GhHost is the default gh-CLI implementation.
type PRHost interface {
	// Viewer returns the login of the authenticated user (the approver).
	Viewer(ctx context.Context, dir string) (login string, err error)
	// Info returns metadata for the PR selector (number/branch/url).
	Info(ctx context.Context, dir, selector string) (PRMeta, error)
	// List returns the open pull requests (for the queue loop).
	List(ctx context.Context, dir string) ([]PRMeta, error)
	// Checkout materializes the PR head in an ephemeral worktree for review and
	// returns its path, the head ref, and a cleanup func.
	Checkout(ctx context.Context, dir, selector string) (worktree, ref string, cleanup func(), err error)
	// Approve registers an approving review on the PR as the viewer identity.
	Approve(ctx context.Context, dir, selector, body string) error
	// ReviewComment posts a review with inline line comments (event COMMENT),
	// anchored to headSHA, in a single call.
	ReviewComment(ctx context.Context, dir string, number int, headSHA, body string, comments []LineComment) error
	// Close closes the PR, optionally leaving a comment.
	Close(ctx context.Context, dir, selector, comment string) error
}

// PRReviewer runs the reviewer over a checked-out PR. It defaults to
// review.Review; tests inject a fake to avoid spawning an agent.
type PRReviewer func(ctx context.Context, o review.Opts) review.Verdict

// ReviewPROpts configures ReviewPR.
type ReviewPROpts struct {
	Selector string        // PR number, branch, or URL
	Approve  bool          // register an approving review (the operator's explicit instruction)
	Comment  bool          // post koryph's findings as inline PR comments
	Lines    []LineComment // operator-specified inline comments to post
	Resume   bool          // re-display the persisted analysis (after an IDE handoff)
	Close    bool          // close the PR
	Body     string        // review/approval body, or the close comment
	Out      io.Writer     // human-readable output; nil = silent
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

	// A PR that ended by any means (merged or closed in the UI, by another
	// tool, or by koryph) is reconciled here: drop any stale saved analysis
	// and report the terminal state instead of acting on a dead PR.
	if meta.State == "MERGED" || meta.State == "CLOSED" {
		clearPRState(rec, meta.Number)
		res.Verdict = strings.ToLower(meta.State)
		if o.Out != nil {
			fmt.Fprintf(o.Out, "PR #%d is %s — nothing to review (local state reconciled)\n", meta.Number, strings.ToLower(meta.State))
		}
		return res, nil
	}

	if o.Close {
		if err := host.Close(ctx, rec.Root, o.Selector, o.Body); err != nil {
			return res, err
		}
		res.Verdict = "closed"
		if o.Out != nil {
			fmt.Fprintf(o.Out, "closed PR #%d (%s)\n", meta.Number, meta.URL)
		}
		return res, nil
	}

	if o.Resume {
		return resumePR(rec, meta, o.Out)
	}

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

	if o.Comment || len(o.Lines) > 0 {
		return commentPR(ctx, rec, cfg, host, reviewer, meta, o)
	}

	// Analysis: check out the PR head and run the reviewer over its diff.
	return analyzePR(ctx, rec, cfg, host, reviewer, meta, o.Selector, o.Out)
}

// commentPR posts inline PR review comments: koryph's line-anchored findings
// (when o.Comment) plus any operator-specified lines, in a single COMMENT
// review. Findings without a line fold into the review body. Approval stays a
// separate step.
func commentPR(ctx context.Context, rec *registry.Record, cfg *project.Config, host PRHost, reviewer PRReviewer, meta PRMeta, o ReviewPROpts) (ReviewPRResult, error) {
	res := ReviewPRResult{Number: meta.Number, Author: meta.Author, URL: meta.URL, Verdict: "commented"}
	comments := append([]LineComment(nil), o.Lines...)
	bodyLines := []string{}
	if strings.TrimSpace(o.Body) != "" {
		bodyLines = append(bodyLines, o.Body)
	}

	if o.Comment {
		ar, err := analyzePR(ctx, rec, cfg, host, reviewer, meta, o.Selector, o.Out)
		if err != nil {
			return res, err
		}
		res.Blocking, res.Degraded, res.Findings = ar.Blocking, ar.Degraded, ar.Findings
		if ar.Degraded {
			return res, fmt.Errorf("review degraded; not posting comments")
		}
		lc, general := findingComments(ar.Findings)
		comments = append(comments, lc...)
		bodyLines = append(bodyLines, general...)
	}

	if len(comments) == 0 && len(bodyLines) == 0 {
		if o.Out != nil {
			fmt.Fprintf(o.Out, "PR #%d: nothing to comment (no line-anchored findings or operator lines)\n", meta.Number)
		}
		return res, nil
	}
	if err := host.ReviewComment(ctx, rec.Root, meta.Number, meta.HeadSHA, strings.Join(bodyLines, "\n\n"), comments); err != nil {
		return res, err
	}
	if o.Out != nil {
		fmt.Fprintf(o.Out, "posted %d inline comment(s) on PR #%d (%s)\n", len(comments), meta.Number, meta.URL)
	}
	return res, nil
}

// findingComments splits reviewer findings into inline line comments (those
// with a file and a 1-based line) and general body bullets (the rest).
func findingComments(findings []review.Finding) (inline []LineComment, general []string) {
	for _, f := range findings {
		if f.File != "" && f.Line > 0 {
			inline = append(inline, LineComment{Path: f.File, Line: f.Line, Body: fmt.Sprintf("[%s] %s", f.Severity, f.Summary)})
			continue
		}
		loc := f.File
		if loc == "" {
			loc = "(general)"
		}
		general = append(general, fmt.Sprintf("- [%s] %s: %s", f.Severity, loc, f.Summary))
	}
	return inline, general
}

// QueueResult reports a review-queue pass.
type QueueResult struct {
	Analyzed []ReviewPRResult
	Skipped  []SkippedPR
}

// SkippedPR records a PR the queue did not analyze and why.
type SkippedPR struct {
	Number int
	Reason string
}

// ReviewQueue analyzes every open PR in turn — skipping drafts and PRs the
// operator authored (which they cannot self-approve) — until the queue is
// cleared or the context is cancelled (a clean operator stop). Like the
// single-PR path it only ANALYZES; approval stays an explicit per-PR
// instruction. Skips are logged, never silent.
func ReviewQueue(ctx context.Context, rec *registry.Record, cfg *project.Config, host PRHost, reviewer PRReviewer, out io.Writer) (QueueResult, error) {
	if host == nil {
		host = GhHost{}
	}
	if reviewer == nil {
		reviewer = review.Review
	}
	viewer, _ := host.Viewer(ctx, rec.Root) // best-effort; blank just disables the self-authored skip
	prs, err := host.List(ctx, rec.Root)
	if err != nil {
		return QueueResult{}, err
	}

	var q QueueResult
	for _, pr := range prs {
		if ctx.Err() != nil {
			if out != nil {
				fmt.Fprintln(out, "stopped: queue interrupted")
			}
			break
		}
		switch {
		case pr.Draft:
			q.Skipped = append(q.Skipped, SkippedPR{pr.Number, "draft"})
			logSkip(out, pr.Number, "draft")
			continue
		case viewer != "" && strings.EqualFold(pr.Author, viewer):
			q.Skipped = append(q.Skipped, SkippedPR{pr.Number, "authored by you"})
			logSkip(out, pr.Number, "authored by you ("+viewer+")")
			continue
		}
		if out != nil {
			fmt.Fprintf(out, "\n=== PR #%d ===\n", pr.Number)
		}
		res, aerr := analyzePR(ctx, rec, cfg, host, reviewer, pr, strconv.Itoa(pr.Number), out)
		if aerr != nil {
			q.Skipped = append(q.Skipped, SkippedPR{pr.Number, "analysis error: " + aerr.Error()})
			logSkip(out, pr.Number, "analysis error: "+aerr.Error())
			continue
		}
		q.Analyzed = append(q.Analyzed, res)
	}
	if out != nil {
		fmt.Fprintf(out, "\nqueue: analyzed %d, skipped %d\n", len(q.Analyzed), len(q.Skipped))
	}
	return q, nil
}

func logSkip(out io.Writer, number int, reason string) {
	if out != nil {
		fmt.Fprintf(out, "skip PR #%d: %s\n", number, reason)
	}
}

// analyzePR checks out one PR head, runs the reviewer over its diff, prints the
// analysis, and returns the structured result. Shared by the single-PR and
// queue paths.
func analyzePR(ctx context.Context, rec *registry.Record, cfg *project.Config, host PRHost, reviewer PRReviewer, meta PRMeta, selector string, out io.Writer) (ReviewPRResult, error) {
	res := ReviewPRResult{Number: meta.Number, Author: meta.Author, URL: meta.URL}
	wt, ref, cleanup, err := host.Checkout(ctx, rec.Root, selector)
	if err != nil {
		return res, err
	}
	defer cleanup()

	// AccountFor (koryph-v8u.5) falls back to rec.ClaudeConfigDir for every
	// project today (no runtime_accounts entry) — same ConfigDir as before.
	ra := rec.AccountFor(resolvedRuntimeName)
	prReviewPersona := modelroute.PersonaFor(modelroute.StageReview, cfg.Stages)
	// Same koryph-77r.8 fix as the in-loop reviewer call site (poll.go): honor
	// the persona's declared frontmatter effort instead of silently dropping it.
	prReviewEffort := ""
	if _, metaEffort, _, err := modelroute.PersonaMeta(rec.Root, prReviewPersona); err == nil {
		prReviewEffort = metaEffort
	}
	v := reviewer(ctx, review.Opts{
		RepoRoot:  rec.Root,
		Worktree:  wt,
		Branch:    ref,
		Base:      rec.DefaultBranch,
		Persona:   prReviewPersona,
		Model:     modelroute.TierOpus,
		Effort:    prReviewEffort,
		Profile:   account.Profile{Name: rec.AccountProfile, ConfigDir: ra.ConfigDir},
		ClaudeBin: os.Getenv(envClaudeBin),
		// Deliberately the project's live config, not a bead-scoped arm
		// (koryph-3l1.3): `koryph review-pr`/review-queue reviews arbitrary
		// open GitHub PRs on operator demand — outside the wave/rolling
		// dispatch loop entirely, with no ledger.Slot and so no bead arm to
		// follow (a PR here may not even have been dispatched by koryph). Out
		// of scope for the standing-canary comparison, which is built from
		// ledger data this call path never writes.
		ProxyBaseURL: rec.ProxyBaseURL(),
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
	// Persist the analysis so the operator can hand off to an IDE and resume
	// (post comments / approve) without re-running the reviewer.
	if !v.Degraded {
		savePRState(rec, prReviewState{
			Number: meta.Number, HeadSHA: meta.HeadSHA, Blocking: v.Blocking, Findings: v.Findings,
		})
	}
	if out != nil {
		printPRAnalysis(out, meta, v)
	}
	return res, nil
}

// prReviewState is the persisted analysis for a PR, enabling the IDE-handoff
// loop: analyze in koryph, review in the IDE, resume in koryph.
type prReviewState struct {
	Number   int              `json:"number"`
	HeadSHA  string           `json:"head_sha"`
	Blocking bool             `json:"blocking"`
	Findings []review.Finding `json:"findings"`
}

func prStatePath(rec *registry.Record, number int) string {
	return filepath.Join(paths.KoryphHome(), "review-pr", rec.ProjectID, fmt.Sprintf("pr-%d.json", number))
}

func savePRState(rec *registry.Record, st prReviewState) {
	p := prStatePath(rec, st.Number)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return // best-effort: persistence is a convenience, not required
	}
	_ = fsx.WriteJSONAtomic(p, st)
}

func loadPRState(rec *registry.Record, number int) (prReviewState, bool) {
	var st prReviewState
	if err := fsx.ReadJSON(prStatePath(rec, number), &st); err != nil {
		return prReviewState{}, false
	}
	return st, true
}

func clearPRState(rec *registry.Record, number int) {
	_ = os.Remove(prStatePath(rec, number))
}

// resumePR re-displays the persisted analysis for a PR after an IDE handoff,
// without re-running the reviewer, and warns when the PR head has moved since.
func resumePR(rec *registry.Record, meta PRMeta, out io.Writer) (ReviewPRResult, error) {
	st, ok := loadPRState(rec, meta.Number)
	if !ok {
		return ReviewPRResult{Number: meta.Number}, fmt.Errorf("no saved analysis for PR #%d; run `koryph review-pr %d` first", meta.Number, meta.Number)
	}
	res := ReviewPRResult{Number: meta.Number, Author: meta.Author, URL: meta.URL, Blocking: st.Blocking, Findings: st.Findings}
	res.Verdict = "clean"
	if st.Blocking {
		res.Verdict = "blocking"
	}
	if out != nil {
		printPRAnalysis(out, meta, review.Verdict{Blocking: st.Blocking, Findings: st.Findings})
		if meta.HeadSHA != "" && st.HeadSHA != "" && meta.HeadSHA != st.HeadSHA {
			fmt.Fprintf(out, "\nNOTE: the PR head moved since this analysis (%.12s → %.12s); re-run `koryph review-pr %d` for a fresh review.\n",
				st.HeadSHA, meta.HeadSHA, meta.Number)
		}
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
		"pr", "view", selector, "--json", "number,url,title,state,author,headRefOid,isDraft",
	}})
	if err != nil {
		return PRMeta{}, err
	}
	if res.ExitCode != 0 {
		return PRMeta{}, fmt.Errorf("gh pr view %s: %s", selector, strings.TrimSpace(tailOf(res.Stderr, 300)))
	}
	var v struct {
		Number     int    `json:"number"`
		URL        string `json:"url"`
		Title      string `json:"title"`
		State      string `json:"state"`
		IsDraft    bool   `json:"isDraft"`
		HeadRefOid string `json:"headRefOid"`
		Author     struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &v); err != nil {
		return PRMeta{}, fmt.Errorf("parse gh pr view: %w", err)
	}
	return PRMeta{Number: v.Number, URL: v.URL, Title: v.Title, State: v.State, Draft: v.IsDraft, HeadSHA: v.HeadRefOid, Author: v.Author.Login}, nil
}

// ReviewComment posts a COMMENT-event review with inline line comments in one
// call via the GitHub reviews API (gh api resolves {owner}/{repo}).
func (GhHost) ReviewComment(ctx context.Context, dir string, number int, headSHA, body string, comments []LineComment) error {
	type apiComment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Side string `json:"side"`
		Body string `json:"body"`
	}
	payload := struct {
		CommitID string       `json:"commit_id,omitempty"`
		Event    string       `json:"event"`
		Body     string       `json:"body,omitempty"`
		Comments []apiComment `json:"comments,omitempty"`
	}{CommitID: headSHA, Event: "COMMENT", Body: body}
	for _, c := range comments {
		payload.Comments = append(payload.Comments, apiComment{Path: c.Path, Line: c.Line, Side: "RIGHT", Body: c.Body})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Stdin: string(data), Args: []string{
		"api", "--method", "POST", fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/reviews", number), "--input", "-",
	}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("gh api create review: %s", strings.TrimSpace(tailOf(res.Stderr, 400)))
	}
	return nil
}

// List returns the open pull requests via `gh pr list --json`.
func (GhHost) List(ctx context.Context, dir string) ([]PRMeta, error) {
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: []string{
		"pr", "list", "--state", "open", "--limit", "200",
		"--json", "number,url,title,state,author,isDraft,headRefOid",
	}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("gh pr list: %s", strings.TrimSpace(tailOf(res.Stderr, 300)))
	}
	var raw []struct {
		Number     int    `json:"number"`
		URL        string `json:"url"`
		Title      string `json:"title"`
		State      string `json:"state"`
		IsDraft    bool   `json:"isDraft"`
		HeadRefOid string `json:"headRefOid"`
		Author     struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
		return nil, fmt.Errorf("parse gh pr list: %w", err)
	}
	prs := make([]PRMeta, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, PRMeta{
			Number: r.Number, URL: r.URL, Title: r.Title, State: r.State,
			Draft: r.IsDraft, HeadSHA: r.HeadRefOid, Author: r.Author.Login,
		})
	}
	return prs, nil
}

// Close closes the PR, optionally with a comment.
func (GhHost) Close(ctx context.Context, dir, selector, comment string) error {
	args := []string{"pr", "close", selector}
	if strings.TrimSpace(comment) != "" {
		args = append(args, "--comment", comment)
	}
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: args})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("gh pr close: %s", strings.TrimSpace(tailOf(res.Stderr, 400)))
	}
	return nil
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
