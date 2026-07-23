// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"os"
	"sort"
	"testing"
)

// TestSetCapSeedsMemoryFloorOnCreation proves the koryph-4rk6.1 "seed on
// creation" half: a pool that has never been configured gets
// DefaultMinFreeMemoryMB the moment SetCap first creates it — the uniform
// floor fix for the 2026-07-21 incident, where a fresh "personal"/"work" pool
// got a cap but no memory floor at all.
func TestSetCapSeedsMemoryFloorOnCreation(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("personal", 16); err != nil {
		t.Fatalf("SetCap: %v", err)
	}
	if got := s.MinFreeMemoryMB("personal"); got != DefaultMinFreeMemoryMB {
		t.Errorf("MinFreeMemoryMB(personal) after fresh SetCap = %d, want %d", got, DefaultMinFreeMemoryMB)
	}
}

// TestEstPerAgentMBRoundTripAndPreserve pins the koryph-3xs setter/accessor: a
// fresh pool reads 0 (unset → caller applies the default), SetEstPerAgentMB
// round-trips the raw value, and it preserves the pool's cap AND memory floor
// (the SetMinFreeMemoryMB preserve-don't-reset precedent).
func TestEstPerAgentMBRoundTripAndPreserve(t *testing.T) {
	s := newTestStore(t)
	if got := s.EstPerAgentMB("personal"); got != 0 {
		t.Errorf("EstPerAgentMB(unset pool) = %d, want 0 (unset)", got)
	}
	if err := s.SetCap("personal", 16); err != nil {
		t.Fatalf("SetCap: %v", err)
	}
	if err := s.SetMinFreeMemoryMB("personal", 4096); err != nil {
		t.Fatalf("SetMinFreeMemoryMB: %v", err)
	}
	if err := s.SetEstPerAgentMB("personal", 2048); err != nil {
		t.Fatalf("SetEstPerAgentMB: %v", err)
	}
	if got := s.EstPerAgentMB("personal"); got != 2048 {
		t.Errorf("EstPerAgentMB after set = %d, want 2048", got)
	}
	if got := s.Cap("personal"); got != 16 {
		t.Errorf("Cap after SetEstPerAgentMB = %d, want 16 (preserved)", got)
	}
	if got := s.MinFreeMemoryMB("personal"); got != 4096 {
		t.Errorf("MinFreeMemoryMB after SetEstPerAgentMB = %d, want 4096 (preserved)", got)
	}
	// A negative value (disable) round-trips as the raw setting for readers.
	if err := s.SetEstPerAgentMB("personal", -1); err != nil {
		t.Fatalf("SetEstPerAgentMB(-1): %v", err)
	}
	if got := s.EstPerAgentMB("personal"); got != -1 {
		t.Errorf("EstPerAgentMB after disable = %d, want -1", got)
	}
}

// TestSetCapPreservesExistingMemoryFloor proves a cap-only change to an
// ALREADY-configured pool does not clobber that pool's own explicit floor —
// SetCap's documented wholesale AIMD reset must not extend to the memory
// floor.
func TestSetCapPreservesExistingMemoryFloor(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("personal", 16); err != nil {
		t.Fatalf("SetCap: %v", err)
	}
	if err := s.SetMinFreeMemoryMB("personal", 4096); err != nil {
		t.Fatalf("SetMinFreeMemoryMB: %v", err)
	}
	// A later, unrelated cap change must preserve the explicit 4096, not
	// reset it back to the seeded default.
	if err := s.SetCap("personal", 20); err != nil {
		t.Fatalf("SetCap (again): %v", err)
	}
	if got := s.MinFreeMemoryMB("personal"); got != 4096 {
		t.Errorf("MinFreeMemoryMB(personal) after second SetCap = %d, want 4096 (preserved)", got)
	}
	if got := s.Cap("personal"); got != 20 {
		t.Errorf("Cap(personal) = %d, want 20", got)
	}
}

// TestBackfillMemoryFloorsSeedsPreExistingPools proves the koryph-4rk6.1
// "backfill existing pools on load" half: a governor.json written by an
// older koryph version — a pool with a cap but no min_free_memory_mb key at
// all, exactly the incident's "personal"/"work" shape — gets the uniform
// default floor once BackfillMemoryFloors runs, without disturbing its cap.
func TestBackfillMemoryFloorsSeedsPreExistingPools(t *testing.T) {
	s := newTestStore(t)
	// Simulate a legacy governor.json: "personal" has a cap and nothing else,
	// "anthropic" already ships its own explicit floor (matching the
	// incident's actual pre-fix shape).
	legacy := `{"pools":{"personal":{"max_global_agents":16},"anthropic":{"max_global_agents":8,"min_free_memory_mb":2048}}}`
	if err := os.WriteFile(s.cfgPath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("seed legacy governor.json: %v", err)
	}

	changed, err := s.BackfillMemoryFloors()
	if err != nil {
		t.Fatalf("BackfillMemoryFloors: %v", err)
	}
	if want := []string{"personal"}; !equalStrs(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if got := s.MinFreeMemoryMB("personal"); got != DefaultMinFreeMemoryMB {
		t.Errorf("MinFreeMemoryMB(personal) after backfill = %d, want %d", got, DefaultMinFreeMemoryMB)
	}
	// The already-explicit anthropic pool is untouched.
	if got := s.MinFreeMemoryMB("anthropic"); got != 2048 {
		t.Errorf("MinFreeMemoryMB(anthropic) after backfill = %d, want 2048 (untouched)", got)
	}
	// Caps survive the backfill write.
	if got := s.Cap("personal"); got != 16 {
		t.Errorf("Cap(personal) after backfill = %d, want 16", got)
	}
	if got := s.Cap("anthropic"); got != 8 {
		t.Errorf("Cap(anthropic) after backfill = %d, want 8", got)
	}
}

// TestBackfillMemoryFloorsSkipsExplicitDisable proves a pool an operator
// deliberately disabled (negative sentinel) is left alone by the backfill —
// only a raw 0 (never configured / reset-to-auto) is seeded.
func TestBackfillMemoryFloorsSkipsExplicitDisable(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("work", 4); err != nil {
		t.Fatalf("SetCap: %v", err)
	}
	if err := s.SetMinFreeMemoryMB("work", -1); err != nil {
		t.Fatalf("SetMinFreeMemoryMB: %v", err)
	}
	changed, err := s.BackfillMemoryFloors()
	if err != nil {
		t.Fatalf("BackfillMemoryFloors: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none (explicit disable must survive backfill)", changed)
	}
	if got := s.MinFreeMemoryMB("work"); got != -1 {
		t.Errorf("MinFreeMemoryMB(work) after backfill = %d, want -1 (untouched)", got)
	}
}

// TestBackfillMemoryFloorsNoOpOnFreshHome proves a "fresh" governor.json
// (absent entirely — nothing has ever configured a pool) is a true no-op:
// nothing to backfill, and no file is conjured into existence by the call
// itself. A fresh pool instead gets its floor via SetCap's seed-on-creation
// (TestSetCapSeedsMemoryFloorOnCreation).
func TestBackfillMemoryFloorsNoOpOnFreshHome(t *testing.T) {
	s := newTestStore(t)
	changed, err := s.BackfillMemoryFloors()
	if err != nil {
		t.Fatalf("BackfillMemoryFloors on fresh home: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none on a fresh (pool-less) home", changed)
	}
	if _, err := os.Stat(s.cfgPath); !os.IsNotExist(err) {
		t.Errorf("governor.json should still be absent after a no-op backfill, stat err = %v", err)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
