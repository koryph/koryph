// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// runtimeLabelBD serves a single ready bead labeled runtime:codex — a runtime
// dispatch never actually drives today (koryph-v8u.3).
const runtimeLabelBD = `#!/bin/sh
dir="$FAKE_BD_DIR"
printf '%s\n' "$*" >> "$dir/bd.log"
case "$1" in
  ready)
    if [ -f "$dir/ready_served" ]; then
      echo '[]'
    else
      touch "$dir/ready_served"
      echo '[{"id":"tb1","title":"Test bead one","description":"do the work","status":"open","priority":1,"issue_type":"task","labels":["fp:core","runtime:codex"]}]'
    fi
    ;;
  version) echo "bd version 1.0.5" ;;
  update|close|comment) exit 0 ;;
  show) exit 1 ;;
  *) exit 1 ;;
esac
`

// TestDispatchBlocksUnavailableRuntime proves a bead labeled runtime:<name>
// naming anything other than claude is BLOCKED, never silently dispatched
// under claude (koryph-v8u.3): dispatch today only ever drives the claude
// CLI, so any other selection — registered in runtime.Default or not — must
// fail closed with a clear note.
func TestDispatchBlocksUnavailableRuntime(t *testing.T) {
	f := newFixture(t, fixOpts{bdScript: runtimeLabelBD})
	var out bytes.Buffer

	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 0 || got.Blocked != 1 {
		t.Errorf("Outcome = %+v, want 0 dispatched / 1 blocked", got)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil || sl.Status != ledger.SlotBlocked {
		t.Fatalf("slot = %+v, want blocked", sl)
	}
	if !strings.Contains(sl.Note, "runtime codex not available") {
		t.Errorf("slot note = %q, want it to name the unavailable runtime", sl.Note)
	}
	// Never dispatched: no worktree/backend work, no bd claim.
	if log := f.bdLog(t); strings.Contains(log, "--claim") {
		t.Errorf("bead was claimed despite an unavailable runtime:\n%s", log)
	}
}

// TestDispatchRecordsClaudeRuntimeByDefault proves the compatibility contract
// (koryph-v8u.3): an unlabeled bead, on a project with no default_runtime,
// still dispatches under claude exactly as before, AND the new Slot/Manifest
// Runtime field records "claude" (not left empty) so the additive field is
// actually exercised end-to-end.
func TestDispatchRecordsClaudeRuntimeByDefault(t *testing.T) {
	f := newFixture(t, fixOpts{})
	var out bytes.Buffer

	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 1 || got.Merged != 1 {
		t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged", got)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatal("no slot for tb1")
	}
	if sl.Runtime != "claude" {
		t.Errorf("slot.Runtime = %q, want claude", sl.Runtime)
	}

	m, err := store.LoadManifest(run.RunID, "tb1")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Runtime != "claude" {
		t.Errorf("manifest.Runtime = %q, want claude", m.Runtime)
	}
}
