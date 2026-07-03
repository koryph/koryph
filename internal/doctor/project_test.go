// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/signing"
)

// fabricateProject creates a minimal valid project skeleton under t.TempDir().
func fabricateProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// .git directory (minimal git repo marker)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// koryph.project.json
	cfg := project.Config{
		SchemaVersion:   1,
		ProjectID:       "testproject",
		WorkSource:      "bd",
		Gate:            []string{"make test"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(root, project.ConfigFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// .claude/settings.json with all koryph hook markers
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{
  "hooks": {
    "SessionStart": [{"hooks":[{"type":"command","command":"bd prime --hook-json"}]}],
    "PreToolUse": [
      {"matcher":"Bash","hooks":[{"type":"command","command":"\"${CLAUDE_PROJECT_DIR}/hooks/agent-boundary-guard.sh\""}]},
      {"matcher":"Bash|Edit|Write","hooks":[{"type":"command","command":"\"${CLAUDE_PROJECT_DIR}/hooks/worktree-guard.sh\""}]}
    ]
  },
  "permissions": {"allow": ["Bash(*)", "Read(**)"]}
}`
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// projectOpts builds injectable ProjectOptions for a test repo root.
func projectOpts(root string) ProjectOptions {
	return ProjectOptions{
		RepoRoot:       root,
		Now:            func() time.Time { return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC) },
		Alive:          func(pid int) bool { return false },
		StallThreshold: 30 * time.Minute,
		ListWorktrees:  func(_ string) ([]worktreeEntry, error) { return nil, nil },
	}
}

// writeLedgerRun writes a ledger.json for the given run to repoRoot's koryph dir.
func writeLedgerRun(t *testing.T, repoRoot string, run *ledger.Run) {
	t.Helper()
	koryphRoot := paths.KoryphRoot(repoRoot)
	runDir := filepath.Join(koryphRoot, run.RunID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(run)
	if err := os.WriteFile(filepath.Join(runDir, "ledger.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Also write the latest symlink so LoadLatest works.
	link := filepath.Join(koryphRoot, "latest")
	_ = os.Remove(link)
	if err := os.Symlink(run.RunID, link); err != nil {
		t.Fatal(err)
	}
}

// --- project-config ---

func TestProjectConfigOK(t *testing.T) {
	root := fabricateProject(t)
	r, err := RunProject(projectOpts(root))
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameProjectConfig)
	if f.Level != LevelOK {
		t.Errorf("project-config level=%s msg=%s", f.Level, f.Message)
	}
}

func TestProjectConfigMissing(t *testing.T) {
	root := t.TempDir()
	// .git but no koryph.project.json
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	o := projectOpts(root)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameProjectConfig)
	if f.Level != LevelError {
		t.Errorf("project-config level=%s, want error for missing config", f.Level)
	}
}

// --- git-repo ---

func TestGitRepoOK(t *testing.T) {
	root := fabricateProject(t)
	r, _ := RunProject(projectOpts(root))
	f := findCheck(r, checkNameGitRepo)
	if f.Level != LevelOK {
		t.Errorf("git-repo level=%s msg=%s", f.Level, f.Message)
	}
}

func TestGitRepoMissing(t *testing.T) {
	root := t.TempDir()
	// Write config without .git
	cfg := project.Config{SchemaVersion: 1, ProjectID: "x", WorkSource: "bd",
		Gate: []string{"true"}, MergePolicy: "manual", RiskTierDefault: 2}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(root, project.ConfigFileName), data, 0o644)

	r, _ := RunProject(projectOpts(root))
	f := findCheck(r, checkNameGitRepo)
	if f.Level != LevelError {
		t.Errorf("git-repo level=%s, want error for missing .git", f.Level)
	}
}

// --- hooks-wiring ---

func TestHooksWiringAllPresent(t *testing.T) {
	root := fabricateProject(t)
	r, _ := RunProject(projectOpts(root))
	var warns int
	for _, f := range r.Findings {
		if f.Check == checkNameHooksWiring && f.Level == LevelWarn {
			warns++
		}
	}
	if warns != 0 {
		t.Errorf("hooks-wiring: got %d warnings, want 0", warns)
	}
}

func TestHooksWiringMissingBDPrime(t *testing.T) {
	root := fabricateProject(t)
	// Overwrite settings without "bd prime"
	settings := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"agent-boundary-guard.sh"}]},{"matcher":"Bash|Edit|Write","hooks":[{"type":"command","command":"worktree-guard.sh"}]}]}}`
	_ = os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte(settings), 0o644)

	r, _ := RunProject(projectOpts(root))
	var warnMsgs []string
	for _, f := range r.Findings {
		if f.Check == checkNameHooksWiring && f.Level == LevelWarn {
			warnMsgs = append(warnMsgs, f.Message)
		}
	}
	if len(warnMsgs) != 1 {
		t.Errorf("hooks-wiring: got %d warnings, want 1 (bd-prime missing); msgs: %v", len(warnMsgs), warnMsgs)
	}
}

func TestHooksWiringSettingsAbsent(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	cfg := project.Config{SchemaVersion: 1, ProjectID: "x", WorkSource: "bd",
		Gate: []string{"true"}, MergePolicy: "manual", RiskTierDefault: 2}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(root, project.ConfigFileName), data, 0o644)
	// No .claude/settings.json

	r, _ := RunProject(projectOpts(root))
	f := findCheck(r, checkNameHooksWiring)
	if f.Level != LevelWarn {
		t.Errorf("hooks-wiring level=%s, want warn for absent settings.json", f.Level)
	}
}

// --- signing ---

func TestSigningNotConfigured(t *testing.T) {
	root := fabricateProject(t)
	r, _ := RunProject(projectOpts(root))
	f := findCheck(r, checkNameSigning)
	if f.Level != LevelOK {
		t.Errorf("signing level=%s, want ok when nil", f.Level)
	}
}

func TestSigningIncompleteSetupWarns(t *testing.T) {
	root := fabricateProject(t)
	// A project that has signing configured (provider set) but hasn't run
	// `koryph signing setup` yet (no public_key). This is valid per
	// signing.Config.Validate() when Required=false, but the doctor should warn
	// because commits won't be signed/verified at dispatch.
	cfg := project.Config{
		SchemaVersion:   1,
		ProjectID:       "testproject",
		WorkSource:      "bd",
		Gate:            []string{"make test"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
		Signing: &signing.Config{
			Required: false,
			Mode:     "ssh",
			Provider: "protonpass",
			Identity: "test@example.com",
			// PublicKey intentionally absent — setup not completed
		},
	}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(root, project.ConfigFileName), data, 0o644)

	r, _ := RunProject(projectOpts(root))
	f := findCheck(r, checkNameSigning)
	if f.Level != LevelWarn {
		t.Errorf("signing level=%s, want warn for missing public_key (setup incomplete)", f.Level)
	}
}

func TestSigningConfiguredOK(t *testing.T) {
	root := fabricateProject(t)
	cfg := project.Config{
		SchemaVersion:   1,
		ProjectID:       "testproject",
		WorkSource:      "bd",
		Gate:            []string{"make test"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
		Signing: &signing.Config{
			Required:  true,
			Mode:      "ssh",
			Provider:  "file",
			KeyRef:    "/path/to/key",
			Identity:  "test@example.com",
			PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest",
		},
	}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(root, project.ConfigFileName), data, 0o644)

	r, _ := RunProject(projectOpts(root))
	f := findCheck(r, checkNameSigning)
	if f.Level != LevelOK {
		t.Errorf("signing level=%s msg=%s, want ok for valid signing config", f.Level, f.Message)
	}
}

// --- protected-paths ---

func TestProtectedPathsEmpty(t *testing.T) {
	root := fabricateProject(t)
	r, _ := RunProject(projectOpts(root))
	f := findCheck(r, checkNameProtectedPaths)
	if f.Level != LevelOK {
		t.Errorf("protected-paths level=%s, want ok when empty", f.Level)
	}
}

func TestProtectedPathsDuplicate(t *testing.T) {
	root := fabricateProject(t)
	cfg := project.Config{
		SchemaVersion:   1,
		ProjectID:       "testproject",
		WorkSource:      "bd",
		Gate:            []string{"make test"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
		ProtectedPaths:  []string{"docs/", "docs/"}, // duplicate
	}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(root, project.ConfigFileName), data, 0o644)

	r, _ := RunProject(projectOpts(root))
	f := findCheck(r, checkNameProtectedPaths)
	if f.Level != LevelWarn {
		t.Errorf("protected-paths level=%s, want warn for duplicate", f.Level)
	}
}

func TestProtectedPathsEmptyEntry(t *testing.T) {
	root := fabricateProject(t)
	cfg := project.Config{
		SchemaVersion:   1,
		ProjectID:       "testproject",
		WorkSource:      "bd",
		Gate:            []string{"make test"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
		ProtectedPaths:  []string{"docs/", ""},
	}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(root, project.ConfigFileName), data, 0o644)

	r, _ := RunProject(projectOpts(root))
	f := findCheck(r, checkNameProtectedPaths)
	if f.Level != LevelError {
		t.Errorf("protected-paths level=%s, want error for empty entry", f.Level)
	}
}

// --- stalled-runs ---

func TestStalledRunDetected(t *testing.T) {
	root := fabricateProject(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	staleTime := now.Add(-2 * time.Hour) // 2 h before now, well past 30 min threshold

	writeLedgerRun(t, root, &ledger.Run{
		RunID:     "20260702-100000",
		ProjectID: "testproject",
		Status:    ledger.RunRunning,
		StartedAt: staleTime.Format(time.RFC3339),
		UpdatedAt: staleTime.Format(time.RFC3339),
		Slots: map[string]*ledger.Slot{
			"bead-stale": {
				PhaseID:   "bead-stale",
				Status:    ledger.SlotRunning,
				UpdatedAt: staleTime.Format(time.RFC3339),
			},
		},
	})

	o := projectOpts(root)
	o.Now = func() time.Time { return now }
	o.StallThreshold = 30 * time.Minute

	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameStalledRuns)
	if f.Level != LevelWarn {
		t.Errorf("stalled-runs level=%s, want warn for stale slot", f.Level)
	}
}

func TestStalledRunFreshNotFlagged(t *testing.T) {
	root := fabricateProject(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	freshTime := now.Add(-5 * time.Minute) // only 5 min old — within threshold

	writeLedgerRun(t, root, &ledger.Run{
		RunID:     "20260702-115500",
		ProjectID: "testproject",
		Status:    ledger.RunRunning,
		StartedAt: freshTime.Format(time.RFC3339),
		UpdatedAt: freshTime.Format(time.RFC3339),
		Slots: map[string]*ledger.Slot{
			"bead-fresh": {
				PhaseID:   "bead-fresh",
				Status:    ledger.SlotRunning,
				UpdatedAt: freshTime.Format(time.RFC3339),
			},
		},
	})

	o := projectOpts(root)
	o.Now = func() time.Time { return now }
	o.StallThreshold = 30 * time.Minute

	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameStalledRuns)
	if f.Level != LevelOK {
		t.Errorf("stalled-runs level=%s, want ok for fresh slot", f.Level)
	}
}

func TestStalledRunTerminalSlotIgnored(t *testing.T) {
	root := fabricateProject(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	staleTime := now.Add(-3 * time.Hour)

	writeLedgerRun(t, root, &ledger.Run{
		RunID:     "20260702-090000",
		ProjectID: "testproject",
		Status:    ledger.RunRunning,
		StartedAt: staleTime.Format(time.RFC3339),
		UpdatedAt: staleTime.Format(time.RFC3339),
		Slots: map[string]*ledger.Slot{
			"bead-done": {
				PhaseID:   "bead-done",
				Status:    ledger.SlotDone, // terminal — should be ignored
				UpdatedAt: staleTime.Format(time.RFC3339),
			},
		},
	})

	o := projectOpts(root)
	o.Now = func() time.Time { return now }

	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameStalledRuns)
	if f.Level != LevelOK {
		t.Errorf("stalled-runs level=%s, want ok for terminal slot", f.Level)
	}
}

func TestStalledRunNoRuns(t *testing.T) {
	root := fabricateProject(t)
	r, err := RunProject(projectOpts(root))
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameStalledRuns)
	if f.Level != LevelOK {
		t.Errorf("stalled-runs level=%s, want ok when no runs", f.Level)
	}
}

// --- orphan-worktrees ---

func TestOrphanWorktreeDetected(t *testing.T) {
	root := fabricateProject(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	// A completed run with no active slots.
	writeLedgerRun(t, root, &ledger.Run{
		RunID:     "20260702-110000",
		ProjectID: "testproject",
		Status:    ledger.RunDone,
		StartedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		UpdatedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		Slots:     map[string]*ledger.Slot{},
	})

	wtRoot := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-worktrees")
	orphanPath := filepath.Join(wtRoot, "agent-testproject-bead1")

	o := projectOpts(root)
	o.WorktreeRoot = wtRoot
	o.Now = func() time.Time { return now }
	o.ListWorktrees = func(_ string) ([]worktreeEntry, error) {
		return []worktreeEntry{
			{Path: orphanPath, Branch: "agent/testproject-bead1"},
		}, nil
	}

	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameOrphanWorktrees)
	if f.Level != LevelWarn {
		t.Errorf("orphan-worktrees level=%s, want warn for orphan", f.Level)
	}
}

func TestOrphanWorktreeActiveSlotNotFlagged(t *testing.T) {
	root := fabricateProject(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	wtRoot := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-worktrees")
	worktreePath := filepath.Join(wtRoot, "agent-testproject-bead2")

	// Running run with an active slot that claims our worktree.
	writeLedgerRun(t, root, &ledger.Run{
		RunID:     "20260702-111500",
		ProjectID: "testproject",
		Status:    ledger.RunRunning,
		StartedAt: now.Add(-10 * time.Minute).Format(time.RFC3339),
		UpdatedAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
		Slots: map[string]*ledger.Slot{
			"bead2": {
				PhaseID:   "bead2",
				Status:    ledger.SlotRunning,
				Worktree:  worktreePath,
				UpdatedAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
			},
		},
	})

	o := projectOpts(root)
	o.WorktreeRoot = wtRoot
	o.Now = func() time.Time { return now }
	o.ListWorktrees = func(_ string) ([]worktreeEntry, error) {
		return []worktreeEntry{
			{Path: worktreePath, Branch: "agent/testproject-bead2"},
		}, nil
	}

	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameOrphanWorktrees)
	if f.Level != LevelOK {
		t.Errorf("orphan-worktrees level=%s, want ok for worktree with active slot", f.Level)
	}
}

func TestOrphanWorktreeNonKoryphBranchIgnored(t *testing.T) {
	root := fabricateProject(t)
	wtRoot := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-worktrees")

	o := projectOpts(root)
	o.WorktreeRoot = wtRoot
	o.ListWorktrees = func(_ string) ([]worktreeEntry, error) {
		return []worktreeEntry{
			// Branch doesn't start with "agent/" — should be ignored.
			{Path: filepath.Join(wtRoot, "feature-x"), Branch: "feature/x"},
		}, nil
	}

	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameOrphanWorktrees)
	if f.Level != LevelOK {
		t.Errorf("orphan-worktrees level=%s, want ok for non-koryph branch", f.Level)
	}
}

func TestOrphanWorktreeOutsideRootIgnored(t *testing.T) {
	root := fabricateProject(t)
	wtRoot := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-worktrees")

	o := projectOpts(root)
	o.WorktreeRoot = wtRoot
	o.ListWorktrees = func(_ string) ([]worktreeEntry, error) {
		return []worktreeEntry{
			// Path is outside the worktree root — should be ignored.
			{Path: "/tmp/other-project-worktrees/agent-testproject-bead3", Branch: "agent/testproject-bead3"},
		}, nil
	}

	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameOrphanWorktrees)
	if f.Level != LevelOK {
		t.Errorf("orphan-worktrees level=%s, want ok for path outside wtRoot", f.Level)
	}
}

// --- parseWorktreePorcelain ---

func TestParseWorktreePorcelain(t *testing.T) {
	input := "worktree /a/b/main\nHEAD abc123\nbranch refs/heads/main\n\nworktree /a/b/w1\nHEAD def456\nbranch refs/heads/agent/proj-x\n\n"
	got := parseWorktreePorcelain(input)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Path != "/a/b/main" || got[0].Branch != "main" {
		t.Errorf("entry 0: path=%s branch=%s", got[0].Path, got[0].Branch)
	}
	if got[1].Path != "/a/b/w1" || got[1].Branch != "agent/proj-x" {
		t.Errorf("entry 1: path=%s branch=%s", got[1].Path, got[1].Branch)
	}
}
