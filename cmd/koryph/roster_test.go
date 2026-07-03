// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

// registerMinimalProject registers a bare project (no GitHub remote, no bd).
func registerMinimalProject(t *testing.T, id string) *registry.Record {
	t.Helper()
	root := gitRepo(t)
	ctx := context.Background()
	store := registry.NewStore()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	rec := &registry.Record{
		ProjectID:        id,
		Name:             id,
		Root:             root,
		AccountProfile:   "personal",
		ExpectedIdentity: "me@example.com",
	}
	if err := store.Add(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// seedTestRun writes a ledger run with the given slots and returns it.
func seedTestRun(t *testing.T, rec *registry.Record, slots []*ledger.Slot) *ledger.Run {
	t.Helper()
	ls := ledger.NewStore(rec.Root)
	run, err := ls.NewRun(rec.ProjectID, "bd", "0.3.0")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	for _, sl := range slots {
		if err := ls.SetSlot(run, sl); err != nil {
			t.Fatalf("SetSlot %s: %v", sl.PhaseID, err)
		}
	}
	return run
}

// --- flag validation -------------------------------------------------------

func TestRosterRequiresProject(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("roster")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit", code)
	}
	if !strings.Contains(errb, "--project") {
		t.Errorf("stderr should mention --project; got: %s", errb)
	}
}

func TestRosterUnknownProject(t *testing.T) {
	isolate(t)
	code, _, _ := runCmd("roster", "--project", "does-not-exist")
	if code != engine.ExitFatal {
		t.Errorf("code = %d, want fatal for unknown project", code)
	}
}

func TestRosterNoRuns(t *testing.T) {
	isolate(t)
	registerMinimalProject(t, "norun")
	code, out, errb := runCmd("roster", "--project", "norun")
	if code != 0 {
		t.Fatalf("code = %d; stderr=%s", code, errb)
	}
	if !strings.Contains(out, "no runs found") {
		t.Errorf("output = %q, want 'no runs found'", out)
	}
}

// --- human output ----------------------------------------------------------

func TestRosterHumanFourGroups(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "four")

	slots := []*ledger.Slot{
		{PhaseID: "aaa", BeadID: "aaa", Branch: "agent/aaa", Status: ledger.SlotMerged, LastCommit: "abc123def456"},
		{PhaseID: "bbb", BeadID: "bbb", Branch: "agent/bbb", Status: ledger.SlotRunning, Model: "sonnet", Attempts: 1},
	}
	seedTestRun(t, rec, slots)

	code, out, errb := runCmd("roster", "--project", "four")
	if code != 0 {
		t.Fatalf("roster code = %d; stderr=%s", code, errb)
	}

	for _, want := range []string{"MERGED (1)", "RUNNING (1)", "QUEUED", "DEFERRED"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "aaa") {
		t.Errorf("merged slot 'aaa' missing:\n%s", out)
	}
	if !strings.Contains(out, "bbb") {
		t.Errorf("running slot 'bbb' missing:\n%s", out)
	}
	if !strings.Contains(out, "abc123def456") {
		t.Errorf("merge commit missing from output:\n%s", out)
	}
}

// TestRosterPROpenedInMerged verifies that pr-opened slots land in MERGED.
func TestRosterPROpenedInMerged(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "propen")
	seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "p1", BeadID: "p1", Branch: "agent/p1", Status: ledger.SlotPROpened, LastCommit: "cafebabe"},
	})

	code, out, errb := runCmd("roster", "--project", "propen")
	if code != 0 {
		t.Fatalf("code = %d; stderr=%s", code, errb)
	}
	if !strings.Contains(out, "MERGED (1)") {
		t.Errorf("pr-opened should appear in MERGED:\n%s", out)
	}
}

// TestRosterFailedInRunning verifies that failed slots appear in RUNNING
// (visible in the run but not successful merges).
func TestRosterFailedInRunning(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "failed")
	seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "f1", BeadID: "f1", Branch: "agent/f1", Status: ledger.SlotFailed},
	})

	code, out, errb := runCmd("roster", "--project", "failed")
	if code != 0 {
		t.Fatalf("code = %d; stderr=%s", code, errb)
	}
	if !strings.Contains(out, "RUNNING (1)") {
		t.Errorf("failed slot should appear in RUNNING:\n%s", out)
	}
	if strings.Contains(out, "MERGED (1)") {
		t.Errorf("failed slot should NOT appear in MERGED:\n%s", out)
	}
}

// --- JSON output -----------------------------------------------------------

func TestRosterJSONShape(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "jsonshape")

	slots := []*ledger.Slot{
		{PhaseID: "x1", BeadID: "x1", Branch: "agent/x1", Status: ledger.SlotMerged, LastCommit: "deadbeef1234"},
		{PhaseID: "x2", BeadID: "x2", Branch: "agent/x2", Status: ledger.SlotRunning, Model: "haiku", Attempts: 2},
		{PhaseID: "x3", BeadID: "x3", Branch: "agent/x3", Status: ledger.SlotFailed},
	}
	run := seedTestRun(t, rec, slots)

	code, out, errb := runCmd("roster", "--project", "jsonshape", "--json")
	if code != 0 {
		t.Fatalf("roster --json code = %d; stderr=%s", code, errb)
	}

	var got rosterOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json not valid JSON: %v\n%s", err, out)
	}

	if got.ProjectID != "jsonshape" {
		t.Errorf("project_id = %q, want jsonshape", got.ProjectID)
	}
	if got.RunID != run.RunID {
		t.Errorf("run_id = %q, want %q", got.RunID, run.RunID)
	}
	if len(got.Merged) != 1 {
		t.Errorf("merged len = %d, want 1", len(got.Merged))
	} else {
		if got.Merged[0].ID != "x1" {
			t.Errorf("merged[0].id = %q, want x1", got.Merged[0].ID)
		}
		if got.Merged[0].MergeCommit != "deadbeef1234" {
			t.Errorf("merged[0].merge_commit = %q, want deadbeef1234", got.Merged[0].MergeCommit)
		}
	}
	// running + failed → 2 entries in Running group.
	if len(got.Running) != 2 {
		t.Errorf("running len = %d, want 2 (running + failed)", len(got.Running))
	}
	// Queued and Deferred may be empty (no bd in temp repo), but must never
	// be absent from the JSON (they're always initialized to []).
	if got.Queued == nil {
		t.Error("queued must not be null in JSON output; want []")
	}
	if got.Deferred == nil {
		t.Error("deferred must not be null in JSON output; want []")
	}
}

// TestRosterJSONSortedMerged verifies lexicographic slot ordering in JSON.
func TestRosterJSONSortedMerged(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "sorted")

	slots := []*ledger.Slot{
		{PhaseID: "zzz", BeadID: "zzz", Branch: "agent/zzz", Status: ledger.SlotMerged},
		{PhaseID: "aaa", BeadID: "aaa", Branch: "agent/aaa", Status: ledger.SlotMerged},
	}
	seedTestRun(t, rec, slots)

	code, out, errb := runCmd("roster", "--project", "sorted", "--json")
	if code != 0 {
		t.Fatalf("code = %d; stderr=%s", code, errb)
	}
	var got rosterOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json not valid JSON: %v\n%s", err, out)
	}
	if len(got.Merged) != 2 {
		t.Fatalf("merged len = %d, want 2", len(got.Merged))
	}
	// sortedSlotKeys returns lexicographic order: aaa < zzz.
	if got.Merged[0].ID != "aaa" || got.Merged[1].ID != "zzz" {
		t.Errorf("merged order = [%s, %s], want [aaa, zzz]",
			got.Merged[0].ID, got.Merged[1].ID)
	}
}

// --- --run flag ------------------------------------------------------------

func TestRosterSpecificRun(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "specrun")

	// Seed exactly one run and then request it by ID. A second run is not
	// created here because run IDs have second precision and rapid creation
	// within the same test second would collide.
	run := seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "s1", BeadID: "s1", Branch: "agent/s1", Status: ledger.SlotDone},
	})

	code, out, errb := runCmd("roster", "--project", "specrun", "--run", run.RunID)
	if code != 0 {
		t.Fatalf("code = %d; stderr=%s", code, errb)
	}
	if !strings.Contains(out, run.RunID) {
		t.Errorf("output missing run ID %s:\n%s", run.RunID, out)
	}
	if !strings.Contains(out, "s1") {
		t.Errorf("output missing slot s1 for run %s:\n%s", run.RunID, out)
	}
}

// --- helper function tests -------------------------------------------------

func TestHumanAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{2*time.Minute + 30*time.Second, "2m30s"},
		{90 * time.Minute, "1h30m"},
	}
	for _, tc := range cases {
		if got := humanAge(tc.d); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("abcdef123456789"); got != "abcdef123456" {
		t.Errorf("shortSHA = %q, want 12-char prefix", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA short = %q, want unchanged", got)
	}
}

// --- help ------------------------------------------------------------------

func TestRosterAppearsInHelp(t *testing.T) {
	_, out, _ := runCmd("help")
	if !strings.Contains(out, "roster") {
		t.Errorf("help output missing 'roster':\n%s", out)
	}
}
