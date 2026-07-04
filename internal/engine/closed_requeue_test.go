// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// closedBeadBDScript is a fake bd script whose `show` always returns the bead
// as closed, simulating the operator retiring the bead while the agent ran.
const closedBeadBDScript = `#!/bin/sh
dir="$FAKE_BD_DIR"
printf '%s\n' "$*" >> "$dir/bd.log"
case "$1" in
  ready)
    if [ -f "$dir/ready_served" ]; then
      echo '[]'
    else
      touch "$dir/ready_served"
      cat "$dir/ready.json"
    fi
    ;;
  version) echo "bd version 1.0.5" ;;
  update|close|comment) exit 0 ;;
  show)
    # Operator closed the bead mid-flight.
    printf '{"id":"tb1","title":"Test bead one","status":"closed","issue_type":"task","labels":[]}\n'
    ;;
  *) exit 1 ;;
esac
`

// TestClosedBeadMidFlightReleasesSlotWithoutRedispatch proves the koryph-pln
// fix: when an agent dies with no commits AND the bead has been closed by the
// operator, the engine must NOT redispatch — it releases the slot cleanly with
// no attempt burned beyond the first dispatch, and the note names the cause.
func TestClosedBeadMidFlightReleasesSlotWithoutRedispatch(t *testing.T) {
	f := newFixture(t, fixOpts{bdScript: closedBeadBDScript})

	// Replace the fake claude with the ordinary-failure script (dies, no
	// commits, no rate-limit marker) so completeSlot hits the requeue path.
	claudeBin := os.Getenv("KORYPH_CLAUDE_BIN")
	writeFile(t, claudeBin, ordinaryFailureClaudeScript, 0o755)

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The engine must not merge anything: the bead was closed, not implemented.
	if got.Merged != 0 {
		t.Errorf("Merged = %d, want 0 (closed bead must not be merged)", got.Merged)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatalf("no slot tb1 in run: %+v", run.Slots)
	}

	if sl.Status != ledger.SlotBlocked {
		t.Errorf("slot status = %q, want blocked", sl.Status)
	}
	if !strings.Contains(sl.Note, "bead closed while in flight") {
		t.Errorf("slot note = %q, want it to name the closed-mid-flight cause", sl.Note)
	}

	// No extra attempt must have been burned: the closed-bead guard fires
	// before the attempt counter is incremented.
	if sl.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (closed-bead guard must not burn an attempt)", sl.Attempts)
	}

	// Sanity: bd log must not contain a second 'ready' call — the engine should
	// not have gone back to the frontier to try a re-dispatch.
	bdLog := f.bdLog(t)
	readyCount := strings.Count(bdLog, "ready")
	if readyCount > 1 {
		t.Errorf("bd log contains %d 'ready' calls, want ≤1 (no re-dispatch after closed-bead guard)", readyCount)
	}
}
