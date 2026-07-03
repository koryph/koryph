// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Command koryph is the central multi-project koryph CLI: onboard and
// validate projects, drive the wave engine, and observe/operate live runs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/registry"
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
	case "init":
		return cmdInit(rest, stdout, stderr)
	case "version":
		return cmdVersion(rest, stdout, stderr)
	case "project":
		return cmdProject(rest, stdout, stderr)
	case "onboard":
		return cmdOnboard(rest, stdout, stderr)
	case "validate":
		return cmdValidate(rest, stdout, stderr)
	case "intake":
		return cmdIntake(rest, stdout, stderr)
	case "run":
		return cmdRun(rest, stdout, stderr)
	case "board":
		return cmdBoard(rest, stdout, stderr)
	case "roster":
		return cmdRoster(rest, stdout, stderr)
	case "status":
		return cmdStatus(rest, stdout, stderr)
	case "tail":
		return cmdTail(rest, stdout, stderr)
	case "nudge":
		return cmdNudge(rest, stdout, stderr)
	case "stop":
		return cmdStop(rest, stdout, stderr)
	case "merge":
		return cmdMerge(rest, stdout, stderr)
	case "land":
		return cmdLand(rest, stdout, stderr)
	case "review-pr":
		return cmdReviewPR(rest, stdout, stderr)
	case "pr-sync":
		return cmdPRSync(rest, stdout, stderr)
	case "signing":
		return cmdSigning(rest, stdout, stderr)
	case "sign":
		return cmdSign(rest, stdout, stderr)
	case "quota":
		return cmdQuota(rest, stdout, stderr)
	case "batch":
		return cmdBatch(rest, stdout, stderr)
	case "metrics":
		return cmdMetrics(rest, stdout, stderr)
	case "agents":
		return cmdAgents(rest, stdout, stderr)
	case "commands":
		return cmdCommands(rest, stdout, stderr)
	case "rules":
		return cmdRules(rest, stdout, stderr)
	case "governor":
		return cmdGovernor(rest, stdout, stderr)
	case "doctor":
		return cmdDoctor(rest, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "koryph: unknown command %q\n\n", cmd)
		usage(stderr)
		return engine.ExitUsage
	}
}

// usage prints the global command listing.
func usage(w io.Writer) {
	fmt.Fprint(w, `koryph — central multi-project koryph

USAGE
  koryph <command> [flags]

ONBOARDING
  init                  create ~/.koryph, verify tools on PATH, print next steps (idempotent)
  project add <root> --account <personal|work> --identity <email> [--config-dir DIR] [--id slug] [--name N] [--branch B] [--force]
                        register a project (inspect + register + scaffold adapter + install agents, commands & rules)
  project list          list managed projects (id, account, status, root)
  project show <id>|--project ID
                        print one project record as JSON
  project set-account <id>|--project ID --profile P --identity EMAIL [--config-dir DIR] --reason "..."
                        change a project's account (audited; resets validation)
  onboard <root> [--json]
                        read-only inventory of a project (mode-5 report)
  validate <project-id>|--project ID
                        run the pre-dispatch gate; promotes registered->migrated on green
  agents install <root> [--force]
                        install fallback personas into <root>/.claude/agents (idempotent; --force overwrites differing files)
  commands install <root> [--force]
                        install koryph-* Claude slash commands into <root>/.claude/commands (idempotent; --force overwrites)
  rules install <root> [--force]
                        install the hook scripts + merge hook/permission wiring into <root>/.claude/settings.json (additive)

RUN
  run --project ID [--once] [--max N] [--parent EPIC] [--only BEAD] [--budget USD]
      [--default-model M] [--auto-merge] [--direct] [--dry-run] [--resume] [--review]
      [--allow-api-spend] [--allow-unvalidated] [--manual]
                        execute one engine run over a project (--only dispatches a
                        single bead; --budget caps cumulative run cost; --direct
                        skips PRs and merges straight to the default branch)
  intake --project ID [--label triage] [--limit 20] [--dry-run] [--comment]
                        poll a project's labeled GitHub issues into no-dispatch planning beads

OBSERVE / OPERATE
  doctor [--json] [--fix]
                        global health check: layout, binaries, registry, governor, zombie
                        leases, stale demand heartbeats, quota calibration, vault providers;
                        --fix removes zombie slots + stale demand; exits 0/1/2 (ok/warn/err)
  board [--json]        one-line-per-project run overview
  roster --project ID [--run ID] [--json]
                        per-bead titled roster grouped by lifecycle: MERGED /
                        RUNNING / QUEUED / DEFERRED (defaults to latest run)
  governor [show]       show the machine-wide concurrency cap, active leases, and demand
  governor set --max-global N
                        set the machine-wide cap on concurrently running agents
  status --project ID [--json]
                        latest-run per-slot detail
  tail --project ID <phase-id> [-n 40] [--follow]
                        tail a phase's session.log + stderr.log; --follow streams
                        new lines and surfaces INBOX nudges live (Ctrl-C to stop)
  nudge --project ID <phase-id> "text"
                        append an operator note to the phase INBOX (+ bd comment)
  stop --project ID <phase-id> [--force] | stop --all [--force]
                        stop an agent (or every agent, --all) — SIGTERM, or
                        SIGKILL with --force (uncommitted work is lost)
  merge --project ID <branch> [--push] [--squash] [--keep-worktree] [--close-bead BEAD --reason R]
                        land a finished agent branch on the default branch
  land --project ID <bead> [--method ff|squash] [--reason R]
                        land an engine-opened PR (a pr-opened bead) fast-forward-only,
                        preserving signed SHAs; closes the bead on success
  review-pr --project ID <pr> [--approve|--comment|--comment-on path:line:msg|--resume|--close] [--body B]
  review-pr --project ID --all
                        analyze another author's PR (or every open PR with --all) using
                        koryph's reviewer (prints findings, never approves). --comment posts
                        findings as inline comments; --comment-on adds your own line
                        comments; --resume replays a saved analysis after an IDE handoff;
                        --approve/--close register your approval or close the PR
  pr-sync --project ID  reconcile pr-opened beads against live PR state: a PR merged or
                        closed by any means marks its slot merged/blocked (nothing stranded)

SIGNING
  signing setup --project ID --provider P --key-ref REF --identity EMAIL [--mode ssh|gitsign]
      [--public-key "ssh-ed25519 ..."] [--artifacts]
                        write the vault-backed signing policy into the project adapter
                        (protonpass + no --public-key: auto-discover via the agent)
  signing enable --project ID
                        load the key into the SSH agent + apply repo git config
  signing status --project ID
                        mode/provider/agent-ready/repo-config/allowed_signers summary
  signing verify --project ID --branch BR
                        verify branch commit signatures against the default branch (exit 1 on any bad)
  sign blob --project ID <path>
                        cosign sign-blob an artifact via the vault key (writes <path>.sig)

BILLING / METRICS
  quota [--account A] [--json]
                        per-account governor snapshot (ccusage probe may take up to 40 s)
  quota calibrate --account A --window <5h|weekly> --observed-usd X --observed-pct Y [--plan-tier T]
                        calibrate a governor ceiling from an observed /usage reading
  batch run --key-env VAR --model TIER --input FILE.jsonl [--max-tokens N] [--cache-prefix] [--out FILE] [--yes]
                        submit a Message Batch (explicit per-token spend)
  metrics [--project ID] [--json]
                        burn + reliability rollup across projects

  version               print the engine version
`)
}

func cmdVersion(_ []string, stdout, _ io.Writer) int {
	fmt.Fprintf(stdout, "koryph %s\n", engine.EngineVersion)
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

// newFlagSet builds a ContinueOnError flag set whose usage/errors go to stderr.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags parses args that may lead with positional arguments (stdlib flag
// stops at the first non-flag token, so leading positionals are lifted out
// first, then trailing fs.Args() are appended).
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pre []string
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		pre = append(pre, args[i])
		i++
	}
	if err := fs.Parse(args[i:]); err != nil {
		return nil, err
	}
	return append(pre, fs.Args()...), nil
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
