// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// badSubjectClaudeScript is a well-behaved implementer that nonetheless writes
// a NON-conventional commit subject, to exercise the commit-style gate.
const badSubjectClaudeScript = `#!/bin/sh
cat > /dev/null
echo "work" >> agent-work.txt
git add agent-work.txt
git commit -q --no-verify -m "did some work"
printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
printf '{"type":"result","total_cost_usd":0.10}\n'
exit 0
`

// TestRunCommitStyleBouncesThenBlocks verifies conventional-commit enforcement
// is ON by default (commit_style empty): a non-conforming subject is bounced
// back to the implementer once (like a gate failure) and, when it persists,
// blocks the bead with a commit-style note — never merging it to main.
func TestRunCommitStyleBouncesThenBlocks(t *testing.T) {
	f := newFixture(t, fixOpts{}) // default commit_style ("") enforces
	// Swap in an implementer that commits a non-conventional subject.
	badBin := filepath.Join(t.TempDir(), "bad-claude")
	writeFile(t, badBin, badSubjectClaudeScript, 0o755)
	t.Setenv("KORYPH_CLAUDE_BIN", badBin)

	mainBefore := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main"))

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Merged != 0 || got.Blocked != 1 {
		t.Errorf("Outcome = %+v, want 0 merged / 1 blocked", got)
	}

	sl := slotStatus(t, f.repo, "tb1")
	if sl.Status != ledger.SlotBlocked {
		t.Fatalf("slot status = %q, want blocked (note=%q)", sl.Status, sl.Note)
	}
	if !strings.Contains(sl.Note, "commit-style") {
		t.Errorf("slot note = %q, want it to mention commit-style", sl.Note)
	}
	if sl.Attempts < 2 {
		t.Errorf("attempts = %d, want >= 2 (bounced back to the implementer once)", sl.Attempts)
	}
	// Nothing non-conventional reached main.
	if after := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main")); after != mainBefore {
		t.Errorf("main advanced to %s (was %s); a commit-style failure must not merge", after, mainBefore)
	}
}

// TestRunCommitStyleOptOutMerges verifies commit_style none disables the gate:
// the same non-conventional subject merges cleanly.
func TestRunCommitStyleOptOutMerges(t *testing.T) {
	f := newFixture(t, fixOpts{commitStyle: "none"})
	badBin := filepath.Join(t.TempDir(), "bad-claude")
	writeFile(t, badBin, badSubjectClaudeScript, 0o755)
	t.Setenv("KORYPH_CLAUDE_BIN", badBin)

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Merged != 1 || got.Blocked != 0 {
		t.Errorf("Outcome = %+v, want 1 merged / 0 blocked (commit_style none opts out)", got)
	}
	if log := runGit(t, f.repo, "log", "--format=%s", "main"); !strings.Contains(log, "did some work") {
		t.Errorf("non-conventional commit did not land on main under opt-out:\n%s", log)
	}
}
