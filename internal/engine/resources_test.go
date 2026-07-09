// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"os"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/sysmem"
)

// TestClassifyAdmitSkipVsBreak pins the koryph-4ql.3 (design L3) verdict mapping:
// a per-bead denial (a declared resource kind at capacity, or the candidate's
// own memory reservation tipping the floor) SKIPS just that bead, while a
// machine-wide denial (pool cap / fair share / breaker / smoothing, or a pure
// memory floor breach even a zero-reserve bead fails) BREAKS the batch.
func TestClassifyAdmitSkipVsBreak(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	tests := []struct {
		name string
		res  govern.AdmitResult
		want admitVerdict
	}{
		{"granted", govern.AdmitResult{Granted: true, Outcome: govern.AdmitGranted}, admitGranted},
		{"resource at capacity → skip", govern.AdmitResult{
			Outcome: govern.AdmitDeniedResource, DeniedKind: "kind-cluster",
			DeniedCapacity: 1, DeniedHolders: 1, HolderProject: "other", HolderBead: "y1",
		}, admitSkip},
		{"pool cap → break", govern.AdmitResult{Outcome: govern.AdmitDeniedCap}, admitBreak},
		{"candidate-tipped memory → skip", govern.AdmitResult{
			Outcome: govern.AdmitDeniedMemory, CandidateTipped: true,
		}, admitSkip},
		{"pure floor breach → break", govern.AdmitResult{
			Outcome: govern.AdmitDeniedMemory, CandidateTipped: false,
		}, admitBreak},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.classifyAdmit("cand", 4096, tc.res); got != tc.want {
				t.Errorf("classifyAdmit(%+v) = %v, want %v", tc.res, got, tc.want)
			}
		})
	}
}

// TestResolveMemReserveMB pins the L5 reservation resolution order: machine
// ledger mem_mb (govern Store.Resources) overrides the project vocabulary, which
// overrides 0 for an unconfigured kind.
func TestResolveMemReserveMB(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	// Machine ledger: kind-cluster has an explicit per-host mem_mb; "shared" is
	// present in BOTH machine and project vocab so the machine wins.
	if err := r.gov.SetResource("kind-cluster", govern.ResourceKind{Capacity: 1, MemMB: 6144}); err != nil {
		t.Fatal(err)
	}
	if err := r.gov.SetResource("shared", govern.ResourceKind{Capacity: 2, MemMB: 999}); err != nil {
		t.Fatal(err)
	}
	// Project vocabulary: docker only here; shared also here (loses to machine).
	r.cfg.Resources = map[string]project.ResourceSpec{
		"docker": {MemMB: 512},
		"shared": {MemMB: 500},
	}

	tests := []struct {
		name  string
		kinds []string
		want  int
	}{
		{"nil → 0", nil, 0},
		{"machine ledger wins", []string{"kind-cluster"}, 6144},
		{"project vocabulary fallback", []string{"docker"}, 512},
		{"machine overrides project vocab", []string{"shared"}, 999},
		{"unconfigured kind → 0", []string{"mystery"}, 0},
		{"sum across kinds", []string{"kind-cluster", "docker"}, 6656},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.resolveMemReserveMB(tc.kinds); got != tc.want {
				t.Errorf("resolveMemReserveMB(%v) = %d, want %d", tc.kinds, got, tc.want)
			}
		})
	}
}

// TestResourceCapacities proves the engine exposes the machine ledger's per-kind
// capacities to sched (design L4), defaulting a configured-but-zero capacity to
// DefaultResourceCapacity and returning nil when nothing is configured.
func TestResourceCapacities(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()

	if got := r.resourceCapacities(); got != nil {
		t.Errorf("resourceCapacities with no kinds = %v, want nil", got)
	}
	if err := r.gov.SetResource("kind-cluster", govern.ResourceKind{Capacity: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.gov.SetResource("docker", govern.ResourceKind{Capacity: 3}); err != nil {
		t.Fatal(err)
	}
	if err := r.gov.SetResource("weird", govern.ResourceKind{MemMB: 100}); err != nil { // capacity 0 → default 1
		t.Fatal(err)
	}
	got := r.resourceCapacities()
	want := map[string]int{"kind-cluster": 1, "docker": 3, "weird": govern.DefaultResourceCapacity}
	if len(got) != len(want) {
		t.Fatalf("resourceCapacities = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("resourceCapacities[%q] = %d, want %d", k, got[k], v)
		}
	}
}

// TestResolveDispatchResourcesFreeze is the koryph-4ql.3 freeze-through-requeue
// analogue of TestRequeueFreezesModel: a requeue (q.resources set) re-attaches
// the resolved kinds + reservation from the first attempt VERBATIM, ignoring a
// mid-run relabel AND a mid-run vocabulary edit; only a fresh dispatch
// (q.resources nil) recomputes from the bead's live labels and the current
// vocabulary. resolveDispatchResources is the single seam every dispatch funnels
// through, so exercising it here covers all three poll.go requeue paths.
func TestResolveDispatchResourcesFreeze(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	if err := r.gov.SetResource("kind-cluster", govern.ResourceKind{Capacity: 1, MemMB: 6144}); err != nil {
		t.Fatal(err)
	}

	// A FRESH dispatch honors the bead's live res:* labels + current vocabulary.
	fresh := beads.Issue{ID: "tb1", Labels: []string{"res:kind-cluster"}}
	kinds, mem := r.resolveDispatchResources(dispatchReq{issue: fresh})
	if len(kinds) != 1 || kinds[0] != "kind-cluster" || mem != 6144 {
		t.Fatalf("fresh resolveDispatchResources = %v/%d, want [kind-cluster]/6144", kinds, mem)
	}

	// Now simulate a mid-run relabel (label removed) AND a vocabulary edit
	// (mem_mb halved). A REQUEUE carrying the frozen claim must ignore both.
	relabeled := beads.Issue{ID: "tb1", Labels: nil}
	if err := r.gov.SetResource("kind-cluster", govern.ResourceKind{Capacity: 1, MemMB: 3072}); err != nil {
		t.Fatal(err)
	}
	frozenKinds, frozenMem := r.resolveDispatchResources(dispatchReq{
		issue:     relabeled,
		resources: &dispatchResources{kinds: []string{"kind-cluster"}, memReserveMB: 6144},
	})
	if len(frozenKinds) != 1 || frozenKinds[0] != "kind-cluster" || frozenMem != 6144 {
		t.Errorf("frozen resolveDispatchResources = %v/%d, want [kind-cluster]/6144 (frozen, not the relabel/vocab edit)",
			frozenKinds, frozenMem)
	}

	// And a fresh dispatch of the relabeled bead now sees the empty set — proof
	// the freeze, not a stale cache, is what protected the requeue above.
	nowKinds, nowMem := r.resolveDispatchResources(dispatchReq{issue: relabeled})
	if len(nowKinds) != 0 || nowMem != 0 {
		t.Errorf("fresh dispatch of relabeled bead = %v/%d, want empty/0", nowKinds, nowMem)
	}
}

// TestActiveResourcesPersistedFirstAndPermissiveFallback proves the L3
// persisted-first fallback chain AND the documented asymmetry: unlike
// activeFootprints (whose unrecoverable-bead fallback is the conservative
// domain:unknown), a slot with no persisted Resources whose bead cannot be
// recovered contributes NOTHING — the maximally-permissive empty set.
func TestActiveResourcesPersistedFirstAndPermissiveFallback(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	ctx := context.Background()

	// Slot A: persisted Resources present → used verbatim (no adapter touch).
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "persisted-1", BeadID: "persisted-1", Status: ledger.SlotRunning,
		Resources: []string{"kind-cluster"},
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	// Slot B: no persisted Resources, but its bead is recoverable via r.issues
	// and carries a res:* label → recompute yields the kind.
	r.issues["recompute-1"] = beads.Issue{ID: "recompute-1", Labels: []string{"res:docker"}}
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "recompute-1", BeadID: "recompute-1", Status: ledger.SlotRunning,
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	// Slot C: no persisted Resources and its bead is unrecoverable (Show fails)
	// → permissive empty set, i.e. NO entry.
	r.adapter = &failingShowSource{}
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "ghost-1", BeadID: "ghost-1", Status: ledger.SlotRunning,
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	active := r.activeIDs()
	got := r.activeResources(ctx, active)

	if len(got["persisted-1"]) != 1 || got["persisted-1"][0] != "kind-cluster" {
		t.Errorf("persisted-1 holdings = %v, want [kind-cluster] (persisted-first)", got["persisted-1"])
	}
	if len(got["recompute-1"]) != 1 || got["recompute-1"][0] != "docker" {
		t.Errorf("recompute-1 holdings = %v, want [docker] (recompute fallback)", got["recompute-1"])
	}
	if _, ok := got["ghost-1"]; ok {
		t.Errorf("ghost-1 holdings = %v, want NO entry (permissive empty fallback)", got["ghost-1"])
	}
}

// TestAcquireGlobalSlotResourceSkip drives acquireGlobalSlot end to end against a
// real seeded governor: a capacity-1 kind already held by a foreign project's
// live lease denies this project's declaring bead with admitSkip (per-bead), not
// admitBreak — proving the L2 capacity clause is wired through AcquireEx.
func TestAcquireGlobalSlotResourceSkip(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	if err := r.gov.SetResource("kind-cluster", govern.ResourceKind{Capacity: 1}); err != nil {
		t.Fatal(err)
	}
	// A foreign project already holds the sole kind-cluster slot (alive pid).
	if err := r.gov.Hold(govern.Lease{
		Project: "other", Bead: "y1", PID: os.Getpid(), EnginePID: os.Getpid(),
		Resources: []string{"kind-cluster"},
	}); err != nil {
		t.Fatal(err)
	}

	if got := r.acquireGlobalSlot("tb1", []string{"kind-cluster"}, 0); got != admitSkip {
		t.Errorf("acquireGlobalSlot for a full capacity-1 kind = %v, want admitSkip", got)
	}
	// A bead declaring NOTHING still admits behind it (the whole point of skip).
	if got := r.acquireGlobalSlot("tb2", nil, 0); got != admitGranted {
		t.Errorf("acquireGlobalSlot for an undeclared bead = %v, want admitGranted", got)
	}
}

// TestAcquireGlobalSlotCapBreak proves a pool-cap denial classifies as
// admitBreak (machine-wide): the sole slot is held by a foreign lease, so this
// project's bead cannot fit and the batch breaks, exactly as before koryph-4ql.3.
func TestAcquireGlobalSlotCapBreak(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	if err := r.gov.SetCap("", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.gov.Hold(govern.Lease{
		Project: "other", Bead: "y1", PID: os.Getpid(), EnginePID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := r.acquireGlobalSlot("tb1", nil, 0); got != admitBreak {
		t.Errorf("acquireGlobalSlot at a full pool cap = %v, want admitBreak", got)
	}
}

// TestAcquireGlobalSlotMemInputHandoff proves the engine computes MemInput from
// its memProbe/floor OUTSIDE the flock and hands it to AcquireEx (L5): the
// candidate's own reservation tipping the floor SKIPS, while a foreign ramping
// reservation that breaches the floor even for a zero-reserve candidate BREAKS.
func TestAcquireGlobalSlotMemInputHandoff(t *testing.T) {
	f := newFixture(t, fixOpts{})
	// Enable an explicit floor (the fixture disables the gate); avail is above it
	// so the pre-flock memoryAdmits passes and AcquireEx's clause is what fires.
	t.Setenv("KORYPH_MIN_FREE_MEMORY_MB", "2000")

	t.Run("candidate-tipped → skip", func(t *testing.T) {
		r := runnerFromFixture(t, f)
		r.gov = govern.NewStore()
		r.memProbe = func() (sysmem.Stat, bool) { return memStatMB(16000, 5000), true }
		// availLessReserved = 5000 (no other reservations) ≥ 2000 floor, but the
		// candidate's own 4000 MB reservation drops it to 1000 < 2000 → tipped.
		if got := r.acquireGlobalSlot("tb1", []string{"kind-cluster"}, 4000); got != admitSkip {
			t.Errorf("candidate-tipped memory denial = %v, want admitSkip", got)
		}
	})

	t.Run("pure floor breach → break", func(t *testing.T) {
		r := runnerFromFixture(t, f)
		r.gov = govern.NewStore()
		r.memProbe = func() (sysmem.Stat, bool) { return memStatMB(16000, 5000), true }
		// A foreign, still-ramping lease reserves 4000 MB → availLessReserved =
		// 1000 < 2000 floor even for this zero-reserve candidate → pure breach.
		if err := r.gov.Hold(govern.Lease{
			Project: "other", Bead: "y1", PID: os.Getpid(), EnginePID: os.Getpid(),
			Resources: []string{"kind-cluster"}, MemReserveMB: 4000,
		}); err != nil {
			t.Fatal(err)
		}
		if got := r.acquireGlobalSlot("tb1", nil, 0); got != admitBreak {
			t.Errorf("pure floor breach = %v, want admitBreak", got)
		}
	})
}
