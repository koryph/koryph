// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sched

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/project"
)

func issue(id string, prio int, labels ...string) beads.Issue {
	return beads.Issue{ID: id, Title: id, Status: "open", Priority: prio, IssueType: "task", Labels: labels}
}

func testCfg() *project.Config {
	return &project.Config{
		AreaMap: map[string][]string{
			"api": {"go:api", "db:schema"},
			"web": {"app:web"},
		},
	}
}

func TestFootprintFor(t *testing.T) {
	cfg := testCfg()
	cases := []struct {
		name       string
		labels     []string
		wantReads  []string
		wantWrites []string
	}{
		{"fp labels used as-is, sorted (writes)", []string{"fp:go:api", "fp:app:web"}, nil, []string{"app:web", "go:api"}},
		{"fp and area compose (writes union)", []string{"fp:go:api", "area:web"}, nil, []string{"app:web", "go:api"}},
		{"area map fallback", []string{"area:api"}, nil, []string{"db:schema", "go:api"}},
		{"multiple areas merge+sort", []string{"area:api", "area:web"}, nil, []string{"app:web", "db:schema", "go:api"}},
		{"unmapped area -> unknown", []string{"area:mystery"}, nil, []string{TokenUnknown}},
		{"no labels -> unknown", nil, nil, []string{TokenUnknown}},
		{"dedupe", []string{"fp:x", "fp:x", "fp:y"}, nil, []string{"x", "y"}},
		{"fp:read: labels are reads, not writes", []string{"fp:read:go:engine"}, []string{"go:engine"}, nil},
		{"mixed read+write labels", []string{"fp:read:go:engine", "fp:docs"}, []string{"go:engine"}, []string{"docs"}},
		{"read+write of the same token collapses to write-only", []string{"fp:read:x", "fp:x"}, nil, []string{"x"}},
		{"fp:read: composes with area writes (the burn-in bug: the area write must survive)", []string{"fp:read:go:engine", "area:web"}, []string{"go:engine"}, []string{"app:web"}},
		{"reads of one package + writes of another via area", []string{"fp:read:go:engine", "area:api"}, []string{"go:engine"}, []string{"db:schema", "go:api"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FootprintFor(issue("i", 1, tc.labels...), cfg)
			if !reflect.DeepEqual(got.Reads, tc.wantReads) {
				t.Fatalf("reads = %v, want %v", got.Reads, tc.wantReads)
			}
			wantWrites := tc.wantWrites
			if wantWrites == nil {
				wantWrites = []string{}
			}
			if !reflect.DeepEqual(got.Writes, wantWrites) {
				t.Fatalf("writes = %v, want %v", got.Writes, wantWrites)
			}
		})
	}
}

func TestFootprintForNilConfig(t *testing.T) {
	got := FootprintFor(issue("i", 1, "area:api"), nil)
	if !reflect.DeepEqual(got.Writes, []string{TokenUnknown}) {
		t.Fatalf("nil cfg area -> %v, want unknown", got.Writes)
	}
	if len(got.Reads) != 0 {
		t.Fatalf("nil cfg area reads = %v, want none", got.Reads)
	}
}

func TestFootprintString(t *testing.T) {
	fp := Footprint{Reads: []string{"docs"}, Writes: []string{"go:engine"}}
	if got, want := fp.String(), "[r:docs w:go:engine]"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestConflicts(t *testing.T) {
	cases := []struct {
		name string
		a, b Footprint
		want bool
	}{
		{"disjoint writes never conflict", Footprint{Writes: []string{"go:api", "db"}}, Footprint{Writes: []string{"app:web"}}, false},
		{"shared write conflicts", Footprint{Writes: []string{"go:api", "db"}}, Footprint{Writes: []string{"db"}}, true},
		{"two unknowns always conflict", Footprint{Writes: []string{TokenUnknown}}, Footprint{Writes: []string{TokenUnknown}}, true},
		{"read/read co-runs, no conflict", Footprint{Reads: []string{"docs"}}, Footprint{Reads: []string{"docs"}}, false},
		{"read/write on same token conflicts", Footprint{Reads: []string{"go:engine"}}, Footprint{Writes: []string{"go:engine"}}, true},
		{"write/read on same token conflicts (symmetric)", Footprint{Writes: []string{"go:engine"}}, Footprint{Reads: []string{"go:engine"}}, true},
		{"write/write on same token conflicts", Footprint{Writes: []string{"go:engine"}}, Footprint{Writes: []string{"go:engine"}}, true},
		{"disjoint read and write never conflict", Footprint{Reads: []string{"docs"}}, Footprint{Writes: []string{"go:engine"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Conflicts(tc.a, tc.b); got != tc.want {
				t.Fatalf("Conflicts(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestEligible(t *testing.T) {
	active := map[string]bool{"busy-1": true}
	cases := []struct {
		name    string
		iss     beads.Issue
		wantOK  bool
		wantSub string // substring of reason when not ok
	}{
		{"plain task", issue("t", 1), true, ""},
		{"epic", beads.Issue{ID: "e", IssueType: "epic"}, false, "epic"},
		{"feature", beads.Issue{ID: "f", IssueType: "feature"}, false, "feature"},
		{"decision", beads.Issue{ID: "d", IssueType: "decision"}, false, "decision"},
		{"merge-request", beads.Issue{ID: "m", IssueType: "merge-request"}, false, "merge-request"},
		{"no-dispatch", issue("n", 1, "no-dispatch"), false, "no-dispatch"},
		{"refactor-core", issue("r", 1, "refactor-core"), false, "refactor-core"},
		{"gate label", issue("g", 1, "gt:merge"), false, "gate"},
		{"already active", issue("busy-1", 1), false, "already active"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := Eligible(tc.iss, active)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason %q)", ok, tc.wantOK, reason)
			}
			if !ok && tc.wantSub != "" && !strings.Contains(reason, tc.wantSub) {
				t.Fatalf("reason = %q, want substring %q", reason, tc.wantSub)
			}
		})
	}
}

func TestResolveModel(t *testing.T) {
	cases := []struct {
		name      string
		labels    []string
		def       string
		wantModel string
		wantWhy   string
	}{
		{"implement-scoped wins", []string{"model:opus", "model:implement:haiku"}, "sonnet", "haiku", "label model:implement:haiku"},
		{"bare tier", []string{"model:sonnet"}, "opus", "sonnet", "label model:sonnet"},
		{"ignore other-stage label", []string{"model:plan:opus"}, "", "", "stage default"},
		{"run default", nil, "sonnet", "sonnet", "run default"},
		{"stage default", nil, "", "", "stage default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, why := resolveModel(issue("i", 1, tc.labels...), Opts{DefaultModel: tc.def})
			if m != tc.wantModel || why != tc.wantWhy {
				t.Fatalf("model=%q why=%q, want %q / %q", m, why, tc.wantModel, tc.wantWhy)
			}
		})
	}
}

func TestBuildWaveComprehensive(t *testing.T) {
	epic := beads.Issue{ID: "epic-1", Title: "epic", IssueType: "epic"}
	gate := issue("gt-1", 0, "gt:slot")
	nodisp := issue("nd-1", 0, "no-dispatch")
	refcore := issue("rc-1", 0, "refactor-core")
	act := issue("act-1", 0, "fp:go:api")
	container := issue("con-1", 0, "fp:misc")
	t1 := issue("t1", 0, "fp:go:api") // P0, selected first
	t2 := issue("t2", 1, "fp:go:api") // conflicts with t1
	t3 := issue("t3", 1, "fp:app:web")
	t3.ParentID = "epic-1"
	t4 := issue("t4", 2, "fp:db") // spills over Max=2

	issues := []beads.Issue{epic, gate, nodisp, refcore, act, container, t1, t2, t3, t4}

	children := func(id string) (bool, error) { return id == "con-1", nil }
	opts := Opts{Max: 2, ActiveIDs: map[string]bool{"act-1": true}}

	w, err := BuildWave(context.Background(), issues, testCfg(), opts, children)
	if err != nil {
		t.Fatal(err)
	}

	// Selected: t1 then t3 (t2 conflicts, t4 over cap).
	gotIDs := itemIDs(w.Items)
	if !reflect.DeepEqual(gotIDs, []string{"t1", "t3"}) {
		t.Fatalf("wave items = %v, want [t1 t3]", gotIDs)
	}
	if w.ReadyCount != len(issues) {
		t.Fatalf("ready_count = %d, want %d", w.ReadyCount, len(issues))
	}
	if w.Max != 2 {
		t.Fatalf("max = %d", w.Max)
	}

	// EpicID propagated from ParentID.
	if w.Items[1].EpicID != "epic-1" {
		t.Fatalf("t3 epic = %q, want epic-1", w.Items[1].EpicID)
	}
	if w.Items[0].Persona != "" {
		t.Fatalf("persona should be engine-resolved (empty), got %q", w.Items[0].Persona)
	}

	deferred := deferMap(w.Deferred)
	// Recorded deferrals.
	assertReason(t, deferred, "nd-1", "no-dispatch label")
	assertReason(t, deferred, "rc-1", "refactor-core label")
	assertReason(t, deferred, "act-1", "already active")
	assertReason(t, deferred, "con-1", "container bead")
	assertReason(t, deferred, "t2", "footprint conflict with t1")
	assertReason(t, deferred, "t4", "wave full")

	// Structural skips: epic + gate go to Skipped (surfaced once by the engine),
	// never Deferred.
	if _, ok := deferred["epic-1"]; ok {
		t.Fatal("epic should be in Skipped, not Deferred")
	}
	if _, ok := deferred["gt-1"]; ok {
		t.Fatal("gate bead should be in Skipped, not Deferred")
	}
	skipped := deferMap(w.Skipped)
	assertReason(t, skipped, "epic-1", "non-dispatch issue_type epic")
	assertReason(t, skipped, "gt-1", "gate label gt:slot")
}

func TestBuildWaveTwoUnknownsSerialize(t *testing.T) {
	u1 := issue("u1", 0) // no fp/area -> unknown
	u2 := issue("u2", 0) // unknown too
	w, err := BuildWave(context.Background(), []beads.Issue{u1, u2}, testCfg(), Opts{Max: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids := itemIDs(w.Items); !reflect.DeepEqual(ids, []string{"u1"}) {
		t.Fatalf("only one unknown may dispatch, got %v", ids)
	}
	assertReason(t, deferMap(w.Deferred), "u2", "footprint conflict with u1")
}

// TestBuildWaveNilActiveUnchanged pins down that an unset (nil) Opts.Active
// reproduces exactly the pre-koryph-2im.1 behavior: no in-flight gating, only
// intra-batch coloring. Every other BuildWave test in this file already
// exercises this (none set Active), but this test makes the guarantee
// explicit and named so a regression here is unambiguous.
func TestBuildWaveNilActiveUnchanged(t *testing.T) {
	t1 := issue("t1", 0, "fp:go:api")
	t2 := issue("t2", 1, "fp:app:web")
	w, err := BuildWave(context.Background(), []beads.Issue{t1, t2}, testCfg(), Opts{Max: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemIDs(w.Items); !reflect.DeepEqual(got, []string{"t1", "t2"}) {
		t.Fatalf("nil Active: items = %v, want both dispatched", got)
	}
}

// TestBuildWaveInFlightConflict covers koryph-2im.1's L2 gating: a candidate
// whose footprint conflicts with an in-flight (opts.Active) footprint is
// deferred with the exact "(in-flight)" reason, checked BEFORE intra-batch
// coloring — and never makes it into w.Items at all.
func TestBuildWaveInFlightConflict(t *testing.T) {
	t1 := issue("t1", 0, "fp:go:engine")
	active := map[string]Footprint{
		"running-1": {Writes: []string{"go:engine"}},
	}
	w, err := BuildWave(context.Background(), []beads.Issue{t1}, testCfg(), Opts{Max: 5, Active: active}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Items) != 0 {
		t.Fatalf("expected no items dispatched, got %v", itemIDs(w.Items))
	}
	assertReason(t, deferMap(w.Deferred), "t1", "footprint conflict with running-1 (in-flight)")
}

// TestBuildWaveInFlightConflictStableBlocker pins the "iterate Active in
// sorted-key order" requirement: with multiple conflicting in-flight
// footprints, the reported blocker id must be deterministic across runs
// (map iteration order is randomized by Go, so this would flake without the
// sort).
func TestBuildWaveInFlightConflictStableBlocker(t *testing.T) {
	t1 := issue("t1", 0, "fp:go:engine")
	active := map[string]Footprint{
		"zzz-late":  {Writes: []string{"go:engine"}},
		"aaa-early": {Writes: []string{"go:engine"}},
	}
	for i := 0; i < 20; i++ {
		w, err := BuildWave(context.Background(), []beads.Issue{t1}, testCfg(), Opts{Max: 5, Active: active}, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertReason(t, deferMap(w.Deferred), "t1", "footprint conflict with aaa-early (in-flight)")
	}
}

// TestBuildWaveInFlightReadReadCoRuns covers the RW relaxation (L4): a
// read-only candidate against a read-only in-flight footprint on the SAME
// token must co-run, not defer — this is the whole point of splitting
// footprints into read/write sets.
func TestBuildWaveInFlightReadReadCoRuns(t *testing.T) {
	docsReader := issue("docs-1", 0, "fp:read:go:engine")
	active := map[string]Footprint{
		"running-1": {Reads: []string{"go:engine"}},
	}
	w, err := BuildWave(context.Background(), []beads.Issue{docsReader}, testCfg(), Opts{Max: 5, Active: active}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemIDs(w.Items); !reflect.DeepEqual(got, []string{"docs-1"}) {
		t.Fatalf("read/read in-flight should co-run, got items=%v deferred=%v", got, w.Deferred)
	}
}

func TestBuildWavePriorityOrder(t *testing.T) {
	// Distinct footprints so nothing conflicts; verify P0-first stable sort.
	lo := issue("lo", 2, "fp:a")
	mid := issue("mid", 1, "fp:b")
	hi := issue("hi", 0, "fp:c")
	tieA := issue("tieA", 1, "fp:d")
	tieB := issue("tieB", 1, "fp:e")
	// Input order: lo, tieA, mid, hi, tieB
	w, err := BuildWave(context.Background(), []beads.Issue{lo, tieA, mid, hi, tieB}, testCfg(), Opts{Max: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// hi(P0) first; then P1 group in input order: tieA, mid, tieB; then lo(P2).
	want := []string{"hi", "tieA", "mid", "tieB", "lo"}
	if got := itemIDs(w.Items); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestBuildWaveChildListerError(t *testing.T) {
	boom := func(string) (bool, error) { return false, context.DeadlineExceeded }
	_, err := BuildWave(context.Background(), []beads.Issue{issue("x", 0, "fp:a")}, testCfg(), Opts{Max: 1}, boom)
	if err == nil {
		t.Fatal("expected error propagated from child lister")
	}
}

func TestWaveJSONRoundTrip(t *testing.T) {
	w, err := BuildWave(context.Background(),
		[]beads.Issue{issue("t1", 0, "fp:go:api"), issue("t2", 1, "fp:go:api")},
		testCfg(), Opts{Max: 3, DefaultModel: "sonnet"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var back Wave
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("round-trip mismatch:\n%s\n%s", first, second)
	}
}

// --- helpers ---------------------------------------------------------------

func itemIDs(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Issue.ID)
	}
	return out
}

func deferMap(rs []Reason) map[string]string {
	m := make(map[string]string, len(rs))
	for _, r := range rs {
		m[r.ID] = r.Reason
	}
	return m
}

func assertReason(t *testing.T, m map[string]string, id, want string) {
	t.Helper()
	got, ok := m[id]
	if !ok {
		t.Fatalf("%s not in deferred set %v", id, m)
	}
	if got != want {
		t.Fatalf("%s reason = %q, want %q", id, got, want)
	}
}
