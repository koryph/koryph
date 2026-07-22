// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// --- shared git/fs test helpers (also used by adopt_integration_test.go) -----

// initGitRepo creates a fresh git repo on main with a repo-local identity and
// signing disabled, so a commit made against it never depends on the host's
// global git config.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitCmd(t, dir, "init", "-b", "main")
	runGitCmd(t, dir, "config", "user.email", "t@t.t")
	runGitCmd(t, dir, "config", "user.name", "t")
	runGitCmd(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

// runGitCmd runs git in dir, failing the test on a non-zero exit, and returns
// combined stdout+stderr (trimmed).
func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeTestFile writes content to path, creating parent directories as needed.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- ResolveBeadsRemote --------------------------------------------------------

func TestResolveBeadsRemote(t *testing.T) {
	cases := []struct {
		name string
		opts BeadsOpts
		want string
	}{
		{
			name: "no-remote forces empty even with an override present",
			opts: BeadsOpts{NoRemote: true, RemoteOverride: "git+https://custom.example/x.git", RemoteURL: "https://github.com/o/r.git"},
			want: "",
		},
		{
			name: "explicit override wins over the derived origin",
			opts: BeadsOpts{RemoteOverride: "git+https://custom.example/x.git", RemoteURL: "https://github.com/o/r"},
			want: "git+https://custom.example/x.git",
		},
		{
			name: "derived from origin when no override",
			opts: BeadsOpts{RemoteURL: "https://github.com/o/r"},
			want: "git+https://github.com/o/r.git",
		},
		{
			name: "all empty yields empty",
			opts: BeadsOpts{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveBeadsRemote(tc.opts); got != tc.want {
				t.Errorf("ResolveBeadsRemote(%+v) = %q, want %q", tc.opts, got, tc.want)
			}
		})
	}
}

// --- DirtyAdoptionPaths + CommitAdoption ---------------------------------------

func TestDirtyAdoptionPathsAndCommitAdoption(t *testing.T) {
	ctx := context.Background()
	root := initGitRepo(t)

	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "agents\n")
	writeTestFile(t, filepath.Join(root, ".claude", "settings.json"), "{}\n")
	writeTestFile(t, filepath.Join(root, "unrelated.txt"), "not an adoption path\n")

	dirty, err := DirtyAdoptionPaths(ctx, root)
	if err != nil {
		t.Fatalf("DirtyAdoptionPaths: %v", err)
	}
	wantSet := map[string]bool{"AGENTS.md": true, ".claude/": true}
	if len(dirty) != len(wantSet) {
		t.Fatalf("DirtyAdoptionPaths = %v, want exactly %v", dirty, wantSet)
	}
	for _, p := range dirty {
		if !wantSet[p] {
			t.Errorf("DirtyAdoptionPaths returned %q, want only adoption paths %v", p, wantSet)
		}
	}

	committed, files, err := CommitAdoption(ctx, root, "")
	if err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	if !committed {
		t.Fatalf("committed = false, want true (there was work to commit): files=%v", files)
	}

	logOut := runGitCmd(t, root, "log", "-1", "--name-only", "--pretty=format:%s")
	if !strings.Contains(logOut, "chore: adopt koryph") {
		t.Errorf("commit message = %q, want it to contain 'chore: adopt koryph'", logOut)
	}
	if !strings.Contains(logOut, "AGENTS.md") || !strings.Contains(logOut, filepath.Join(".claude", "settings.json")) {
		t.Errorf("committed files = %q, want AGENTS.md and .claude/settings.json", logOut)
	}
	if strings.Contains(logOut, "unrelated.txt") {
		t.Errorf("committed files = %q, must NOT include unrelated.txt", logOut)
	}

	status := runGitCmd(t, root, "status", "--porcelain")
	if !strings.Contains(status, "unrelated.txt") {
		t.Errorf("git status after commit = %q, want unrelated.txt to remain uncommitted", status)
	}

	// Second call: nothing left to commit (unrelated.txt is outside
	// AdoptionCommitPaths, so it never counts as dirty here).
	committed2, _, err := CommitAdoption(ctx, root, "")
	if err != nil {
		t.Fatalf("second CommitAdoption: %v", err)
	}
	if committed2 {
		t.Error("second CommitAdoption committed = true, want false (nothing to commit)")
	}
}

// --- RegisterAndConfigure -------------------------------------------------------

func newAdoptTestStore(t *testing.T) *registry.Store {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	s := registry.NewStore()
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	return s
}

func TestRegisterAndConfigure_FreshRepoRegistersAndScaffolds(t *testing.T) {
	ctx := context.Background()
	store := newAdoptTestStore(t)
	root := initGitRepo(t)

	snap := &Snapshot{
		Root:      root,
		ProjectID: "acme-widgets",
		Inventory: &onboard.Inventory{Root: root, IsGitRepo: true, DefaultBranch: "main"},
	}
	acct := AccountChoice{Profile: "personal", Identity: "me@example.com", Provenance: "test fixture"}
	gate := []string{"make test"}
	forgeName := "github"
	areaMap := map[string][]string{"src": {"go:src"}}

	rec, cfg, err := RegisterAndConfigure(ctx, store, snap, acct, gate, forgeName, areaMap, false)
	if err != nil {
		t.Fatalf("RegisterAndConfigure: %v", err)
	}
	if rec == nil || rec.ProjectID != "acme-widgets" {
		t.Fatalf("rec = %+v, want project_id acme-widgets", rec)
	}
	if rec.AccountProfile != "personal" || rec.ExpectedIdentity != "me@example.com" {
		t.Errorf("rec account = %+v, want personal/me@example.com", rec)
	}

	if cfg == nil {
		t.Fatal("cfg is nil")
	}
	if strings.Join(cfg.Gate, ",") != "make test" {
		t.Errorf("cfg.Gate = %v, want [make test]", cfg.Gate)
	}
	if cfg.Forge != "github" {
		t.Errorf("cfg.Forge = %q, want github", cfg.Forge)
	}
	if len(cfg.AreaMap) != 1 || strings.Join(cfg.AreaMap["src"], ",") != "go:src" {
		t.Errorf("cfg.AreaMap = %v, want {src: [go:src]}", cfg.AreaMap)
	}

	// koryph.project.json on disk must agree with the returned cfg.
	onDisk, err := project.Load(root)
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if strings.Join(onDisk.Gate, ",") != "make test" || onDisk.Forge != "github" {
		t.Errorf("on-disk config = gate:%v forge:%q, want [make test]/github", onDisk.Gate, onDisk.Forge)
	}

	got, err := store.Get("acme-widgets")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.ExpectedIdentity != "me@example.com" {
		t.Errorf("stored record identity = %q, want me@example.com", got.ExpectedIdentity)
	}
}

func TestRegisterAndConfigure_PreExistingConfigGateNotOverwritten(t *testing.T) {
	ctx := context.Background()
	store := newAdoptTestStore(t)
	root := initGitRepo(t)

	// A koryph.project.json already exists with its own gate/forge — this is
	// what AdapterPresent=true in the snapshot represents.
	existing := project.Default("acme-widgets")
	existing.Gate = []string{"make ci"}
	existing.Forge = "gitlab"
	if err := existing.Save(root); err != nil {
		t.Fatalf("seed existing config: %v", err)
	}

	snap := &Snapshot{
		Root:      root,
		ProjectID: "acme-widgets",
		Inventory: &onboard.Inventory{Root: root, IsGitRepo: true, DefaultBranch: "main", AdapterPresent: true},
	}
	acct := AccountChoice{Profile: "personal", Identity: "existing@example.com"}
	// The confirm phase derived DIFFERENT values than what's on disk; they
	// must never be applied because AdapterPresent is true.
	newGate := []string{"make test"}
	newForge := "github"
	newAreaMap := map[string][]string{"src": {"go:src"}}

	rec, cfg, err := RegisterAndConfigure(ctx, store, snap, acct, newGate, newForge, newAreaMap, false)
	if err != nil {
		t.Fatalf("RegisterAndConfigure: %v", err)
	}
	if rec == nil {
		t.Fatal("rec is nil")
	}
	if cfg == nil {
		t.Fatal("cfg is nil")
	}
	if strings.Join(cfg.Gate, ",") != "make ci" {
		t.Errorf("cfg.Gate = %v, want the pre-existing [make ci] preserved", cfg.Gate)
	}
	if cfg.Forge != "gitlab" {
		t.Errorf("cfg.Forge = %q, want the pre-existing gitlab preserved", cfg.Forge)
	}

	onDisk, err := project.Load(root)
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if strings.Join(onDisk.Gate, ",") != "make ci" || onDisk.Forge != "gitlab" {
		t.Errorf("on-disk config changed: gate=%v forge=%q, want the pre-existing values untouched", onDisk.Gate, onDisk.Forge)
	}
}
