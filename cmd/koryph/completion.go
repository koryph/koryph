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

// command is one node in the data-driven command tree. It is the single source
// of truth for the mux (run() dispatches through lookupCommand), for usage
// discovery, and for completion. A parent carries subs; a leaf carries none.
// run may be nil for a sub that only names a value (e.g. `completion bash`),
// in which case it contributes no flags to completion.
type command struct {
	name    string
	summary string
	run     func([]string, io.Writer, io.Writer) int
	subs    []command
}

// commandTable is the ordered list of top-level koryph commands. The mux, the
// completion engine, and future usage rendering all derive from it, so a new
// command is wired in exactly one place. The hidden __complete verb is handled
// directly in run() and intentionally absent here.
var commandTable = []command{
	{name: "init", summary: "create ~/.koryph, verify tools on PATH, print next steps", run: cmdInit},
	{name: "project", summary: "onboard and manage registered projects", run: cmdProject, subs: []command{
		{name: "add", summary: "register a project", run: cmdProjectAdd},
		{name: "list", summary: "list managed projects", run: cmdProjectList},
		{name: "show", summary: "print one project record as JSON", run: cmdProjectShow},
		{name: "set-account", summary: "change a project's account (audited)", run: cmdProjectSetAccount},
	}},
	{name: "onboard", summary: "read-only inventory of a project", run: cmdOnboard},
	{name: "validate", summary: "run the pre-dispatch gate", run: cmdValidate},
	{name: "intake", summary: "poll labeled GitHub issues into planning beads", run: cmdIntake},
	{name: "run", summary: "execute one engine run over a project", run: cmdRun},
	{name: "board", summary: "one-line-per-project run overview", run: cmdBoard},
	{name: "roster", summary: "per-bead titled roster grouped by lifecycle", run: cmdRoster},
	{name: "status", summary: "latest-run per-slot detail", run: cmdStatus},
	{name: "tail", summary: "tail a phase's session.log + stderr.log", run: cmdTail},
	{name: "nudge", summary: "append an operator note to a phase INBOX", run: cmdNudge},
	{name: "stop", summary: "stop an agent (or every agent with --all)", run: cmdStop},
	{name: "drain", summary: "gracefully wind down a run: finish active slots, dispatch nothing new", run: cmdDrain},
	{name: "resize", summary: "live width override for a running loop", run: cmdResize},
	{name: "merge", summary: "land a finished agent branch", run: cmdMerge},
	{name: "land", summary: "land an engine-opened PR fast-forward-only", run: cmdLand},
	{name: "review-pr", summary: "analyze another author's PR", run: cmdReviewPR},
	{name: "pr-sync", summary: "reconcile pr-opened beads against live PR state", run: cmdPRSync},
	{name: "bot", summary: "provision and manage koryph GitHub App bots", run: cmdBot, subs: []command{
		{name: "create", summary: "create a GitHub App via the manifest flow (one browser click)", run: cmdBotCreate},
		{name: "install", summary: "print/open the installation page for a provisioned bot", run: cmdBotInstall},
		{name: "list", summary: "list provisioned bots in ~/.koryph/bots/", run: cmdBotList},
	}},
	{name: "release", summary: "configure and operate the project release pipeline", run: cmdRelease, subs: []command{
		{name: "setup", summary: "render and install release workflow + release-please config", run: cmdReleaseSetup},
		{name: "kick", summary: "close+reopen the Release PR so checks fire under your gh auth", run: cmdReleaseKick},
	}},
	{name: "signing", summary: "configure and operate vault-backed commit signing", run: cmdSigning, subs: []command{
		{name: "setup", summary: "write the signing policy into the adapter", run: cmdSigningSetup},
		{name: "enable", summary: "load the key + apply repo git config", run: cmdSigningEnable},
		{name: "status", summary: "mode/provider/agent-ready summary", run: cmdSigningStatus},
		{name: "verify", summary: "verify branch commit signatures", run: cmdSigningVerify},
	}},
	{name: "sign", summary: "cosign sign-blob an artifact", run: cmdSign, subs: []command{
		{name: "blob", summary: "sign a file via the vault key", run: cmdSignBlob},
	}},
	{name: "quota", summary: "per-account governor snapshot", run: cmdQuota, subs: []command{
		{name: "calibrate", summary: "calibrate a governor ceiling", run: cmdQuotaCalibrate},
	}},
	{name: "batch", summary: "submit a Message Batch (explicit per-token spend)", run: cmdBatch, subs: []command{
		{name: "run", summary: "submit a batch from a JSONL file", run: cmdBatchRun},
	}},
	{name: "metrics", summary: "burn + reliability rollup across projects", run: cmdMetrics},
	{name: "agents", summary: "install fallback personas", run: cmdAgents, subs: []command{
		{name: "install", summary: "install personas into <root>/.claude/agents", run: cmdAgentsInstall},
	}},
	{name: "commands", summary: "install koryph-* Claude slash commands", run: cmdCommands, subs: []command{
		{name: "install", summary: "install commands into <root>/.claude/commands", run: cmdCommandsInstall},
	}},
	{name: "rules", summary: "install hook scripts + merge wiring", run: cmdRules, subs: []command{
		{name: "install", summary: "install hooks into <root>/.claude/settings.json", run: cmdRulesInstall},
	}},
	{name: "repo", summary: "check or apply .github IaC (rulesets, repo settings)", run: cmdRepo, subs: []command{
		{name: "check", summary: "diff live GitHub settings/rulesets against .github IaC (exit 1 on drift)", run: cmdRepoCheck},
		{name: "apply", summary: "apply .github IaC (rulesets, repo settings) to the live repo", run: cmdRepoApply},
	}},
	{name: "posture", summary: "apply a named desired-state profile to a GitHub repo", run: cmdPosture, subs: []command{
		{name: "list", summary: "list built-in and user-defined profiles", run: cmdPostureList},
		{name: "check", summary: "diff live GitHub state against a profile (exit 1 on drift)", run: cmdPostureCheck},
		{name: "diff", summary: "show drift between live state and a profile (always exit 0)", run: cmdPostureDiff},
		{name: "apply", summary: "show diff then apply a profile to the live GitHub repo", run: cmdPostureApply},
	}},
	{name: "governor", summary: "inspect and set the machine-wide concurrency cap", run: cmdGovernor, subs: []command{
		{name: "show", summary: "show the cap, leases, and demand"},
		{name: "set", summary: "set the machine-wide cap", run: cmdGovernorSet},
	}},
	{name: "doctor", summary: "health check: layout, binaries, registry, governor", run: cmdDoctor},
	{name: "plan", summary: "plan and analyze the project bead corpus", run: cmdPlan, subs: []command{
		{name: "audit", summary: "read-only corpus conflict analysis", run: cmdPlanAudit},
	}},
	{name: "version", summary: "print the engine version", run: cmdVersion},
	{name: "completion", summary: "print or install a shell completion script", run: cmdCompletion, subs: []command{
		{name: "bash", summary: "print the bash completion script"},
		{name: "zsh", summary: "print the zsh completion script"},
		{name: "install", summary: "install the completion script to the standard location", run: cmdCompletionInstall},
	}},
}

// lookupCommand returns the top-level command node with the given name, or nil.
func lookupCommand(name string) *command {
	for i := range commandTable {
		if commandTable[i].name == name {
			return &commandTable[i]
		}
	}
	return nil
}

// findSub returns c's subcommand with the given name, or nil.
func findSub(c *command, name string) *command {
	for i := range c.subs {
		if c.subs[i].name == name {
			return &c.subs[i]
		}
	}
	return nil
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

// topLevelNames returns every top-level command name.
func topLevelNames() []string {
	names := make([]string, 0, len(commandTable))
	for _, c := range commandTable {
		names = append(names, c.name)
	}
	return names
}

// subNames returns c's subcommand names.
func subNames(c *command) []string {
	names := make([]string, 0, len(c.subs))
	for _, s := range c.subs {
		names = append(names, s.name)
	}
	return names
}

// flagNames returns the "--"-prefixed flag names of a command, enumerated from
// the real flag.FlagSet the command builds (captured without side effects), so
// completion never hand-duplicates any flag list.
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
		names = append(names, "--"+f.Name)
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
