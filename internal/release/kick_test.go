// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package release_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/release"
)

// --- test helpers -----------------------------------------------------------

// openReleasePR returns a PRSummary representing an open release-please PR.
func openReleasePR(number int) release.PRSummary {
	return release.PRSummary{
		Number: number,
		Title:  "chore(main): release 1.2.0",
		URL:    fmt.Sprintf("https://github.com/owner/repo/pull/%d", number),
		State:  "open",
		Labels: []struct {
			Name string `json:"name"`
		}{{Name: release.ReleasePRLabel}},
	}
}

// nonReleasePR returns a PRSummary for a PR without the release label.
func nonReleasePR(number int) release.PRSummary {
	return release.PRSummary{
		Number: number,
		Title:  "feat: add feature",
		URL:    fmt.Sprintf("https://github.com/owner/repo/pull/%d", number),
		State:  "open",
	}
}

// --- tests ------------------------------------------------------------------

// TestKick_AutoDetect verifies that kick finds the Release PR by label and
// performs close+reopen.
func TestKick_AutoDetect(t *testing.T) {
	pr := openReleasePR(42)
	var closeCount, reopenCount int

	opts := release.KickOptions{
		Repo:   "owner/repo",
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		GHPRList: func(_, _ string) ([]release.PRSummary, error) {
			return []release.PRSummary{pr}, nil
		},
		GHPRClose:  func(_ string, _ int) error { closeCount++; return nil },
		GHPRReopen: func(_ string, _ int) error { reopenCount++; return nil },
	}

	res, err := release.Kick(opts)
	if err != nil {
		t.Fatalf("Kick: %v", err)
	}
	if res.PR.Number != 42 {
		t.Errorf("PR.Number = %d, want 42", res.PR.Number)
	}
	if !res.Closed {
		t.Error("Closed should be true")
	}
	if !res.Reopened {
		t.Error("Reopened should be true")
	}
	if closeCount != 1 {
		t.Errorf("close called %d times, want 1", closeCount)
	}
	if reopenCount != 1 {
		t.Errorf("reopen called %d times, want 1", reopenCount)
	}
}

// TestKick_ExplicitPR verifies that an explicit --pr bypasses auto-detect.
func TestKick_ExplicitPR(t *testing.T) {
	pr := openReleasePR(7)
	var listCalled bool

	opts := release.KickOptions{
		Repo:   "owner/repo",
		PR:     7,
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		GHPRList: func(_, _ string) ([]release.PRSummary, error) {
			listCalled = true
			return nil, nil
		},
		GHPRGet:    func(_ string, _ int) (*release.PRSummary, error) { return &pr, nil },
		GHPRClose:  func(_ string, _ int) error { return nil },
		GHPRReopen: func(_ string, _ int) error { return nil },
	}

	res, err := release.Kick(opts)
	if err != nil {
		t.Fatalf("Kick: %v", err)
	}
	if listCalled {
		t.Error("GHPRList should not be called when --pr is explicit")
	}
	if res.PR.Number != 7 {
		t.Errorf("PR.Number = %d, want 7", res.PR.Number)
	}
}

// TestKick_ExplicitPR_NonReleaseWarns verifies that a non-release PR with
// --pr explicit emits a warning but still proceeds.
func TestKick_ExplicitPR_NonReleaseWarns(t *testing.T) {
	pr := nonReleasePR(99)
	var stderr bytes.Buffer

	opts := release.KickOptions{
		Repo:       "owner/repo",
		PR:         99,
		Stdout:     &bytes.Buffer{},
		Stderr:     &stderr,
		GHPRGet:    func(_ string, _ int) (*release.PRSummary, error) { return &pr, nil },
		GHPRClose:  func(_ string, _ int) error { return nil },
		GHPRReopen: func(_ string, _ int) error { return nil },
	}

	res, err := release.Kick(opts)
	if err != nil {
		t.Fatalf("Kick: %v", err)
	}
	if res.PR.Number != 99 {
		t.Errorf("PR.Number = %d, want 99", res.PR.Number)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("expected warning about non-release PR, got: %q", stderr.String())
	}
}

// TestKick_NoReleasePRFound verifies that auto-detect returns an error when
// no PR with the release label is found.
func TestKick_NoReleasePRFound(t *testing.T) {
	opts := release.KickOptions{
		Repo:   "owner/repo",
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		GHPRList: func(_, _ string) ([]release.PRSummary, error) {
			return []release.PRSummary{nonReleasePR(1)}, nil
		},
	}

	_, err := release.Kick(opts)
	if err == nil {
		t.Error("expected error when no release PR found")
	}
	if !strings.Contains(err.Error(), "no open PR") {
		t.Errorf("error message should mention 'no open PR', got: %v", err)
	}
}

// TestKick_NoRepo verifies that missing --repo returns an error.
func TestKick_NoRepo(t *testing.T) {
	_, err := release.Kick(release.KickOptions{})
	if err == nil {
		t.Error("expected error with no repo")
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("error should mention --repo, got: %v", err)
	}
}

// TestKick_AlreadyClosed verifies that when the PR is already closed the
// close step is skipped and reopen still runs.
func TestKick_AlreadyClosed(t *testing.T) {
	pr := openReleasePR(5)
	pr.State = "closed"
	var closeCount, reopenCount int

	opts := release.KickOptions{
		Repo:       "owner/repo",
		PR:         5,
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		GHPRGet:    func(_ string, _ int) (*release.PRSummary, error) { return &pr, nil },
		GHPRClose:  func(_ string, _ int) error { closeCount++; return nil },
		GHPRReopen: func(_ string, _ int) error { reopenCount++; return nil },
	}

	res, err := release.Kick(opts)
	if err != nil {
		t.Fatalf("Kick: %v", err)
	}
	if res.Closed {
		t.Error("Closed should be false when PR was already closed")
	}
	if closeCount != 0 {
		t.Errorf("close called %d times, want 0", closeCount)
	}
	if reopenCount != 1 {
		t.Errorf("reopen called %d times, want 1", reopenCount)
	}
}

// TestKick_CloseError propagates close errors.
func TestKick_CloseError(t *testing.T) {
	pr := openReleasePR(3)

	opts := release.KickOptions{
		Repo:      "owner/repo",
		PR:        3,
		Stdout:    &bytes.Buffer{},
		Stderr:    &bytes.Buffer{},
		GHPRGet:   func(_ string, _ int) (*release.PRSummary, error) { return &pr, nil },
		GHPRClose: func(_ string, _ int) error { return errors.New("HTTP 422") },
	}

	_, err := release.Kick(opts)
	if err == nil {
		t.Error("expected error from close failure")
	}
	if !strings.Contains(err.Error(), "close PR") {
		t.Errorf("error should mention 'close PR', got: %v", err)
	}
}

// TestKick_Wait_AllSucceeded verifies --wait polling returns when all checks
// are succeeded.
func TestKick_Wait_AllSucceeded(t *testing.T) {
	pr := openReleasePR(10)

	concluded := []release.CheckRun{
		{Name: "build", Status: "completed", Conclusion: "success"},
		{Name: "test", Status: "completed", Conclusion: "success"},
	}

	opts := release.KickOptions{
		Repo:         "owner/repo",
		PR:           10,
		Wait:         true,
		WaitTimeout:  30 * time.Second,
		WaitInterval: time.Millisecond, // fast for tests
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
		GHPRGet:      func(_ string, _ int) (*release.PRSummary, error) { return &pr, nil },
		GHPRClose:    func(_ string, _ int) error { return nil },
		GHPRReopen:   func(_ string, _ int) error { return nil },
		GHPRChecks:   func(_ string, _ int) ([]release.CheckRun, error) { return concluded, nil },
	}

	res, err := release.Kick(opts)
	if err != nil {
		t.Fatalf("Kick: %v", err)
	}
	if res.ChecksConclusion == "" {
		t.Error("ChecksConclusion should be set when --wait is used and checks concluded")
	}
	if !strings.Contains(res.ChecksConclusion, "success") {
		t.Errorf("ChecksConclusion should contain 'success', got: %q", res.ChecksConclusion)
	}
}

// TestIsReleasePR verifies label-based detection.
func TestIsReleasePR(t *testing.T) {
	with := openReleasePR(1)
	without := nonReleasePR(2)

	if !with.IsReleasePR() {
		t.Error("PR with release label should be a release PR")
	}
	if without.IsReleasePR() {
		t.Error("PR without release label should not be a release PR")
	}
}
