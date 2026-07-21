// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// koryph-bzf: resume must honor the RESUMING run's --max, not the stalled run's
// width. The pre-fix resume() re-dispatched every classified stalled bead at
// once (requeueSlot → dispatchBead), respecting neither the effective width nor
// the global governor — so lowering --max on resume was silently ignored. The
// fix parks stalled beads in a SlotQueued backlog and drainResumeBacklog
// promotes them capped at the current width.

// seedQueued installs a slot already parked in the resume backlog, as resume()
// would leave it: SlotQueued, dead agent (PID 0), model/persona frozen so the
// drain's requeueSlot → dispatchBead resolves without touching live labels.
func seedQueued(t *testing.T, r *runner, id string) {
	t.Helper()
	sl := &ledger.Slot{
		PhaseID:  id,
		BeadID:   id,
		Status:   ledger.SlotQueued,
		Model:    "sonnet",
		Agent:    "koryph-implementer",
		ModelWhy: "test frozen",
		Commits:  1,
	}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot(%s): %v", id, err)
	}
}

// drainRunner wires a runner able to drive drainResumeBacklog end-to-end: a fake
// work source (bead never closed), a stub runtime, and a capturing backend so a
// promotion reaches dispatchBead and replaces the SlotQueued slot with a live one.
func drainRunner(t *testing.T, r *runner) *capturingBackend {
	t.Helper()
	// Disable the memory admission floor so acquireGlobalSlot's verdict never
	// depends on the host's free memory (gov is nil, so only the floor could
	// deny) — otherwise this test would flake on a loaded machine.
	t.Setenv("KORYPH_MIN_FREE_MEMORY_MB", "-1")
	r.adapter = &fakeSource{}
	r.quotaCfg = &quota.Config{}
	r.rt = runtimetest.Stub{}
	backend := &capturingBackend{}
	r.backend = backend
	return backend
}

// TestResumeParksStalledBeadsAsBacklog proves resume() no longer fans every
// stalled bead out immediately: each dead-with-commits slot is classified
// requeue-resume and parked SlotQueued (dead PID cleared), and nothing is
// dispatched during resume() itself — dispatch is deferred to the width-gated
// boundary drain.
func TestResumeParksStalledBeadsAsBacklog(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	backend := drainRunner(t, r)

	// Three beads stalled by a killed run: dead agents, each with a landed
	// commit (Commits>0 → requeue-resume without a git probe).
	for _, id := range []string{"tb1", "tb2", "tb3"} {
		sl := &ledger.Slot{PhaseID: id, BeadID: id, Status: ledger.SlotRunning, PID: deadPID(t), Commits: 1}
		if err := r.store.SetSlot(r.run, sl); err != nil {
			t.Fatalf("SetSlot(%s): %v", id, err)
		}
	}

	resumed, err := r.resume(context.Background())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !resumed {
		t.Fatal("resume returned false — stalled beads should be adopted, not discarded for a fresh run")
	}
	if len(backend.specs) != 0 {
		t.Fatalf("resume dispatched %d agent(s); it must park the backlog and dispatch nothing itself (koryph-bzf)", len(backend.specs))
	}
	if got := len(r.queuedResumeIDs()); got != 3 {
		t.Fatalf("queued backlog = %d, want 3 (every stalled bead parked SlotQueued)", got)
	}
	if got := r.liveActiveCount(); got != 0 {
		t.Fatalf("liveActiveCount = %d, want 0 (a parked backlog reserves width but runs no agent)", got)
	}
	for _, id := range r.queuedResumeIDs() {
		if pid := r.run.Slots[id].PID; pid != 0 {
			t.Errorf("slot %s kept PID %d after parking; the dead agent's PID must be cleared", id, pid)
		}
	}
}

// TestDrainResumeBacklogHonorsWidth is the core regression: three queued beads,
// width 2 — one boundary drain promotes exactly two, the third waits until a
// live slot frees. Before the fix all three would have dispatched at once.
func TestDrainResumeBacklogHonorsWidth(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	backend := drainRunner(t, r)
	ctx := context.Background()

	seedQueued(t, r, "tb1")
	seedQueued(t, r, "tb2")
	seedQueued(t, r, "tb3")

	// First boundary at width 2: promote two, hold one back.
	r.drainResumeBacklog(ctx, 2, true)
	if len(backend.specs) != 2 {
		t.Fatalf("dispatched %d, want 2 — the drain must cap at width, not fan out the whole backlog", len(backend.specs))
	}
	if got := r.liveActiveCount(); got != 2 {
		t.Fatalf("liveActiveCount = %d, want 2", got)
	}
	if got := len(r.queuedResumeIDs()); got != 1 {
		t.Fatalf("queued backlog = %d, want 1 held back under the width cap", got)
	}

	// Free the two live slots (as if they merged), then drain again: the last
	// backlog bead promotes. Only the live slots — activePhaseIDs also lists the
	// still-queued backlog slot, which must stay SlotQueued.
	for _, id := range r.activePhaseIDs() {
		if r.run.Slots[id].Status == ledger.SlotQueued {
			continue
		}
		_ = r.store.UpdateSlot(r.run, id, func(s *ledger.Slot) { s.Status = ledger.SlotMerged })
	}
	r.drainResumeBacklog(ctx, 2, true)
	if len(backend.specs) != 3 {
		t.Fatalf("dispatched %d, want 3 after a slot freed — the held-back bead must resume", len(backend.specs))
	}
	if got := len(r.queuedResumeIDs()); got != 0 {
		t.Fatalf("queued backlog = %d, want 0 (backlog fully drained)", got)
	}
}

// TestResumeRedispatchDoesNotConsumeAttempt proves D4 (faults ≠ dispatches): a
// resume backlog re-dispatch — an engine restart or width deferral, not a bead
// fault — must leave the attempt counter unchanged, so a mid-run restart never
// pushes a bead toward the final-attempt model escalation it did not earn.
func TestResumeRedispatchDoesNotConsumeAttempt(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	drainRunner(t, r)
	ctx := context.Background()

	// A bead that already spent two real fault attempts, parked in the resume
	// backlog when the engine was interrupted.
	sl := &ledger.Slot{
		PhaseID: "tb1", BeadID: "tb1", Status: ledger.SlotQueued,
		Model: "sonnet", Agent: "koryph-implementer", ModelWhy: "test frozen",
		Commits: 1, Attempts: 2,
	}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	r.drainResumeBacklog(ctx, 2, true)

	if got := r.run.Slots["tb1"].Attempts; got != 2 {
		t.Errorf("Attempts=%d after a resume re-dispatch, want 2 unchanged — resume is not a fault (D4)", got)
	}
}

// TestDrainResumeBacklogSkippedWhenDispatchForbidden proves the drain is a no-op
// when dispatch is not allowed this boundary (quota stop / operator drain): the
// backlog is preserved untouched for a later --resume rather than force-placed.
func TestDrainResumeBacklogSkippedWhenDispatchForbidden(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	backend := drainRunner(t, r)

	seedQueued(t, r, "tb1")
	seedQueued(t, r, "tb2")

	r.drainResumeBacklog(context.Background(), 4, false)
	if len(backend.specs) != 0 {
		t.Fatalf("dispatched %d with dispatch forbidden; want 0", len(backend.specs))
	}
	if got := len(r.queuedResumeIDs()); got != 2 {
		t.Fatalf("queued backlog = %d, want 2 preserved for a later resume", got)
	}
}

// TestDrainResumeBacklogClosedBeadDropsOut proves a bead the operator retired
// while the run was down leaves the backlog (blocked) instead of spinning: a
// closed bead would otherwise fail requeueSlot every boundary and stay queued
// forever.
func TestDrainResumeBacklogClosedBeadDropsOut(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	backend := drainRunner(t, r)
	// The bead reports closed, so beadClosedMidFlight blocks it.
	r.adapter = &closedSource{}

	seedQueued(t, r, "tb1")

	r.drainResumeBacklog(context.Background(), 2, true)
	if len(backend.specs) != 0 {
		t.Fatalf("dispatched %d; a closed bead must not be re-dispatched", len(backend.specs))
	}
	if got := len(r.queuedResumeIDs()); got != 0 {
		t.Fatalf("queued backlog = %d, want 0 — the closed bead must drop out, not spin", got)
	}
	if got := r.run.Slots["tb1"].Status; got != ledger.SlotBlocked {
		t.Errorf("closed backlog slot status = %q, want %q", got, ledger.SlotBlocked)
	}
}

// closedSource is a fakeSource whose Show reports every bead closed, driving
// beadClosedMidFlight true.
type closedSource struct{ fakeSource }

func (closedSource) Show(_ context.Context, id string) (beads.Issue, error) {
	return beads.Issue{ID: id, Status: "closed"}, nil
}
