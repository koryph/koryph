// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneOldRemovesStale(t *testing.T) {
	dir := t.TempDir()

	// Write two "old" files and one "fresh" file.
	old1 := filepath.Join(dir, "koryph-20260101.jsonl")
	old2 := filepath.Join(dir, "koryph-20260102.jsonl")
	fresh := filepath.Join(dir, "koryph-20260704.jsonl")
	for _, p := range []string{old1, old2, fresh} {
		if err := os.WriteFile(p, []byte(`{}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Back-date the old files to 60 days ago.
	pastTime := time.Now().Add(-60 * 24 * time.Hour)
	for _, p := range []string{old1, old2} {
		if err := os.Chtimes(p, pastTime, pastTime); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	pruned, err := PruneOld(dir, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneOld: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned = %d, want 2", pruned)
	}

	// The fresh file must still be there.
	if _, serr := os.Stat(fresh); serr != nil {
		t.Errorf("fresh file %s unexpectedly removed", fresh)
	}
	// The old files must be gone.
	for _, p := range []string{old1, old2} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("expected %s to be removed, but stat returned: %v", p, serr)
		}
	}
}

func TestPruneOldMissingDir(t *testing.T) {
	// A missing directory is not an error.
	pruned, err := PruneOld("/nonexistent-koryph-test-dir/telemetry", 24*time.Hour)
	if err != nil {
		t.Errorf("expected no error for missing dir, got: %v", err)
	}
	if pruned != 0 {
		t.Errorf("expected 0 pruned, got %d", pruned)
	}
}

func TestPruneOldKeepsRecentFiles(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "koryph-today.jsonl")
	if err := os.WriteFile(fresh, []byte(`{}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	pruned, err := PruneOld(dir, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneOld: %v", err)
	}
	if pruned != 0 {
		t.Errorf("pruned = %d, want 0 (fresh file should be kept)", pruned)
	}
}
