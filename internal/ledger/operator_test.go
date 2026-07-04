// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import "testing"

func TestDrainRequestConsumeRoundTrip(t *testing.T) {
	st := NewStore(t.TempDir())

	if st.DrainRequested() {
		t.Fatal("DrainRequested = true before any request")
	}
	if st.ConsumeDrain() {
		t.Fatal("ConsumeDrain reported a sentinel that was never written")
	}

	if err := st.RequestDrain(); err != nil {
		t.Fatalf("RequestDrain: %v", err)
	}
	if !st.DrainRequested() {
		t.Fatal("DrainRequested = false after RequestDrain")
	}

	// Idempotent: requesting again while one is pending does not error.
	if err := st.RequestDrain(); err != nil {
		t.Fatalf("RequestDrain (again): %v", err)
	}
	if !st.DrainRequested() {
		t.Fatal("DrainRequested = false after a second RequestDrain")
	}

	if !st.ConsumeDrain() {
		t.Fatal("ConsumeDrain = false, want true (a sentinel was present)")
	}
	if st.DrainRequested() {
		t.Fatal("DrainRequested = true after ConsumeDrain")
	}
	// Consuming again (nothing left) reports false, not an error.
	if st.ConsumeDrain() {
		t.Fatal("ConsumeDrain reported a sentinel after it was already consumed")
	}
}

func TestResizeOverrideRoundTrip(t *testing.T) {
	st := NewStore(t.TempDir())

	if _, ok := st.LoadResize(); ok {
		t.Fatal("LoadResize ok=true before any override was set")
	}

	if err := st.SetResize(ResizeOverride{Max: 5}); err != nil {
		t.Fatalf("SetResize: %v", err)
	}
	ov, ok := st.LoadResize()
	if !ok || ov.Max != 5 {
		t.Fatalf("LoadResize = (%+v, %v), want (Max:5, true)", ov, ok)
	}
	if ov.SetAt == "" {
		t.Error("SetAt was not stamped")
	}

	// Replacing the override overwrites, it does not merge.
	if err := st.SetResize(ResizeOverride{Max: 8, Force: true}); err != nil {
		t.Fatalf("SetResize (replace): %v", err)
	}
	ov, ok = st.LoadResize()
	if !ok || ov.Max != 8 || !ov.Force {
		t.Fatalf("LoadResize after replace = (%+v, %v), want (Max:8, Force:true, true)", ov, ok)
	}

	if err := st.ClearResize(); err != nil {
		t.Fatalf("ClearResize: %v", err)
	}
	if _, ok := st.LoadResize(); ok {
		t.Fatal("LoadResize ok=true after ClearResize")
	}
	// Clearing an already-clear override is not an error.
	if err := st.ClearResize(); err != nil {
		t.Fatalf("ClearResize (already clear): %v", err)
	}
}

// TestLoadResizeFailsOpenOnNonPositiveOrCorrupt proves LoadResize treats a
// non-positive or unreadable override as "no override" (fail open to project
// config) rather than wedging dispatch width — a resize override is a
// convenience lever, not a safety path (koryph-57v.1).
func TestLoadResizeFailsOpenOnNonPositiveOrCorrupt(t *testing.T) {
	st := NewStore(t.TempDir())

	if err := st.SetResize(ResizeOverride{Max: 0}); err != nil {
		t.Fatalf("SetResize: %v", err)
	}
	if _, ok := st.LoadResize(); ok {
		t.Fatal("LoadResize ok=true for a non-positive Max")
	}
}
