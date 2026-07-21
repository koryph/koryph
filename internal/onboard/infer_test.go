// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package onboard

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// --- InferForge --------------------------------------------------------

func TestInferForge(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"github https", "https://github.com/acme/widgets.git", "github"},
		{"github ssh", "git@github.com:acme/widgets.git", "github"},
		{"gitlab https", "https://gitlab.com/acme/app.git", "gitlab"},
		{"self-hosted gitlab", "https://gitlab.company.com/acme/app.git", "gitlab"},
		{"codeberg unknown", "https://codeberg.org/acme/app.git", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InferForge(tt.url)
			if got.Value != tt.want {
				t.Errorf("InferForge(%q).Value = %q, want %q", tt.url, got.Value, tt.want)
			}
			if got.Provenance == "" {
				t.Errorf("InferForge(%q).Provenance is empty, want an explanation", tt.url)
			}
		})
	}
}

// --- InferGate -----------------------------------------------------------

// gateValues extracts just the Value field, in order, for assertion.
func gateValues(props []Proposal) []string {
	out := make([]string, len(props))
	for i, p := range props {
		out[i] = p.Value
	}
	return out
}

func assertGateValues(t *testing.T, got []Proposal, want []string) {
	t.Helper()
	gotValues := gateValues(got)
	if len(gotValues) != len(want) {
		t.Fatalf("InferGate values = %v, want %v", gotValues, want)
	}
	for i := range want {
		if gotValues[i] != want[i] {
			t.Errorf("InferGate values = %v, want %v", gotValues, want)
			return
		}
	}
	for _, p := range got {
		if p.Provenance == "" {
			t.Errorf("proposal %q has empty Provenance", p.Value)
		}
	}
}

func TestInferGate_MakefileWithGateTarget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Makefile"), `.PHONY: gate test lint build
gate: test lint build
	@echo gate

test:
	go test ./...

lint:
	golangci-lint run

build:
	go build ./...
`)
	got := InferGate(root)
	assertGateValues(t, got, []string{"make gate"})
	if !strings.Contains(got[0].Provenance, "Makefile") || !strings.Contains(got[0].Provenance, "gate") {
		t.Errorf("Provenance = %q, want it to name the Makefile and the gate target", got[0].Provenance)
	}
}

func TestInferGate_MakefileWithoutGateTarget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Makefile"), `.PHONY: test lint build
test:
	go test ./...

lint:
	golangci-lint run

build:
	go build ./...
`)
	got := InferGate(root)
	assertGateValues(t, got, []string{"make test", "make lint", "make build"})
}

func TestInferGate_MakefilePartialTargets(t *testing.T) {
	root := t.TempDir()
	// Only test and build exist -- lint must be omitted, not proposed empty.
	writeFile(t, filepath.Join(root, "Makefile"), `test:
	go test ./...

build:
	go build ./...
`)
	got := InferGate(root)
	assertGateValues(t, got, []string{"make test", "make build"})
}

func TestInferGate_MakefileNoUsableTargetsFallsThrough(t *testing.T) {
	root := t.TempDir()
	// A Makefile exists but has none of gate/test/lint/build -- it must not
	// suppress the go.mod evidence underneath it.
	writeFile(t, filepath.Join(root, "Makefile"), `clean:
	rm -rf out
`)
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/x\n\ngo 1.26\n")
	got := InferGate(root)
	assertGateValues(t, got, []string{"go vet ./...", "go test ./..."})
}

func TestInferGate_PackageJSONScripts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{
  "name": "widgets",
  "scripts": {
    "test": "jest",
    "lint": "eslint .",
    "build": "tsc"
  }
}`)
	got := InferGate(root)
	assertGateValues(t, got, []string{"npm test", "npm run lint", "npm run build"})
}

func TestInferGate_PackageJSONPartialScripts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"scripts": {"test": "jest"}}`)
	got := InferGate(root)
	assertGateValues(t, got, []string{"npm test"})
}

func TestInferGate_GoModOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/x\n\ngo 1.26\n")
	got := InferGate(root)
	assertGateValues(t, got, []string{"go vet ./...", "go test ./..."})
}

func TestInferGate_Cargo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Cargo.toml"), "[package]\nname = \"widgets\"\nversion = \"0.1.0\"\n")
	got := InferGate(root)
	assertGateValues(t, got, []string{"cargo test"})
}

func TestInferGate_PyProjectToml(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = \"widgets\"\n")
	got := InferGate(root)
	assertGateValues(t, got, []string{"pytest"})
	if !strings.Contains(got[0].Provenance, "pyproject.toml") {
		t.Errorf("Provenance = %q, want it to name pyproject.toml", got[0].Provenance)
	}
}

func TestInferGate_SetupPyEvidence(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "setup.py"), "from setuptools import setup\nsetup(name='widgets')\n")
	got := InferGate(root)
	assertGateValues(t, got, []string{"pytest"})
	if !strings.Contains(got[0].Provenance, "setup.py") {
		t.Errorf("Provenance = %q, want it to name setup.py", got[0].Provenance)
	}
}

func TestInferGate_MixedGoAndNode(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/x\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "package.json"), `{"scripts": {"test": "jest", "lint": "eslint .", "build": "tsc"}}`)
	got := InferGate(root)
	// design §6: "go.mod + package.json -> both sets, go first".
	assertGateValues(t, got, []string{
		"go vet ./...", "go test ./...",
		"npm test", "npm run lint", "npm run build",
	})
}

func TestInferGate_NoEvidence(t *testing.T) {
	root := t.TempDir()
	got := InferGate(root)
	if len(got) != 0 {
		t.Errorf("InferGate on an empty dir = %v, want none", got)
	}
}

// --- InferAreaMap ----------------------------------------------------------

func writeN(t *testing.T, dir, ext string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%d%s", i, ext)), "// x\n")
	}
}

func TestInferAreaMap_SkipsAndLangDetection(t *testing.T) {
	root := t.TempDir()

	// Eligible: dominant-language directories.
	writeN(t, filepath.Join(root, "cmd"), ".go", 2)
	writeN(t, filepath.Join(root, "web"), ".ts", 3)
	writeN(t, filepath.Join(root, "scripts"), ".py", 1)

	// A nested vendor/ tree inside an otherwise-eligible dir must not count
	// toward that dir's file total or leak in as its own area.
	writeN(t, filepath.Join(root, "cmd", "vendor"), ".go", 5)

	// Skipped entirely: docs/vendor/node_modules/dot-dir/empty.
	writeN(t, filepath.Join(root, "docs"), ".md", 4)
	writeN(t, filepath.Join(root, "vendor"), ".go", 4)
	writeN(t, filepath.Join(root, "node_modules"), ".js", 4)
	writeN(t, filepath.Join(root, ".git"), ".go", 1)
	writeFile(t, filepath.Join(root, "empty_dir", "README.md"), "nothing but docs\n")

	areas, provenance := InferAreaMap(root)

	want := map[string][]string{
		"cmd":     {"go:cmd"},
		"web":     {"ts:web"},
		"scripts": {"py:scripts"},
	}
	if len(areas) != len(want) {
		t.Fatalf("InferAreaMap areas = %v, want %v", areas, want)
	}
	for k, v := range want {
		got, ok := areas[k]
		if !ok {
			t.Errorf("area %q missing from result %v", k, areas)
			continue
		}
		if len(got) != 1 || got[0] != v[0] {
			t.Errorf("area %q = %v, want %v", k, got, v)
		}
	}
	for _, skip := range []string{"docs", "doc", "vendor", "node_modules", ".git", "empty_dir"} {
		if _, ok := areas[skip]; ok {
			t.Errorf("area %q should have been skipped, got entry %v", skip, areas[skip])
		}
	}
	if provenance == "" {
		t.Error("InferAreaMap provenance is empty")
	}
	if !strings.Contains(provenance, "cmd") {
		t.Errorf("provenance = %q, want it to name the proposed directories", provenance)
	}
}

func TestInferAreaMap_LanguageTieBreak(t *testing.T) {
	root := t.TempDir()
	// Equal counts of .go and .ts: areaExtPriority puts go first, so "go"
	// must win the tie deterministically.
	writeN(t, filepath.Join(root, "mixed"), ".go", 2)
	writeN(t, filepath.Join(root, "mixed"), ".ts", 2)

	areas, _ := InferAreaMap(root)
	got, ok := areas["mixed"]
	if !ok || len(got) != 1 || got[0] != "go:mixed" {
		t.Errorf("areas[\"mixed\"] = %v, want [\"go:mixed\"]", got)
	}
}

func TestInferAreaMap_UnrecognizedExtensionOnlyIsNotSource(t *testing.T) {
	root := t.TempDir()
	// Only .md and .txt files: neither counts as "source", so this dir must
	// not appear in the result at all.
	writeFile(t, filepath.Join(root, "notes", "a.md"), "# hi\n")
	writeFile(t, filepath.Join(root, "notes", "b.txt"), "hi\n")

	areas, _ := InferAreaMap(root)
	if _, ok := areas["notes"]; ok {
		t.Errorf("areas[\"notes\"] = %v, want absent (no recognized source files)", areas["notes"])
	}
}

func TestInferAreaMap_CapAtTwelveLargestWin(t *testing.T) {
	root := t.TempDir()
	// 15 directories, descending file counts: area01 has the most files,
	// area15 the fewest. Only the top 12 by file count should survive.
	const n = 15
	for i := 1; i <= n; i++ {
		dir := fmt.Sprintf("area%02d", i)
		writeN(t, filepath.Join(root, dir), ".go", n-i+1)
	}

	areas, provenance := InferAreaMap(root)
	if len(areas) != areaMapCap {
		t.Fatalf("len(areas) = %d, want %d", len(areas), areaMapCap)
	}
	for i := 1; i <= areaMapCap; i++ {
		dir := fmt.Sprintf("area%02d", i)
		if _, ok := areas[dir]; !ok {
			t.Errorf("expected surviving area %q missing from %v", dir, areas)
		}
	}
	for i := areaMapCap + 1; i <= n; i++ {
		dir := fmt.Sprintf("area%02d", i)
		if _, ok := areas[dir]; ok {
			t.Errorf("area %q should have been dropped by the cap, present in %v", dir, areas)
		}
	}
	if !strings.Contains(provenance, "capped") {
		t.Errorf("provenance = %q, want it to mention the cap", provenance)
	}
}

func TestInferAreaMap_EmptyRoot(t *testing.T) {
	root := t.TempDir()
	areas, provenance := InferAreaMap(root)
	if len(areas) != 0 {
		t.Errorf("InferAreaMap on an empty root = %v, want none", areas)
	}
	if provenance == "" {
		t.Error("InferAreaMap provenance is empty even for a trivial result")
	}
}
