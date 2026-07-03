// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
)

// fakePRHost implements PRHost without a live GitHub remote.
type fakePRHost struct {
	meta         PRMeta
	list         []PRMeta
	viewer       string
	approved     bool
	approveBod   string
	checkedOut   []string
	comments     []LineComment
	commentBody  string
	commentHead  string
	closed       bool
	closeComment string
}

func (f *fakePRHost) Viewer(context.Context, string) (string, error)       { return f.viewer, nil }
func (f *fakePRHost) Info(context.Context, string, string) (PRMeta, error) { return f.meta, nil }
func (f *fakePRHost) List(context.Context, string) ([]PRMeta, error)       { return f.list, nil }
func (f *fakePRHost) Checkout(_ context.Context, _, selector string) (string, string, func(), error) {
	f.checkedOut = append(f.checkedOut, selector)
	return "", "deadbeef", func() {}, nil
}
func (f *fakePRHost) Approve(_ context.Context, _, _, body string) error {
	f.approved = true
	f.approveBod = body
	return nil
}

func (f *fakePRHost) ReviewComment(_ context.Context, _ string, _ int, headSHA, body string, comments []LineComment) error {
	f.commentBody = body
	f.commentHead = headSHA
	f.comments = append(f.comments, comments...)
	return nil
}

func (f *fakePRHost) Close(_ context.Context, _, _, comment string) error {
	f.closed = true
	f.closeComment = comment
	return nil
}

func fakeReviewer(v review.Verdict) PRReviewer {
	return func(context.Context, review.Opts) review.Verdict { return v }
}

// TestReviewPRAnalysisPrintsFindingsNeverApproves: analysis mode surfaces the
// reviewer's findings for the operator and never registers an approval.
func TestReviewPRAnalysisPrintsFindingsNeverApproves(t *testing.T) {
	host := &fakePRHost{meta: PRMeta{Number: 5, Author: "alice", URL: "https://x/pull/5", Title: "Add feature"}}
	v := review.Verdict{Blocking: true, Findings: []review.Finding{
		{Severity: "blocking", File: "a.go", Summary: "possible nil deref"},
	}}
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main", AccountProfile: "work"}

	var out bytes.Buffer
	res, err := ReviewPR(context.Background(), rec, &project.Config{}, host, fakeReviewer(v),
		ReviewPROpts{Selector: "5", Out: &out})
	if err != nil {
		t.Fatalf("ReviewPR: %v", err)
	}
	if res.Verdict != "blocking" {
		t.Errorf("verdict=%q, want blocking", res.Verdict)
	}
	if res.Approved || host.approved {
		t.Error("analysis mode must never approve")
	}
	s := out.String()
	if !strings.Contains(s, "possible nil deref") || !strings.Contains(s, "a.go") {
		t.Errorf("analysis output missing the finding:\n%s", s)
	}
	if !strings.Contains(s, "not an approval") {
		t.Errorf("analysis output should make clear it is not an approval:\n%s", s)
	}
}

// TestReviewPRApproveRegistersApproval: --approve registers the operator's
// approval via the host (with the body) and reports it.
func TestReviewPRApproveRegistersApproval(t *testing.T) {
	host := &fakePRHost{meta: PRMeta{Number: 7, Author: "bob", URL: "https://x/pull/7"}, viewer: "maintainer"}
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main"}

	var out bytes.Buffer
	res, err := ReviewPR(context.Background(), rec, &project.Config{}, host, nil,
		ReviewPROpts{Selector: "7", Approve: true, Body: "LGTM", Out: &out})
	if err != nil {
		t.Fatalf("ReviewPR: %v", err)
	}
	if !res.Approved || res.Verdict != "approved" {
		t.Errorf("res=%+v, want approved", res)
	}
	if !host.approved || host.approveBod != "LGTM" {
		t.Errorf("host approved=%v body=%q, want true/LGTM", host.approved, host.approveBod)
	}
}

// TestReviewPRCommentPostsAgentFindingsAsLineComments: --comment turns
// line-anchored findings into inline comments and folds the rest into the body.
func TestReviewPRCommentPostsAgentFindingsAsLineComments(t *testing.T) {
	host := &fakePRHost{meta: PRMeta{Number: 5, Author: "alice", HeadSHA: "abc123"}}
	v := review.Verdict{Findings: []review.Finding{
		{Severity: "major", File: "a.go", Line: 42, Summary: "unchecked error"},
		{Severity: "minor", File: "b.go", Summary: "general note (no line)"},
	}}
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main"}

	var out bytes.Buffer
	_, err := ReviewPR(context.Background(), rec, &project.Config{}, host, fakeReviewer(v),
		ReviewPROpts{Selector: "5", Comment: true, Out: &out})
	if err != nil {
		t.Fatalf("ReviewPR: %v", err)
	}
	if len(host.comments) != 1 {
		t.Fatalf("posted %d inline comments, want 1 (only the line-anchored finding)", len(host.comments))
	}
	c := host.comments[0]
	if c.Path != "a.go" || c.Line != 42 || !strings.Contains(c.Body, "unchecked error") {
		t.Errorf("inline comment = %+v, want a.go:42 with the finding", c)
	}
	if host.commentHead != "abc123" {
		t.Errorf("comment anchored to %q, want the PR head abc123", host.commentHead)
	}
	if !strings.Contains(host.commentBody, "general note") {
		t.Errorf("body should carry the line-less finding:\n%s", host.commentBody)
	}
}

// TestReviewPRCommentOnPostsOperatorLines: operator-specified --comment-on
// lines are posted without needing an analysis.
func TestReviewPRCommentOnPostsOperatorLines(t *testing.T) {
	host := &fakePRHost{meta: PRMeta{Number: 8, Author: "bob", HeadSHA: "def456"}}
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main"}

	_, err := ReviewPR(context.Background(), rec, &project.Config{}, host, nil, ReviewPROpts{
		Selector: "8",
		Lines:    []LineComment{{Path: "main.go", Line: 10, Body: "please rename"}},
	})
	if err != nil {
		t.Fatalf("ReviewPR: %v", err)
	}
	if len(host.comments) != 1 || host.comments[0].Path != "main.go" || host.comments[0].Line != 10 {
		t.Fatalf("comments = %+v, want main.go:10", host.comments)
	}
}

// TestReviewPRClose closes the PR via the host with the given comment.
func TestReviewPRClose(t *testing.T) {
	host := &fakePRHost{meta: PRMeta{Number: 3, Author: "carol", URL: "https://x/pull/3"}}
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main"}

	res, err := ReviewPR(context.Background(), rec, &project.Config{}, host, nil,
		ReviewPROpts{Selector: "3", Close: true, Body: "superseded"})
	if err != nil {
		t.Fatalf("ReviewPR: %v", err)
	}
	if res.Verdict != "closed" || !host.closed || host.closeComment != "superseded" {
		t.Errorf("res=%+v closed=%v comment=%q, want closed with 'superseded'", res, host.closed, host.closeComment)
	}
}

// TestReviewPRResumeReplaysPersistedAnalysis: analysis persists state; --resume
// re-displays it without re-running the reviewer, and flags a moved head.
func TestReviewPRResumeReplaysPersistedAnalysis(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main", ProjectID: "proj"}
	finding := review.Finding{Severity: "major", File: "x.go", Line: 3, Summary: "leaky handle"}

	// 1. Analyze at head "sha-1" (persists state).
	analyzeHost := &fakePRHost{meta: PRMeta{Number: 12, Author: "dan", HeadSHA: "sha-1"}}
	if _, err := ReviewPR(context.Background(), rec, &project.Config{}, analyzeHost,
		fakeReviewer(review.Verdict{Blocking: true, Findings: []review.Finding{finding}}),
		ReviewPROpts{Selector: "12", Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// 2. Resume at the SAME head — replays the finding, no staleness note.
	var out bytes.Buffer
	resumeHost := &fakePRHost{meta: PRMeta{Number: 12, Author: "dan", HeadSHA: "sha-1"}}
	if _, err := ReviewPR(context.Background(), rec, &project.Config{}, resumeHost, nil,
		ReviewPROpts{Selector: "12", Resume: true, Out: &out}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(resumeHost.checkedOut) != 0 {
		t.Error("resume must not check out / re-run the reviewer")
	}
	if !strings.Contains(out.String(), "leaky handle") {
		t.Errorf("resume did not replay the persisted finding:\n%s", out.String())
	}
	if strings.Contains(out.String(), "head moved") {
		t.Errorf("unexpected staleness note when head is unchanged:\n%s", out.String())
	}

	// 3. Resume at a MOVED head — flags staleness.
	var out2 bytes.Buffer
	movedHost := &fakePRHost{meta: PRMeta{Number: 12, Author: "dan", HeadSHA: "sha-2"}}
	if _, err := ReviewPR(context.Background(), rec, &project.Config{}, movedHost, nil,
		ReviewPROpts{Selector: "12", Resume: true, Out: &out2}); err != nil {
		t.Fatalf("resume2: %v", err)
	}
	if !strings.Contains(out2.String(), "head moved") {
		t.Errorf("expected a staleness note after the head moved:\n%s", out2.String())
	}
}

// TestReviewQueueSkipsDraftsAndSelfAuthored: --all analyzes every eligible open
// PR and skips drafts and PRs authored by the operator, with logged reasons.
func TestReviewQueueSkipsDraftsAndSelfAuthored(t *testing.T) {
	host := &fakePRHost{
		viewer: "me",
		list: []PRMeta{
			{Number: 1, Author: "alice"},            // eligible
			{Number: 2, Author: "me"},               // self-authored → skip
			{Number: 3, Author: "bob", Draft: true}, // draft → skip
			{Number: 4, Author: "carol"},            // eligible
		},
	}
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main"}

	var out bytes.Buffer
	q, err := ReviewQueue(context.Background(), rec, &project.Config{}, host,
		fakeReviewer(review.Verdict{}), &out)
	if err != nil {
		t.Fatalf("ReviewQueue: %v", err)
	}
	if len(q.Analyzed) != 2 || len(q.Skipped) != 2 {
		t.Fatalf("analyzed=%d skipped=%d, want 2/2 (%+v)", len(q.Analyzed), len(q.Skipped), q)
	}
	// Only the eligible PRs were checked out (by number).
	if strings.Join(host.checkedOut, ",") != "1,4" {
		t.Errorf("checked out %v, want [1 4]", host.checkedOut)
	}
	s := out.String()
	if !strings.Contains(s, "skip PR #2: authored by you") {
		t.Errorf("missing self-authored skip reason:\n%s", s)
	}
	if !strings.Contains(s, "skip PR #3: draft") {
		t.Errorf("missing draft skip reason:\n%s", s)
	}
}

// TestReviewQueueStopsOnCancel: a cancelled context stops the loop cleanly
// before processing further PRs.
func TestReviewQueueStopsOnCancel(t *testing.T) {
	host := &fakePRHost{list: []PRMeta{{Number: 1, Author: "a"}, {Number: 2, Author: "b"}}}
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the loop starts

	var out bytes.Buffer
	q, err := ReviewQueue(ctx, rec, &project.Config{}, host, fakeReviewer(review.Verdict{}), &out)
	if err != nil {
		t.Fatalf("ReviewQueue: %v", err)
	}
	if len(q.Analyzed) != 0 {
		t.Errorf("analyzed=%d, want 0 (cancelled before any PR)", len(q.Analyzed))
	}
	if !strings.Contains(out.String(), "interrupted") {
		t.Errorf("expected an interrupted notice:\n%s", out.String())
	}
}

// TestReviewPRApproveRefusesSelfApproval: approving your own PR is refused up
// front with a clear error (GitHub rejects self-approval).
func TestReviewPRApproveRefusesSelfApproval(t *testing.T) {
	host := &fakePRHost{meta: PRMeta{Number: 9, Author: "me"}, viewer: "me"}
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main"}

	_, err := ReviewPR(context.Background(), rec, &project.Config{}, host, nil,
		ReviewPROpts{Selector: "9", Approve: true})
	if err == nil || !strings.Contains(err.Error(), "self-approval") {
		t.Fatalf("err=%v, want a self-approval refusal", err)
	}
	if host.approved {
		t.Error("host.Approve called despite the self-approval refusal")
	}
}
