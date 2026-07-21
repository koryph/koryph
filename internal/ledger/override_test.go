// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOverridesRecordAndLoad(t *testing.T) {
	root := t.TempDir()
	s := &Store{KoryphRoot: root}
	runID := "20260101-000000"
	if err := os.MkdirAll(filepath.Join(root, runID), 0o755); err != nil {
		t.Fatal(err)
	}

	// Absent sidecar → empty, no error.
	if of, err := s.LoadOverrides(runID); err != nil || len(of.Overrides) != 0 {
		t.Fatalf("empty sidecar: got %+v err=%v", of, err)
	}

	if err := s.RecordOverride(runID, SlotOverride{BeadID: "b1", Status: SlotMerged, Note: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOverride(runID, SlotOverride{BeadID: "b2", Status: SlotBlocked}); err != nil {
		t.Fatal(err)
	}
	// Re-recording b1 replaces it (newest directive per bead wins), not appends.
	if err := s.RecordOverride(runID, SlotOverride{BeadID: "b1", Status: SlotMerged, Note: "second"}); err != nil {
		t.Fatal(err)
	}

	of, err := s.LoadOverrides(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(of.Overrides) != 2 {
		t.Fatalf("overrides = %d, want 2 (b1 collapsed)", len(of.Overrides))
	}
	byID := map[string]SlotOverride{}
	for _, o := range of.Overrides {
		byID[o.BeadID] = o
	}
	if byID["b1"].Note != "second" {
		t.Errorf("b1 note = %q, want the newest 'second'", byID["b1"].Note)
	}
	if byID["b1"].At == "" {
		t.Error("b1 override not timestamped")
	}
	if byID["b2"].Status != SlotBlocked {
		t.Errorf("b2 status = %q, want blocked", byID["b2"].Status)
	}
}
