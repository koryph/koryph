// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package personas_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/agents"
	"github.com/koryph/koryph/internal/personas"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// TestInstallForRuntimeClaudeIsVerbatim proves InstallForRuntime(root, force,
// "claude") — and the "" (unset) spelling of the same request — installs
// every embedded persona byte-identically to the source, i.e. runs no
// rendering pass at all (koryph-v8u.12's hard compatibility requirement).
func TestInstallForRuntimeClaudeIsVerbatim(t *testing.T) {
	for _, runtimeName := range []string{"", "claude"} {
		t.Run("runtime="+runtimeName, func(t *testing.T) {
			root := t.TempDir()
			results, untiered, err := personas.InstallForRuntime(root, false, runtimeName)
			if err != nil {
				t.Fatalf("InstallForRuntime: %v", err)
			}
			if len(untiered) != 0 {
				t.Errorf("untiered = %v, want none (claude path never renders)", untiered)
			}
			entries, err := os.ReadDir(filepath.Join(root, ".claude", "agents"))
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != len(results) {
				t.Fatalf("installed %d files, want %d (one per result)", len(entries), len(results))
			}
			for _, e := range entries {
				want, rerr := agents.FS.ReadFile(e.Name())
				if rerr != nil {
					t.Fatalf("read embedded %s: %v", e.Name(), rerr)
				}
				got, rerr := os.ReadFile(filepath.Join(root, ".claude", "agents", e.Name()))
				if rerr != nil {
					t.Fatalf("read installed %s: %v", e.Name(), rerr)
				}
				if string(got) != string(want) {
					t.Errorf("%s: installed content != embedded source (claude must be byte-identical)", e.Name())
				}
			}
		})
	}
}

// TestInstallForRuntimeStubRendersTierPins proves a non-claude runtime's
// ModelMap is consulted to rewrite each tiered persona's `model:` value,
// keyed by that persona's own `tier:` scalar — and that every OTHER byte of
// the file (including the exact key/spacing of the model: line and every
// surrounding line) is left alone, per the "minimal string substitution, not
// a YAML round-trip" requirement.
func TestInstallForRuntimeStubRendersTierPins(t *testing.T) {
	const name = "personas-test-stub-render"
	stub := runtimetest.Stub{StubName: name, Models: runtime.ModelMap{
		runtime.TierFrontier: "stub-frontier-model",
		runtime.TierStandard: "stub-standard-model",
		runtime.TierLight:    "stub-light-model",
	}}
	if err := runtime.Default.Register(stub); err != nil {
		t.Fatalf("Register(%s): %v", name, err)
	}

	root := t.TempDir()
	results, untiered, err := personas.InstallForRuntime(root, false, name)
	if err != nil {
		t.Fatalf("InstallForRuntime: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("no results returned")
	}

	cases := []struct {
		persona   string
		wantModel string
	}{
		{"koryph-architect", "stub-frontier-model"},   // tier: frontier
		{"koryph-implementer", "stub-standard-model"}, // tier: standard
		{"koryph-explorer", "stub-light-model"},       // tier: light
	}
	for _, tc := range cases {
		wantLines := strings.Split(mustReadEmbedded(t, tc.persona+".md"), "\n")
		gotLines := strings.Split(mustReadFile(t, filepath.Join(root, ".claude", "agents", tc.persona+".md")), "\n")
		if len(gotLines) != len(wantLines) {
			t.Fatalf("%s: line count changed (%d -> %d); rendering must not reformat the file",
				tc.persona, len(wantLines), len(gotLines))
		}
		sawModelLine := false
		for i := range wantLines {
			if strings.HasPrefix(strings.TrimSpace(wantLines[i]), "model:") {
				sawModelLine = true
				want := "model: " + tc.wantModel
				if gotLines[i] != want {
					t.Errorf("%s: model line = %q, want %q", tc.persona, gotLines[i], want)
				}
				continue
			}
			if gotLines[i] != wantLines[i] {
				t.Errorf("%s: line %d changed beyond the model: value:\n got  %q\n want %q",
					tc.persona, i, gotLines[i], wantLines[i])
			}
		}
		if !sawModelLine {
			t.Fatalf("%s: fixture has no model: line to assert against", tc.persona)
		}
	}

	// README.md carries no frontmatter at all, so it must be reported
	// untiered and installed unchanged.
	if !containsName(untiered, "README") {
		t.Errorf("untiered = %v, want it to include README (no frontmatter)", untiered)
	}
	readmeGot := mustReadFile(t, filepath.Join(root, ".claude", "agents", "README.md"))
	if readmeGot != mustReadEmbedded(t, "README.md") {
		t.Errorf("README.md was modified by rendering; want it left verbatim (no frontmatter)")
	}
}

// TestInstallForRuntimeSparseModelMapLeavesPersonaVerbatim proves a persona
// whose `tier:` the target runtime's ModelMap does not cover is installed
// UNCHANGED (still carrying claude's legacy model: pin) rather than having
// its model: value blanked or fabricated — runtime.ModelMap is documented as
// permissibly sparse.
func TestInstallForRuntimeSparseModelMapLeavesPersonaVerbatim(t *testing.T) {
	const name = "personas-test-stub-sparse"
	// Only "standard" is mapped; "frontier"/"light"-tiered personas must
	// fall through untouched.
	stub := runtimetest.Stub{StubName: name, Models: runtime.ModelMap{
		runtime.TierStandard: "stub-standard-model",
	}}
	if err := runtime.Default.Register(stub); err != nil {
		t.Fatalf("Register(%s): %v", name, err)
	}

	root := t.TempDir()
	_, untiered, err := personas.InstallForRuntime(root, false, name)
	if err != nil {
		t.Fatalf("InstallForRuntime: %v", err)
	}
	if !containsName(untiered, "koryph-architect") { // tier: frontier, unmapped
		t.Errorf("untiered = %v, want it to include koryph-architect (frontier unmapped)", untiered)
	}
	got := mustReadFile(t, filepath.Join(root, ".claude", "agents", "koryph-architect.md"))
	if got != mustReadEmbedded(t, "koryph-architect.md") {
		t.Errorf("koryph-architect.md rewritten despite an unmapped tier; want verbatim")
	}
}

// TestInstallForRuntimeUnregisteredRuntimeFailsClosed proves an unrecognized
// runtime name is a hard error — never a silent claude-shaped fallback —
// and that no files are written as a side effect of the failed attempt.
func TestInstallForRuntimeUnregisteredRuntimeFailsClosed(t *testing.T) {
	root := t.TempDir()
	results, untiered, err := personas.InstallForRuntime(root, false, "totally-unregistered-runtime")
	if err == nil {
		t.Fatalf("expected an error for an unregistered runtime, got nil")
	}
	if !strings.Contains(err.Error(), "unregistered-runtime") {
		t.Errorf("error = %q, want it to name the unknown runtime", err.Error())
	}
	if results != nil || untiered != nil {
		t.Errorf("results=%v untiered=%v, want both nil on a fail-closed error", results, untiered)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".claude", "agents")); !os.IsNotExist(statErr) {
		t.Errorf(".claude/agents was created despite the fail-closed error")
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func mustReadEmbedded(t *testing.T, name string) string {
	t.Helper()
	data, err := agents.FS.ReadFile(name)
	if err != nil {
		t.Fatalf("read embedded %s: %v", name, err)
	}
	return string(data)
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
