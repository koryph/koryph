// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"os"
	"strings"
	"testing"
)

// writeCorrupt drops unparseable bytes at s.cfgPath, simulating disk
// corruption / a torn write / a bad hand-edit.
func writeCorrupt(t *testing.T, s *Store, content string) {
	t.Helper()
	if err := os.MkdirAll(s.slotsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCorruptGovernorFailsOpenForReadsButBacksUp is the koryph audit finding
// #27 fix's read-path half: admission reads (Cap) must still never block
// dispatch on a corrupt governor.json, but the original bytes must survive
// instead of being silently discarded.
func TestCorruptGovernorFailsOpenForReadsButBacksUp(t *testing.T) {
	s := newTestStore(t)
	const junk = `{"pools": not valid json`
	writeCorrupt(t, s, junk)

	if got := s.Cap(""); got != DefaultMaxGlobalAgents {
		t.Errorf("Cap on corrupt file = %d, want package default %d (fail open)", got, DefaultMaxGlobalAgents)
	}

	backup, err := os.ReadFile(s.cfgPath + corruptBackupSuffix)
	if err != nil {
		t.Fatalf("expected a corrupt-backup file, read failed: %v", err)
	}
	if string(backup) != junk {
		t.Errorf("backup content = %q, want original corrupt bytes %q", backup, junk)
	}
}

// TestCorruptGovernorRefusesWrite is the koryph audit finding #27 fix's write
// path half: SetCap (and the other Set*/Unset* mutators, which share
// readFileForWrite) must refuse rather than silently treat a corrupt
// governor.json as empty and overwrite it — that used to be a machine-wide
// config wipe (every other pool's cap + the resource ledger gone) plus a
// silent cap relaxation back to the package default, with no error returned.
func TestCorruptGovernorRefusesWrite(t *testing.T) {
	s := newTestStore(t)
	const junk = `{totally not json`
	writeCorrupt(t, s, junk)

	if err := s.SetCap("", 7); err == nil {
		t.Fatal("SetCap on corrupt governor.json succeeded, want a refusal error")
	} else if !strings.Contains(err.Error(), "corrupt") && !strings.Contains(err.Error(), "failed to parse") {
		t.Errorf("SetCap error = %v, want it to mention the parse failure", err)
	}

	// The corrupt file itself must be untouched (not clobbered by a
	// wholesale rewrite before the caller ever sees the error).
	got, err := os.ReadFile(s.cfgPath)
	if err != nil {
		t.Fatalf("governor.json unreadable after refused write: %v", err)
	}
	if string(got) != junk {
		t.Errorf("governor.json content = %q after refused SetCap, want untouched original %q", got, junk)
	}

	// A forensic backup still exists.
	if _, err := os.Stat(s.cfgPath + corruptBackupSuffix); err != nil {
		t.Errorf("expected a corrupt-backup file: %v", err)
	}

	// Same refusal for the resource mutators, which share readFileForWrite.
	if err := s.SetResource("gpu", ResourceKind{Capacity: 1}); err == nil {
		t.Error("SetResource on corrupt governor.json succeeded, want a refusal error")
	}
	if err := s.SetMinFreeMemoryMB("", 512); err == nil {
		t.Error("SetMinFreeMemoryMB on corrupt governor.json succeeded, want a refusal error")
	}
	if err := s.UnsetResource("gpu"); err == nil {
		t.Error("UnsetResource on corrupt governor.json succeeded, want a refusal error")
	}
}

// TestAbsentGovernorStillFailsOpenForWrites proves the fix is scoped to
// PRESENT-but-corrupt files: a freshly initialized ~/.koryph with no
// governor.json yet must not be treated as "corrupt" — SetCap still succeeds
// and creates the file, exactly as before.
func TestAbsentGovernorStillFailsOpenForWrites(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("", 5); err != nil {
		t.Fatalf("SetCap on absent governor.json: %v", err)
	}
	if got := s.Cap(""); got != 5 {
		t.Errorf("Cap after SetCap = %d, want 5", got)
	}
	if _, err := os.Stat(s.cfgPath + corruptBackupSuffix); err == nil {
		t.Error("absent governor.json should never produce a corrupt-backup file")
	}
}
