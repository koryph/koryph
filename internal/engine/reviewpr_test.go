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
	meta       PRMeta
	viewer     string
	approved   bool
	approveBod string
}

func (f *fakePRHost) Viewer(context.Context, string) (string, error)       { return f.viewer, nil }
func (f *fakePRHost) Info(context.Context, string, string) (PRMeta, error) { return f.meta, nil }
func (f *fakePRHost) Checkout(context.Context, string, string) (string, string, func(), error) {
	return "", "deadbeef", func() {}, nil
}
func (f *fakePRHost) Approve(_ context.Context, _, _, body string) error {
	f.approved = true
	f.approveBod = body
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
