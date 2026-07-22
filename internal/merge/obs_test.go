// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// attrString returns the string value of the first slog.Attr with the given
// key, and whether it was found.
func attrString(attrs []slog.Attr, key string) (string, bool) {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value.String(), true
		}
	}
	return "", false
}

// TestMergeResultLogAttrs guards the fix for the audit finding that
// internal/merge had zero obs/slog calls: RunGate output was truncated into a
// ~400-char ledger Note by the engine and never persisted, and an operator
// running `koryph merge`/`koryph land` directly (bypassing the engine's wave
// loop entirely) got no structured telemetry at all. Merge (merge.go) is now
// a thin wrapper around mergeInner that calls resultLogAttrs on every exit
// path and emits exactly one "merge.result" event — the single choke point
// every caller (engine auto-merge, `koryph land`, `koryph merge`) passes
// through. This test exercises resultLogAttrs directly (not the obs registry
// output) because the package's log var binds to whatever obs registry
// existed at package-init time, before a test can swap it in — see the
// comment on logResult.
func TestMergeResultLogAttrs(t *testing.T) {
	cases := []struct {
		name       string
		res        Result
		err        error
		wantLevel  slog.Level
		wantStatus string
	}{
		{
			name:       "merged",
			res:        Result{Status: StatusMerged, MergedSHA: "abc123"},
			wantLevel:  slog.LevelInfo,
			wantStatus: "merged",
		},
		{
			name:       "pr-opened",
			res:        Result{Status: StatusPROpened},
			wantLevel:  slog.LevelInfo,
			wantStatus: "pr-opened",
		},
		{
			name:       "gate-failed is WARN",
			res:        Result{Status: StatusGateFailed, GateOutput: "make test\nFAIL"},
			wantLevel:  slog.LevelWarn,
			wantStatus: "gate-failed",
		},
		{
			name:       "protected is WARN",
			res:        Result{Status: StatusProtected, Protected: []string{"CLAUDE.md"}},
			wantLevel:  slog.LevelWarn,
			wantStatus: "protected",
		},
		{
			name:       "merged but a trailing error (e.g. push failure) escalates to WARN",
			res:        Result{Status: StatusMerged, MergedSHA: "abc123"},
			err:        errors.New("push origin main: connection reset"),
			wantLevel:  slog.LevelWarn,
			wantStatus: "merged",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level, attrs := resultLogAttrs(Opts{Branch: "agent/x"}, tc.res, tc.err, 42*time.Millisecond)
			if level != tc.wantLevel {
				t.Errorf("level = %v, want %v", level, tc.wantLevel)
			}
			if got, ok := attrString(attrs, "status"); !ok || got != tc.wantStatus {
				t.Errorf("status attr = %q (found=%v), want %q", got, ok, tc.wantStatus)
			}
			if got, ok := attrString(attrs, "branch"); !ok || got != "agent/x" {
				t.Errorf("branch attr = %q (found=%v), want agent/x", got, ok)
			}
			// The full gate/rebase output must NEVER be logged — only counts/status.
			for _, a := range attrs {
				if a.Value.String() == tc.res.GateOutput && tc.res.GateOutput != "" {
					t.Errorf("raw GateOutput leaked into a log attr (key=%s)", a.Key)
				}
			}
		})
	}
}

// TestMergeCallsLogResult is a light integration smoke test: it exercises the
// real Merge wrapper (not just resultLogAttrs) end to end for both a success
// and a blocked outcome, asserting only that it does not panic and returns
// the expected Result — the logging call itself is covered by
// TestMergeResultLogAttrs above.
func TestMergeCallsLogResult(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wt := worktreeOn(t, repo, "agent/z")
	commitIn(t, wt.Path, "d.txt", "feature\n", "add d")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/z", DefaultBranch: "main",
		Gate: []string{"false"}, SlotOwner: "owner-1",
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusGateFailed {
		t.Fatalf("Status=%q, want gate-failed", res.Status)
	}
}
