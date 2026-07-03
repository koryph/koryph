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

// TestRunDirectOverridesPRPolicy verifies the owner --direct override: a
// project configured for merge_policy pr merges STRAIGHT to the default branch
// (no PR opened, no pr-opened slot) when the run passes --direct.
func TestRunDirectOverridesPRPolicy(t *testing.T) {
	f := newFixture(t, fixOpts{mergePolicy: "pr"})

	opts := baseOptions(nil)
	var out bytes.Buffer
	opts.Out = &out
	opts.Direct = true // owner override

	got, err := Run(context.Background(), opts)
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Merged != 1 || got.PROpened != 0 || got.Blocked != 0 {
		t.Errorf("Outcome = %+v, want 1 merged / 0 pr-opened (--direct skips the PR flow)", got)
	}
	if sl := slotStatus(t, f.repo, "tb1"); sl.Status != ledger.SlotMerged {
		t.Errorf("slot status = %q, want merged", sl.Status)
	}
	// The agent commit landed on main directly.
	if log := runGit(t, f.repo, "log", "--format=%s", "main"); !strings.Contains(log, "feat(tb1): work") {
		t.Errorf("main log missing agent commit (direct merge did not land):\n%s", log)
	}
}
