// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package scripts: docs-lint tests catch designs/-relative links (e.g.
// ../designs/<doc>.md) in docs/ pages outside docs/designs/ before CI does.
//
// Background: CI runs `rm -rf docs/designs` then `zensical build --strict`
// (.github/workflows/docs.yml). A user-guide page that links ../designs/foo.md
// passes a local build (designs/ present) but breaks the published-site build.
// This caused post-merge red main twice: tui.md via 9b41aee, and
// vscode-extension.md via ew2.8.
//
// Two test groups:
//   - Fixture tests (fast, hermetic): drive scripts/docs-lint.sh against
//     testdata/docs-lint/ to confirm detection logic on the ew2.8 regression
//     shape.
//   - Live check (integration): run docs-lint.sh against the real docs/ tree;
//     fails the gate if a bad link is committed.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// docsLintScript returns the absolute path to scripts/docs-lint.sh from
// within the scripts/ test working directory.
func docsLintScript(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("docs-lint.sh")
	if err != nil {
		t.Fatalf("cannot resolve docs-lint.sh: %v", err)
	}
	return abs
}

// runDocsLint runs docs-lint.sh against docsDir, returning stdout+stderr and exit code.
func runDocsLint(t *testing.T, docsDir string) (output string, exitCode int) {
	t.Helper()
	cmd := exec.Command("bash", docsLintScript(t), docsDir)
	out, err := cmd.CombinedOutput()
	output = string(out)
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("docs-lint.sh: %v\noutput:\n%s", err, output)
		}
	}
	return output, exitCode
}

// TestDocsLint_BadFixture_Detected reproduces the ew2.8 regression shape: a
// user-guide page containing a ../designs/ relative link must be caught and
// the script must exit non-zero.
func TestDocsLint_BadFixture_Detected(t *testing.T) {
	badDir := filepath.Join("testdata", "docs-lint")
	// Confirm the fixture is actually bad (guard against accidental cleanup).
	badFile := filepath.Join(badDir, "bad-link.md")
	content, err := os.ReadFile(badFile)
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	if !strings.Contains(string(content), "../designs/") {
		t.Fatalf("fixture %s does not contain ../designs/ — test is invalid", badFile)
	}

	out, code := runDocsLint(t, badDir)
	if code == 0 {
		t.Fatalf("docs-lint.sh exited 0 (clean) for a tree with ../designs/ links:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("output missing FAIL marker:\n%s", out)
	}
	if !strings.Contains(out, "bad-link.md") {
		t.Errorf("output does not name the offending file (bad-link.md):\n%s", out)
	}
	if !strings.Contains(out, "../designs/") {
		t.Errorf("output does not echo the offending pattern:\n%s", out)
	}
}

// TestDocsLint_GoodFixture_Passes confirms that a clean docs tree (no
// ../designs/ links) exits zero.
func TestDocsLint_GoodFixture_Passes(t *testing.T) {
	// Use a temp dir with only the good fixture so the bad one doesn't bleed in.
	tmp := t.TempDir()
	src := filepath.Join("testdata", "docs-lint", "good-link.md")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "good-link.md"), data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, code := runDocsLint(t, tmp)
	if code != 0 {
		t.Fatalf("docs-lint.sh exited %d (failure) for a clean tree:\n%s", code, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("output missing OK marker:\n%s", out)
	}
}

// TestDocsLint_DesignsFilesExcluded confirms that ../designs/ references
// inside docs/designs/ itself are NOT flagged (they may link sibling designs).
func TestDocsLint_DesignsFilesExcluded(t *testing.T) {
	tmp := t.TempDir()
	designsDir := filepath.Join(tmp, "designs")
	if err := os.MkdirAll(designsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A design doc that cross-links another design — should be ignored.
	crossLink := "# Design A\n\nSee also [Design B](../designs/design-b.md).\n"
	if err := os.WriteFile(filepath.Join(designsDir, "design-a.md"), []byte(crossLink), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, code := runDocsLint(t, tmp)
	if code != 0 {
		t.Fatalf("docs-lint.sh flagged a ../designs/ link inside docs/designs/ (should be ignored):\n%s", out)
	}
	_ = out
}

// TestDocsLint_LiveDocsTree is the gate integration test: run docs-lint.sh
// against the real docs/ tree in this repository. Fails the suite (and
// therefore make test / go test ./...) if any user-guide page contains a
// ../designs/ link that would break the CI docs build.
func TestDocsLint_LiveDocsTree(t *testing.T) {
	docsDir, err := filepath.Abs("../docs")
	if err != nil {
		t.Fatalf("cannot resolve ../docs: %v", err)
	}
	if _, err := os.Stat(docsDir); err != nil {
		t.Skipf("docs/ not found at %s (not in a full checkout?): %v", docsDir, err)
	}

	out, code := runDocsLint(t, docsDir)
	if code != 0 {
		t.Fatalf("designs/-relative link(s) found in docs/ — CI build would fail:\n%s", out)
	}
	t.Logf("docs-lint live check: %s", strings.TrimSpace(out))
}
