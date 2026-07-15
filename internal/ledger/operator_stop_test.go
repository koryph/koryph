// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import "testing"

// TestOperatorStopSentinelRoundtrip pins koryph-a1x (F1a): per-phase stop
// markers are independent, presence is the signal, and ConsumeStop clears
// exactly one phase.
func TestOperatorStopSentinelRoundtrip(t *testing.T) {
	s := NewStore(t.TempDir())

	if s.StopRequested("p1") {
		t.Fatal("no stop requested yet")
	}
	if err := s.RequestStop("p1"); err != nil {
		t.Fatalf("RequestStop: %v", err)
	}
	if !s.StopRequested("p1") {
		t.Fatal("stop should be requested after RequestStop")
	}
	if s.StopRequested("p2") {
		t.Fatal("p2 must be independent of p1")
	}
	if !s.ConsumeStop("p1") {
		t.Fatal("ConsumeStop should report the marker was present")
	}
	if s.StopRequested("p1") {
		t.Fatal("stop should be cleared after ConsumeStop")
	}
	if s.ConsumeStop("p1") {
		t.Fatal("second ConsumeStop should report absent")
	}
}

// TestClearStopsRemovesAll pins the run-start safety: a stranded marker from a
// prior run must not survive into a fresh run.
func TestClearStopsRemovesAll(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, p := range []string{"a", "b", "c"} {
		if err := s.RequestStop(p); err != nil {
			t.Fatalf("RequestStop(%s): %v", p, err)
		}
	}
	s.ClearStops()
	for _, p := range []string{"a", "b", "c"} {
		if s.StopRequested(p) {
			t.Errorf("StopRequested(%s) after ClearStops = true, want false", p)
		}
	}
	// ClearStops on an already-clear store is a no-op, not an error.
	s.ClearStops()
}

// TestStopMarkerNameFlattensSeparator proves a stray path separator cannot let a
// stop marker escape stopDir.
func TestStopMarkerNameFlattensSeparator(t *testing.T) {
	if got := stopMarkerName("a/b"); got != "a_b" {
		t.Errorf("stopMarkerName(a/b) = %q, want a_b", got)
	}
}
