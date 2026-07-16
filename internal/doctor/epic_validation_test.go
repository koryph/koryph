// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/epicreview"
)

// --- fixture-locked tests ---------------------------------------------------

// fakeEpics injects a static list of epics (no bd subprocess).
func fakeEpics(issues ...beads.Issue) func(string) ([]beads.Issue, error) {
	return func(_ string) ([]beads.Issue, error) {
		return issues, nil
	}
}

// makeParkedEpic builds a minimal parked epic with the note format written by
// engine.parkEpic. Round cap: at round N, the note says "round N would exceed
// max_rounds=M".
func makeParkedEpic(id, title string, exceedRound, maxRounds int) beads.Issue {
	note := fmt.Sprintf(
		"validation parked: round %d would exceed max_rounds=%d. Operator recovery: koryph epic validate %s --project testproject",
		exceedRound, maxRounds, id)
	return beads.Issue{
		ID:        id,
		Title:     title,
		IssueType: "epic",
		Status:    "open",
		Labels:    []string{"validation:parked"},
		Notes:     note,
	}
}

// makeDegradedEpic builds a minimal degraded epic with the note format written
// by epicreview.Act when the validator infra fails.
func makeDegradedEpic(id, title string, round int, reason string) beads.Issue {
	note := fmt.Sprintf("validation:degraded (round %d): %s", round, reason)
	return beads.Issue{
		ID:        id,
		Title:     title,
		IssueType: "epic",
		Status:    "open",
		Labels:    []string{"validation:degraded"},
		Notes:     note,
	}
}

// --- checkEpicValidations unit tests ----------------------------------------

// TestEpicValidationsParkedAppearsInReport verifies that an open epic labeled
// validation:parked appears in the doctor report at LevelWarn with its round
// and reason.
func TestEpicValidationsParkedAppearsInReport(t *testing.T) {
	epic := makeParkedEpic("koryph-abc", "some feature epic", 3, 2)
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(epic),
	}
	findings := checkEpicValidations(opts, opts.RepoRoot)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(findings), findings)
	}
	f := findings[0]
	if f.Check != checkNameEpicValidations {
		t.Errorf("check = %q, want %q", f.Check, checkNameEpicValidations)
	}
	if f.Level != LevelWarn {
		t.Errorf("level = %q, want %q", f.Level, LevelWarn)
	}
	if !strings.Contains(f.Message, "koryph-abc") {
		t.Errorf("message missing epic ID %q: %s", "koryph-abc", f.Message)
	}
	if !strings.Contains(f.Message, "round=3") {
		t.Errorf("message missing round=3: %s", f.Message)
	}
	if !strings.Contains(f.Message, "max_rounds=2") {
		t.Errorf("message missing max_rounds=2: %s", f.Message)
	}
	if !strings.Contains(f.Message, "parked") {
		t.Errorf("message should mention parked: %s", f.Message)
	}
}

// TestEpicValidationsDegradedAppearsInReport verifies that an open epic labeled
// validation:degraded appears in the doctor report at LevelWarn with its round
// and reason.
func TestEpicValidationsDegradedAppearsInReport(t *testing.T) {
	epic := makeDegradedEpic("koryph-xyz", "big redesign epic", 1, "claude subprocess exited 1")
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(epic),
	}
	findings := checkEpicValidations(opts, opts.RepoRoot)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(findings), findings)
	}
	f := findings[0]
	if f.Level != LevelWarn {
		t.Errorf("level = %q, want %q", f.Level, LevelWarn)
	}
	if !strings.Contains(f.Message, "koryph-xyz") {
		t.Errorf("message missing epic ID: %s", f.Message)
	}
	if !strings.Contains(f.Message, "round=1") {
		t.Errorf("message missing round=1: %s", f.Message)
	}
	if !strings.Contains(f.Message, "claude subprocess exited 1") {
		t.Errorf("message missing reason: %s", f.Message)
	}
	if !strings.Contains(f.Message, "degraded") {
		t.Errorf("message should mention degraded: %s", f.Message)
	}
}

// TestEpicValidationsBothLabels verifies that when both a parked and a degraded
// epic are open, both appear as separate LevelWarn findings.
func TestEpicValidationsBothLabels(t *testing.T) {
	parked := makeParkedEpic("koryph-p1", "parked epic", 3, 2)
	degraded := makeDegradedEpic("koryph-d1", "degraded epic", 2, "timeout after 420s")
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(parked, degraded),
	}
	findings := checkEpicValidations(opts, opts.RepoRoot)

	if len(findings) != 2 {
		t.Fatalf("want 2 findings, got %d: %v", len(findings), findings)
	}
	for _, f := range findings {
		if f.Level != LevelWarn {
			t.Errorf("finding %q: level = %q, want warn", f.Message, f.Level)
		}
	}
}

// TestEpicValidationsCleanEpicNotReported verifies that an open epic with
// neither label does not produce any finding (returns a clean OK finding).
func TestEpicValidationsCleanEpicNotReported(t *testing.T) {
	clean := beads.Issue{
		ID:        "koryph-c1",
		Title:     "active epic no validation label",
		IssueType: "epic",
		Status:    "open",
		Labels:    []string{"area:sched"},
	}
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(clean),
	}
	findings := checkEpicValidations(opts, opts.RepoRoot)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding (ok), got %d: %v", len(findings), findings)
	}
	if findings[0].Level != LevelOK {
		t.Errorf("level = %q, want ok", findings[0].Level)
	}
	if strings.Contains(findings[0].Message, "koryph-c1") {
		t.Errorf("clean epic should not appear in finding: %s", findings[0].Message)
	}
}

// TestEpicValidationsClosedParkedEpicSkipped verifies that a closed epic
// carrying validation:parked is NOT reported — it has already been resolved.
func TestEpicValidationsClosedParkedEpicSkipped(t *testing.T) {
	closed := makeParkedEpic("koryph-old", "closed parked epic", 3, 2)
	closed.Status = "closed"
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(closed),
	}
	findings := checkEpicValidations(opts, opts.RepoRoot)

	if len(findings) != 1 || findings[0].Level != LevelOK {
		t.Errorf("closed parked epic should not warn; got: %v", findings)
	}
}

// TestEpicValidationsNoBd verifies that when bd is unavailable (nil result,
// nil error) the check returns OK, not a spurious warn.
func TestEpicValidationsNoBd(t *testing.T) {
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: func(_ string) ([]beads.Issue, error) { return nil, nil },
	}
	findings := checkEpicValidations(opts, opts.RepoRoot)

	if len(findings) != 1 || findings[0].Level != LevelOK {
		t.Errorf("bd-unavailable should yield OK; got: %v", findings)
	}
}

// TestEpicValidationsListError verifies that a list error surfaces as LevelWarn.
func TestEpicValidationsListError(t *testing.T) {
	opts := ProjectOptions{
		RepoRoot: t.TempDir(),
		ListEpics: func(_ string) ([]beads.Issue, error) {
			return nil, fmt.Errorf("bd: exit status 1")
		},
	}
	findings := checkEpicValidations(opts, opts.RepoRoot)

	if len(findings) != 1 || findings[0].Level != LevelWarn {
		t.Errorf("list error should yield warn; got: %v", findings)
	}
	if !strings.Contains(findings[0].Message, "cannot list epics") {
		t.Errorf("unexpected message: %s", findings[0].Message)
	}
}

// --- RunProject integration test --------------------------------------------

// TestRunProjectEpicValidationsIntegrated verifies that checkEpicValidations
// appears in the RunProject output when ListEpics is injected.
func TestRunProjectEpicValidationsIntegrated(t *testing.T) {
	root := fabricateProject(t)
	parked := makeParkedEpic("koryph-ep1", "integrated parked epic", 3, 2)
	opts := projectOpts(root)
	opts.ListEpics = fakeEpics(parked)

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameEpicValidations)
	if f.Level != LevelWarn {
		t.Errorf("RunProject: epic-validations level = %q, want warn", f.Level)
	}
	if !strings.Contains(f.Message, "koryph-ep1") {
		t.Errorf("RunProject: epic-validations message missing epic ID: %s", f.Message)
	}
	// ExitCode should be 1 (warnings)
	if code := r.ExitCode(); code != 1 {
		t.Errorf("ExitCode = %d, want 1 (warnings present)", code)
	}
}

// --- checkUnvalidatedEpics unit tests ----------------------------------------

// fakeChildrenAll injects a static open-epic-id -> children map (no bd
// subprocess).
func fakeChildrenAll(byEpic map[string][]beads.Issue) func(string, string) ([]beads.Issue, error) {
	return func(_ string, epicID string) ([]beads.Issue, error) {
		return byEpic[epicID], nil
	}
}

// TestUnvalidatedEpicsAllClosedChildrenWarns verifies that an open, unlabeled
// epic whose every child is closed produces a LevelWarn finding naming the
// recovery command — the offline mirror of
// TestPatrolCheckUnvalidatedEpics_ReQueuesAndValidationStarts in
// internal/engine/health_test.go.
func TestUnvalidatedEpicsAllClosedChildrenWarns(t *testing.T) {
	epic := beads.Issue{ID: "koryph-duzu", Title: "stranded epic", IssueType: "epic", Status: "open"}
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(epic),
		ListChildrenAll: fakeChildrenAll(map[string][]beads.Issue{
			"koryph-duzu": {
				{ID: "c1", Status: "closed"},
				{ID: "c2", Status: "done"},
			},
		}),
	}
	findings := checkUnvalidatedEpics(opts, opts.RepoRoot)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(findings), findings)
	}
	f := findings[0]
	if f.Check != checkNameUnvalidatedEpics {
		t.Errorf("check = %q, want %q", f.Check, checkNameUnvalidatedEpics)
	}
	if f.Level != LevelWarn {
		t.Errorf("level = %q, want warn", f.Level)
	}
	if !strings.Contains(f.Message, "koryph-duzu") {
		t.Errorf("message missing epic ID: %s", f.Message)
	}
	if !strings.Contains(f.Message, "koryph epic validate koryph-duzu") {
		t.Errorf("message missing recovery command: %s", f.Message)
	}
}

// TestUnvalidatedEpicsPassedLabelStillWarns verifies the close-after-docs
// stall case: an epic already carrying validation:passed with all children
// (including the docs bead) closed still warns, with a reason naming the
// stalled close-after-docs path.
func TestUnvalidatedEpicsPassedLabelStillWarns(t *testing.T) {
	epic := beads.Issue{
		ID: "koryph-q9v", Title: "passed but unclosed", IssueType: "epic", Status: "open",
		Labels: []string{epicreview.LabelPassed},
	}
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(epic),
		ListChildrenAll: fakeChildrenAll(map[string][]beads.Issue{
			"koryph-q9v": {{ID: "docs-1", Status: "closed"}},
		}),
	}
	findings := checkUnvalidatedEpics(opts, opts.RepoRoot)

	if len(findings) != 1 || findings[0].Level != LevelWarn {
		t.Fatalf("want 1 warn finding, got %v", findings)
	}
	if !strings.Contains(findings[0].Message, "close-after-docs") {
		t.Errorf("message should name the close-after-docs stall: %s", findings[0].Message)
	}
}

// TestUnvalidatedEpicsOpenChildNotReported verifies an epic with at least one
// still-open child is not flagged — it is genuinely still in progress.
func TestUnvalidatedEpicsOpenChildNotReported(t *testing.T) {
	epic := beads.Issue{ID: "koryph-e1", Title: "in progress", IssueType: "epic", Status: "open"}
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(epic),
		ListChildrenAll: fakeChildrenAll(map[string][]beads.Issue{
			"koryph-e1": {{ID: "c1", Status: "closed"}, {ID: "c2", Status: "open"}},
		}),
	}
	findings := checkUnvalidatedEpics(opts, opts.RepoRoot)

	if len(findings) != 1 || findings[0].Level != LevelOK {
		t.Errorf("epic with an open child should not warn; got: %v", findings)
	}
}

// TestUnvalidatedEpicsChildlessNotReported verifies a genuinely childless
// epic (never decomposed) is not flagged — len(children) == 0 must stay a
// valid "nothing to validate" signal, not be conflated with "all closed".
func TestUnvalidatedEpicsChildlessNotReported(t *testing.T) {
	epic := beads.Issue{ID: "koryph-e2", Title: "never decomposed", IssueType: "epic", Status: "open"}
	opts := ProjectOptions{
		RepoRoot:        t.TempDir(),
		ListEpics:       fakeEpics(epic),
		ListChildrenAll: fakeChildrenAll(nil),
	}
	findings := checkUnvalidatedEpics(opts, opts.RepoRoot)

	if len(findings) != 1 || findings[0].Level != LevelOK {
		t.Errorf("childless epic should not warn; got: %v", findings)
	}
}

// TestUnvalidatedEpicsSkipsParkedDegradedNoValidate verifies parked,
// degraded, and no-validate epics are left to checkEpicValidations (or are
// deliberately excluded) rather than double-reported here, even when every
// child is closed.
func TestUnvalidatedEpicsSkipsParkedDegradedNoValidate(t *testing.T) {
	for _, label := range []string{epicreview.LabelParked, epicreview.LabelDegraded, epicreview.LabelNoValidate} {
		epic := beads.Issue{ID: "koryph-sk", Title: "skip me", IssueType: "epic", Status: "open", Labels: []string{label}}
		opts := ProjectOptions{
			RepoRoot:  t.TempDir(),
			ListEpics: fakeEpics(epic),
			ListChildrenAll: fakeChildrenAll(map[string][]beads.Issue{
				"koryph-sk": {{ID: "c1", Status: "closed"}},
			}),
		}
		findings := checkUnvalidatedEpics(opts, opts.RepoRoot)
		if len(findings) != 1 || findings[0].Level != LevelOK {
			t.Errorf("label %s: epic must not be reported; got %v", label, findings)
		}
	}
}

// TestUnvalidatedEpicsClosedEpicSkipped verifies an already-closed epic is
// never inspected, regardless of its children.
func TestUnvalidatedEpicsClosedEpicSkipped(t *testing.T) {
	epic := beads.Issue{ID: "koryph-done", Title: "already closed", IssueType: "epic", Status: "closed"}
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(epic),
		ListChildrenAll: fakeChildrenAll(map[string][]beads.Issue{
			"koryph-done": {{ID: "c1", Status: "closed"}},
		}),
	}
	findings := checkUnvalidatedEpics(opts, opts.RepoRoot)
	if len(findings) != 1 || findings[0].Level != LevelOK {
		t.Errorf("closed epic should not warn; got: %v", findings)
	}
}

// TestUnvalidatedEpicsListChildrenError verifies a per-epic list-children
// error surfaces as its own LevelWarn finding rather than aborting the scan.
func TestUnvalidatedEpicsListChildrenError(t *testing.T) {
	epic := beads.Issue{ID: "koryph-e3", Title: "boom", IssueType: "epic", Status: "open"}
	opts := ProjectOptions{
		RepoRoot:  t.TempDir(),
		ListEpics: fakeEpics(epic),
		ListChildrenAll: func(_, _ string) ([]beads.Issue, error) {
			return nil, fmt.Errorf("bd: exit status 1")
		},
	}
	findings := checkUnvalidatedEpics(opts, opts.RepoRoot)
	if len(findings) != 1 || findings[0].Level != LevelWarn {
		t.Fatalf("want 1 warn finding, got %v", findings)
	}
	if !strings.Contains(findings[0].Message, "koryph-e3") || !strings.Contains(findings[0].Message, "list children") {
		t.Errorf("unexpected message: %s", findings[0].Message)
	}
}

// TestRunProjectUnvalidatedEpicsIntegrated verifies that checkUnvalidatedEpics
// appears in the RunProject output when both injection seams are set.
func TestRunProjectUnvalidatedEpicsIntegrated(t *testing.T) {
	root := fabricateProject(t)
	epic := beads.Issue{ID: "koryph-int1", Title: "integrated stranded epic", IssueType: "epic", Status: "open"}
	opts := projectOpts(root)
	opts.ListEpics = fakeEpics(epic)
	opts.ListChildrenAll = fakeChildrenAll(map[string][]beads.Issue{
		"koryph-int1": {{ID: "c1", Status: "closed"}},
	})

	r, err := RunProject(opts)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameUnvalidatedEpics)
	if f.Level != LevelWarn {
		t.Errorf("RunProject: unvalidated-epics level = %q, want warn", f.Level)
	}
	if !strings.Contains(f.Message, "koryph-int1") {
		t.Errorf("RunProject: unvalidated-epics message missing epic ID: %s", f.Message)
	}
	if code := r.ExitCode(); code != 1 {
		t.Errorf("ExitCode = %d, want 1 (warnings present)", code)
	}
}

// --- note-parsing unit tests ------------------------------------------------

func TestParseDegradedNote(t *testing.T) {
	cases := []struct {
		name       string
		notes      string
		wantRound  int
		wantReason string
	}{
		{
			name:       "single line",
			notes:      "validation:degraded (round 1): claude subprocess exited 1",
			wantRound:  1,
			wantReason: "claude subprocess exited 1",
		},
		{
			name:       "multiline notes, last line wins",
			notes:      "some earlier note\nvalidation:degraded (round 1): first failure\nvalidation:degraded (round 2): second failure",
			wantRound:  2,
			wantReason: "second failure",
		},
		{
			name:      "no matching line",
			notes:     "just a regular note",
			wantRound: 0,
		},
		{
			name:       "leading whitespace",
			notes:      "  validation:degraded (round 3): timeout after 420s  ",
			wantRound:  3,
			wantReason: "timeout after 420s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			round, reason := parseDegradedNote(tc.notes)
			if round != tc.wantRound {
				t.Errorf("round = %d, want %d", round, tc.wantRound)
			}
			if tc.wantReason != "" && reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

func TestParseParkedNote(t *testing.T) {
	cases := []struct {
		name       string
		notes      string
		wantRound  int
		wantMaxStr string
	}{
		{
			name:       "basic",
			notes:      "validation parked: round 3 would exceed max_rounds=2. Operator recovery: koryph epic validate x-1 --project myproj",
			wantRound:  3,
			wantMaxStr: "max_rounds=2",
		},
		{
			name:       "multiline",
			notes:      "early note\nvalidation parked: round 2 would exceed max_rounds=2. Operator recovery: ...",
			wantRound:  2,
			wantMaxStr: "max_rounds=2",
		},
		{
			name:      "no matching line",
			notes:     "unrelated note",
			wantRound: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			round, reason := parseParkedNote(tc.notes)
			if round != tc.wantRound {
				t.Errorf("round = %d, want %d", round, tc.wantRound)
			}
			if tc.wantMaxStr != "" && !strings.Contains(reason, tc.wantMaxStr) {
				t.Errorf("reason = %q, want it to contain %q", reason, tc.wantMaxStr)
			}
		})
	}
}
