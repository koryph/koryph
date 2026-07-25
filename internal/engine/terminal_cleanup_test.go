// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

func TestReleaseGlobalSlotImmediatelyPrunesDurableTerminalScratch(t *testing.T) {
	repo := t.TempDir()
	store := ledger.NewStore(repo)
	run, err := store.NewRun("test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	slot := &ledger.Slot{PhaseID: "bead", BeadID: "bead", Status: ledger.SlotMerged}
	if err := store.SetSlot(run, slot); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(store.PhaseDir(run.RunID, slot.PhaseID), "go-cache", "entry")
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("scratch"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &runner{
		rec:   &registry.Record{Root: repo},
		store: store,
		run:   run,
	}
	r.releaseGlobalSlot(slot.PhaseID)
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("terminal cache survived durable slot release: %v", err)
	}
}
