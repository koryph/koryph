// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/epicreview"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// epicFakeStore is an in-memory WorkSource that ALSO satisfies
// epicreview.BeadStore, so the epic-validation hook exercises its real act
// path (the plain fakeSource deliberately lacks the bead-store verbs — that
// combination is covered by TestEpicValidationSkipsWithoutBeadStore).
type epicFakeStore struct {
	fakeSource
	issues   map[string]beads.Issue
	children map[string][]beads.Issue

	created []beads.CreateInput
	labels  []string // "<id>:<label>"
	notes   []string // "<id>:<text>"
	closed  []string // "<id>:<reason>"
	deps    []string // "<id>:<blockedBy>"

	// listCalls and childrenCalls count bd-subprocess-equivalent calls, used
	// by the health-patrol cadence tests (koryph-bbe) to verify the epic
	// listing is actually throttled rather than re-fetched every tick.
	listCalls     int
	childrenCalls int
}

func (f *epicFakeStore) Show(_ context.Context, id string) (beads.Issue, error) {
	if iss, ok := f.issues[id]; ok {
		return iss, nil
	}
	return beads.Issue{}, fmt.Errorf("no such issue %s", id)
}
func (f *epicFakeStore) ListChildren(_ context.Context, id string) ([]beads.Issue, error) {
	f.childrenCalls++
	return f.children[id], nil
}

// List mirrors beads.Adapter.List's contract: only non-closed issues, sorted
// by ID for deterministic test assertions (the real bd CLI has no ordering
// guarantee either, so callers must not depend on it — sorting here just
// keeps the fixtures reproducible).
func (f *epicFakeStore) List(_ context.Context) ([]beads.Issue, error) {
	f.listCalls++
	ids := make([]string, 0, len(f.issues))
	for id := range f.issues {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]beads.Issue, 0, len(ids))
	for _, id := range ids {
		iss := f.issues[id]
		if iss.Status != "closed" && iss.Status != "done" {
			out = append(out, iss)
		}
	}
	return out, nil
}
func (f *epicFakeStore) Create(_ context.Context, in beads.CreateInput) (string, error) {
	f.created = append(f.created, in)
	id := fmt.Sprintf("created-%d", len(f.created))
	iss := beads.Issue{ID: id, Title: in.Title, Status: "open", Labels: in.Labels, ParentID: in.Parent}
	f.issues[id] = iss
	if in.Parent != "" {
		f.children[in.Parent] = append(f.children[in.Parent], iss)
	}
	return id, nil
}
func (f *epicFakeStore) Close(_ context.Context, id, reason string) error {
	f.closed = append(f.closed, id+":"+reason)
	iss := f.issues[id]
	iss.Status = "closed"
	f.issues[id] = iss
	return nil
}
func (f *epicFakeStore) AppendNotes(_ context.Context, id, text string) error {
	f.notes = append(f.notes, id+":"+text)
	return nil
}
func (f *epicFakeStore) AddLabel(_ context.Context, id, label string) error {
	f.labels = append(f.labels, id+":"+label)
	iss, ok := f.issues[id]
	if ok {
		iss.Labels = append(iss.Labels, label)
		f.issues[id] = iss
	}
	return nil
}
func (f *epicFakeStore) DepAdd(_ context.Context, id, blockedBy string) error {
	f.deps = append(f.deps, id+":"+blockedBy)
	return nil
}

// epicRunner builds a minimal runner around the fake, with the validator
// stubbed to return verdict.
func epicRunner(t *testing.T, fake *epicFakeStore, verdict epicreview.Verdict) (*runner, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	r := &runner{
		opts:    Options{ProjectID: "proj"},
		cfg:     &project.Config{},
		rec:     &registry.Record{Root: t.TempDir()},
		adapter: fake,
		run:     &ledger.Run{RunID: "test-run", Slots: map[string]*ledger.Slot{}},
		epicValidateFn: func(context.Context, epicreview.Opts) epicreview.Verdict {
			calls.Add(1)
			return verdict
		},
	}
	return r, calls
}

// drainVerdict pumps the tick-loop pair (start + drain) until the in-flight
// validation completes or the deadline passes.
func drainVerdict(t *testing.T, r *runner) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.drainEpicResults(t.Context())
		if r.epicInFlight == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("validation verdict never drained")
}

func closedEpicFixture() *epicFakeStore {
	return &epicFakeStore{
		issues: map[string]beads.Issue{
			"ep1": {ID: "ep1", Title: "Epic one", IssueType: "epic", Status: "open"},
			"c1":  {ID: "c1", Title: "child", IssueType: "task", Status: "closed", ParentID: "ep1"},
		},
		children: map[string][]beads.Issue{
			"ep1": {{ID: "c1", Title: "child", Status: "closed", ParentID: "ep1"}},
		},
	}
}

func TestEpicValidation_MetFilesDocsBeadAndDefersClose(t *testing.T) {
	fake := closedEpicFixture()
	r, calls := epicRunner(t, fake, epicreview.Verdict{Met: true, Summary: "all good"})

	r.noteEpicCandidate(t.Context(), "c1")
	if !r.epicPending["ep1"] {
		t.Fatal("closed child's parent not queued as candidate")
	}
	r.maybeStartEpicValidation(t.Context(), true)
	if r.epicInFlight != "ep1" {
		t.Fatalf("epicInFlight = %q, want ep1", r.epicInFlight)
	}
	drainVerdict(t, r)

	if got := calls.Load(); got != 1 {
		t.Errorf("validator calls = %d, want 1", got)
	}
	if len(fake.closed) != 0 {
		t.Errorf("epic must not close while docs bead open; closed = %v", fake.closed)
	}
	wantLabel := "ep1:" + epicreview.LabelPassed
	if !containsStr(fake.labels, wantLabel) {
		t.Errorf("labels = %v, want %s", fake.labels, wantLabel)
	}
	if len(fake.created) != 1 || !containsStr(fake.created[0].Labels, epicreview.LabelDocs) {
		t.Fatalf("created = %+v, want one docs bead with %s", fake.created, epicreview.LabelDocs)
	}

	// The docs bead closing re-queues the epic; validation:passed now closes
	// it WITHOUT another validator run.
	docsID := "created-1"
	_ = fake.Close(t.Context(), docsID, "merged")
	fake.children["ep1"][1].Status = "closed"
	r.noteEpicCandidate(t.Context(), docsID)
	r.maybeStartEpicValidation(t.Context(), true)
	if r.epicInFlight != "" {
		t.Fatalf("no second validation expected; in flight %q", r.epicInFlight)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("validator calls after docs close = %d, want still 1", got)
	}
	if !anyPrefixed(fake.closed, "ep1:") {
		t.Errorf("epic not closed after docs merge; closed = %v", fake.closed)
	}
}

func TestEpicValidation_GapsFiledExactlyOnce(t *testing.T) {
	fake := closedEpicFixture()
	r, _ := epicRunner(t, fake, epicreview.Verdict{
		Met: false,
		Gaps: []epicreview.Gap{
			{Title: "gap A", Why: "w", Acceptance: "a", Type: "task", Labels: []string{"area:docs"}},
			{Title: "gap B", Why: "w", Acceptance: "a", DependsOn: []string{"0"}},
		},
	})

	r.epicPending = map[string]bool{"ep1": true}
	r.maybeStartEpicValidation(t.Context(), true)
	drainVerdict(t, r)

	var gapCreates []beads.CreateInput
	for _, c := range fake.created {
		if c.Parent == "ep1" {
			gapCreates = append(gapCreates, c)
		}
	}
	if len(gapCreates) != 2 {
		t.Fatalf("gap beads = %d, want 2 (%+v)", len(gapCreates), fake.created)
	}
	roundLabel := epicreview.RoundLabel(2)
	if !containsStr(gapCreates[0].Labels, roundLabel) {
		t.Errorf("gap labels = %v, want %s", gapCreates[0].Labels, roundLabel)
	}
	if !containsStr(fake.deps, "created-2:created-1") {
		t.Errorf("sibling dep not wired; deps = %v", fake.deps)
	}
	if len(fake.closed) != 0 {
		t.Errorf("epic must stay open on gaps; closed = %v", fake.closed)
	}
}

func TestEpicValidation_RoundCapParks(t *testing.T) {
	fake := closedEpicFixture()
	r, calls := epicRunner(t, fake, epicreview.Verdict{Met: true})

	// Two prior verdict files → next round 3 > default max_rounds 2.
	outDir := filepath.Join(r.rec.Root, ".koryph", "epic-reviews")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if err := os.WriteFile(filepath.Join(outDir, fmt.Sprintf("ep1-round%d.json", i)), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r.epicPending = map[string]bool{"ep1": true}
	r.maybeStartEpicValidation(t.Context(), true)

	if calls.Load() != 0 {
		t.Error("validator must not run past the round cap")
	}
	if !containsStr(fake.labels, "ep1:"+epicreview.LabelParked) {
		t.Errorf("parked label missing; labels = %v", fake.labels)
	}
	if !anyPrefixed(fake.notes, "ep1:") {
		t.Error("parked note missing")
	}
}

func TestEpicValidation_GuardStopDefers(t *testing.T) {
	fake := closedEpicFixture()
	r, calls := epicRunner(t, fake, epicreview.Verdict{Met: true})

	r.epicPending = map[string]bool{"ep1": true}
	r.maybeStartEpicValidation(t.Context(), false) // guard drain/stop

	if calls.Load() != 0 || r.epicInFlight != "" {
		t.Error("validation must not start while dispatch is guarded")
	}
	if !r.epicPending["ep1"] {
		t.Error("candidate must survive the guard (defer, never skip)")
	}
}

func TestEpicValidation_NoValidateSkips(t *testing.T) {
	fake := closedEpicFixture()
	iss := fake.issues["ep1"]
	iss.Labels = []string{epicreview.LabelNoValidate}
	fake.issues["ep1"] = iss
	r, calls := epicRunner(t, fake, epicreview.Verdict{Met: true})

	r.epicPending = map[string]bool{"ep1": true}
	r.maybeStartEpicValidation(t.Context(), true)

	if calls.Load() != 0 || r.epicInFlight != "" {
		t.Error("no-validate epic must be skipped")
	}
	if len(fake.labels)+len(fake.created)+len(fake.closed) != 0 {
		t.Error("no-validate epic must not be mutated")
	}
}

func TestEpicValidation_OpenChildWaits(t *testing.T) {
	fake := closedEpicFixture()
	fake.children["ep1"] = append(fake.children["ep1"], beads.Issue{ID: "c2", Status: "open", ParentID: "ep1"})
	r, calls := epicRunner(t, fake, epicreview.Verdict{Met: true})

	r.epicPending = map[string]bool{"ep1": true}
	r.maybeStartEpicValidation(t.Context(), true)

	if calls.Load() != 0 {
		t.Error("validation must wait for all children to close")
	}
}

func TestEpicValidationSkipsWithoutBeadStore(t *testing.T) {
	// A WorkSource without the bead-store verbs (the plain fakeSource) must
	// degrade gracefully: verdicts are dropped with a log line, never a panic.
	r := &runner{
		opts:        Options{ProjectID: "proj"},
		cfg:         &project.Config{},
		rec:         &registry.Record{Root: t.TempDir()},
		adapter:     &fakeSource{},
		run:         &ledger.Run{RunID: "test-run"},
		epicResults: make(chan epicValidationResult, 1),
	}
	r.epicInFlight = "ep1"
	r.epicResults <- epicValidationResult{epicID: "ep1", round: 1, verdict: epicreview.Verdict{Met: true}}
	r.drainEpicResults(t.Context()) // must not panic
	if r.epicInFlight != "" {
		t.Error("in-flight not cleared")
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func anyPrefixed(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
