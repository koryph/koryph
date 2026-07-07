// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package epicreview

import "testing"

func TestParkedNoteRoundTrip(t *testing.T) {
	note := FormatParkedNote(3, 2, "koryph epic validate koryph-abc --project myproj")
	round, maxRounds, ok := ParseParkedNote(note)
	if !ok {
		t.Fatalf("ParseParkedNote(%q) ok = false, want true", note)
	}
	if round != 3 {
		t.Errorf("round = %d, want 3", round)
	}
	if maxRounds != 2 {
		t.Errorf("maxRounds = %d, want 2", maxRounds)
	}
}

func TestParseParkedNoteLegacyForm(t *testing.T) {
	// Space-form note emitted by engine.parkEpic before koryph-qta.7 —
	// existing beads in the wild carry this; the parser must still accept it.
	note := "validation parked: round 3 would exceed max_rounds=2. Operator recovery: koryph epic validate koryph-abc --project myproj"
	round, maxRounds, ok := ParseParkedNote(note)
	if !ok {
		t.Fatalf("ParseParkedNote(%q) ok = false, want true", note)
	}
	if round != 3 {
		t.Errorf("round = %d, want 3", round)
	}
	if maxRounds != 2 {
		t.Errorf("maxRounds = %d, want 2", maxRounds)
	}
}

func TestParseParkedNoteNoMatch(t *testing.T) {
	if _, _, ok := ParseParkedNote("unrelated note"); ok {
		t.Error("ParseParkedNote(unrelated) ok = true, want false")
	}
}

func TestDegradedNoteRoundTrip(t *testing.T) {
	note := FormatDegradedNote(1, "claude subprocess exited 1")
	round, reason, ok := ParseDegradedNote(note)
	if !ok {
		t.Fatalf("ParseDegradedNote(%q) ok = false, want true", note)
	}
	if round != 1 {
		t.Errorf("round = %d, want 1", round)
	}
	if reason != "claude subprocess exited 1" {
		t.Errorf("reason = %q, want %q", reason, "claude subprocess exited 1")
	}
}

func TestParseDegradedNoteNoMatch(t *testing.T) {
	if _, _, ok := ParseDegradedNote("unrelated note"); ok {
		t.Error("ParseDegradedNote(unrelated) ok = true, want false")
	}
}
