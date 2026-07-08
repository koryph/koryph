// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Command koryph is the central multi-project koryph CLI: onboard and
// validate projects, drive the wave engine, and observe/operate live runs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the command mux. It returns the process exit code so main() is a thin
// os.Exit wrapper and every command is unit-testable via (args, stdout, stderr).
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return engine.ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help":
		usage(stdout)
		return 0
	case "help":
		// `koryph help` prints the global usage; `koryph help <cmd> [sub]`
		// routes to that command's own -h so a user never has to remember the
		// flag form.
		if len(rest) == 0 {
			usage(stdout)
			return 0
		}
		if rest[0] == "help" || isHelpArg(rest[0]) {
			usage(stdout)
			return 0
		}
		return run(append(rest, "-h"), stdout, stderr)
	case completeVerb:
		// Hidden: the shell-completion resolver. Never mutates state.
		return cmdComplete(rest, stdout, stderr)
	}
	if c := lookupCommand(cmd); c != nil {
		return c.run(rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "koryph: unknown command %q\n\n", cmd)
	usage(stderr)
	return engine.ExitUsage
}

// usage prints the global command listing.
func usage(w io.Writer) {
	fmt.Fprint(w, `koryph — central multi-project koryph

USAGE
  koryph <command> [flags]

New here? Run the top four in order: init -> project add -> validate -> run.

GETTING STARTED
  init                  create ~/.koryph, verify tools on PATH, print next steps (idempotent)
  project add <root> --account <personal|work> --identity <email> [--config-dir DIR] [--id slug] [--name N] [--branch B] [--force]
                        register a project (inspect + register + scaffold adapter + install agents, commands & rules)
  validate [<project-id>|--project ID]
                        run the pre-dispatch gate; promotes registered->migrated on green
  run [--project ID] [--once] [--max N] [--parent EPIC] [--only BEAD] [--budget USD]
      [--default-model M] [--auto-merge] [--direct] [--dry-run] [--resume] [--review]
      [--allow-api-spend] [--allow-unvalidated] [--manual]
                        execute one engine run over a project (--only dispatches a
                        single bead; --budget caps cumulative run cost; --direct
                        skips PRs and merges straight to the default branch)

OPERATE
  intake [--project ID] [--label triage] [--limit 20] [--dry-run] [--comment]
                        poll a project's labeled GitHub issues into no-dispatch planning beads
  nudge [--project ID] <phase-id> "text"
                        append an operator note to the phase INBOX (+ bd comment)
  stop [--project ID] <phase-id> [--force] | stop --all [--force]
                        stop an agent (or every agent, --all) — SIGTERM, or
                        SIGKILL with --force (uncommitted work is lost)
  drain [--project ID] | --all
                        graceful loop wind-down: stop new dispatch, let active
                        slots finish, exit drained (operator-drain); consumes
                        its own one-shot request so the next run starts clean
  resize ([--project ID] | --all) (--max N [--force] | --clear)
                        live wave-width override, re-read by the loop at every
                        boundary (no restart); clamped to max_concurrent_slots
                        unless --force; --clear removes the override
  merge [--project ID] <branch> [--push] [--squash] [--keep-worktree] [--close-bead BEAD --reason R]
                        land a finished agent branch on the default branch
  land [--project ID] <bead> [--method ff|squash] [--reason R]
                        land an engine-opened PR (a pr-opened bead) fast-forward-only,
                        preserving signed SHAs; closes the bead on success
  review-pr [--project ID] <pr> [--approve|--comment|--comment-on path:line:msg|--resume|--close] [--body B]
  review-pr [--project ID] --all
                        analyze another author's PR (or every open PR with --all) using
                        koryph's reviewer (prints findings, never approves). --comment posts
                        findings as inline comments; --comment-on adds your own line
                        comments; --resume replays a saved analysis after an IDE handoff;
                        --approve/--close register your approval or close the PR
  pr-sync [--project ID]  reconcile pr-opened beads against live PR state: a PR merged or
                        closed by any means marks its slot merged/blocked (nothing stranded)
  bot create [--name N] [--org ORG] [--public] [--headless]
                        create a GitHub App via the Manifest flow (one browser click);
                        persists {name, app_id, slug, owner, public, pem} to
                        ~/.koryph/bots/<name>.json (0600); permissions: contents:write +
                        pull_requests:write only (no org permissions → guest-org repo-admin
                        installs work); no webhook; --public for guest-org scenario
  bot install --name N  print/open https://github.com/apps/<slug>/installations/new;
                        explains private/public/org install scenarios and approval-request
                        behaviour when org policy restricts third-party app installs
  bot attach --name N --repo OWNER/REPO [--org-secrets]
                        wire a repo to a bot (idempotent): mint app JWT, resolve installation,
                        add repo, set RELEASE_BOT_APP_ID/PRIVATE_KEY secrets (per-repo by
                        default; --org-secrets sets org-level selected-repo secrets), enable
                        Actions can_approve_pull_request_reviews toggle
  bot list [--check]    list provisioned bots in ~/.koryph/bots/; --check adds offline PEM
                        validity check per bot (full identity check: use 'koryph bot check')
  bot check --name N [--repo OWNER/REPO]
                        validator chain with precise remediation per failure: JWT valid +
                        app_id match, installation exists/covers repo, secrets present
                        (best-effort), toggle on, caller workflow present; exit 0/1/2

  signing setup [--project ID] --provider P --key-ref REF --identity EMAIL [--mode ssh|gitsign]
      [--public-key "ssh-ed25519 ..."] [--artifacts]
                        write the vault-backed signing policy into the project adapter
                        (protonpass + no --public-key: auto-discover via the agent)
  signing enable [--project ID]
                        load the key into the SSH agent + apply repo git config
  signing verify [--project ID] --branch BR
                        verify branch commit signatures against the default branch (exit 1 on any bad)
  sign blob [--project ID] <path>
                        cosign sign-blob an artifact via the vault key (writes <path>.sig)
  ci setup [--project ID] [--force]
                        render and install the koryph gate pipeline
                        (.github/workflows/koryph-gate.yml) into the project from its gate
                        commands; re-run to update after changing koryph.project.json
  release setup [--project ID] [--mode goreleaser|commands] [--version V]
                        render and install the caller release workflow, release-please-config.json,
                        and .release-please-manifest.json into the project; --mode selects the
                        build toolchain (goreleaser = mode A, commands = mode B) when the
                        project has no release block; prints remaining HUMAN steps + current rung
  release kick --repo OWNER/REPO [--pr N] [--wait [--wait-timeout D]]
                        close+reopen the open Release PR so GitHub fires checks under your real
                        gh auth token (bot-less rung-2 per-release step); auto-detects by the
                        "autorelease: pending" label, or use --pr to name one explicitly;
                        --wait polls until all checks conclude

OBSERVE
  board [--json]        one-line-per-project run overview
  roster [--project ID] [--run ID] [--json]
                        per-bead titled roster grouped by lifecycle: MERGED /
                        RUNNING / QUEUED / DEFERRED (defaults to latest run)
  status [--project ID] [--json]
                        latest-run per-slot detail
  tail [--project ID] <phase-id> [-n 40] [--follow]
                        tail a phase's session.log + stderr.log; --follow streams
                        new lines and surfaces INBOX nudges live (Ctrl-C to stop)
  doctor [--project ID] [--json] [--fix] [--force]
                        health check: layout, binaries, registry, governor, zombie leases,
                        stale demand, quota calibration, vault providers, asset drift;
                        --project scopes the check to one registered project (adds asset-drift
                        and stalled-run checks); --fix installs missing assets (project mode)
                        or removes zombies/stale-demand (global mode); --force (with --fix
                        --project) also overwrites stale asset files; exits 0/1/2 (ok/warn/err)
  plan audit [--project ID] [--json]
                        read-only corpus conflict analysis: footprint gaps, non-dispatchable
                        beads, dependency-unordered conflicting pairs, achievable parallel
                        width; --json for machine-readable output (koryph-replan input)
  signing status [--project ID]
                        mode/provider/agent-ready/repo-config/allowed_signers summary
  governor [show]       show the machine-wide concurrency cap, active leases, and demand
  quota [--account A] [--json]
                        per-account governor snapshot (ccusage probe may take up to 40 s)
  metrics [--project ID] [--json]
                        burn + reliability rollup across projects

REPO IaC  (desired-state files: .github/rulesets/*.json, .github/repo-settings.json)
  repo describe [--repo owner/name]
                        explain every setting in .github IaC and why (with --repo: shows live value)
  repo check [--repo owner/name]
                        diff live GitHub settings/rulesets against .github IaC (exit 1 on drift)
  repo apply [--repo owner/name]
                        apply .github IaC (rulesets, repo settings) to the live repo

POSTURE  (named desired-state profiles — built-in or ~/.koryph/postures/<name>)
  posture list          list built-in and user-defined profiles
  posture describe <profile> [--repo owner/name] [--param k=v]...
                        explain every setting a profile enforces and why (with --repo: shows live value)
  posture check <profile> [--repo owner/name] [--param k=v]...
                        diff live GitHub state against profile (exit 1 on drift)
  posture diff <profile> [--repo owner/name] [--param k=v]...
                        show drift between live state and profile (always exits 0)
  posture apply <profile> [--repo owner/name] [--param k=v]...
                        print diff then apply profile to the live GitHub repo
                        (repo-local .github/ IaC overrides profile per section — ejectability)

ASSETS  (installed automatically by 'project add'; use these to refresh or repair)
  project install-assets (<root> | --all-projects) [agents|commands|rules|all] [--force]
                        (re)install koryph assets — agents, commands & rules (default all);
                        the canonical grouped verb for the three installers below
  agents install (<root> | --all-projects) [--force]
                        install fallback personas into <root>/.claude/agents (idempotent; --force overwrites differing files;
                        --all-projects refreshes every registered project)
  commands install (<root> | --all-projects) [--force]
                        install koryph-* Claude slash commands into <root>/.claude/commands (idempotent; --force overwrites;
                        --all-projects refreshes every registered project)
  rules install <root> [--force]
                        install the hook scripts + merge hook/permission wiring into <root>/.claude/settings.json (additive)

ADVANCED
  onboard <root> [--json]
                        read-only inventory of a project (mode-5 report; inspect before 'project add')
  project list          list managed projects (id, account, status, root)
  project show [<id>|--project ID]
                        print one project record as JSON
  project set-account [<id>|--project ID] --profile P --identity EMAIL [--config-dir DIR] --reason "..."
                        change a project's account (audited; resets validation)
  governor set --max-global N
                        set the machine-wide cap on concurrently running agents
  quota calibrate --account A --window <5h|weekly> --observed-usd X --observed-pct Y [--plan-tier T]
                        calibrate a governor ceiling from an observed /usage reading
  batch run --key-env VAR --model TIER --input FILE.jsonl [--max-tokens N] [--cache-prefix] [--out FILE] [--yes]
                        submit a Message Batch (explicit per-token spend)
  version               print the engine version

SHELL COMPLETION
  completion bash|zsh   print a completion script for the shell (source it)
  completion install [--shell bash|zsh]
                        install the completion script to the standard user
                        location (run 'koryph completion -h' for details)

ENVIRONMENT
  KORYPH_HOME           central registry + governor root (default ~/.koryph)
  KORYPH_BD_BIN         path to the bd (beads) binary (default: bd on PATH)
  KORYPH_GH_BIN         path to the gh (GitHub CLI) binary (default: gh on PATH)
  KORYPH_NO_NPX         set to any value to disable npx-based tool fallbacks (e.g. ccusage)
                        Run 'koryph doctor' to check these and the rest of your installation.

HELP
  koryph help <command>       show a command's flags (same as 'koryph <command> -h')
  koryph <command> -h         show one command's usage and flags
`)
}

func init() {
	registerCmd(command{
		name:    "version",
		summary: "print the engine version",
		run:     cmdVersion,
		// No DocLinks: version has no associated concept or user-guide page.
	})
}

func cmdVersion(_ []string, stdout, _ io.Writer) int {
	// First line stays exactly "koryph <semver>": release tooling parses its
	// last field (goreleaser's version-parity hook, `make version-check`). The
	// build-provenance lines below appear only on a linker-stamped or VCS-stamped
	// binary, so `go run`/`go test` still emit the single canonical line.
	fmt.Fprintf(stdout, "koryph %s\n", engine.EngineVersion)
	if b := version.Build(); b != "" && b != "v"+engine.EngineVersion {
		fmt.Fprintf(stdout, "  build:  %s\n", b)
	}
	if c := version.Commit(); c != "" {
		fmt.Fprintf(stdout, "  commit: %s\n", c)
	}
	if d := version.Date(); d != "" {
		fmt.Fprintf(stdout, "  built:  %s\n", d)
	}
	return 0
}

// --- shared helpers --------------------------------------------------------

// openStore returns the initialized central registry store (honors
// KORYPH_HOME). Init is idempotent.
func openStore(ctx context.Context) (*registry.Store, error) {
	s := registry.NewStore()
	if err := s.Init(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// newFlagSet builds a ContinueOnError flag set whose parse errors go to stderr.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// errHelp is returned by parseFlags when the user asked for help (-h/--help).
// Callers map it to a clean exit 0 via flagExit; the help text has already
// been printed to stdout by the FlagSet's usage function.
var errHelp = errors.New("help requested")

// isHelpArg reports whether tok is one of the help tokens a parent command
// treats as a request to list its sub-verbs (-h, --help, or the word help).
func isHelpArg(tok string) bool {
	return tok == "-h" || tok == "--help" || tok == "help"
}

// setUsage installs a FlagSet usage function that prints a one-line purpose, a
// positional/flag synopsis, and (when the set defines flags) the flag
// defaults. It replaces stdlib's bare "Usage of X:" so every leaf command's
// -h is self-documenting. synopsis is the text after the command name (e.g.
// "--project ID [flags]" or "<root> [--force]"); pass "" for a flagless,
// positional-less command. Help output goes to stdout; genuine parse-error
// messages still go to the FlagSet's stderr output.
func setUsage(fs *flag.FlagSet, stdout io.Writer, purpose, synopsis string) {
	fs.Usage = func() {
		fmt.Fprintf(stdout, "koryph %s — %s\n\nUSAGE\n  koryph %s", fs.Name(), purpose, fs.Name())
		if synopsis != "" {
			fmt.Fprintf(stdout, " %s", synopsis)
		}
		fmt.Fprintln(stdout)
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(stdout, "\nFLAGS")
			fs.SetOutput(stdout)
			fs.PrintDefaults()
		}
	}
}

// flagCapture, when non-nil, diverts parseFlags: the fully-populated FlagSet is
// stashed for the completion engine and a sentinel error is returned so the
// calling command returns immediately (via flagExit) before any side effects.
// This lets `koryph __complete` enumerate a command's real flags without
// hand-duplicating any flag list. Access is single-threaded (the CLI is not
// concurrent), so a package var is sufficient.
var flagCapture *flag.FlagSet

// errCaptured is the sentinel parseFlags returns while flagCapture is active.
var errCaptured = errors.New("flagset captured for completion")

// captureFlags runs cmd with capture mode armed and returns the FlagSet the
// command built (nil for a command that defines no flags, e.g. a pure parent).
// Only flag registration runs — every command returns at its parseFlags guard
// before touching the registry, filesystem, or network.
func captureFlags(cmd func([]string, io.Writer, io.Writer) int) *flag.FlagSet {
	prev := flagCapture
	flagCapture = &flag.FlagSet{} // non-nil sentinel; replaced by the real set
	defer func() { flagCapture = prev }()
	cmd(nil, io.Discard, io.Discard)
	if flagCapture == nil {
		return nil
	}
	fs := flagCapture
	if fs.Name() == "" { // untouched sentinel: the command defined no flags
		return nil
	}
	return fs
}

// parseFlags parses args that may lead with positional arguments (stdlib flag
// stops at the first non-flag token, so leading positionals are lifted out
// first, then trailing fs.Args() are appended). A -h/--help request prints the
// usage (via fs.Usage) and returns errHelp so callers can exit 0.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	if flagCapture != nil {
		flagCapture = fs
		return nil, errCaptured
	}
	var pre []string
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		pre = append(pre, args[i])
		i++
	}
	if err := fs.Parse(args[i:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		return nil, err
	}
	return append(pre, fs.Args()...), nil
}

// flagExit maps a parseFlags error to a process exit code: a help request is a
// clean exit 0; any other parse error is a usage error. The usage/help text
// has already been printed.
func flagExit(err error) int {
	if errors.Is(err, errHelp) || errors.Is(err, errCaptured) {
		return 0
	}
	return engine.ExitUsage
}

// subVerb is one row in a parent command's sub-verb listing: the invocation
// synopsis and a one-line purpose.
type subVerb struct {
	syn     string
	purpose string
}

// parentHelp prints a parent command's sub-verb listing to stdout and is used
// when a parent is invoked bare or with -h/--help/help. purpose is the
// parent's one-liner; verbs are its sub-verbs.
func parentHelp(stdout io.Writer, parent, purpose string, verbs []subVerb) {
	fmt.Fprintf(stdout, "koryph %s — %s\n\nUSAGE\n  koryph %s <subcommand> [flags]\n\nSUBCOMMANDS\n", parent, purpose, parent)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, v := range verbs {
		fmt.Fprintf(tw, "  %s\t%s\n", v.syn, v.purpose)
	}
	tw.Flush()
	fmt.Fprintf(stdout, "\nRun `koryph %s <subcommand> -h` for a subcommand's flags.\n", parent)
}

// flagPassed reports whether flag name was explicitly set on fs.
func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// printJSON writes v as indented JSON with a trailing newline.
func printJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// fail prints an error to stderr and returns the fatal exit code.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "koryph:", err)
	return engine.ExitFatal
}

// usageErr prints a message to stderr and returns the usage exit code.
func usageErr(stderr io.Writer, msg string) int {
	fmt.Fprintln(stderr, "koryph:", msg)
	return engine.ExitUsage
}

// resolveProjectID merges a positional project id with a --project flag value.
// Rules:
//   - --project wins when both sources agree on the same value (both accepted).
//   - A conflict (both non-empty but different) is a usage error.
//   - If neither is provided, a usage error is returned.
//
// The returned int is 0 on success, or engine.ExitUsage on error (the error
// has already been printed to stderr).
func resolveProjectID(stderr io.Writer, cmd, posVal, flagVal string) (string, int) {
	if posVal != "" && flagVal != "" && posVal != flagVal {
		return "", usageErr(stderr,
			cmd+": positional <id> "+posVal+" and --project "+flagVal+" conflict; pass only one")
	}
	if flagVal != "" {
		return flagVal, 0
	}
	if posVal != "" {
		return posVal, 0
	}
	return "", usageErr(stderr, cmd+": <id> is required (positional or --project flag)")
}

// sortStrings sorts s in place (ascending) for stable table/summary output.
func sortStrings(s []string) { sort.Strings(s) }
