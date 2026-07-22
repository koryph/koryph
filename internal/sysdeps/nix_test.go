// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sysdeps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/execx"
)

// writeFlake writes content as flake.nix in a fresh temp dir and returns the
// dir (the "root" PlanFlakeBeads takes).
func writeFlake(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "flake.nix"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile flake.nix: %v", err)
	}
	return dir
}

// wantVerifyArgv mirrors flakeVerifyArgv's real (non-injectable) PATH check —
// PlanFlakeBeads's Verify field is fixed by the API to depend on the real
// execx.LookPath("direnv"), so tests assert against whatever is actually true
// of the machine running them rather than a stubbed value.
func wantVerifyArgv(root string) []string {
	if execx.LookPath("direnv") {
		return []string{"direnv", "exec", root, "bd", "version"}
	}
	return []string{"nix", "develop", "-c", "bd", "version"}
}

// --- input form: classic `inputs = { ... };` block + mkShell + @inputs ----

const classicInputsFixture = `{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs, ... }@inputs:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
    in {
      devShells.${system}.default = pkgs.mkShell {
        packages = [
          pkgs.hello
        ];
      };
    };
}
`

func TestPlanFlakeBeads_ClassicInputsAtInputsBinding(t *testing.T) {
	dir := writeFlake(t, classicInputsFixture)

	edit, err := PlanFlakeBeads(dir)
	if err != nil {
		t.Fatalf("PlanFlakeBeads: %v", err)
	}
	if edit == nil {
		t.Fatal("PlanFlakeBeads returned a nil edit for a flake.nix that exists")
	}
	if edit.AlreadyIntegrated {
		t.Fatal("AlreadyIntegrated must be false — no beads input existed")
	}
	if edit.Path != filepath.Join(dir, "flake.nix") {
		t.Errorf("Path = %q, want %s", edit.Path, filepath.Join(dir, "flake.nix"))
	}

	// The input is inserted as the block's new first sibling, matching the
	// existing entry's 4-space indentation.
	if !strings.Contains(edit.NewText, "  inputs = {\n    beads.url = \"github:gastownhall/beads\";\n    nixpkgs.url =") {
		t.Errorf("beads input not inserted with sibling indentation:\n%s", edit.NewText)
	}
	// An @inputs binding is present, so the outputs header itself is untouched...
	if !strings.Contains(edit.NewText, "outputs = { self, nixpkgs, ... }@inputs:") {
		t.Errorf("outputs header must stay untouched under an @inputs binding:\n%s", edit.NewText)
	}
	// ...and the package reference goes through `inputs.beads...`.
	if !strings.Contains(edit.NewText, "        packages = [\n          inputs.beads.packages.${system}.default\n          pkgs.hello\n") {
		t.Errorf("beads package not inserted via inputs.beads reference:\n%s", edit.NewText)
	}
	if edit.Diff == "" {
		t.Error("Diff must not be empty for a real edit")
	}
	if !strings.Contains(edit.Diff, "+ ") {
		t.Errorf("Diff must show at least one added line: %q", edit.Diff)
	}
	if want := wantVerifyArgv(dir); !reflect.DeepEqual(edit.Verify, want) {
		t.Errorf("Verify = %v, want %v", edit.Verify, want)
	}
}

// --- input form: top-level shorthand `inputs.foo.url = ...;` lines --------

const shorthandInputsFixture = `{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.flake-utils.url = "github:numtide/flake-utils";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
    in
    {
      devShells.${system}.default = pkgs.mkShell {
        buildInputs = [
          pkgs.hello
        ];
      };
    };
}
`

func TestPlanFlakeBeads_ShorthandInputsExplicitArgs(t *testing.T) {
	dir := writeFlake(t, shorthandInputsFixture)

	edit, err := PlanFlakeBeads(dir)
	if err != nil {
		t.Fatalf("PlanFlakeBeads: %v", err)
	}
	if edit == nil {
		t.Fatal("edit is nil")
	}

	// New shorthand line added beside the existing ones, same indentation.
	if !strings.Contains(edit.NewText,
		"  inputs.nixpkgs.url = \"github:NixOS/nixpkgs/nixos-unstable\";\n"+
			"  inputs.flake-utils.url = \"github:numtide/flake-utils\";\n"+
			"  inputs.beads.url = \"github:gastownhall/beads\";\n") {
		t.Errorf("beads shorthand input not appended beside the existing ones:\n%s", edit.NewText)
	}
	// No @inputs binding, no ellipsis: `beads` is added as a plain lambda arg.
	if !strings.Contains(edit.NewText, "outputs = { self, beads, nixpkgs }:") {
		t.Errorf("outputs arg set not updated with a bare `beads` arg:\n%s", edit.NewText)
	}
	// ...and the package reference is the bare (non-`inputs.`-qualified) form.
	if !strings.Contains(edit.NewText, "        buildInputs = [\n          beads.packages.${system}.default\n          pkgs.hello\n") {
		t.Errorf("beads package not inserted via the bare reference:\n%s", edit.NewText)
	}
}

// --- outputs arg form: ellipsis (`...`), no @inputs binding ----------------

const ellipsisArgsFixture = `{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs, ... }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
    in
    {
      devShells.${system}.default = pkgs.mkShell {
        packages = [
          pkgs.hello
        ];
      };
    };
}
`

func TestPlanFlakeBeads_EllipsisArgsAddsBeadsArg(t *testing.T) {
	dir := writeFlake(t, ellipsisArgsFixture)

	edit, err := PlanFlakeBeads(dir)
	if err != nil {
		t.Fatalf("PlanFlakeBeads: %v", err)
	}
	if edit == nil {
		t.Fatal("edit is nil")
	}

	// `beads` is inserted after the first arg, ellipsis preserved verbatim.
	if !strings.Contains(edit.NewText, "outputs = { self, beads, nixpkgs, ... }:") {
		t.Errorf("outputs arg set not updated to add `beads` ahead of the ellipsis:\n%s", edit.NewText)
	}
	if !strings.Contains(edit.NewText, "        packages = [\n          beads.packages.${system}.default\n          pkgs.hello\n") {
		t.Errorf("beads package not inserted via the bare reference:\n%s", edit.NewText)
	}
}

// --- already integrated: input + devShell package both already present ----

const alreadyIntegratedFixture = `{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    beads.url = "github:gastownhall/beads";
  };

  outputs = { self, nixpkgs, beads }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
    in
    {
      devShells.${system}.default = pkgs.mkShell {
        packages = [
          pkgs.hello
          beads.packages.${system}.default
        ];
      };
    };
}
`

func TestPlanFlakeBeads_AlreadyIntegrated(t *testing.T) {
	dir := writeFlake(t, alreadyIntegratedFixture)

	edit, err := PlanFlakeBeads(dir)
	if err != nil {
		t.Fatalf("PlanFlakeBeads: %v", err)
	}
	if edit == nil {
		t.Fatal("edit is nil")
	}
	if !edit.AlreadyIntegrated {
		t.Fatal("AlreadyIntegrated must be true — the input and package are both already wired in")
	}
	if edit.Diff != "" {
		t.Errorf("Diff must be empty when AlreadyIntegrated, got %q", edit.Diff)
	}
}

// --- no flake.nix at all: (nil, nil), not an error -------------------------

func TestPlanFlakeBeads_NoFlakeNix(t *testing.T) {
	dir := t.TempDir() // empty; no flake.nix written

	edit, err := PlanFlakeBeads(dir)
	if err != nil {
		t.Fatalf("PlanFlakeBeads on a repo with no flake.nix must not error, got: %v", err)
	}
	if edit != nil {
		t.Fatalf("edit = %+v, want nil", edit)
	}
}

// --- no recognizable devShell: hard error, never a guess-edit --------------

const noDevShellFixture = `{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    {
      packages.default = nixpkgs.legacyPackages.x86_64-linux.hello;
    };
}
`

func TestPlanFlakeBeads_NoDevShellErrors(t *testing.T) {
	dir := writeFlake(t, noDevShellFixture)

	edit, err := PlanFlakeBeads(dir)
	if err == nil {
		t.Fatal("expected an error — there is no mkShell devShell to insert the package into")
	}
	if edit != nil {
		t.Fatalf("edit = %+v, want nil alongside the error", edit)
	}
	if !strings.Contains(err.Error(), "mkShell") {
		t.Errorf("error should name the missing mkShell devShell: %v", err)
	}
}

// --- fixture modeled on koryph's own flake.nix -----------------------------
//
// koryph's real flake.nix (read at /Users/marshallmccain/src/github.com/koryph/koryph/flake.nix
// as of this bead) has:
//   - a classic `inputs = { nixpkgs.url = ...; };` block (no beads input yet)
//   - `outputs = { self, nixpkgs }:` — explicit args, no ellipsis, no @inputs
//     binding, so PlanFlakeBeads must add a bare `beads` arg and reference
//     `beads.packages.${system}.default` (not `inputs.beads...`)
//   - one `pkgs.mkShell { ... packages = [ ... ] ++ (with pkgs; [ ... ]); }`
//     devShell, whose FIRST `packages = [` is the one to insert into
//   - `system` bound as the `forAllSystems`/`devShells` lambda parameter, so
//     the `${system}` token-in-scope heuristic is satisfied truthfully (this
//     is exactly the flake-utils-style shape the design's §4.1 example
//     assumes)
//
// Read directly from disk (rather than embedded as a string literal) because
// the real file contains backtick characters in a comment ("go.mod's `go`
// directive"), which cannot appear inside a Go raw string literal; reading it
// also keeps this test honest as the file evolves instead of drifting from a
// frozen copy.
func TestPlanFlakeBeads_KoryphRepoFlake(t *testing.T) {
	src := filepath.Join("..", "..", "flake.nix")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("koryph's own flake.nix not found at %s (%v) — skipping", src, err)
	}

	dir := writeFlake(t, string(data))

	edit, err := PlanFlakeBeads(dir)
	if err != nil {
		t.Fatalf("PlanFlakeBeads(koryph's own flake.nix): %v", err)
	}
	if edit == nil {
		t.Fatal("edit is nil")
	}
	if edit.AlreadyIntegrated {
		t.Fatal("koryph's flake.nix has no beads input yet — AlreadyIntegrated must be false")
	}

	if !strings.Contains(edit.NewText,
		"  inputs = {\n    beads.url = \"github:gastownhall/beads\";\n"+
			"    # Determinate Systems' weekly nixpkgs carries the tool versions this\n") {
		t.Errorf("beads input not inserted ahead of the nixpkgs input, sibling-indented:\n%s", edit.NewText)
	}
	if !strings.Contains(edit.NewText, "outputs = { self, beads, nixpkgs }:") {
		t.Errorf("outputs arg set not updated with a bare `beads` arg:\n%s", edit.NewText)
	}
	if !strings.Contains(edit.NewText,
		"            packages = [\n              beads.packages.${system}.default\n"+
			"              # Go toolchain") {
		t.Errorf("beads package not inserted at the top of the devShell's packages list:\n%s", edit.NewText)
	}
}

// --- ApplyFlakeEdit ---------------------------------------------------------

// stubFlakeLock replaces the package-level flakeLock hook for the duration of
// the calling test, restoring the original on cleanup, so no test here ever
// shells out to a real nix binary.
func stubFlakeLock(t *testing.T, fn func(ctx context.Context, root string) error) *bool {
	t.Helper()
	called := false
	orig := flakeLock
	flakeLock = func(ctx context.Context, root string) error {
		called = true
		return fn(ctx, root)
	}
	t.Cleanup(func() { flakeLock = orig })
	return &called
}

func TestApplyFlakeEdit_WritesAndCallsLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flake.nix")
	edit := &FlakeEdit{Path: path, NewText: "{ edited = true; }\n"}

	var gotRoot string
	called := stubFlakeLock(t, func(_ context.Context, root string) error {
		gotRoot = root
		return nil
	})

	if err := ApplyFlakeEdit(context.Background(), dir, edit); err != nil {
		t.Fatalf("ApplyFlakeEdit: %v", err)
	}
	if !*called {
		t.Fatal("flakeLock was not invoked")
	}
	if gotRoot != dir {
		t.Fatalf("flakeLock root = %q, want %q", gotRoot, dir)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != edit.NewText {
		t.Fatalf("written content = %q, want %q", got, edit.NewText)
	}
}

func TestApplyFlakeEdit_LockFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flake.nix")
	edit := &FlakeEdit{Path: path, NewText: "{ edited = true; }\n"}

	stubFlakeLock(t, func(context.Context, string) error {
		return fmt.Errorf("nix flake lock: exit 1: some real nix output")
	})

	err := ApplyFlakeEdit(context.Background(), dir, edit)
	if err == nil {
		t.Fatal("expected an error from a failing flakeLock")
	}
	if !strings.Contains(err.Error(), "some real nix output") {
		t.Fatalf("error = %v, want it to surface the lock's combined output", err)
	}
	// The write must have already happened — only the post-write lock failed.
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("file was not written before the lock failure: %v", rerr)
	}
	if string(got) != edit.NewText {
		t.Fatalf("written content = %q, want %q", got, edit.NewText)
	}
}

func TestApplyFlakeEdit_AlreadyIntegratedNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flake.nix")
	edit := &FlakeEdit{Path: path, AlreadyIntegrated: true}

	called := stubFlakeLock(t, func(context.Context, string) error {
		return nil
	})

	if err := ApplyFlakeEdit(context.Background(), dir, edit); err != nil {
		t.Fatalf("ApplyFlakeEdit: %v", err)
	}
	if *called {
		t.Fatal("flakeLock must not run for an AlreadyIntegrated edit")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("an AlreadyIntegrated edit must not write a file")
	}
}

func TestApplyFlakeEdit_NilEditNoop(t *testing.T) {
	called := stubFlakeLock(t, func(context.Context, string) error {
		return nil
	})
	if err := ApplyFlakeEdit(context.Background(), t.TempDir(), nil); err != nil {
		t.Fatalf("ApplyFlakeEdit(nil): %v", err)
	}
	if *called {
		t.Fatal("flakeLock must not run for a nil edit")
	}
}
