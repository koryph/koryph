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
		name   string
		labels []string
		want   []string
	}{
		{"fp labels used as-is, sorted", []string{"fp:go:api", "fp:app:web"}, []string{"app:web", "go:api"}},
		{"fp wins over area", []string{"fp:go:api", "area:web"}, []string{"go:api"}},
		{"area map fallback", []string{"area:api"}, []string{"db:schema", "go:api"}},
		{"multiple areas merge+sort", []string{"area:api", "area:web"}, []string{"app:web", "db:schema", "go:api"}},
		{"unmapped area -> unknown", []string{"area:mystery"}, []string{TokenUnknown}},
		{"no labels -> unknown", nil, []string{TokenUnknown}},
		{"dedupe", []string{"fp:x", "fp:x", "fp:y"}, []string{"x", "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FootprintFor(issue("i", 1, tc.labels...), cfg)
			if !reflect.DeepEqual(got.Tokens, tc.want) {
				t.Fatalf("tokens = %v, want %v", got.Tokens, tc.want)
			}
		})
	}
}

func TestFootprintForNilConfig(t *testing.T) {
	got := FootprintFor(issue("i", 1, "area:api"), nil)
	if !reflect.DeepEqual(got.Tokens, []string{TokenUnknown}) {
		t.Fatalf("nil cfg area -> %v, want unknown", got.Tokens)
	}
}

func TestConflicts(t *testing.T) {
	a := Footprint{Tokens: []string{"go:api", "db"}}
	b := Footprint{Tokens: []string{"app:web"}}
	c := Footprint{Tokens: []string{"db"}}
	if Conflicts(a, b) {
		t.Fatal("disjoint footprints must not conflict")
	}
	if !Conflicts(a, c) {
		t.Fatal("shared token must conflict")
	}
	// Two unknowns always conflict.
	u := Footprint{Tokens: []string{TokenUnknown}}
	if !Conflicts(u, u) {
		t.Fatal("two unknowns must conflict")
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
