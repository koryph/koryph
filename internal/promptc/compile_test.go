// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package promptc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
)

func baseInput() Input {
	return Input{
		EngineVersion: "v9.9.9-test",
		ProjectName:   "demo-project",
		Conventions:   "Run gofmt. Keep commits small.",
		Gate:          []string{"make lint", "make test"},
		CrossGates:    []string{"make schema-check"},
		Bootstrap:     []string{"pnpm install --frozen-lockfile"},
		Bead: beads.Issue{
			ID:          "bd-42",
			Title:       "Wire the widget",
			Description: "Implement the widget and its handler.",
		},
		PhaseDir:    "/phases/p1",
		SummaryPath: "/phases/p1/SUMMARY.md",
		StatusPath:  "/phases/p1/status.json",
		LogPath:     "/phases/p1/log.txt",
	}
}

func TestCompileThreeSectionsInOrder(t *testing.T) {
	out := Compile(baseInput())

	// Exactly two separators when no section body contains "\n---\n".
	if n := strings.Count(out, sectionSep); n != 2 {
		t.Fatalf("separator count = %d, want 2\n%s", n, out)
	}

	iPreamble := strings.Index(out, "# Koryph dispatch (engine v9.9.9-test)")
	iProject := strings.Index(out, "## Project: demo-project")
	iTask := strings.Index(out, "## Task bd-42: Wire the widget")
	if iPreamble != 0 {
		t.Errorf("preamble should start the prompt, index = %d", iPreamble)
	}
	if !(iPreamble < iProject && iProject < iTask) {
		t.Errorf("sections out of order: preamble=%d project=%d task=%d", iPreamble, iProject, iTask)
	}

	// Project block content present.
	for _, want := range []string{
		"Run gofmt. Keep commits small.",
		"Green gate (keep these green):",
		"- make lint",
		"- make test",
		"Cross-cutting gates:",
		"- make schema-check",
		"Worktree bootstrap (already run for you, rerun if needed):",
		"- pnpm install --frozen-lockfile",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}

	// Reporting paths always present.
	for _, want := range []string{
		"### Reporting paths",
		"/phases/p1/SUMMARY.md",
		"/phases/p1/status.json",
		"/phases/p1/log.txt",
		"/phases/p1/INBOX.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing reporting path %q", want)
		}
	}
}

func TestCompileDeterministic(t *testing.T) {
	in := baseInput()
	in.PlanYAML = "steps:\n  - build\n  - test"
	a := Compile(in)
	b := Compile(in)
	if a != b {
		t.Fatalf("Compile is not deterministic:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

func TestPreambleForbiddenOperations(t *testing.T) {
	p := Preamble("v1")
	for _, want := range []string{
		"git checkout main",
		"git merge",
		"git push",
		"bd close",
		"gh pr merge",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("preamble missing forbidden op %q", want)
		}
	}
	// Reporting contract markers.
	for _, want := range []string{
		`{"state","step","pct"}`,
		"$KORYPH_STATUS_PATH",
		"$KORYPH_LOG_PATH",
		"$KORYPH_SUMMARY_PATH",
		"What shipped",
		"Changes requiring orchestrator review",
		"INBOX.md",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("preamble missing reporting marker %q", want)
		}
	}
}

// TestPreambleInboxCheckedAtStartAndFinish is the koryph-o72 leg-3 regression
// test: the preamble must tell the agent to read INBOX.md at the start and
// again before finishing, not just "between steps" — a nudge landed after
// dispatch but before the first poll (or right before the agent wraps up)
// would otherwise go unseen.
func TestPreambleInboxCheckedAtStartAndFinish(t *testing.T) {
	p := Preamble("v1")
	if !strings.Contains(p, "when you start") {
		t.Errorf("preamble missing an explicit at-start INBOX.md check: %q", p)
	}
	if !strings.Contains(p, "before you finish") {
		t.Errorf("preamble missing an explicit before-finishing INBOX.md check: %q", p)
	}
}

func TestResumeAndReviewBlocksConditional(t *testing.T) {
	// Absent by default.
	out := Compile(baseInput())
	if strings.Contains(out, "### RESUMING") {
		t.Errorf("RESUMING block should be absent when ResumeSHA is empty")
	}
	if strings.Contains(out, "### Blocking review findings") {
		t.Errorf("review block should be absent when ReviewPath is empty")
	}

	// Present when set.
	in := baseInput()
	in.ResumeSHA = "abc1234"
	in.ReviewPath = "/phases/p1/review.md"
	out = Compile(in)
	if !strings.Contains(out, "### RESUMING") {
		t.Errorf("RESUMING block missing when ResumeSHA is set")
	}
	if !strings.Contains(out, "git log --oneline abc1234..HEAD") {
		t.Errorf("RESUMING block missing the commit-log guidance")
	}
	if !strings.Contains(out, "### Blocking review findings") {
		t.Errorf("review block missing when ReviewPath is set")
	}
	if !strings.Contains(out, "/phases/p1/review.md") {
		t.Errorf("review block missing the review path")
	}
}

func TestPlanBlockConditional(t *testing.T) {
	out := Compile(baseInput())
	if strings.Contains(out, "### Execution plan (koryph-plan)") {
		t.Errorf("plan block should be absent when PlanYAML is empty")
	}
	in := baseInput()
	in.PlanYAML = "steps:\n  - one"
	out = Compile(in)
	if !strings.Contains(out, "### Execution plan (koryph-plan)") {
		t.Errorf("plan block missing when PlanYAML is set")
	}
	if !strings.Contains(out, "```yaml\nsteps:\n  - one\n```") {
		t.Errorf("plan block not fenced as expected:\n%s", out)
	}
}

// TestOperatorNotesAppearVerbatim is the koryph-o72 leg-1 regression test: a
// note appended to a bead pre-dispatch (bd's --append-notes, which lands in
// Issue.Notes via the adapter) must appear verbatim in the compiled prompt,
// clearly delimited from the original description.
func TestOperatorNotesAppearVerbatim(t *testing.T) {
	// Absent by default — no phantom section when notes are empty.
	out := Compile(baseInput())
	if strings.Contains(out, "OPERATOR NOTES") {
		t.Errorf("OPERATOR NOTES section should be absent when Bead.Notes is empty")
	}

	in := baseInput()
	in.Bead.Notes = "scope also covers the retry path — do not ship without it"
	out = Compile(in)
	if !strings.Contains(out, "### OPERATOR NOTES") {
		t.Errorf("OPERATOR NOTES section missing when Bead.Notes is set:\n%s", out)
	}
	if !strings.Contains(out, "scope also covers the retry path — do not ship without it") {
		t.Errorf("operator note text missing verbatim from compiled prompt:\n%s", out)
	}
	// Must appear in the volatile tail (after the task heading), not the
	// cache-stable preamble/project sections.
	iTask := strings.Index(out, "## Task bd-42")
	iNotes := strings.Index(out, "### OPERATOR NOTES")
	if iNotes < iTask {
		t.Errorf("OPERATOR NOTES section should be in the volatile tail (after %d), got index %d", iTask, iNotes)
	}
}

// TestNoTimestampsInStableSections asserts that neither the engine preamble
// nor the project block leaks a timestamp — here proxied by the current year.
func TestNoTimestampsInStableSections(t *testing.T) {
	year := fmt.Sprintf("%d", time.Now().Year())
	out := Compile(baseInput())
	parts := strings.SplitN(out, sectionSep, 3)
	if len(parts) != 3 {
		t.Fatalf("expected 3 sections, got %d", len(parts))
	}
	if strings.Contains(parts[0], year) {
		t.Errorf("section [1] preamble contains the current year %q", year)
	}
	if strings.Contains(parts[1], year) {
		t.Errorf("section [2] project block contains the current year %q", year)
	}
}
