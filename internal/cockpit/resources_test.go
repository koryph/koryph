// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"testing"
	"time"

	"github.com/koryph/koryph/internal/govern"
)

// koryph-4ql.10 (design docs/designs/2026-07-resource-governor.md L7, §4
// "Cockpit snapshots"): the governor view's per-kind resources section, and
// its old-snapshot tolerance (a governor with no res:* activity anywhere
// must not error and must leave GovernorSnapshot.Resources nil/empty).

// newTestGovernorStore points a fresh govern.Store at an isolated KORYPH_HOME
// so tests never share state (the newTestStore precedent from
// internal/govern/govern_test.go, reimplemented here since it is unexported).
func newTestGovernorStore(t *testing.T) *govern.Store {
	t.Helper()
	t.Setenv("KORYPH_HOME", t.TempDir())
	return govern.NewStore()
}

// awaitDerived joins the background refreshDerived pass that Refresh kicks when
// the derived caches are stale (always, on a fresh provider). That goroutine
// runs bd/git subprocesses against the provider's repoRoot (and reads the
// govern store under KORYPH_HOME) with no lock held; if the test returns while
// it is still in flight, t.TempDir()'s RemoveAll races those writes and fails
// teardown with "directory not empty" (koryph-1hz). Registered via t.Cleanup,
// which runs LIFO — before the earlier t.TempDir() removals — so the join
// always precedes removal. Mirrors the derivedRefreshing poll in
// TestDerivedRefreshTimeoutResetsLatch.
func awaitDerived(t *testing.T, p *LedgerProvider) {
	t.Helper()
	t.Cleanup(func() {
		// Ceiling above derivedRefreshTimeout (60s) so a bd bounded by its own
		// timeout still resolves here; the poll returns the instant the latch
		// clears, which is sub-second once bd/git error out on the temp repo.
		deadline := time.Now().Add(90 * time.Second)
		for {
			p.mu.Lock()
			refreshing := p.derivedRefreshing
			p.mu.Unlock()
			if !refreshing {
				return
			}
			if time.Now().After(deadline) {
				t.Error("refreshDerived still in flight at cleanup; TempDir removal will race it")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// TestRefreshGovernor_ResourcesAbsent is the old-snapshot-tolerance half of
// the round-trip: a governor with no resources configured and no lease ever
// declaring a res:<kind> must refresh cleanly with a nil/empty Resources
// section (additive/omitempty — old consumers see nothing new).
func TestRefreshGovernor_ResourcesAbsent(t *testing.T) {
	newTestGovernorStore(t) // sets KORYPH_HOME; the provider builds its own Store on it

	p := NewLedgerProvider("proj", t.TempDir(), "")
	awaitDerived(t, p) // join the background refreshDerived before TempDir cleanup (koryph-1hz)
	snap, err := p.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(snap.Governor.Resources) != 0 {
		t.Errorf("Resources = %+v, want empty for a governor with no res:* activity", snap.Governor.Resources)
	}
}

// TestRefreshGovernor_ResourcesPresent is the populated half of the
// round-trip: a configured kind with a live holder must surface on
// GovernorSnapshot.Resources with capacity, the holder (project/bead), and
// the reserved-vs-materialized split.
func TestRefreshGovernor_ResourcesPresent(t *testing.T) {
	gs := newTestGovernorStore(t)
	if err := gs.SetResource("kind-cluster", govern.ResourceKind{Capacity: 1, MemMB: 6144}); err != nil {
		t.Fatalf("SetResource: %v", err)
	}
	ok, err := gs.Acquire(govern.Lease{
		Project: "proj", Bead: "koryph-abc", PID: 10, EnginePID: 1,
		Resources: []string{"kind-cluster"}, MemReserveMB: 6144,
	})
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}

	p := NewLedgerProvider("proj", t.TempDir(), "")
	awaitDerived(t, p) // join the background refreshDerived before TempDir cleanup (koryph-1hz)
	// The lease's PID (10) is not a real live process; override liveness so
	// govern's prune pass (which ResourcesStatus runs first) does not reap it
	// before we can observe it — the newTestStore precedent from
	// internal/govern/govern_test.go, applied to the provider's own Store
	// (same package, unexported field).
	p.gs.Alive = func(int) bool { return true }
	snap, err := p.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if len(snap.Governor.Resources) != 1 {
		t.Fatalf("Resources = %+v, want 1 kind", snap.Governor.Resources)
	}
	rs := snap.Governor.Resources[0]
	if rs.Kind != "kind-cluster" || rs.Capacity != 1 || rs.MemMB != 6144 {
		t.Errorf("resource snapshot = %+v, want kind-cluster cap=1 mem=6144", rs)
	}
	if len(rs.Holders) != 1 || rs.Holders[0].Project != "proj" || rs.Holders[0].Bead != "koryph-abc" {
		t.Fatalf("Holders = %+v, want one holder proj/koryph-abc", rs.Holders)
	}
	// A freshly acquired lease starts ramping (AcquiredAt just stamped), so
	// its reservation is still subtracted, not yet materialized.
	if !rs.Holders[0].Ramping {
		t.Error("Holders[0].Ramping = false, want true for a just-acquired lease")
	}
	if rs.ReservedMB != 6144 || rs.MaterializedMB != 0 {
		t.Errorf("ReservedMB/MaterializedMB = %d/%d, want 6144/0 while ramping", rs.ReservedMB, rs.MaterializedMB)
	}
}
