// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Rollback finds the target snapshot (latest, or by timestamp prefix), shows
// the diff between the snapshot and the current live state, then applies the
// snapshot through the standard apply machinery.
//
// root is the repository root (used to locate <root>/.koryph/snapshots/).
// to is either "" / "latest" (use the most recent snapshot for repo) or a
// RFC3339 timestamp prefix (e.g. "2026-07-04T16" to select all snapshots on
// that hour).
//
// When the selection is ambiguous (multiple timestamps match the prefix),
// Rollback lists the candidates and returns an error asking the caller to be
// more specific.
//
// Returns the path of the snapshot that was applied, or an error.
func Rollback(ctx context.Context, ghBin, repo, root, to string, stdout, stderr io.Writer) (string, error) {
	all, err := ListSnapshots(root)
	if err != nil {
		return "", err
	}

	// Filter by repo if we have multiple repos in the same snapshot dir.
	var candidates []SnapshotEntry
	for _, e := range all {
		if e.Repo == repo {
			candidates = append(candidates, e)
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("posture rollback: no snapshots found for repo %q under %s", repo, SnapshotsDir(root))
	}

	// Select the target snapshot.
	target, err := selectSnapshot(candidates, to)
	if err != nil {
		return "", err
	}

	snap, err := LoadSnapshot(target.Path)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(stdout, "snapshot  %s → %s\n", target.CapturedAt.Format(time.RFC3339), target.Path)

	// Build a temporary source from the snapshot.
	src, cleanup, err := newSnapshotSource(snap)
	if err != nil {
		return "", err
	}
	defer cleanup()

	// --- diff first --------------------------------------------------------
	fmt.Fprintln(stdout, "--- snapshot diff (snapshot → live) ---")
	drift := false

	if _, err2 := src.RulesetsDir(); err2 == nil {
		fmt.Fprintln(stdout, "--- rulesets ---")
		d, err2 := CheckRulesets(ctx, ghBin, repo, src, stdout)
		if err2 != nil {
			return "", fmt.Errorf("posture rollback: check rulesets: %w", err2)
		}
		if d {
			drift = true
		}
	}

	if _, err2 := src.RepoSettingsFile(); err2 == nil {
		fmt.Fprintln(stdout, "--- settings ---")
		d, err2 := CheckSettings(ctx, ghBin, repo, src, stdout)
		if err2 != nil {
			return "", fmt.Errorf("posture rollback: check settings: %w", err2)
		}
		if d {
			drift = true
		}
	}

	if !drift {
		fmt.Fprintln(stdout, "no drift — live state already matches the snapshot; nothing to do")
		return target.Path, nil
	}

	// --- apply -------------------------------------------------------------
	fmt.Fprintln(stdout, "--- applying snapshot ---")

	if _, err2 := src.RulesetsDir(); err2 == nil {
		if err2 := ApplyRulesets(ctx, ghBin, repo, src, stdout); err2 != nil {
			return "", fmt.Errorf("posture rollback: apply rulesets: %w", err2)
		}
	}
	if _, err2 := src.RepoSettingsFile(); err2 == nil {
		if err2 := ApplySettings(ctx, ghBin, repo, src, stdout); err2 != nil {
			return "", fmt.Errorf("posture rollback: apply settings: %w", err2)
		}
	}

	fmt.Fprintf(stdout, "rolled back to %s\n", target.Path)
	return target.Path, nil
}

// selectSnapshot picks one SnapshotEntry from candidates based on the to
// selector:
//
//   - "" or "latest": the first (newest) candidate.
//   - RFC3339 timestamp prefix: all candidates whose timestamp string starts
//     with the prefix. Exactly one match → selected. Multiple → error listing
//     them. Zero → error.
func selectSnapshot(candidates []SnapshotEntry, to string) (SnapshotEntry, error) {
	norm := strings.TrimSpace(strings.ToLower(to))
	if norm == "" || norm == "latest" {
		return candidates[0], nil
	}

	var matched []SnapshotEntry
	for _, c := range candidates {
		ts := c.CapturedAt.Format(time.RFC3339)
		if strings.HasPrefix(ts, to) {
			matched = append(matched, c)
		}
	}
	switch len(matched) {
	case 0:
		return SnapshotEntry{}, fmt.Errorf("posture rollback: no snapshot matches %q; available:\n%s", to, listCandidates(candidates))
	case 1:
		return matched[0], nil
	default:
		return SnapshotEntry{}, fmt.Errorf("posture rollback: ambiguous prefix %q — %d snapshots match; be more specific:\n%s", to, len(matched), listCandidates(matched))
	}
}

// listCandidates formats a bullet list of snapshot timestamps + paths.
func listCandidates(entries []SnapshotEntry) string {
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "  %s  %s\n", e.CapturedAt.Format(time.RFC3339), e.Path)
	}
	return strings.TrimRight(sb.String(), "\n")
}
