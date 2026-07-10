// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// docs_drift_test.go — drift checks that keep the docs in lockstep with the
// tree, in the same live-check style as docs_lint_test.go:
//
//   - TestDocsDrift_PackagesMDCoversInternal: every top-level internal/<pkg>
//     directory (one that directly contains a non-test .go file) must have a
//     matching "## <pkg>" section in docs/developer-guide/packages.md, whose
//     header promises "one section per internal/ package". A heading with no
//     matching package is also flagged (a deleted/renamed package must take
//     its section with it).
//   - TestDocsDrift_MkdocsNavListsGuidePages: every docs/user-guide/*.md and
//     docs/developer-guide/*.md page must appear in mkdocs.yml. zensical has
//     no auto-nav for explicit `nav:` blocks (see the NAV COUPLING note in
//     mkdocs.yml), so a page missing from nav silently vanishes from the
//     published book.
//
// Both are live checks against the real repo (resolved relative to scripts/,
// like TestDocsLint_LiveDocsTree) and skip when run outside a full checkout.

package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoPath resolves a path relative to the repo root from within the
// scripts/ test working directory, skipping the test when it is absent
// (not a full checkout).
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", rel))
	if err != nil {
		t.Fatalf("cannot resolve ../%s: %v", rel, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("%s not found at %s (not in a full checkout?): %v", rel, abs, err)
	}
	return abs
}

// internalPackages returns the sorted names of top-level internal/ packages:
// directories directly containing at least one non-test .go file. Nested
// packages (internal/forge/github, internal/runtime/claude, …) are sections
// of their parent and are deliberately not required individually.
func internalPackages(t *testing.T, internalDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(internalDir)
	if err != nil {
		t.Fatalf("read %s: %v", internalDir, err)
	}
	var pkgs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(internalDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Join(internalDir, e.Name()), err)
		}
		for _, f := range files {
			name := f.Name()
			if !f.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
				pkgs = append(pkgs, e.Name())
				break
			}
		}
	}
	sort.Strings(pkgs)
	return pkgs
}

var packagesMDHeadingRe = regexp.MustCompile(`(?m)^## +(\S+)\s*$`)

// TestDocsDrift_PackagesMDCoversInternal fails when an internal/ package has
// no "## <pkg>" section in packages.md, or when packages.md keeps a section
// for a package that no longer exists.
func TestDocsDrift_PackagesMDCoversInternal(t *testing.T) {
	internalDir := repoPath(t, "internal")
	docPath := repoPath(t, filepath.Join("docs", "developer-guide", "packages.md"))

	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}

	headings := map[string]bool{}
	for _, m := range packagesMDHeadingRe.FindAllStringSubmatch(string(doc), -1) {
		headings[m[1]] = true
	}
	if len(headings) == 0 {
		t.Fatalf("no '## <pkg>' headings parsed from %s — regex or file shape changed?", docPath)
	}

	pkgs := internalPackages(t, internalDir)
	if len(pkgs) == 0 {
		t.Fatalf("no Go packages found under %s — walk logic broken?", internalDir)
	}

	var missing []string
	seen := map[string]bool{}
	for _, p := range pkgs {
		seen[p] = true
		if !headings[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Errorf("packages.md is missing a '## <pkg>' section for %d internal package(s): %s\n"+
			"add a section to %s (its header promises one section per internal/ package)",
			len(missing), strings.Join(missing, ", "), docPath)
	}

	var stale []string
	for h := range headings {
		if !seen[h] {
			stale = append(stale, h)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("packages.md has section(s) for package(s) that do not exist under internal/: %s\n"+
			"remove or rename the stale section(s) in %s", strings.Join(stale, ", "), docPath)
	}
}

// TestDocsDrift_MkdocsNavListsGuidePages fails when a published guide page
// (docs/user-guide/*.md or docs/developer-guide/*.md) does not appear in
// mkdocs.yml — zensical's explicit nav means such a page never reaches the
// published book.
func TestDocsDrift_MkdocsNavListsGuidePages(t *testing.T) {
	mkdocsPath := repoPath(t, "mkdocs.yml")
	mkdocs, err := os.ReadFile(mkdocsPath)
	if err != nil {
		t.Fatalf("read %s: %v", mkdocsPath, err)
	}
	nav := string(mkdocs)

	for _, guide := range []string{"user-guide", "developer-guide"} {
		guideDir := repoPath(t, filepath.Join("docs", guide))
		entries, err := os.ReadDir(guideDir)
		if err != nil {
			t.Fatalf("read %s: %v", guideDir, err)
		}
		var missing []string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			ref := guide + "/" + e.Name()
			if !strings.Contains(nav, ref) {
				missing = append(missing, ref)
			}
		}
		if len(missing) > 0 {
			t.Errorf("mkdocs.yml nav does not reference %d page(s) under docs/%s: %s\n"+
				"add them to the nav block in %s (zensical has no auto-nav — see the NAV COUPLING note there)",
				len(missing), guide, strings.Join(missing, ", "), mkdocsPath)
		}
	}
}
