// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/registry"
)

// complete runs `koryph __complete -- <cword> <words...>` and returns the
// candidate lines (split, trailing blank trimmed).
func complete(cword string, words ...string) []string {
	args := append([]string{completeVerb, "--", cword}, words...)
	_, out, _ := runCmd(args...)
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// contains reports whether want is in got.
func containsStr(got []string, want string) bool {
	for _, g := range got {
		if g == want {
			return true
		}
	}
	return false
}

func TestCompleteTopLevel(t *testing.T) {
	// Empty current word at position 1: every top-level command is a candidate.
	got := complete("1", "koryph", "")
	for _, want := range []string{"run", "project", "completion", "doctor", "quota"} {
		if !containsStr(got, want) {
			t.Errorf("top-level completion missing %q; got %v", want, got)
		}
	}
	// The hidden __complete verb must never be suggested.
	if containsStr(got, completeVerb) {
		t.Errorf("hidden verb %q leaked into completion: %v", completeVerb, got)
	}
}

func TestCompleteTopLevelPrefix(t *testing.T) {
	got := complete("1", "koryph", "pr")
	if !containsStr(got, "pr-sync") || !containsStr(got, "project") {
		t.Errorf("prefix 'pr' should match pr-sync and project; got %v", got)
	}
	if containsStr(got, "run") {
		t.Errorf("prefix 'pr' should not match run; got %v", got)
	}
}

func TestCompletePerCommandFlags(t *testing.T) {
	// `koryph run --<TAB>` enumerates run's real flags, captured from its
	// FlagSet (never hand-duplicated).
	got := complete("2", "koryph", "run", "--")
	for _, want := range []string{"--project", "--once", "--auto-merge", "--dispatch-mode", "--runtime-only", "--runtime-equivalent"} {
		if !containsStr(got, want) {
			t.Errorf("run flag completion missing %q; got %v", want, got)
		}
	}
}

func TestCompleteSubcommandFlags(t *testing.T) {
	// A parent's leaf flags are reachable: `koryph project add --<TAB>`.
	got := complete("3", "koryph", "project", "add", "--")
	for _, want := range []string{"--account", "--identity", "--force"} {
		if !containsStr(got, want) {
			t.Errorf("project add flag completion missing %q; got %v", want, got)
		}
	}
}

func TestCompleteSubcommands(t *testing.T) {
	got := complete("2", "koryph", "project", "")
	for _, want := range []string{"add", "list", "show", "set-account", "set-runtime-account"} {
		if !containsStr(got, want) {
			t.Errorf("project subcommand completion missing %q; got %v", want, got)
		}
	}
}

func TestCompleteStaticValueFlags(t *testing.T) {
	model := complete("3", "koryph", "run", "--default-model", "")
	for _, want := range []string{"haiku", "sonnet", "opus", "fable"} {
		if !containsStr(model, want) {
			t.Errorf("--default-model completion missing %q; got %v", want, model)
		}
	}
	shell := complete("4", "koryph", "completion", "install", "--shell", "")
	if !containsStr(shell, "bash") || !containsStr(shell, "zsh") {
		t.Errorf("--shell completion should offer bash and zsh; got %v", shell)
	}
	shellZ := complete("4", "koryph", "completion", "install", "--shell", "z")
	if len(shellZ) != 1 || shellZ[0] != "zsh" {
		t.Errorf("--shell prefix 'z' should yield only zsh; got %v", shellZ)
	}
}

func TestCompleteProjectValuesFromRegistry(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	store := registry.NewStore()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha", "beta"} {
		rec := &registry.Record{
			ProjectID:        id,
			Name:             id,
			Root:             gitRepo(t),
			AccountProfile:   "personal",
			ExpectedIdentity: "me@example.com",
		}
		if err := store.Add(ctx, rec); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	got := complete("3", "koryph", "run", "--project", "")
	if !containsStr(got, "alpha") || !containsStr(got, "beta") {
		t.Errorf("--project completion should list registered ids; got %v", got)
	}
	// Prefix filter narrows to one id.
	only := complete("3", "koryph", "run", "--project", "al")
	if len(only) != 1 || only[0] != "alpha" {
		t.Errorf("--project prefix 'al' should yield only alpha; got %v", only)
	}
}

func TestCompleteProjectValuesEmptyRegistry(t *testing.T) {
	isolate(t) // fresh KORYPH_HOME with no registry.d — must not error, just no candidates
	got := complete("3", "koryph", "status", "--project", "")
	if len(got) != 0 {
		t.Errorf("empty registry should yield no --project candidates; got %v", got)
	}
}

func TestCompleteUnknownPositionEmpty(t *testing.T) {
	// A leaf's positional argument has no completion source: exit 0, no output.
	code, out, _ := runCmd(completeVerb, "--", "2", "koryph", "run", "")
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("unknown position should print nothing; got %q", out)
	}
	// A wholly unknown command completes to nothing.
	if got := complete("2", "koryph", "frobnicate", "--"); len(got) != 0 {
		t.Errorf("unknown command flags should be empty; got %v", got)
	}
}

func TestCompletionScriptGolden(t *testing.T) {
	code, bash, _ := runCmd("completion", "bash")
	if code != 0 {
		t.Fatalf("completion bash code = %d", code)
	}
	if bash != bashCompletionScript {
		t.Errorf("completion bash output drifted from bashCompletionScript")
	}
	for _, want := range []string{"koryph __complete", "complete -F _koryph_complete koryph", "source <(koryph completion bash)"} {
		if !strings.Contains(bash, want) {
			t.Errorf("bash script missing %q", want)
		}
	}

	code, zsh, _ := runCmd("completion", "zsh")
	if code != 0 {
		t.Fatalf("completion zsh code = %d", code)
	}
	if zsh != zshCompletionScript {
		t.Errorf("completion zsh output drifted from zshCompletionScript")
	}
	for _, want := range []string{"#compdef koryph", "koryph __complete", "compadd"} {
		if !strings.Contains(zsh, want) {
			t.Errorf("zsh script missing %q", want)
		}
	}
}

func TestCompletionInstallBash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")

	code, out, errb := runCmd("completion", "install", "--shell", "bash")
	if code != 0 {
		t.Fatalf("install bash code = %d; stderr=%s", code, errb)
	}
	want := filepath.Join(home, ".local", "share", "bash-completion", "completions", "koryph")
	if !strings.Contains(out, want) {
		t.Errorf("install output should name the path %q; got %s", want, out)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("script not written: %v", err)
	}
	if string(data) != bashCompletionScript {
		t.Errorf("installed bash script content mismatch")
	}
	// Idempotent: a second install overwrites the same path without error.
	if code, _, _ := runCmd("completion", "install", "--shell", "bash"); code != 0 {
		t.Errorf("second install code = %d, want 0", code)
	}
}

func TestCompletionInstallBashXDGOverride(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", xdg)

	code, _, errb := runCmd("completion", "install", "--shell", "bash")
	if code != 0 {
		t.Fatalf("install code = %d; stderr=%s", code, errb)
	}
	want := filepath.Join(xdg, "bash-completion", "completions", "koryph")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("XDG_DATA_HOME override not honored; expected %q: %v", want, err)
	}
}

func TestCompletionInstallZsh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	code, out, errb := runCmd("completion", "install", "--shell", "zsh")
	if code != 0 {
		t.Fatalf("install zsh code = %d; stderr=%s", code, errb)
	}
	want := filepath.Join(home, ".koryph", "completions", "_koryph")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("zsh script not written to %q: %v", want, err)
	}
	// The activation snippet (fpath + compinit) must be printed.
	for _, snip := range []string{"fpath=", "compinit"} {
		if !strings.Contains(out, snip) {
			t.Errorf("zsh install should print activation snippet %q; got %s", snip, out)
		}
	}
}

func TestCompletionInstallDetectsShell(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/usr/bin/zsh")

	code, _, errb := runCmd("completion", "install")
	if code != 0 {
		t.Fatalf("install (detect) code = %d; stderr=%s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(home, ".koryph", "completions", "_koryph")); err != nil {
		t.Errorf("detect-from-$SHELL should have installed the zsh script: %v", err)
	}
}

func TestDetectShell(t *testing.T) {
	cases := map[string]string{
		"/bin/bash":             "bash",
		"/usr/bin/zsh":          "zsh",
		"/opt/homebrew/bin/zsh": "zsh",
		"":                      "bash", // unknown → default bash
		"/bin/fish":             "bash",
	}
	for in, want := range cases {
		if got := detectShell(in); got != want {
			t.Errorf("detectShell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompletionUnknownShell(t *testing.T) {
	if code, _, _ := runCmd("completion", "install", "--shell", "fish"); code == 0 {
		t.Errorf("unsupported shell should be a usage error, got exit 0")
	}
	if code, _, _ := runCmd("completion", "nonsense"); code == 0 {
		t.Errorf("unknown completion subcommand should be a usage error, got exit 0")
	}
}

func TestCompletionVerbInUsage(t *testing.T) {
	_, out, _ := runCmd("help")
	if !strings.Contains(out, "completion") {
		t.Errorf("global usage missing 'completion' verb:\n%s", out)
	}
	// The hidden __complete verb must stay out of usage.
	if strings.Contains(out, completeVerb) {
		t.Errorf("usage leaked the hidden %q verb:\n%s", completeVerb, out)
	}
}

func TestCompletionParentHelp(t *testing.T) {
	for _, tok := range []string{"", "-h", "--help", "help"} {
		var args []string
		if tok == "" {
			args = []string{"completion"}
		} else {
			args = []string{"completion", tok}
		}
		code, out, _ := runCmd(args...)
		if code != 0 {
			t.Errorf("completion %q: code = %d, want 0", tok, code)
		}
		if !strings.Contains(out, "SUBCOMMANDS") || !strings.Contains(out, "install") {
			t.Errorf("completion %q help missing listing:\n%s", tok, out)
		}
	}
}
