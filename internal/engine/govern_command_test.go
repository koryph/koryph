// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

func TestDuplicateBroadCommandRejectsDuplicateJSONKeys(t *testing.T) {
	store := ledger.NewStore(t.TempDir())
	phaseDir := filepath.Join(store.KoryphRoot, "run-1", "b1")
	commandDir := filepath.Join(phaseDir, ".koryph-command")
	if err := os.MkdirAll(commandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	event := `{"schema":"koryph.command-event/v1","event":"complete","event":"start",` +
		`"at":"2026-07-25T12:00:01Z","class":"broad","signature":"abc"}`
	if err := os.WriteFile(
		filepath.Join(commandDir, "events.jsonl"), []byte(event+"\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	r := &runner{store: store}
	if duplicate, proven := r.duplicateBroadCommand("run-1", "b1"); duplicate || proven {
		t.Fatalf("duplicate-key lifecycle = duplicate %t, proven %t; want untrusted", duplicate, proven)
	}
}
