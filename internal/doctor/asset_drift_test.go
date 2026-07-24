// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// fakeFS builds a minimal test FS with the given filename→content map.
func fakeFS(files map[string]string) fs.FS {
	m := make(fstest.MapFS)
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

// projectOptsWithFakeAssets returns ProjectOptions wired with fake embedded
// asset FSes for isolation — no real embedded binaries in the test binary.
func projectOptsWithFakeAssets(root string, cmdFS, agentFS fs.FS) ProjectOptions {
	o := projectOpts(root)
	o.CommandsFS = cmdFS
	o.AgentsFS = agentFS
	return o
}

// --- asset-drift: all up to date ---

func TestAssetDriftAllUpToDate(t *testing.T) {
	root := fabricateProject(t)

	cmdFS := fakeFS(map[string]string{
		"koryph-build.md": "build content",
	})
	agentFS := fakeFS(map[string]string{
		"koryph-implementer.md": "implementer content",
		"README.md":             "ignored", // not koryph-*, should be skipped
	})

	// Pre-install identical content.
	writeInstalled(t, root, "commands/koryph-build.md", "build content")
	writeInstalled(t, root, "agents/koryph-implementer.md", "implementer content")

	o := projectOptsWithFakeAssets(root, cmdFS, agentFS)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameAssetDrift)
	if f.Level != LevelOK {
		t.Errorf("asset-drift level=%s msg=%q, want ok when all assets match", f.Level, f.Message)
	}
}

// --- asset-drift: missing command ---

func TestAssetDriftMissingCommand(t *testing.T) {
	root := fabricateProject(t)

	cmdFS := fakeFS(map[string]string{
		"koryph-build.md": "build content",
	})
	agentFS := fakeFS(map[string]string{})

	// Do NOT pre-install koryph-build.md → it should be reported as missing.

	o := projectOptsWithFakeAssets(root, cmdFS, agentFS)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameAssetDrift)
	if f.Level != LevelWarn {
		t.Errorf("asset-drift level=%s msg=%q, want warn for missing command", f.Level, f.Message)
	}
}

// --- asset-drift: stale command ---

func TestAssetDriftStaleCommand(t *testing.T) {
	root := fabricateProject(t)

	cmdFS := fakeFS(map[string]string{
		"koryph-build.md": "new embedded content",
	})
	agentFS := fakeFS(map[string]string{})

	// Install OLD content — hash will differ from embedded.
	writeInstalled(t, root, "commands/koryph-build.md", "old installed content")

	o := projectOptsWithFakeAssets(root, cmdFS, agentFS)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameAssetDrift)
	if f.Level != LevelWarn {
		t.Errorf("asset-drift level=%s msg=%q, want warn for stale command", f.Level, f.Message)
	}
}

// --- asset-drift: missing agent (koryph-* filtered) ---

func TestAssetDriftMissingAgent(t *testing.T) {
	root := fabricateProject(t)

	cmdFS := fakeFS(map[string]string{})
	agentFS := fakeFS(map[string]string{
		"koryph-implementer.md": "impl content",
		"README.md":             "readme ignored",
	})

	// Do NOT pre-install koryph-implementer.md.

	o := projectOptsWithFakeAssets(root, cmdFS, agentFS)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameAssetDrift)
	if f.Level != LevelWarn {
		t.Errorf("asset-drift level=%s msg=%q, want warn for missing koryph-* agent", f.Level, f.Message)
	}
}

// README.md in agents FS is not koryph-* and must be silently ignored.
func TestAssetDriftReadmeIgnored(t *testing.T) {
	root := fabricateProject(t)

	cmdFS := fakeFS(map[string]string{})
	agentFS := fakeFS(map[string]string{
		"README.md": "read the manual",
	})

	// No koryph-*.md in agents FS and none on disk — should be OK (nothing to check).

	o := projectOptsWithFakeAssets(root, cmdFS, agentFS)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameAssetDrift)
	if f.Level != LevelOK {
		t.Errorf("asset-drift level=%s msg=%q, want ok when only README.md in agentsFS (filtered out)", f.Level, f.Message)
	}
}

// --- asset-drift: fix installs missing file ---

func TestAssetDriftFixInstallsMissing(t *testing.T) {
	root := fabricateProject(t)

	cmdFS := fakeFS(map[string]string{
		"koryph-build.md": "build content",
	})
	agentFS := fakeFS(map[string]string{})

	// Do NOT pre-install — fix should install it.

	o := projectOptsWithFakeAssets(root, cmdFS, agentFS)
	o.Fix = true
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameAssetDrift)
	if !f.Fixed {
		t.Errorf("asset-drift fixed=%v level=%s msg=%q, want fixed=true after --fix installs missing file", f.Fixed, f.Level, f.Message)
	}

	// File should now exist on disk with the embedded content.
	dest := filepath.Join(root, "commands", "koryph-build.md")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("installed file not found: %v", err)
	}
	if string(got) != "build content" {
		t.Errorf("installed content=%q, want %q", got, "build content")
	}
	if r.FixedCount != 1 {
		t.Errorf("FixedCount=%d, want 1", r.FixedCount)
	}
}

// --- asset-drift: fix without force leaves stale file untouched ---

func TestAssetDriftFixNoForceSkipsStale(t *testing.T) {
	root := fabricateProject(t)

	cmdFS := fakeFS(map[string]string{
		"koryph-build.md": "new content",
	})
	agentFS := fakeFS(map[string]string{})

	writeInstalled(t, root, "commands/koryph-build.md", "old content")

	o := projectOptsWithFakeAssets(root, cmdFS, agentFS)
	o.Fix = true
	// Force NOT set: stale file must be left untouched.
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameAssetDrift)
	if f.Fixed {
		t.Errorf("asset-drift fixed=%v, stale file should NOT be overwritten without --force", f.Fixed)
	}
	if f.Level != LevelWarn {
		t.Errorf("asset-drift level=%s, want warn for skipped stale file", f.Level)
	}

	// On-disk content must remain the old version.
	dest := filepath.Join(root, "commands", "koryph-build.md")
	got, _ := os.ReadFile(dest)
	if string(got) != "old content" {
		t.Errorf("on-disk content=%q, want old content to be preserved", got)
	}
}

// --- asset-drift: fix + force overwrites stale file ---

func TestAssetDriftFixForceOverwritesStale(t *testing.T) {
	root := fabricateProject(t)

	cmdFS := fakeFS(map[string]string{
		"koryph-build.md": "new content",
	})
	agentFS := fakeFS(map[string]string{})

	writeInstalled(t, root, "commands/koryph-build.md", "old content")

	o := projectOptsWithFakeAssets(root, cmdFS, agentFS)
	o.Fix = true
	o.Force = true
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameAssetDrift)
	if !f.Fixed {
		t.Errorf("asset-drift fixed=%v level=%s, want fixed=true when --fix --force overwrites stale", f.Fixed, f.Level)
	}

	dest := filepath.Join(root, "commands", "koryph-build.md")
	got, _ := os.ReadFile(dest)
	if string(got) != "new content" {
		t.Errorf("on-disk content=%q, want %q after force overwrite", got, "new content")
	}
}

// --- asset-drift: multiple assets, some ok some missing ---

func TestAssetDriftMixedFindings(t *testing.T) {
	root := fabricateProject(t)

	cmdFS := fakeFS(map[string]string{
		"koryph-build.md": "build",
		"koryph-loop.md":  "loop",
	})
	agentFS := fakeFS(map[string]string{})

	// Only install one of the two commands.
	writeInstalled(t, root, "commands/koryph-build.md", "build")
	// koryph-loop.md is missing.

	o := projectOptsWithFakeAssets(root, cmdFS, agentFS)
	r, err := RunProject(o)
	if err != nil {
		t.Fatal(err)
	}

	var warns int
	for _, f := range r.Findings {
		if f.Check == checkNameAssetDrift && f.Level == LevelWarn {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("asset-drift: got %d warn findings, want 1 (for missing koryph-loop.md)", warns)
	}
}

// --- helpers ---

// writeInstalled creates a file in the project tree as if previously installed.
func writeInstalled(t *testing.T, root, relPath, content string) {
	t.Helper()
	dest := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
