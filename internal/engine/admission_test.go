// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/epicreview"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

const threeReadyJSON = `[
{"id":"tb1","title":"one","description":"work one","acceptance_criteria":"AC1: work is committed","status":"open","priority":1,"issue_type":"task","labels":["fp:one"]},
{"id":"tb2","title":"two","description":"work two","acceptance_criteria":"AC1: work is committed","status":"open","priority":1,"issue_type":"task","labels":["fp:two"]},
{"id":"tb3","title":"three","description":"work three","acceptance_criteria":"AC1: work is committed","status":"open","priority":1,"issue_type":"task","labels":["fp:three"]}
]`

func TestAllowedIDsFixedCohortCannotDispatchReadyOutsider(t *testing.T) {
	f := newFixture(t, fixOpts{})
	writeFile(t, f.bdDir+"/ready.json", threeReadyJSON, 0o644)
	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.Max = 3
	opts.AllowedIDs = []string{"tb2", "tb1"}
	var publishedRunID string
	opts.OnRunStart = func(runID string) error {
		publishedRunID = runID
		return nil
	}

	got, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if got.Dispatched != 2 {
		t.Fatalf("dispatched = %d, want fixed cohort size 2\n%s", got.Dispatched, out.String())
	}
	if publishedRunID == "" || publishedRunID != got.RunID {
		t.Fatalf("published run id = %q, outcome run id = %q", publishedRunID, got.RunID)
	}
	run, err := ledger.NewStore(f.repo).LoadRun(got.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Slots["tb1"] == nil || run.Slots["tb2"] == nil {
		t.Fatalf("allowed cohort slots = %+v, want tb1 and tb2", run.Slots)
	}
	if run.Slots["tb3"] != nil {
		t.Fatalf("outside bead acquired a slot: %+v", run.Slots["tb3"])
	}
	if log := f.bdLog(t); strings.Contains(log, "update tb3 --claim") {
		t.Fatalf("outside bead was claimed:\n%s", log)
	}
}

func TestPinnedResumeNeverSubstitutesNewerLatestRun(t *testing.T) {
	f := newFixture(t, fixOpts{})
	store := ledger.NewStore(f.repo)
	observed, err := store.NewRun("proj", "bd", EngineVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlot(observed, &ledger.Slot{
		PhaseID: "observed", BeadID: "observed", Status: ledger.SlotMerged,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := store.NewRun("proj", "bd", EngineVersion); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.Resume = true
	opts.RecoveryRunID = observed.RunID

	got, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if got.RunID != observed.RunID || got.Dispatched != 0 ||
		got.Reason != "pinned recovery run already terminal" {
		t.Fatalf("outcome = %+v, want pinned terminal run %s", got, observed.RunID)
	}
	if log := f.bdLog(t); strings.Contains(log, "--claim") {
		t.Fatalf("pinned terminal recovery dispatched fresh work:\n%s", log)
	}
}

func TestRunStartDurabilityBarrierFailsBeforeImplementationLaunch(t *testing.T) {
	f := newFixture(t, fixOpts{})
	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.OnRunStart = func(string) error { return errors.New("disk full") }

	got, err := Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Run outcome=%+v error=%v", got, err)
	}
	if got.RunID == "" || got.Code != ExitFatal {
		t.Fatalf("outcome = %+v", got)
	}
	run, loadErr := ledger.NewStore(f.repo).LoadRun(got.RunID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if run.Status != ledger.RunAborted {
		t.Fatalf("run status = %q, want aborted", run.Status)
	}
	if log := f.bdLog(t); strings.Contains(log, "--claim") {
		t.Fatalf("durability barrier failure launched implementation:\n%s", log)
	}
}

func TestAuthoritativeWidthFailsClosedAgainstProjectCap(t *testing.T) {
	f := newFixture(t, fixOpts{})
	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.Max = 3
	opts.AuthoritativeWidth = true

	got, err := Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "exceeds project max_concurrent_slots 2") {
		t.Fatalf("Run outcome=%+v error=%v", got, err)
	}
	if log := f.bdLog(t); strings.Contains(log, "--claim") {
		t.Fatalf("unsupported authoritative width launched implementation:\n%s", log)
	}
}

func TestDispatchDefensePublishesCohortAdmissionTripwire(t *testing.T) {
	var events []SafetyTripwire
	r := &runner{
		opts: Options{OnSafetyTripwire: func(event SafetyTripwire) {
			events = append(events, event)
		}},
		allowedIDs: map[string]struct{}{"inside": {}},
	}
	r.dispatchBead(context.Background(), dispatchReq{
		issue: beads.Issue{ID: "outside"},
	})
	if len(events) != 1 ||
		events[0].Kind != SafetyTripwireCohortAdmission ||
		events[0].BeadID != "outside" {
		t.Fatalf("tripwires = %+v", events)
	}
	if r.dispatchCircuitReason == "" {
		t.Fatal("outside dispatch did not open the in-run admission circuit")
	}
}

func TestResumeRefusesNonTerminalSlotOutsideFixedCohort(t *testing.T) {
	r := &runner{allowedIDs: map[string]struct{}{"inside": {}}}
	run := &ledger.Run{Slots: map[string]*ledger.Slot{
		"inside":  {PhaseID: "inside", Status: ledger.SlotRunning},
		"outside": {PhaseID: "outside", Status: ledger.SlotQueued},
		"history": {PhaseID: "history", Status: ledger.SlotMerged},
	}}
	if err := r.validateAllowedResume(run); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("validateAllowedResume error = %v", err)
	}
}

func TestEngineInvariantPublishesTypedLiveTripwire(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.reg = registry.NewStore()
	r.adapter = &fakeSource{}
	var events []SafetyTripwire
	r.opts = Options{
		ProjectID: "proj",
		OnSafetyTripwire: func(event SafetyTripwire) {
			events = append(events, event)
		},
	}

	r.parkTypedRecovery(t.Context(), sl, OutcomeEngineInvariant, "evidence digest mismatch")

	if len(events) != 1 {
		t.Fatalf("tripwire count = %d, want 1", len(events))
	}
	event := events[0]
	if event.Kind != SafetyTripwireEngineInvariant ||
		event.RunID != r.run.RunID ||
		event.BeadID != sl.PhaseID ||
		event.Detail != "evidence digest mismatch" {
		t.Fatalf("tripwire = %+v", event)
	}
}

func TestCanaryMissingTerminalContractStopsBeforeRepairDispatch(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.hardStopKinds = safetyTripwireSet([]SafetyTripwireKind{
		SafetyTripwireMissingTerminal,
	})
	var events []SafetyTripwire
	r.opts.OnSafetyTripwire = func(event SafetyTripwire) {
		events = append(events, event)
	}

	r.finishAssessedCandidate(t.Context(), sl, candidateAssessment{
		outcome: OutcomeCompletionContractMissing,
		reason:  "result absent",
	})

	if r.dispatched != 0 {
		t.Fatalf("dispatches = %d, want zero completion repair dispatches", r.dispatched)
	}
	if len(events) != 1 || events[0].Kind != SafetyTripwireMissingTerminal ||
		events[0].BeadID != sl.PhaseID {
		t.Fatalf("tripwires = %+v", events)
	}
	if got := r.run.Slots[sl.PhaseID]; got == nil || got.Status != ledger.SlotBlocked {
		t.Fatalf("slot = %+v, want blocked", got)
	}
}

func TestCanonicalCanaryMissingTerminalExhaustionParksOnlyItsSlot(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts.NativeCanary = true
	sl.Retry.CompletionRepairs = 1
	sibling := &ledger.Slot{
		PhaseID: "sibling", BeadID: "sibling", Status: ledger.SlotRunning,
	}
	if err := r.store.SetSlot(r.run, sibling); err != nil {
		t.Fatal(err)
	}
	var events []SafetyTripwire
	r.opts.OnSafetyTripwire = func(event SafetyTripwire) {
		events = append(events, event)
	}

	r.finishAssessedCandidate(t.Context(), sl, candidateAssessment{
		outcome: OutcomeCompletionContractMissing,
		reason:  "result absent",
	})

	if len(events) != 0 || r.safetyTripwireFired || r.dispatchCircuitReason != "" {
		t.Fatalf("slot-local missing terminal opened circuit: events=%+v fired=%t reason=%q",
			events, r.safetyTripwireFired, r.dispatchCircuitReason)
	}
	if got := r.run.Slots[sl.PhaseID]; got == nil || got.Status != ledger.SlotBlocked {
		t.Fatalf("failed slot = %+v, want blocked", got)
	}
	if got := r.run.Slots[sibling.PhaseID]; got == nil || got.Status != ledger.SlotRunning {
		t.Fatalf("sibling = %+v, want still running", got)
	}
}

func TestExplicitUnchangedRetryTripwireStillStopsCanary(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts.NativeCanary = true
	r.hardStopKinds = safetyTripwireSet([]SafetyTripwireKind{
		SafetyTripwireUnchangedRetry,
	})
	var events []SafetyTripwire
	r.opts.OnSafetyTripwire = func(event SafetyTripwire) {
		events = append(events, event)
	}

	r.recoverTyped(t.Context(), sl, typedRecoveryRequest{
		outcome: OutcomeCodeDefect,
		reason:  "same failure evidence",
	})

	if len(events) != 1 || events[0].Kind != SafetyTripwireUnchangedRetry ||
		!r.safetyTripwireFired || r.dispatchCircuitReason == "" {
		t.Fatalf("explicit unchanged tripwire = events=%+v fired=%t reason=%q",
			events, r.safetyTripwireFired, r.dispatchCircuitReason)
	}
}

func TestExhaustedRetryDoesNotSatisfyExplicitUnchangedTripwire(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts.NativeCanary = true
	r.hardStopKinds = safetyTripwireSet([]SafetyTripwireKind{
		SafetyTripwireUnchangedRetry,
	})
	sl.Retry.CodeRepairs = 1
	var events []SafetyTripwire
	r.opts.OnSafetyTripwire = func(event SafetyTripwire) {
		events = append(events, event)
	}

	r.recoverTyped(t.Context(), sl, typedRecoveryRequest{
		outcome:         OutcomeCodeDefect,
		evidenceChanged: true,
		reason:          "new evidence after the repair budget",
	})

	if len(events) != 0 || r.safetyTripwireFired || r.dispatchCircuitReason != "" {
		t.Fatalf("exhaustion was conflated with unchanged evidence: events=%+v fired=%t reason=%q",
			events, r.safetyTripwireFired, r.dispatchCircuitReason)
	}
	if got := r.run.Slots[sl.PhaseID]; got == nil || got.Status != ledger.SlotBlocked ||
		!strings.Contains(got.Note, "code-repair-exhausted") {
		t.Fatalf("exhausted slot = %+v, want local exhausted park", got)
	}
}

func TestFixedCohortDefersOutsideEpicValidationModel(t *testing.T) {
	fake := closedEpicFixture()
	r, calls := epicRunner(t, fake, epicreview.Verdict{Met: true})
	r.allowedIDs = map[string]struct{}{"inside": {}}
	r.epicPending = map[string]bool{"ep1": true}

	r.maybeStartEpicValidation(t.Context(), true)

	if calls.Load() != 0 || r.epicInFlight != "" {
		t.Fatalf("outside epic launched validator: calls=%d in_flight=%q",
			calls.Load(), r.epicInFlight)
	}
	if r.epicPending["ep1"] {
		t.Fatal("outside epic remained queued inside the fixed-cohort run")
	}
}
