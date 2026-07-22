// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/koryph/koryph/internal/registry"
)

// completeVerb is the hidden dispatcher verb the shell wrappers call. It stays
// out of usage() and the completion candidate lists.
const completeVerb = "__complete"

func init() {
	registerCmd(command{
		name:    "completion",
		summary: "print or install a shell completion script",
		run:     cmdCompletion,
		DocLinks: []string{
			"user-guide/installation.md",
		},
		subs: []command{
			{name: "bash", summary: "print the bash completion script", DocLinks: []string{"user-guide/installation.md"}},
			{name: "zsh", summary: "print the zsh completion script", DocLinks: []string{"user-guide/installation.md"}},
			{
				name:     "install",
				summary:  "install the completion script to the standard location",
				run:      cmdCompletionInstall,
				DocLinks: []string{"user-guide/installation.md"},
			},
		},
	})
}

// --- __complete -------------------------------------------------------------

// cmdComplete resolves shell-completion candidates for a partial command line
// and prints them newline-separated. The invocation, matching the wrapper
// scripts, is:
//
//	koryph __complete -- <cword> <word0> <word1> ...
//
// where <cword> is the 0-based index of the word under the cursor and the words
// are the full command line (word0 is the program name). It never mutates
// state, makes no network or bd calls, and prints nothing (exit 0) for
// positions it cannot complete.
func cmdComplete(args []string, stdout, _ io.Writer) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return 0
	}
	cword, err := strconv.Atoi(args[0])
	if err != nil {
		return 0
	}
	words := args[1:]

	current := ""
	if cword >= 0 && cword < len(words) {
		current = words[cword]
	}
	// Completed tokens before the cursor, excluding the program name (words[0]).
	end := cword
	if end > len(words) {
		end = len(words)
	}
	var line []string
	if end > 1 {
		line = words[1:end]
	}

	for _, c := range completeCandidates(line, current) {
		fmt.Fprintln(stdout, c)
	}
	return 0
}

// completeCandidates returns the sorted, prefix-filtered completion candidates
// for the completed tokens in line and the partial word current.
func completeCandidates(line []string, current string) []string {
	// A value-taking flag immediately before the cursor wins: complete its
	// (cheap, read-only) value set rather than a flag or subcommand. Only a
	// real flag token (leading "-") qualifies — a bare word like the "project"
	// subcommand must not be mistaken for the --project flag.
	if n := len(line); n > 0 && strings.HasPrefix(line[n-1], "-") {
		if vals := flagValues(line[n-1]); vals != nil {
			return filterPrefix(vals, current)
		}
	}

	if len(line) == 0 {
		return filterPrefix(topLevelNames(), current)
	}

	node, atParent := resolveNode(line)
	if node == nil {
		return nil
	}
	if strings.HasPrefix(current, "-") {
		return filterPrefix(flagNames(node), current)
	}
	if atParent && len(node.subs) > 0 {
		return filterPrefix(subNames(node), current)
	}
	return nil
}

// resolveNode walks line through the command tree and returns the deepest
// matched command plus atParent — true when that command still expects a
// subcommand (it has subs and no sub token followed it).
func resolveNode(line []string) (*command, bool) {
	c := lookupCommand(line[0])
	if c == nil {
		return nil, false
	}
	i := 1
	for i < len(line) {
		next := findSub(c, line[i])
		if next == nil {
			break
		}
		c = next
		i++
	}
	atParent := len(c.subs) > 0 && i == len(line)
	return c, atParent
}

// topLevelNames returns every visible top-level command name (hidden aliases
// and internal verbs like __docgen/__complete are excluded from completion).
func topLevelNames() []string {
	names := make([]string, 0, len(commandRegistry))
	for i := range commandRegistry {
		if visibleTopLevel(&commandRegistry[i]) {
			names = append(names, commandRegistry[i].name)
		}
	}
	return names
}

// subNames returns c's visible subcommand names — hidden two-word aliases
// (e.g. "sign blob" once "sign" flattened to a single verb, koryph-b8g #24)
// are still dispatchable via findSub but are not offered as completions.
func subNames(c *command) []string {
	names := make([]string, 0, len(c.subs))
	for _, s := range c.subs {
		if s.hidden {
			continue
		}
		names = append(names, s.name)
	}
	return names
}

// flagNames returns the "--"-prefixed flag names of a command, enumerated from
// the real flag.FlagSet the command builds (captured without side effects), so
// completion never hand-duplicates any flag list. Flags marked via hideFlag
// (back-compat spelling aliases) are excluded so completion only ever offers
// the current spelling.
func flagNames(c *command) []string {
	if c.run == nil {
		return nil
	}
	fs := captureFlags(c.run)
	if fs == nil {
		return nil
	}
	var names []string
	fs.VisitAll(func(f *flag.Flag) {
		if !hiddenFlags[f] {
			names = append(names, "--"+f.Name)
		}
	})
	return names
}

// flagValues returns the completion values for a value-taking flag, or nil when
// the flag takes no completable value. All sources are cheap and read-only.
func flagValues(flagTok string) []string {
	switch strings.TrimLeft(flagTok, "-") {
	case "project":
		return projectIDs()
	case "model", "default-model", "fallback-model":
		return []string{"fable", "haiku", "opus", "sonnet"}
	case "shell":
		return []string{"bash", "zsh"}
	}
	return nil
}

// projectIDs returns the registered project ids, read-only (no Init, no
// mutation). Any error yields no candidates rather than a completion failure.
func projectIDs() []string {
	recs, err := registry.NewStore().List()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ProjectID)
	}
	return ids
}

// filterPrefix returns the members of cands that have the given prefix, sorted
// and de-duplicated.
func filterPrefix(cands []string, prefix string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if strings.HasPrefix(c, prefix) && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// --- completion (print / install) ------------------------------------------

// bashCompletionScript is the static bash wrapper. It delegates every request
// to `koryph __complete`, so it never changes across releases even as commands
// and flags evolve — the binary answers dynamically.
const bashCompletionScript = `# koryph bash completion
#
# Source it for the current shell:
#   source <(koryph completion bash)
# or install it permanently:
#   koryph completion install --shell bash
#
# It delegates every request to 'koryph __complete', so this script stays
# stable across releases while the binary answers dynamically.
_koryph_complete() {
    local IFS=$'\n'
    COMPREPLY=( $(koryph __complete -- "$COMP_CWORD" "${COMP_WORDS[@]}") )
}
complete -F _koryph_complete koryph
`

// zshCompletionScript is the static zsh wrapper. The leading #compdef tag binds
// it when the file is autoloaded from fpath as _koryph; the trailing compdef
// call binds it when the script is sourced directly.
const zshCompletionScript = `#compdef koryph
# koryph zsh completion
#
# Source it for the current shell:
#   source <(koryph completion zsh)
# or install it permanently:
#   koryph completion install --shell zsh
#
# It delegates every request to 'koryph __complete', so this script stays
# stable across releases while the binary answers dynamically.
_koryph() {
    local -a candidates
    local cword=$(( CURRENT - 1 ))
    candidates=( ${(f)"$(koryph __complete -- $cword $words)"} )
    compadd -- $candidates
}
compdef _koryph koryph 2>/dev/null
`

// completionScript returns the wrapper script for a shell.
func completionScript(shell string) (string, bool) {
	switch shell {
	case "bash":
		return bashCompletionScript, true
	case "zsh":
		return zshCompletionScript, true
	}
	return "", false
}

// cmdCompletion prints or installs a shell completion script.
func cmdCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "completion", "print or install a shell completion script", []subVerb{
			{"bash", "print the bash completion script to stdout"},
			{"zsh", "print the zsh completion script to stdout"},
			{"install [--shell bash|zsh]", "install the script to the standard user location (detects $SHELL)"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "bash", "zsh":
		if len(rest) > 0 && !isHelpArg(rest[0]) {
			return usageErr(stderr, fmt.Sprintf("completion %s: unexpected argument %q", sub, rest[0]))
		}
		script, _ := completionScript(sub)
		fmt.Fprint(stdout, script)
		return 0
	case "install":
		return cmdCompletionInstall(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown completion subcommand %q (want bash|zsh|install)", sub))
	}
}

// cmdCompletionInstall writes the completion script to the standard user-level
// location for the shell and prints the path plus any activation step. It is
// idempotent (overwrites the same path) and never edits shell rc files.
func cmdCompletionInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("completion install", stderr)
	shell := fs.String("shell", "", "target shell: bash|zsh (default: detect from $SHELL)")
	setUsage(fs, stdout, "install the completion script to the standard user location",
		"[--shell bash|zsh]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	sh := *shell
	if sh == "" {
		sh = detectShell(os.Getenv("SHELL"))
	}
	script, ok := completionScript(sh)
	if !ok {
		return usageErr(stderr, fmt.Sprintf("completion install: unsupported shell %q (want bash or zsh)", sh))
	}

	path, err := completionInstallPath(sh)
	if err != nil {
		return fail(stderr, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fail(stderr, err)
	}
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		return fail(stderr, err)
	}

	fmt.Fprintf(stdout, "installed %s completion: %s\n", sh, path)
	switch sh {
	case "bash":
		fmt.Fprintln(stdout, "Activation: ensure bash-completion is enabled (it loads this directory on new shells).")
		fmt.Fprintln(stdout, "Start a new shell, or run: source "+path)
	case "zsh":
		dir := filepath.Dir(path)
		fmt.Fprintln(stdout, "Activation: if this directory is not already on your fpath, add to ~/.zshrc:")
		fmt.Fprintf(stdout, "  fpath=(%s $fpath)\n", dir)
		fmt.Fprintln(stdout, "  autoload -U compinit && compinit")
		fmt.Fprintln(stdout, "Then start a new shell.")
	}
	return 0
}

// detectShell maps a $SHELL path to a supported shell name, defaulting to bash.
func detectShell(shellPath string) string {
	base := filepath.Base(strings.TrimSpace(shellPath))
	switch {
	case strings.Contains(base, "zsh"):
		return "zsh"
	case strings.Contains(base, "bash"):
		return "bash"
	default:
		return "bash"
	}
}

// completionInstallPath resolves the standard user-level install path for a
// shell's completion script:
//   - bash: ${XDG_DATA_HOME:-~/.local/share}/bash-completion/completions/koryph
//   - zsh:  ~/.koryph/completions/_koryph
func completionInstallPath(shell string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch shell {
	case "bash":
		dataHome := os.Getenv("XDG_DATA_HOME")
		if dataHome == "" {
			dataHome = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(dataHome, "bash-completion", "completions", "koryph"), nil
	case "zsh":
		return filepath.Join(home, ".koryph", "completions", "_koryph"), nil
	default:
		return "", fmt.Errorf("unsupported shell %q", shell)
	}
}
