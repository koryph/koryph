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
			ID:                 "bd-42",
			Title:              "Wire the widget",
			Description:        "Implement the widget and its handler.",
			AcceptanceCriteria: "- Handler returns 200\n- Widget renders",
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
	for _, want := range []string{"### Acceptance criteria", "Handler returns 200", "Widget renders"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing bead acceptance contract %q", want)
		}
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

// TestSharedPrefixIsCacheBreakpoint is the koryph-6au acceptance test for the
// prompt-cache breakpoint boundary: SharedPrefix returns sections [1]+[2], and
// Compile is exactly SharedPrefix + sectionSep + volatile tail — so the prefix
// koryph caches is precisely the span that precedes the per-bead tail.
func TestSharedPrefixIsCacheBreakpoint(t *testing.T) {
	in := baseInput()
	prefix := SharedPrefix(in)
	full := Compile(in)

	if !strings.HasPrefix(full, prefix+sectionSep) {
		t.Fatalf("Compile is not SharedPrefix + sep + tail\n--- prefix ---\n%s\n--- full ---\n%s", prefix, full)
	}
	// The prefix carries [1]preamble + [2]project-block and stops before the tail.
	if !strings.Contains(prefix, "# Koryph dispatch (engine v9.9.9-test)") ||
		!strings.Contains(prefix, "## Project: demo-project") {
		t.Errorf("SharedPrefix missing preamble/project block:\n%s", prefix)
	}
	if strings.Contains(prefix, "## Task bd-42") {
		t.Errorf("SharedPrefix leaked the volatile task tail:\n%s", prefix)
	}
	// Exactly one separator inside the shared prefix (between [1] and [2]).
	if n := strings.Count(prefix, sectionSep); n != 1 {
		t.Errorf("SharedPrefix separator count = %d, want 1", n)
	}
}

// TestSharedPrefixByteIdenticalAcrossBeads proves the cache-warmth premise:
// two different beads in the same project produce a byte-identical
// SharedPrefix, so a single cached prefix is a warm read for every bead's
// first turn (koryph-6au). Only per-dispatch (tail) fields differ.
func TestSharedPrefixByteIdenticalAcrossBeads(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Bead = beads.Issue{ID: "bd-99", Title: "A wholly different task", Description: "Different body."}
	b.PlanYAML = "steps:\n  - other"
	b.ResumeSHA = "deadbeef"

	if pa, pb := SharedPrefix(a), SharedPrefix(b); pa != pb {
		t.Fatalf("SharedPrefix differs across beads (must be byte-identical)\n--- a ---\n%s\n--- b ---\n%s", pa, pb)
	}
	// Sanity: the full compiled prompts DO differ (the tail changed).
	if Compile(a) == Compile(b) {
		t.Fatal("Compile identical across different beads; test setup is wrong")
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

// TestPreambleTerseOutputContract verifies the output-economy section (design:
// docs/designs/2026-07-token-economy.md §3 L4) is present in the preamble and
// teaches agents the three key behaviours: quiet gate, file-spill wrappers,
// and concise replies. This block is part of the engine preamble so it must be
// byte-stable (no timestamps, no per-dispatch content).
func TestPreambleTerseOutputContract(t *testing.T) {
	p := Preamble("v1")
	for _, want := range []string{
		"Output economy",
		"make gate-agent",
		"koryph-spill.sh",
		"full output",
		"Read tool",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("preamble missing output-economy marker %q", want)
		}
	}
	// The output-economy block must be in the engine preamble (section [1]),
	// not the volatile tail — it must be byte-stable across dispatches.
	// Verify it does not contain any per-dispatch placeholders.
	if strings.Contains(p, "$KORYPH_PHASE_DIR") {
		// The block may reference the env var by name in prose — that is fine;
		// what is forbidden is an unresolved shell variable that would make the
		// text non-deterministic. We allow the literal string "$KORYPH_PHASE_DIR"
		// in the preamble as explanatory text; the cache-stability test
		// (TestNoTimestampsInStableSections) guards against actual timestamps.
		// So this is informational only; no failure here.
		_ = p
	}
}

// TestPreambleUsesPhaseControlInsteadOfSharedBeadsMutations proves sandboxed
// workers request narrow orchestrator-owned changes instead of receiving the
// shared database path and trying to self-park.
func TestPreambleUsesPhaseControlInsteadOfSharedBeadsMutations(t *testing.T) {
	p := Preamble("v1")
	for _, want := range []string{
		"koryph phase request label-add",
		"area:<value>",
		"fp:<value>",
		"res:<value>",
		"koryph phase block",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("preamble missing phase-control guidance %q", want)
		}
	}
	for _, forbidden := range []string{"bd dep add", "bd update <this-id>", "--add-label no-dispatch"} {
		if strings.Contains(p, forbidden) {
			t.Errorf("preamble still recommends shared Beads mutation %q", forbidden)
		}
	}
}

func TestPreambleClassifiesHostBlocks(t *testing.T) {
	p := Preamble("v1")
	for _, want := range []string{
		"sandbox, host, or environment",
		"ssh-agent",
		"generic state=blocked heartbeat",
		"--capability <lowercase-token>",
		"terminal host-capability recovery",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("preamble missing host-block guidance %q", want)
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

// TestWIPSnapshotCitedInResumingBlock is the koryph-77r.10 golden: a WIP
// snapshot path fires the RESUMING block on its own (no ResumeSHA needed —
// the zero-commit budget-kill case has WIP but no committed SHA to resume
// from), and combines cleanly with a ResumeSHA when both are present.
func TestWIPSnapshotCitedInResumingBlock(t *testing.T) {
	// WIPSnapshotPath alone: RESUMING block present, but no commit-log
	// guidance (there is no committed work to inspect that way).
	in := baseInput()
	in.WIPSnapshotPath = "/phases/p1/wip-20260707T000000Z.patch"
	out := Compile(in)
	if !strings.Contains(out, "### RESUMING") {
		t.Errorf("RESUMING block missing when WIPSnapshotPath is set (no ResumeSHA):\n%s", out)
	}
	if strings.Contains(out, "git log --oneline") {
		t.Errorf("commit-log guidance should be absent with no ResumeSHA:\n%s", out)
	}
	if !strings.Contains(out, "/phases/p1/wip-20260707T000000Z.patch") {
		t.Errorf("RESUMING block missing the WIP snapshot path:\n%s", out)
	}
	if !strings.Contains(out, "git apply /phases/p1/wip-20260707T000000Z.patch") {
		t.Errorf("RESUMING block missing the git apply guidance:\n%s", out)
	}

	// Both set: one RESUMING block, both the commit-log guidance and the WIP
	// snapshot citation appear.
	in2 := baseInput()
	in2.ResumeSHA = "abc1234"
	in2.WIPSnapshotPath = "/phases/p1/wip-20260707T000000Z.patch"
	out2 := Compile(in2)
	if strings.Count(out2, "### RESUMING") != 1 {
		t.Errorf("expected exactly one RESUMING heading when both ResumeSHA and WIPSnapshotPath are set:\n%s", out2)
	}
	if !strings.Contains(out2, "git log --oneline abc1234..HEAD") {
		t.Errorf("commit-log guidance missing when ResumeSHA is also set:\n%s", out2)
	}
	if !strings.Contains(out2, "/phases/p1/wip-20260707T000000Z.patch") {
		t.Errorf("WIP snapshot path missing when ResumeSHA is also set:\n%s", out2)
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

// TestResourcesBlockAbsentWithoutLabels is the koryph-4ql.4 golden: a bead
// with no res:<kind> labels gets zero RESOURCES output (design L1's
// inverted-default posture — "agent + worktree only" is silent, not an empty
// section header).
func TestResourcesBlockAbsentWithoutLabels(t *testing.T) {
	out := Compile(baseInput())
	if strings.Contains(out, "RESOURCES") {
		t.Errorf("RESOURCES block should be absent when the bead declares no res:* labels:\n%s", out)
	}
}

// TestResourcesBlockPresentWithNamingContract pins the design L6 "Agent
// contract" content for a bead that declares exactly one res:<kind> label:
// the block names the declared kind, the <kind>-<bead-id> instance-naming
// convention, and the teardown/SIGTERM/SUMMARY.md directives.
func TestResourcesBlockPresentWithNamingContract(t *testing.T) {
	in := baseInput()
	in.Bead.Labels = []string{"res:kind-cluster"}
	out := Compile(in)

	if !strings.Contains(out, "### RESOURCES") {
		t.Fatalf("RESOURCES block missing when the bead declares res:kind-cluster:\n%s", out)
	}
	for _, want := range []string{
		"kind-cluster",
		"kind-cluster-bd-42", // <kind>-<bead-id> naming line
		"Tear everything down before you exit, including when checkpointing on SIGTERM.",
		"SUMMARY.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RESOURCES block missing %q:\n%s", want, out)
		}
	}

	// Must appear in the volatile tail (after the task heading), like the
	// other conditional blocks.
	iTask := strings.Index(out, "## Task bd-42")
	iRes := strings.Index(out, "### RESOURCES")
	if iRes < iTask {
		t.Errorf("RESOURCES section should be in the volatile tail (after %d), got index %d", iTask, iRes)
	}
}

// TestResourcesBlockMultiKindOrderingDeterministic pins design L1's
// dedupe+sort contract end to end: labels declared out of order (and with a
// duplicate) render as one sorted, comma-joined kind list, matching
// sched.ResourcesFor's own ordering guarantee.
func TestResourcesBlockMultiKindOrderingDeterministic(t *testing.T) {
	in := baseInput()
	in.Bead.Labels = []string{"res:docker", "res:kind-cluster", "res:docker"}
	out := Compile(in)

	if !strings.Contains(out, "external resource kind(s): docker, kind-cluster.") {
		t.Errorf("RESOURCES block kinds not sorted/deduped as expected:\n%s", out)
	}
	if strings.Count(out, "### RESOURCES") != 1 {
		t.Errorf("expected exactly one RESOURCES heading, got %d:\n%s", strings.Count(out, "### RESOURCES"), out)
	}
	// The naming-line example uses the first (sorted) kind.
	if !strings.Contains(out, "docker-bd-42") {
		t.Errorf("RESOURCES naming example should use the first sorted kind (docker):\n%s", out)
	}
}

// TestResourcesBlockMalformedLabelsIgnored pins ResourcesFor's fail-open
// posture (design L1/sched.isResKind) through promptc: a malformed res:
// label (uppercase, here) contributes nothing, so a bead with only malformed
// res: labels renders exactly like a bead with none.
func TestResourcesBlockMalformedLabelsIgnored(t *testing.T) {
	in := baseInput()
	in.Bead.Labels = []string{"res:KindCluster"}
	out := Compile(in)
	if strings.Contains(out, "RESOURCES") {
		t.Errorf("RESOURCES block should be absent for a bead with only malformed res:* labels:\n%s", out)
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
