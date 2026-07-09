// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// koryph-4ql.1 (docs/designs/2026-07-resource-governor.md L2/L5): the machine
// resource capacity ledger and reservation-aware admission. These tests
// exercise the govern half — capacity accounting across pools under the flock,
// the reservation-aware memory clause, the half-open probe interaction, ramp
// expiry, the schema decoder/setter round-trip, and additive back-compat.

// resLease is a lease declaring one or more resolved resource kinds.
func resLease(project, bead string, pid int, kinds ...string) Lease {
	return Lease{Project: project, Bead: bead, PID: pid, EnginePID: 1, Resources: kinds}
}

// memLease is a lease declaring a memory reservation (MB), no resource kinds.
func memLease(project, bead string, pid, memMB int) Lease {
	return Lease{Project: project, Bead: bead, PID: pid, EnginePID: 1, MemReserveMB: memMB}
}

// newStoreOnCurrentEnv builds a second Store rooted at the ALREADY-set
// KORYPH_HOME (so two Stores share one dir/flock, simulating two engines),
// with a fixed epoch-0 clock and an all-alive pid probe.
func newStoreOnCurrentEnv() *Store {
	s := NewStore()
	s.Now = func() time.Time { return time.Unix(0, 0).UTC() }
	s.Alive = func(int) bool { return true }
	return s
}

// --- capacity clause (L2) --------------------------------------------------

// TestDefaultCapacityOneSerializesUnconfiguredKind is §1.2's core assertion:
// with NO resources section at all, two beads declaring the same kind must not
// co-dispatch — the fail-safe default capacity 1 binds without configuration.
func TestDefaultCapacityOneSerializesUnconfiguredKind(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("", 8); err != nil { // pool cap is not the limiter here
		t.Fatal(err)
	}
	if ok, err := s.Acquire(resLease("p", "b1", 10, "kind-cluster")); err != nil || !ok {
		t.Fatalf("first res-kind acquire: ok=%v err=%v, want granted", ok, err)
	}
	res, err := s.AcquireEx(resLease("p", "b2", 11, "kind-cluster"), MemInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Granted {
		t.Fatal("second holder of an unconfigured kind admitted at default capacity 1")
	}
	if res.Outcome != AdmitDeniedResource || res.DeniedKind != "kind-cluster" ||
		res.DeniedCapacity != 1 || res.DeniedHolders != 1 || res.HolderBead != "b1" {
		t.Errorf("denial = %+v, want resource kind-cluster 1/1 held by b1", res)
	}
}

// TestCapacityTwoAdmitsBoth: a configured capacity 2 admits two holders and
// defers only the third.
func TestCapacityTwoAdmitsBoth(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("", 8); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResource("kind-cluster", ResourceKind{Capacity: 2}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Acquire(resLease("p", "b1", 10, "kind-cluster")); !ok {
		t.Fatal("first acquire denied at capacity 2")
	}
	if ok, _ := s.Acquire(resLease("p", "b2", 11, "kind-cluster")); !ok {
		t.Fatal("second acquire denied at capacity 2")
	}
	res, err := s.AcquireEx(resLease("p", "b3", 12, "kind-cluster"), MemInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Granted || res.Outcome != AdmitDeniedResource || res.DeniedCapacity != 2 || res.DeniedHolders != 2 {
		t.Errorf("third acquire = %+v, want denied resource 2/2", res)
	}
}

// TestMultipleKindsCapacityIndependent: distinct kinds do NOT collide with
// each other (unlike footprints' domain:unknown), and a lease holding several
// kinds is denied on the FIRST exhausted one.
func TestMultipleKindsCapacityIndependent(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 8)
	// kind-a at capacity 1 is already held; kind-b is free.
	if ok, _ := s.Acquire(resLease("p", "holder", 10, "kind-a")); !ok {
		t.Fatal("holder of kind-a denied")
	}
	// A bead needing only kind-b co-dispatches fine.
	if ok, _ := s.Acquire(resLease("p", "onlyb", 11, "kind-b")); !ok {
		t.Error("kind-b holder denied though kind-a's exhaustion is unrelated")
	}
	// A bead needing kind-a AND kind-b is denied on kind-a.
	res, err := s.AcquireEx(resLease("p", "both", 12, "kind-a", "kind-b"), MemInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Granted || res.DeniedKind != "kind-a" {
		t.Errorf("multi-kind denial = %+v, want denied on kind-a", res)
	}
}

// TestHoldWithoutAcquireCountedByCapacity: a lease attached via Hold (the
// requeue/resume path, no prior Acquire) still counts against a kind's
// capacity.
func TestHoldWithoutAcquireCountedByCapacity(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 8)
	if err := s.Hold(resLease("p", "b1", 10, "kind-cluster")); err != nil {
		t.Fatal(err)
	}
	res, err := s.AcquireEx(resLease("p", "b2", 11, "kind-cluster"), MemInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Granted {
		t.Error("Hold-attached holder was not counted by the capacity clause")
	}
}

// TestPruneFreesCapacity: a dead-pid holder is pruned at Acquire, freeing the
// kind for the next caller.
func TestPruneFreesCapacity(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 8)
	if ok, _ := s.Acquire(resLease("p", "b1", 10, "kind-cluster")); !ok {
		t.Fatal("first acquire denied")
	}
	s.Alive = func(pid int) bool { return pid != 10 } // b1's agent dies
	if ok, err := s.Acquire(resLease("p", "b2", 11, "kind-cluster")); err != nil || !ok {
		t.Errorf("acquire after dead holder pruned: ok=%v err=%v, want granted", ok, err)
	}
}

// TestCapacityCrossPool: machine resources are cross-pool — a kind held in the
// anthropic pool is at capacity for a candidate in the openai pool.
func TestCapacityCrossPool(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("anthropic", 8)
	_ = s.SetCap("openai", 8)
	// A kind-cluster holder in the anthropic pool.
	if ok, _ := s.Acquire(Lease{Project: "p", Bead: "b1", PID: 10, EnginePID: 1, Provider: "anthropic", Resources: []string{"kind-cluster"}}); !ok {
		t.Fatal("anthropic res acquire denied")
	}
	// A candidate in the openai pool declaring the same kind is denied — the
	// capacity clause counts holders across ALL pools.
	res, err := s.AcquireEx(Lease{Project: "p", Bead: "b2", PID: 11, EnginePID: 1, Provider: "openai", Resources: []string{"kind-cluster"}}, MemInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Granted {
		t.Error("openai candidate admitted though the kind is held in the anthropic pool (cross-pool capacity breached)")
	}
}

// --- half-open probe interaction (L2) --------------------------------------

// TestHalfOpenProbeRespectsCapacity: a resource-declared probe candidate that
// would breach capacity is denied WITHOUT claiming the probe (the slot stays
// open for the next caller), and a resource-free candidate then becomes the
// probe.
func TestHalfOpenProbeRespectsCapacity(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	s.Jitter = func() float64 { return -1 } // not a smoothing test
	if err := s.SetAdaptiveCap("", 4, 8, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a live holder of kind-cluster (survives prune: all-alive).
	if err := s.Hold(resLease("holder", "h1", 500, "kind-cluster")); err != nil {
		t.Fatal(err)
	}
	c := seedBreakerOpen(t, s, now)
	now = now.Add(time.Duration(c.BreakerBreakSeconds) * time.Second) // break elapsed → half-open eligible

	// The resource-declared candidate is denied on capacity and does NOT take
	// the probe.
	res, err := s.AcquireEx(resLease("p1", "b1", 100, "kind-cluster"), MemInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Granted || res.Outcome != AdmitDeniedResource {
		t.Fatalf("resource-declared probe candidate = %+v, want denied resource", res)
	}
	status, err := s.AIMDStatus("")
	if err != nil {
		t.Fatal(err)
	}
	if status.BreakerState != "half-open" || status.ProbeProject != "" {
		t.Fatalf("after resource-denied probe: state=%q probe=%q, want half-open with NO probe claimed", status.BreakerState, status.ProbeProject)
	}

	// A resource-free candidate now wins the still-open probe slot.
	res, err = s.AcquireEx(lease("p2", "b2", 200), MemInput{})
	if err != nil || !res.Granted {
		t.Fatalf("resource-free probe candidate = %+v err=%v, want granted (the probe)", res, err)
	}
	status, err = s.AIMDStatus("")
	if err != nil {
		t.Fatal(err)
	}
	if status.BreakerState != "half-open" || status.ProbeProject != "p2" || status.ProbeBead != "b2" {
		t.Errorf("status = %+v, want half-open with probe p2/b2", status)
	}
}

// --- reservation-aware memory clause (L5) ----------------------------------

// TestReservationAwareMemoryTwoStoreRace is §1.3's race: two Stores (two
// engines) sharing one dir/flock both try to admit a 6 GB-reserving bead
// against 8 GB free with a 2 GB floor — exactly one wins, because the first
// admission's ramping reservation is visible (under the flock) to the second.
func TestReservationAwareMemoryTwoStoreRace(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	s1 := newStoreOnCurrentEnv()
	s2 := newStoreOnCurrentEnv()
	_ = s1.SetCap("", 8) // pool cap is not the limiter

	mem := MemInput{AvailMB: 8192, FloorMB: 2048}
	r1, err := s1.AcquireEx(memLease("p", "b1", 10, 6144), mem)
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Granted {
		t.Fatalf("first 6 GB candidate = %+v, want granted", r1)
	}
	r2, err := s2.AcquireEx(memLease("p", "b2", 11, 6144), mem)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Granted {
		t.Fatalf("second 6 GB candidate = %+v, want denied (reservation-aware floor)", r2)
	}
	if r2.Outcome != AdmitDeniedMemory || !r2.CandidateTipped {
		t.Errorf("second denial = %+v, want denied-memory candidate-tipped", r2)
	}
	// Exactly one lease on disk.
	if _, leases, _, err := s1.Snapshot(""); err != nil {
		t.Fatal(err)
	} else if len(leases) != 1 {
		t.Errorf("leases on disk = %d, want exactly 1", len(leases))
	}
}

// TestMemoryPureFloorBreachNotTipped: a reading below the floor even for a
// ZERO-reserve candidate is a pure floor breach (CandidateTipped false →
// batch-break), distinct from a candidate-tipped skip.
func TestMemoryPureFloorBreachNotTipped(t *testing.T) {
	s := newTestStore(t)
	res, err := s.AcquireEx(memLease("p", "b1", 10, 0), MemInput{AvailMB: 1000, FloorMB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if res.Granted || res.Outcome != AdmitDeniedMemory {
		t.Fatalf("below-floor acquire = %+v, want denied-memory", res)
	}
	if res.CandidateTipped {
		t.Error("CandidateTipped=true for a pure floor breach (a 0-reserve bead would also fail)")
	}
}

// TestMemoryZeroReadingSkipsClause: no reading (or a zero floor) skips the
// memory clause entirely, degrading to today's behavior — even a huge
// reservation is admitted when the caller passes MemInput{}.
func TestMemoryZeroReadingSkipsClause(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 8)
	if ok, err := s.Acquire(memLease("p", "b1", 10, 999999)); err != nil || !ok {
		t.Errorf("no-reading acquire of a huge reservation: ok=%v err=%v, want granted (clause skipped)", ok, err)
	}
	// A zero floor also skips the clause.
	res, err := s.AcquireEx(memLease("p", "b2", 11, 999999), MemInput{AvailMB: 1, FloorMB: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Granted {
		t.Errorf("zero-floor acquire = %+v, want granted (clause skipped)", res)
	}
}

// TestRampExpiryFreesReservation: once a holder's ramp window elapses its
// reservation retires (the resource is assumed materialized in the real
// reading), so a second 6 GB candidate that was denied during the ramp is
// admitted after it.
func TestRampExpiryFreesReservation(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	_ = s.SetCap("", 8)
	mem := MemInput{AvailMB: 8192, FloorMB: 2048}

	if r, err := s.AcquireEx(memLease("p", "b1", 10, 6144), mem); err != nil || !r.Granted {
		t.Fatalf("first candidate at t0 = %+v err=%v, want granted", r, err)
	}
	// Still inside the ramp: the reservation is live, second candidate denied.
	if r, err := s.AcquireEx(memLease("p", "b2", 11, 6144), mem); err != nil {
		t.Fatal(err)
	} else if r.Granted {
		t.Fatal("second candidate admitted while the first is still ramping")
	}
	// Advance past the default ramp window: b1's reservation retires.
	now = now.Add(time.Duration(DefaultRampSeconds+1) * time.Second)
	if r, err := s.AcquireEx(memLease("p", "b3", 12, 6144), mem); err != nil || !r.Granted {
		t.Errorf("candidate after ramp expiry = %+v err=%v, want granted (reservation retired)", r, err)
	}
}

// TestPerKindRampOverride: a per-kind ramp_seconds override extends the window
// past the global default.
func TestPerKindRampOverride(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	_ = s.SetCap("", 8)
	// kind-cluster reserves 6 GB and ramps for a long time.
	if err := s.SetResource("kind-cluster", ResourceKind{Capacity: 8, MemMB: 6144, RampSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	mem := MemInput{AvailMB: 8192, FloorMB: 2048}
	first := Lease{Project: "p", Bead: "b1", PID: 10, EnginePID: 1, Resources: []string{"kind-cluster"}, MemReserveMB: 6144}
	if r, err := s.AcquireEx(first, mem); err != nil || !r.Granted {
		t.Fatalf("first = %+v err=%v, want granted", r, err)
	}
	// Past the GLOBAL default but inside the per-kind override → still ramping.
	now = now.Add(time.Duration(DefaultRampSeconds+60) * time.Second)
	second := Lease{Project: "p", Bead: "b2", PID: 11, EnginePID: 1, Resources: []string{"kind-cluster"}, MemReserveMB: 6144}
	if r, err := s.AcquireEx(second, mem); err != nil {
		t.Fatal(err)
	} else if r.Granted {
		t.Error("second candidate admitted inside the per-kind ramp override window")
	}
}

// --- Hold persistence (L2) -------------------------------------------------

// TestHoldRestampsAcquiredAtAndPreservesResources: Hold rewrites the lease with
// the caller-supplied Resources/MemReserveMB and (leaving AcquiredAt unset, as
// the engine does) restamps it — the ramp clock restarts per (re)bind.
func TestHoldRestampsAcquiredAtAndPreservesResources(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	_ = s.SetCap("", 8)

	acq := Lease{Project: "p", Bead: "b1", PID: 10, EnginePID: 1, Resources: []string{"kind-cluster"}, MemReserveMB: 6144}
	if ok, err := s.Acquire(acq); err != nil || !ok {
		t.Fatalf("acquire = ok=%v err=%v", ok, err)
	}
	t0 := now.UTC().Format(time.RFC3339)

	now = now.Add(5 * time.Minute) // rebind later
	hold := Lease{Project: "p", Bead: "b1", PID: 4242, EnginePID: 1, Resources: []string{"kind-cluster"}, MemReserveMB: 6144}
	if err := s.Hold(hold); err != nil {
		t.Fatal(err)
	}
	_, leases, _, err := s.Snapshot("")
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	got := leases[0]
	if got.PID != 4242 {
		t.Errorf("PID = %d, want 4242", got.PID)
	}
	if len(got.Resources) != 1 || got.Resources[0] != "kind-cluster" || got.MemReserveMB != 6144 {
		t.Errorf("resources after Hold = %v mem=%d, want [kind-cluster] 6144", got.Resources, got.MemReserveMB)
	}
	if got.AcquiredAt == t0 {
		t.Errorf("AcquiredAt = %q, want restamped away from the original %q (ramp restarts per rebind)", got.AcquiredAt, t0)
	}
	if want := now.UTC().Format(time.RFC3339); got.AcquiredAt != want {
		t.Errorf("AcquiredAt = %q, want restamped to %q", got.AcquiredAt, want)
	}
}

// --- schema: accessor, setters, decoder round-trip, back-compat ------------

// TestResourcesAccessorDefault: absent config fails open to an empty ledger in
// which every declared kind still resolves to the default capacity 1.
func TestResourcesAccessorDefault(t *testing.T) {
	s := newTestStore(t)
	rc := s.Resources()
	if rc.Kinds != nil || rc.RampSeconds != 0 {
		t.Errorf("Resources() with no config = %+v, want zero ResourcesConfig", rc)
	}
	if got := rc.capacityOf("anything"); got != DefaultResourceCapacity {
		t.Errorf("capacityOf(unconfigured) = %d, want %d", got, DefaultResourceCapacity)
	}
	if got := rc.rampSecondsOf("anything"); got != DefaultRampSeconds {
		t.Errorf("rampSecondsOf(unconfigured) = %d, want %d", got, DefaultRampSeconds)
	}
}

// TestSetUnsetResourcePreservesState: SetResource/UnsetResource preserve the
// pool cap, the memory floor, and every OTHER kind (the SetMinFreeMemoryMB
// preserve-don't-reset precedent, not SetCap's wholesale reset).
func TestSetUnsetResourcePreservesState(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("", 5); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMinFreeMemoryMB("", 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResource("kind-cluster", ResourceKind{Capacity: 2, MemMB: 6144, Probe: "kind get clusters"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResource("docker", ResourceKind{Capacity: 3}); err != nil {
		t.Fatal(err)
	}

	rc := s.Resources()
	if rc.capacityOf("kind-cluster") != 2 || rc.memMBOf("kind-cluster") != 6144 ||
		rc.probeOf("kind-cluster") != "kind get clusters" || rc.capacityOf("docker") != 3 {
		t.Errorf("resources after two SetResource = %+v, want both kinds set", rc)
	}
	if s.Cap("") != 5 {
		t.Errorf("cap = %d, want 5 preserved across SetResource", s.Cap(""))
	}
	if s.MinFreeMemoryMB("") != 1000 {
		t.Errorf("min free memory = %d, want 1000 preserved across SetResource", s.MinFreeMemoryMB(""))
	}

	if err := s.UnsetResource("docker"); err != nil {
		t.Fatal(err)
	}
	rc = s.Resources()
	if _, ok := rc.Kinds["docker"]; ok {
		t.Error("docker survived UnsetResource")
	}
	if rc.capacityOf("kind-cluster") != 2 {
		t.Error("kind-cluster lost across UnsetResource(docker)")
	}
	if s.Cap("") != 5 || s.MinFreeMemoryMB("") != 1000 {
		t.Error("cap/floor not preserved across UnsetResource")
	}
	// Unsetting a missing kind is idempotent.
	if err := s.UnsetResource("nonexistent"); err != nil {
		t.Errorf("UnsetResource(missing) = %v, want nil (idempotent)", err)
	}
}

// TestDecoderSetterRoundTrip pins the R1 decoder+setter pair: a pools+resources
// document must survive a plain SetCap rewrite. Without the File.UnmarshalJSON
// resources extension the section is dropped on read and then stripped from
// disk by SetCap's whole-file rewrite.
func TestDecoderSetterRoundTrip(t *testing.T) {
	s := newTestStore(t)
	doc := File{
		Pools: map[string]Config{DefaultPool: {MaxGlobalAgents: 8}},
		Resources: &ResourcesConfig{
			RampSeconds: 300,
			Kinds: map[string]ResourceKind{
				"kind-cluster": {Capacity: 2, MemMB: 6144, RampSeconds: 900, Probe: "kind get clusters"},
			},
		},
	}
	writeGovernorFile(t, s, doc)

	// A plain SetCap (which reads-modifies-writes the whole File) must NOT drop
	// the resources section.
	if err := s.SetCap("", 5); err != nil {
		t.Fatal(err)
	}
	if s.Cap("") != 5 {
		t.Errorf("cap after SetCap = %d, want 5", s.Cap(""))
	}
	rc := s.Resources()
	if rc.RampSeconds != 300 {
		t.Errorf("ramp_seconds after SetCap = %d, want 300 (section survived)", rc.RampSeconds)
	}
	k, ok := rc.Kinds["kind-cluster"]
	if !ok || k.Capacity != 2 || k.MemMB != 6144 || k.RampSeconds != 900 || k.Probe != "kind get clusters" {
		t.Errorf("kind-cluster after SetCap = %+v (ok=%v), want fully preserved", k, ok)
	}
}

// TestLegacyFlatDocumentHasNilResources: a pre-koryph-v8u.11 flat document
// migrates into the anthropic pool with NO resources section (genuinely nil),
// and Resources() fails open to the empty ledger.
func TestLegacyFlatDocumentHasNilResources(t *testing.T) {
	s := newTestStore(t)
	// Flat Config document — no "pools" key, no "resources" key.
	data, err := json.Marshal(Config{MaxGlobalAgents: 6})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := s.readFile()
	if err != nil {
		t.Fatal(err)
	}
	if f.Resources != nil {
		t.Errorf("legacy flat document decoded Resources = %+v, want nil", f.Resources)
	}
	if rc := s.Resources(); rc.Kinds != nil {
		t.Errorf("Resources() on legacy doc = %+v, want empty", rc)
	}
	// Migration still works — the cap is preserved.
	if s.Cap("") != 6 {
		t.Errorf("cap = %d, want 6 (legacy migration intact)", s.Cap(""))
	}
}

// TestOldShapeLeaseDecodesResourceFree: a lease file written before the
// resource fields existed decodes with empty Resources / zero MemReserveMB and
// is still counted by the concurrency cap.
func TestOldShapeLeaseDecodesResourceFree(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 1)
	// Old-shape lease: no "resources" / "mem_reserve_mb" keys.
	old := map[string]any{
		"project":     "p",
		"bead":        "b1",
		"pid":         10,
		"engine_pid":  1,
		"acquired_at": epoch0.Format(time.RFC3339),
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.slotsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.leasePath(DefaultPool, "p", "b1"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, leases, _, err := s.Snapshot("")
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	if len(leases[0].Resources) != 0 || leases[0].MemReserveMB != 0 {
		t.Errorf("old lease decoded = %+v, want resource-free", leases[0])
	}
	// It still occupies the cap-1 pool.
	if ok, _ := s.Acquire(lease("p", "b2", 11)); ok {
		t.Error("old-shape lease not counted toward the cap")
	}
}

// --- observability (L7) ----------------------------------------------------

// TestResourcesStatusRendersHoldersAndRampState: ResourcesStatus reports every
// configured and held kind with holders, ramp state, and the
// reserved-vs-materialized split, for `governor show`.
func TestResourcesStatusRendersHoldersAndRampState(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	_ = s.SetCap("", 8)
	if err := s.SetResource("kind-cluster", ResourceKind{Capacity: 2, MemMB: 6144}); err != nil {
		t.Fatal(err)
	}
	// One ramping holder (fresh) and — after advancing — one materialized.
	first := Lease{Project: "p", Bead: "b1", PID: 10, EnginePID: 1, Resources: []string{"kind-cluster"}, MemReserveMB: 6144}
	if ok, _ := s.Acquire(first); !ok {
		t.Fatal("first acquire denied")
	}
	now = now.Add(time.Duration(DefaultRampSeconds+1) * time.Second) // b1 now materialized
	second := Lease{Project: "p", Bead: "b2", PID: 11, EnginePID: 1, Resources: []string{"kind-cluster"}, MemReserveMB: 6144}
	if ok, _ := s.Acquire(second); !ok {
		t.Fatal("second acquire denied at capacity 2")
	}

	sts, err := s.ResourcesStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 1 || sts[0].Kind != "kind-cluster" {
		t.Fatalf("statuses = %+v, want one kind-cluster", sts)
	}
	st := sts[0]
	if st.Capacity != 2 || st.MemMB != 6144 {
		t.Errorf("status config = cap %d mem %d, want 2/6144", st.Capacity, st.MemMB)
	}
	if len(st.Holders) != 2 {
		t.Fatalf("holders = %d, want 2", len(st.Holders))
	}
	// b1 materialized (past ramp), b2 ramping.
	if st.ReservedMB != 6144 || st.MaterializedMB != 6144 {
		t.Errorf("reserved/materialized = %d/%d, want 6144/6144", st.ReservedMB, st.MaterializedMB)
	}
}

// --- helpers ---------------------------------------------------------------

// writeGovernorFile marshals doc and writes it to the store's config path.
func writeGovernorFile(t *testing.T, s *Store, doc File) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
