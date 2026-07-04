// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/posture"
)

// --- EnsureGitignored tests ---------------------------------------------------

func TestEnsureGitignored_CreatesFile(t *testing.T) {
	root := t.TempDir()
	if err := posture.EnsureGitignored(root); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("expected .gitignore to be created: %v", err)
	}
	if !strings.Contains(string(data), ".koryph/snapshots/") {
		t.Errorf("expected .gitignore to contain '.koryph/snapshots/', got: %q", string(data))
	}
}

func TestEnsureGitignored_Idempotent(t *testing.T) {
	root := t.TempDir()
	if err := posture.EnsureGitignored(root); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := posture.EnsureGitignored(root); err != nil {
		t.Fatalf("second call: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	// Entry must appear exactly once.
	count := strings.Count(string(data), ".koryph/snapshots/")
	if count != 1 {
		t.Errorf("expected .koryph/snapshots/ to appear once, got %d times in: %q", count, string(data))
	}
}

func TestEnsureGitignored_ExistingFile_NoEntry(t *testing.T) {
	root := t.TempDir()
	existing := "node_modules/\n.env\n"
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := posture.EnsureGitignored(root); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), ".koryph/snapshots/") {
		t.Errorf("expected .koryph/snapshots/ to be appended, got: %q", string(data))
	}
	// Original content must be preserved.
	if !strings.Contains(string(data), "node_modules/") {
		t.Errorf("existing .gitignore content was lost: %q", string(data))
	}
}

func TestEnsureGitignored_ExistingFile_EntryPresent(t *testing.T) {
	root := t.TempDir()
	existing := "node_modules/\n.koryph/snapshots/\n"
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := posture.EnsureGitignored(root); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	// Must not add a duplicate.
	count := strings.Count(string(data), ".koryph/snapshots/")
	if count != 1 {
		t.Errorf("expected 1 occurrence, got %d in: %q", count, string(data))
	}
}

// --- CaptureSnapshot tests ----------------------------------------------------

// buildFakeGHForSnapshot returns a fake gh script that responds to the
// API calls made by CaptureSnapshot.
func buildFakeGHForSnapshot(t *testing.T) string {
	t.Helper()
	repoJSON := `{
		"allow_merge_commit": true,
		"allow_squash_merge": false,
		"allow_rebase_merge": false,
		"allow_auto_merge": false,
		"delete_branch_on_merge": false,
		"allow_update_branch": false,
		"web_commit_signoff_required": false,
		"description": "test repo",
		"homepage": "",
		"security_and_analysis": {
			"secret_scanning": {"status": "enabled"},
			"secret_scanning_push_protection": {"status": "disabled"},
			"dependabot_security_updates": {"status": "enabled"}
		}
	}`
	actionsJSON := `{"can_approve_pull_request_reviews": false, "default_workflow_permissions": "read"}`
	rulesetListJSON := `[{"id": 42, "name": "protect-main"}]`
	rulesetJSON := `{"name": "protect-main", "enforcement": "active", "target": "branch", "bypass_actors": []}`

	script := `#!/bin/sh
case "$*" in
  "api repos/owner/repo")
    echo '` + repoJSON + `'
    exit 0
    ;;
  "api repos/owner/repo/vulnerability-alerts")
    exit 0  # 204-like: vuln alerts enabled
    ;;
  "api repos/owner/repo/actions/permissions/workflow")
    echo '` + actionsJSON + `'
    exit 0
    ;;
  "api repos/owner/repo/rulesets")
    echo '` + rulesetListJSON + `'
    exit 0
    ;;
  "api repos/owner/repo/rulesets/42")
    echo '` + rulesetJSON + `'
    exit 0
    ;;
esac
echo "unexpected: $*" >&2
exit 1
`
	return fakeGH(t, script)
}

func TestCaptureSnapshot_WritesFile(t *testing.T) {
	root := t.TempDir()
	ghBin := buildFakeGHForSnapshot(t)

	path, err := posture.CaptureSnapshot(t.Context(), ghBin, "owner/repo", root, "iac")
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file not found at %s: %v", path, err)
	}

	// Parse the snapshot and verify fields.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap posture.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}

	if snap.Repo != "owner/repo" {
		t.Errorf("Repo = %q, want %q", snap.Repo, "owner/repo")
	}
	if snap.IAC != "iac" {
		t.Errorf("IAC = %q, want %q", snap.IAC, "iac")
	}
	if snap.AppliedProfile != "" {
		t.Errorf("AppliedProfile = %q, want empty", snap.AppliedProfile)
	}
	if snap.CapturedAt == "" {
		t.Error("CapturedAt is empty")
	}
	if snap.Sections.RepoFlags == nil {
		t.Error("Sections.RepoFlags is nil")
	}
	if snap.Sections.SecurityAndAnalysis == nil {
		t.Error("Sections.SecurityAndAnalysis is nil")
	}
	if snap.Sections.VulnerabilityAlerts == nil {
		t.Error("Sections.VulnerabilityAlerts is nil")
	}
	if !*snap.Sections.VulnerabilityAlerts {
		t.Error("VulnerabilityAlerts should be true (204 exit code from fake gh)")
	}
	if snap.Sections.Rulesets["protect-main"] == nil {
		t.Error("Sections.Rulesets['protect-main'] is nil")
	}
}

func TestCaptureSnapshot_Profile_Kind(t *testing.T) {
	root := t.TempDir()
	ghBin := buildFakeGHForSnapshot(t)

	path, err := posture.CaptureSnapshot(t.Context(), ghBin, "owner/repo", root, "profile:oss-solo-maintainer")
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap posture.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}

	if snap.AppliedProfile != "oss-solo-maintainer" {
		t.Errorf("AppliedProfile = %q, want %q", snap.AppliedProfile, "oss-solo-maintainer")
	}
	if snap.IAC != "" {
		t.Errorf("IAC = %q, want empty", snap.IAC)
	}
}

func TestCaptureSnapshot_EnsuresGitignore(t *testing.T) {
	root := t.TempDir()
	ghBin := buildFakeGHForSnapshot(t)

	if _, err := posture.CaptureSnapshot(t.Context(), ghBin, "owner/repo", root, "iac"); err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("expected .gitignore to exist: %v", err)
	}
	if !strings.Contains(string(data), ".koryph/snapshots/") {
		t.Errorf(".gitignore does not contain .koryph/snapshots/: %q", string(data))
	}
}

func TestCaptureSnapshot_FilenameFormat(t *testing.T) {
	root := t.TempDir()
	ghBin := buildFakeGHForSnapshot(t)

	path, err := posture.CaptureSnapshot(t.Context(), ghBin, "owner/repo", root, "iac")
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	base := filepath.Base(path)
	if !strings.HasPrefix(base, "settings-") || !strings.HasSuffix(base, ".json") {
		t.Errorf("unexpected filename format: %s", base)
	}
	// Colons in timestamp must have been replaced with hyphens (filesystem safety).
	if strings.Contains(base, ":") {
		t.Errorf("filename contains colon: %s", base)
	}
}

// --- ListSnapshots tests ------------------------------------------------------

func TestListSnapshots_Empty(t *testing.T) {
	root := t.TempDir()
	entries, err := posture.ListSnapshots(root)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestListSnapshots_SortedNewestFirst(t *testing.T) {
	root := t.TempDir()
	dir := posture.SnapshotsDir(root)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	older := posture.Snapshot{
		CapturedAt: "2026-07-04T10:00:00Z",
		Repo:       "owner/repo",
		IAC:        "iac",
	}
	newer := posture.Snapshot{
		CapturedAt: "2026-07-04T12:00:00Z",
		Repo:       "owner/repo",
		IAC:        "iac",
	}

	writeSnap := func(snap posture.Snapshot, name string) {
		t.Helper()
		data, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
			t.Fatal(err)
		}
	}

	writeSnap(older, "settings-2026-07-04T10-00-00Z.json")
	writeSnap(newer, "settings-2026-07-04T12-00-00Z.json")

	entries, err := posture.ListSnapshots(root)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// Newest first.
	if !entries[0].CapturedAt.After(entries[1].CapturedAt) {
		t.Errorf("expected newest-first order; got %v before %v", entries[0].CapturedAt, entries[1].CapturedAt)
	}
}

// --- LoadSnapshot tests -------------------------------------------------------

func TestLoadSnapshot_RoundTrip(t *testing.T) {
	root := t.TempDir()
	dir := posture.SnapshotsDir(root)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	trueBool := true
	orig := posture.Snapshot{
		CapturedAt: "2026-07-04T16:00:00Z",
		Repo:       "owner/repo",
		IAC:        "iac",
		Sections: posture.SnapshotSections{
			RepoFlags:           json.RawMessage(`{"description":"hello"}`),
			VulnerabilityAlerts: &trueBool,
		},
	}

	data, err := json.MarshalIndent(orig, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings-2026-07-04T16-00-00Z.json")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}

	snap, err := posture.LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.Repo != orig.Repo {
		t.Errorf("Repo = %q, want %q", snap.Repo, orig.Repo)
	}
	if snap.IAC != orig.IAC {
		t.Errorf("IAC = %q, want %q", snap.IAC, orig.IAC)
	}
	if snap.Sections.VulnerabilityAlerts == nil || !*snap.Sections.VulnerabilityAlerts {
		t.Error("VulnerabilityAlerts not preserved")
	}
}

// --- SnapshotsDir helper ------------------------------------------------------

func TestSnapshotsDir(t *testing.T) {
	dir := posture.SnapshotsDir("/some/repo")
	want := "/some/repo/.koryph/snapshots"
	if dir != want {
		t.Errorf("SnapshotsDir = %q, want %q", dir, want)
	}
}

// --- selectSnapshot (via Rollback error paths) --------------------------------

func TestListSnapshots_MissingDirReturnsNil(t *testing.T) {
	root := t.TempDir()
	// No .koryph/snapshots dir created.
	entries, err := posture.ListSnapshots(root)
	if err != nil {
		t.Errorf("expected nil error for missing dir, got: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries, got %v", entries)
	}
}

func TestCaptureSnapshot_TimestampParseable(t *testing.T) {
	root := t.TempDir()
	ghBin := buildFakeGHForSnapshot(t)

	path, err := posture.CaptureSnapshot(t.Context(), ghBin, "owner/repo", root, "iac")
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}

	snap, err := posture.LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	ts, err := time.Parse(time.RFC3339, snap.CapturedAt)
	if err != nil {
		t.Fatalf("CapturedAt %q not RFC3339: %v", snap.CapturedAt, err)
	}
	// Must be within the last minute.
	if time.Since(ts) > time.Minute {
		t.Errorf("timestamp too old: %v", ts)
	}
}

// --- Rollback error-path tests -----------------------------------------------

func TestRollback_NoSnapshots(t *testing.T) {
	root := t.TempDir()
	// Fake gh never gets called — should fail before that.
	ghBin := fakeGH(t, `#!/bin/sh
echo "unexpected" >&2; exit 1
`)
	// Need a repo slug to filter by. We expect an error before any gh calls.
	var out, errOut strings.Builder
	_, err := posture.Rollback(t.Context(), ghBin, "owner/repo", root, "latest", &out, &errOut)
	if err == nil {
		t.Fatal("expected error for no snapshots, got nil")
	}
	if !strings.Contains(err.Error(), "no snapshots found") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRollback_AmbiguousPrefix(t *testing.T) {
	root := t.TempDir()
	dir := posture.SnapshotsDir(root)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	snap := posture.Snapshot{
		CapturedAt: "2026-07-04T10:00:00Z",
		Repo:       "owner/repo",
		IAC:        "iac",
	}
	for _, ts := range []string{"2026-07-04T10-00-00Z", "2026-07-04T10-30-00Z"} {
		data, _ := json.MarshalIndent(posture.Snapshot{
			CapturedAt: strings.ReplaceAll(ts, "-", ":") + "0", // not valid but ListSnapshots will skip unparseable
			Repo:       snap.Repo,
			IAC:        snap.IAC,
		}, "", "  ")
		_ = data
	}

	// Write two snapshots with the same hour prefix by using valid RFC3339 timestamps.
	for _, ts := range []string{"2026-07-04T10:00:00Z", "2026-07-04T10:30:00Z"} {
		s := posture.Snapshot{CapturedAt: ts, Repo: "owner/repo", IAC: "iac"}
		data, _ := json.MarshalIndent(s, "", "  ")
		safe := strings.ReplaceAll(ts, ":", "-")
		if err := os.WriteFile(filepath.Join(dir, "settings-"+safe+".json"), data, 0o640); err != nil {
			t.Fatal(err)
		}
	}

	ghBin := fakeGH(t, `#!/bin/sh
echo "unexpected" >&2; exit 1
`)
	var out, errOut strings.Builder
	_, err := posture.Rollback(t.Context(), ghBin, "owner/repo", root, "2026-07-04T10", &out, &errOut)
	if err == nil {
		t.Fatal("expected error for ambiguous prefix, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected 'ambiguous' in error, got: %v", err)
	}
}
