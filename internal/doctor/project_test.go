// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
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
		// Hermetic: stub the bd version probe so RunProject never shells out to
		// a real bd. The dedicated beads-upgrade tests override this.
		BeadsVersion: func() beads.VersionInfo {
			return beads.VersionInfo{Found: true, OK: true, Version: beads.MinVersion, Path: "/usr/bin/bd"}
		},
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

// TestHooksWiringDuplicatePrime is the koryph-14p.2 regression: `bd init`
// run after `koryph project add` appends its own bare "bd prime --hook-json"
// SessionStart entry alongside koryph's koryph-prime.sh wrapper entry,
// producing double session priming. doctor must call this out distinctly
// from the plain "present/missing" marker findings above.
func TestHooksWiringDuplicatePrime(t *testing.T) {
	root := fabricateProject(t)
	settings := `{
	  "hooks": {
	    "SessionStart": [
	      {"hooks":[{"type":"command","command":"\"${KORYPH_HOME:-$HOME/.koryph}/hooks/koryph-prime.sh\"  # replaces: bd prime --hook-json"}]},
	      {"hooks":[{"type":"command","command":"bd prime --hook-json"}]}
	    ],
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

	r, err := RunProject(projectOpts(root))
	if err != nil {
		t.Fatal(err)
	}
	var dup *Finding
	for i, f := range r.Findings {
		if f.Check == checkNameHooksWiring && strings.Contains(f.Message, "duplicate session priming") {
			dup = &r.Findings[i]
		}
	}
	if dup == nil {
		t.Fatalf("hooks-wiring: no duplicate-priming finding among: %+v", r.Findings)
	}
	if dup.Level != LevelWarn {
		t.Errorf("duplicate-priming level=%s, want warn", dup.Level)
	}
	if !strings.Contains(dup.Message, "2 entries") {
		t.Errorf("duplicate-priming message = %q, want it to name the count (2 entries)", dup.Message)
	}
	if !strings.Contains(dup.Message, `"bd prime"`) {
		t.Errorf("duplicate-priming message = %q, want it to name the marker", dup.Message)
	}
}

// TestHooksWiringNoDuplicatePrime proves a single (already-migrated) prime
// entry never trips the duplicate-priming finding — fabricateProject's
// baseline settings.json (one bare "bd prime --hook-json" entry) is the
// steady state after `koryph project add` alone, before any `bd init`.
func TestHooksWiringNoDuplicatePrime(t *testing.T) {
	root := fabricateProject(t)
	r, err := RunProject(projectOpts(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		if f.Check == checkNameHooksWiring && strings.Contains(f.Message, "duplicate session priming") {
			t.Errorf("unexpected duplicate-priming finding with a single entry: %q", f.Message)
		}
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

// --- beads-upgrade offer (nix flake) ---

// stampedeFlake writes a flake.nix pinning a beads input to root.
func stampedeFlake(t *testing.T, root string) {
	t.Helper()
	flake := "{\n  inputs = {\n    beads.url = \"github:gastownhall/beads/v1.1.0\";\n  };\n}\n"
	if err := os.WriteFile(filepath.Join(root, "flake.nix"), []byte(flake), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckBeadsUpgradeCurrentOK(t *testing.T) {
	root := t.TempDir()
	opts := projectOpts(root)
	opts.BeadsVersion = func() beads.VersionInfo {
		return beads.VersionInfo{Found: true, OK: true, Version: "1.1.0", Path: "/opt/homebrew/bin/bd"}
	}
	if f := checkBeadsUpgrade(opts, root); f.Level != LevelOK {
		t.Fatalf("current bd: level = %q, want ok (%q)", f.Level, f.Message)
	}
}

func TestCheckBeadsUpgradeOffersNixFlakeCommand(t *testing.T) {
	root := t.TempDir()
	stampedeFlake(t, root)
	opts := projectOpts(root)
	opts.BeadsVersion = func() beads.VersionInfo {
		return beads.VersionInfo{Found: true, OK: false, Version: "1.0.3",
			Path: "/nix/store/x-beads-1.0.3/bin/bd", FromNix: true}
	}
	f := checkBeadsUpgrade(opts, root)
	if f.Level != LevelWarn {
		t.Fatalf("stale nix bd: level = %q, want warn", f.Level)
	}
	if !strings.Contains(f.Message, "nix flake lock --update-input beads") {
		t.Errorf("offer missing the targeted flake command: %q", f.Message)
	}
	if !strings.Contains(f.Message, "v1.1.0") {
		t.Errorf("offer should name the pinned version: %q", f.Message)
	}
}

func TestCheckBeadsUpgradeFixRunsUpdate(t *testing.T) {
	root := t.TempDir()
	stampedeFlake(t, root)
	var gotDir, gotInput string
	opts := projectOpts(root)
	opts.Fix = true
	opts.BeadsVersion = func() beads.VersionInfo {
		return beads.VersionInfo{Found: true, OK: false, Version: "1.0.3",
			Path: "/nix/store/x-beads-1.0.3/bin/bd", FromNix: true}
	}
	opts.NixFlakeUpdate = func(dir, input string) error { gotDir, gotInput = dir, input; return nil }

	f := checkBeadsUpgrade(opts, root)
	if gotInput != "beads" || gotDir != root {
		t.Fatalf("NixFlakeUpdate called with (%q, %q), want (%q, beads)", gotDir, gotInput, root)
	}
	if !f.Fixed {
		t.Errorf("finding not marked Fixed after a successful update: %q", f.Message)
	}
	if !strings.Contains(strings.ToLower(f.Message), "reload") {
		t.Errorf("post-fix message should tell the operator to reload the devshell: %q", f.Message)
	}
}
