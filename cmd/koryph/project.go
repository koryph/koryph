// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/commands"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/personas"
	"github.com/koryph/koryph/internal/posture"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/scaffold"
)

// cmdProject dispatches the project sub-verbs.
func cmdProject(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "project", "onboard and manage registered projects", []subVerb{
			{"add <root> --account P --identity EMAIL [--posture <profile>|--no-posture] [flags]", "register a project (inspect + register + scaffold); offers default posture profile interactively"},
			{"install-assets (<root> | --all-projects) [agents|commands|rules|all] [--force]", "(re)install koryph assets (agents, commands & rules; normally run by 'add')"},
			{"list", "list managed projects (id, account, status, root)"},
			{"show <id>|--project ID", "print one project record as JSON"},
			{"set-account <id> --profile P --identity EMAIL --reason R", "change a project's account (audited)"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return cmdProjectAdd(rest, stdout, stderr)
	case "install-assets":
		return cmdProjectInstallAssets(rest, stdout, stderr)
	case "list":
		return cmdProjectList(rest, stdout, stderr)
	case "show":
		return cmdProjectShow(rest, stdout, stderr)
	case "set-account":
		return cmdProjectSetAccount(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown project subcommand %q", sub))
	}
}

func cmdProjectAdd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project add", stderr)
	account := fs.String("account", "", "account profile: personal|work (required)")
	identity := fs.String("identity", "", "login email that must match at dispatch (required)")
	configDir := fs.String("config-dir", "", "CLAUDE_CONFIG_DIR for non-personal accounts")
	id := fs.String("id", "", "project slug (default: repo dir name slugified)")
	name := fs.String("name", "", "display name (default: project id)")
	branch := fs.String("branch", "", "default branch (default: detected)")
	force := fs.Bool("force", false, "override an .envrc account-disagreement refusal")
	postureProfile := fs.String("posture", "", "posture profile to apply non-interactively (e.g. oss-solo-maintainer); skips the interactive prompt")
	noPosture := fs.Bool("no-posture", false, "skip the posture profile offer entirely")
	setUsage(fs, stdout, "register a project (inspect + register + scaffold adapter + install agents, commands & rules)",
		"<root> --account <personal|work> --identity EMAIL [--posture <profile>|--no-posture] [flags]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) < 1 {
		return usageErr(stderr, "project add: <root> is required")
	}
	root, err := filepath.Abs(pos[0])
	if err != nil {
		return fail(stderr, err)
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	inv, err := onboard.Inspect(ctx, root)
	if err != nil {
		return fail(stderr, err)
	}
	if flagPassed(fs, "branch") && *branch != "" {
		inv.DefaultBranch = *branch
	}

	rec, err := onboard.Register(ctx, store, inv, onboard.RegisterOpts{
		ProjectID:        *id,
		AccountProfile:   *account,
		ClaudeConfigDir:  *configDir,
		ExpectedIdentity: *identity,
		Force:            *force,
	})
	if err != nil {
		return fail(stderr, err)
	}
	if *name != "" && *name != rec.Name {
		rec.Name = *name
		if err := store.Save(ctx, rec); err != nil {
			return fail(stderr, err)
		}
	}

	// Install the koryph scaffolding so the project enforces koryph semantics
	// whether `koryph` is run explicitly or implied by a prompt. Idempotent;
	// differing files are left untouched (re-run `koryph project install-assets
	// <root> --force` to update them).
	//
	// Asset install order and capability-gating (koryph-v8u.9):
	//
	//   1. AGENTS.md — always installed; the canonical, runtime-neutral
	//      instruction file read natively by Codex, Cursor, Grok, Copilot,
	//      opencode, amp, and Claude Code.
	//
	//   2. agents (.claude/agents) — always installed; personas render correctly
	//      for any runtime via InstallForRuntime (koryph-v8u.12; resolves to
	//      "claude" when unset/unreadable, the pre-koryph-v8u.12 behavior).
	//
	//   3. commands (.claude/commands) — Claude Code only; skip when the project's
	//      runtime does not support .claude/ (Capabilities.Personas == false).
	//      Containment for runtimes without commands: worktree isolation +
	//      merge-time protected-path refusal.
	//
	//   4. rules (hooks + settings.json) — Claude Code only; skip when the
	//      project's runtime does not support lifecycle hooks (Capabilities.Hooks
	//      == false). Same containment note as above.
	onboardInstallAgentsMD(stderr, root)
	onboardInstall(stderr, "agents", func() ([]scaffold.Result, error) {
		results, _, ierr := personas.InstallForRuntime(root, false, resolveInstallRuntime(root, ""))
		return results, ierr
	})
	caps := resolveRuntimeCapabilities(root)
	if caps.Personas {
		onboardInstall(stderr, "commands", func() ([]scaffold.Result, error) { return commands.Install(root, false) })
	} else {
		fmt.Fprintln(stderr, "koryph: commands skipped (runtime does not support .claude/commands; containment via worktree isolation + merge gate)")
	}
	if caps.Hooks {
		onboardRules(stderr, root)
	} else {
		fmt.Fprintln(stderr, "koryph: rules skipped (runtime does not support hooks; containment via worktree isolation + merge gate)")
	}

	// --- posture profile offer -----------------------------------------------
	// Offer the default posture profile (oss-solo-maintainer) unless:
	//   - the project config already has a posture block (idempotent),
	//   - --no-posture was passed (skip), or
	//   - --posture <name> was passed (apply non-interactively).
	// Interactive mode: show the diff and ask for confirmation.
	// NEVER silently mutate an existing repo's settings.
	if !*noPosture {
		if exitCode := cmdProjectAddPosture(root, *postureProfile, stdout, stderr); exitCode != 0 {
			// Posture errors are non-fatal for the main registration flow; the
			// project is already registered. Emit the error and continue.
			fmt.Fprintf(stderr, "koryph: posture offer failed (project registered; run `koryph posture apply` manually): exit %d\n", exitCode)
		}
	}

	// Seed the .koryph/snapshots/ gitignore entry so pre-apply snapshots are
	// never accidentally committed. Idempotent; non-fatal on failure.
	if err := posture.EnsureGitignored(root); err != nil {
		fmt.Fprintf(stderr, "koryph: warning: could not add .koryph/snapshots/ to .gitignore: %v\n", err)
	}

	if err := printJSON(stdout, rec); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdProjectList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project list", stderr)
	asJSON := fs.Bool("json", false, "emit JSON array of project records")
	setUsage(fs, stdout, "list managed projects (id, account, status, root)", "[--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	recs, err := store.List()
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		if recs == nil {
			recs = []*registry.Record{}
		}
		if err := printJSON(stdout, recs); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if len(recs) == 0 {
		fmt.Fprintln(stdout, "no projects registered")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tACCOUNT\tSTATUS\tROOT")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ProjectID, r.AccountProfile, r.MigrationStatus, r.Root)
	}
	tw.Flush()
	return 0
}

func cmdProjectShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project show", stderr)
	flagProject := fs.String("project", "", "project id (alternative to positional <id>)")
	setUsage(fs, stdout, "print one project record as JSON", "<id>|--project ID")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	posVal := ""
	if len(pos) > 0 {
		posVal = pos[0]
	}
	id, code := resolveProjectID(stderr, "project show", posVal, *flagProject)
	if code != 0 {
		return code
	}
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, err := store.Get(id)
	if err != nil {
		return fail(stderr, err)
	}
	if err := printJSON(stdout, rec); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdProjectSetAccount(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project set-account", stderr)
	flagProject := fs.String("project", "", "project id (alternative to positional <id>)")
	profile := fs.String("profile", "", "new account profile: personal|work (required)")
	identity := fs.String("identity", "", "new expected login email (required)")
	configDir := fs.String("config-dir", "", "CLAUDE_CONFIG_DIR for the new account")
	reason := fs.String("reason", "", "why the account is changing (required, audited)")
	setUsage(fs, stdout, "change a project's account (audited; resets validation)",
		"<id>|--project ID --profile P --identity EMAIL [--config-dir DIR] --reason R")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	posVal := ""
	if len(pos) > 0 {
		posVal = pos[0]
	}
	id, code := resolveProjectID(stderr, "project set-account", posVal, *flagProject)
	if code != 0 {
		return code
	}
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if err := store.SetAccount(ctx, id, *profile, *configDir, *identity, *reason); err != nil {
		return fail(stderr, err)
	}
	rec, err := store.Get(id)
	if err != nil {
		return fail(stderr, err)
	}
	if err := printJSON(stdout, rec); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// defaultPostureProfile is the profile offered by `koryph project add` when no
// --posture flag is given.  The future `koryph new` command (koryph-om7, HELD)
// applies this unconditionally on freshly created repos.
const defaultPostureProfile = "oss-solo-maintainer"

// cmdProjectAddPosture offers the posture profile to the operator after a
// successful `koryph project add`.
//
//   - profileName == "": interactive mode — ask the operator.
//   - profileName != "": non-interactive — apply the named profile directly.
//
// All interactive / informational output goes to stderr (stdout is reserved
// for JSON). Returns 0 on success (including graceful skip/decline), non-zero
// on hard error. Callers treat non-zero as a non-fatal warning.
func cmdProjectAddPosture(root, profileName string, _, stderr io.Writer) int {
	// Load the project config to check if a posture block is already present.
	cfg, err := project.Load(root)
	if err != nil {
		fmt.Fprintf(stderr, "koryph: posture: cannot load project config: %v\n", err)
		return 1
	}
	if cfg.Posture != nil {
		fmt.Fprintf(stderr, "koryph: posture: profile %q already declared; skipping offer\n", cfg.Posture.Profile)
		return 0
	}

	home := paths.KoryphHome()
	ghBin := posture.GHBin()
	ctx := context.Background()

	// Resolve which profile to offer.
	offer := profileName
	if offer == "" {
		offer = defaultPostureProfile
	}

	// Load the profile manifest so we can show its description.
	builtins, _ := posture.ListBuiltins()
	var manifest posture.Manifest
	for _, b := range builtins {
		if b.Name == offer {
			manifest = b.Manifest
			break
		}
	}

	// Non-interactive: apply directly without prompting.
	if profileName != "" {
		return applyPostureProfile(ctx, root, ghBin, home, offer, nil, stderr)
	}

	// Interactive: show the profile description and ask for confirmation.
	// All output goes to stderr to keep stdout clean for JSON.
	fmt.Fprintln(stderr, "")
	fmt.Fprintf(stderr, "koryph: posture offer\n")
	fmt.Fprintf(stderr, "  Profile : %s\n", offer)
	if manifest.Description != "" {
		fmt.Fprintf(stderr, "  What    : %s\n", manifest.Description)
	}
	fmt.Fprintln(stderr, "")

	// Show the diff (best-effort: skip gracefully if gh is unavailable).
	repoSlug, ghErr := posture.DetectRepo(ctx, ghBin)
	if ghErr == nil && repoSlug != "" {
		profileSrc, cleanup, renderErr := posture.RenderProfile(offer, nil, home)
		if renderErr == nil {
			defer cleanup()
			var diffBuf bytes.Buffer
			hasRulesets, hasSettings := posture.EjectCheck(root)
			localSrc := posture.LocalSource{Root: root}

			var rulesetSrc posture.Source = profileSrc
			if hasRulesets {
				rulesetSrc = localSrc
			}
			if _, err := rulesetSrc.RulesetsDir(); err == nil {
				_, _ = posture.CheckRulesets(ctx, ghBin, repoSlug, rulesetSrc, &diffBuf)
			}

			var settingsSrc posture.Source = profileSrc
			if hasSettings {
				settingsSrc = localSrc
			}
			if _, err := settingsSrc.RepoSettingsFile(); err == nil {
				_, _ = posture.CheckSettings(ctx, ghBin, repoSlug, settingsSrc, &diffBuf)
			}

			if diffBuf.Len() > 0 {
				fmt.Fprintf(stderr, "--- posture diff (profile %s vs %s) ---\n", offer, repoSlug)
				_, _ = io.Copy(stderr, &diffBuf)
				fmt.Fprintln(stderr, "---")
			}
		}
	} else {
		fmt.Fprintf(stderr, "  (diff skipped: gh not available or not authenticated — run `koryph posture diff %s` later)\n", offer)
	}

	// Check if stdin is a terminal; skip prompt in non-interactive mode.
	if !isTerminal(os.Stdin) {
		fmt.Fprintf(stderr, "koryph: posture: non-interactive; skipping prompt\n")
		fmt.Fprintf(stderr, "         run `koryph project add ... --posture %s` to apply, or\n", offer)
		fmt.Fprintf(stderr, "         run `koryph posture apply %s` after registration.\n", offer)
		return 0
	}

	fmt.Fprintf(stderr, "Apply profile %q to this repository? [y/N] ", offer)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() || !strings.EqualFold(strings.TrimSpace(scanner.Text()), "y") {
		fmt.Fprintln(stderr, "koryph: posture: declined; run `koryph posture apply "+offer+"` any time to apply.")
		return 0
	}

	return applyPostureProfile(ctx, root, ghBin, home, offer, nil, stderr)
}

// applyPostureProfile renders the named profile and applies it to the live
// GitHub repo, then records the choice in koryph.project.json.
// params may be nil to use profile defaults.
// All output goes to w (caller passes stderr to keep stdout clean for JSON).
func applyPostureProfile(ctx context.Context, root, ghBin, home, profileName string, params map[string]string, w io.Writer) int {
	repoSlug, err := posture.DetectRepo(ctx, ghBin)
	if err != nil {
		fmt.Fprintf(w, "koryph: posture: cannot detect GitHub repo (is gh authenticated?): %v\n", err)
		return 1
	}

	profileSrc, cleanup, err := posture.RenderProfile(profileName, params, home)
	if err != nil {
		fmt.Fprintf(w, "koryph: posture: render profile %q: %v\n", profileName, err)
		return 1
	}
	defer cleanup()

	hasRulesets, hasSettings := posture.EjectCheck(root)
	localSrc := posture.LocalSource{Root: root}

	applyErr := false

	// Apply rulesets.
	var rulesetSrc posture.Source = profileSrc
	if hasRulesets {
		rulesetSrc = localSrc
	}
	if _, err := rulesetSrc.RulesetsDir(); err == nil {
		fmt.Fprintln(w, "--- rulesets diff ---")
		d, err := posture.CheckRulesets(ctx, ghBin, repoSlug, rulesetSrc, w)
		if err != nil {
			fmt.Fprintf(w, "koryph: posture: ruleset check: %v\n", err)
			applyErr = true
		} else if d {
			fmt.Fprintln(w, "--- applying rulesets ---")
			if err := posture.ApplyRulesets(ctx, ghBin, repoSlug, rulesetSrc, w); err != nil {
				fmt.Fprintf(w, "koryph: posture: apply rulesets: %v\n", err)
				applyErr = true
			}
		}
	}

	// Apply settings.
	var settingsSrc posture.Source = profileSrc
	if hasSettings {
		settingsSrc = localSrc
	}
	if _, err := settingsSrc.RepoSettingsFile(); err == nil {
		fmt.Fprintln(w, "--- settings diff ---")
		d, err := posture.CheckSettings(ctx, ghBin, repoSlug, settingsSrc, w)
		if err != nil {
			fmt.Fprintf(w, "koryph: posture: settings check: %v\n", err)
			applyErr = true
		} else if d {
			fmt.Fprintln(w, "--- applying settings ---")
			if err := posture.ApplySettings(ctx, ghBin, repoSlug, settingsSrc, w); err != nil {
				fmt.Fprintf(w, "koryph: posture: apply settings: %v\n", err)
				applyErr = true
			}
		}
	}

	if applyErr {
		return 1
	}

	// Record the choice in koryph.project.json.
	cfg, err := project.Load(root)
	if err != nil {
		fmt.Fprintf(w, "koryph: posture: reload project config: %v\n", err)
		return 1
	}
	cfg.Posture = &project.PostureConfig{
		Profile:    profileName,
		Parameters: params,
	}
	if err := cfg.Save(root); err != nil {
		fmt.Fprintf(w, "koryph: posture: save project config: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "koryph: posture: profile %q recorded in koryph.project.json\n", profileName)
	return 0
}

// isTerminal reports whether f is a terminal (file descriptor backed by a TTY).
// Returns false for pipes, files, and non-*os.File writers.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// cmdOnboard is the read-only mode-5 inventory report.
func cmdOnboard(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("onboard", stderr)
	asJSON := fs.Bool("json", false, "emit the inventory as JSON")
	setUsage(fs, stdout, "read-only inventory of a project (mode-5 report)", "<root> [--json]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) < 1 {
		return usageErr(stderr, "onboard: <root> is required")
	}
	root, err := filepath.Abs(pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	inv, err := onboard.Inspect(context.Background(), root)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, inv); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	printInventory(stdout, inv)
	return 0
}

// printInventory renders a human-readable inventory summary.
func printInventory(w io.Writer, inv *onboard.Inventory) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	line := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	line("root", inv.Root)
	line("git repo", yesno(inv.IsGitRepo))
	line("default branch", orDash(inv.DefaultBranch))
	line("remote", orDash(inv.Remote))
	line("beads", beadsSummary(inv))
	line("bd available", fmt.Sprintf("%s %s", yesno(inv.BDAvailable), inv.BDVersion))
	line("claude settings", yesno(inv.ClaudeSettings))
	line("bd prime hook", yesno(inv.BDPrimeHook))
	line("personas", orDash(fmt.Sprintf("%v", inv.Personas)))
	line("legacy koryph", fmt.Sprintf("%s %v", yesno(inv.LegacyKoryph), inv.LegacyHints))
	line("envrc profile", orDash(inv.EnvrcProfile))
	line("envrc dir", orDash(inv.EnvrcDir))
	line("adapter present", yesno(inv.AdapterPresent))
	line("plans dir", orDash(inv.PlansDir))
	line("worktrees", fmt.Sprintf("%d", len(inv.Worktrees)))
	tw.Flush()
}

func beadsSummary(inv *onboard.Inventory) string {
	if !inv.HasBeads {
		return "none"
	}
	s := "initialized"
	if inv.BeadsHardened {
		s = "hardened"
	}
	if inv.BeadsHooks {
		s += " (+hooks)"
	}
	return s
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// cmdValidate runs the pre-dispatch gate and promotes registered->migrated on
// green.
func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("validate", stderr)
	flagProject := fs.String("project", "", "project id (alternative to positional <project-id>)")
	setUsage(fs, stdout, "run the pre-dispatch gate; promotes registered->migrated on green",
		"<project-id>|--project ID")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	posVal := ""
	if len(pos) > 0 {
		posVal = pos[0]
	}
	projectID, code := resolveProjectID(stderr, "validate", posVal, *flagProject)
	if code != 0 {
		return code
	}
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	v, err := onboard.Validate(ctx, store, projectID, stdout)
	if err != nil {
		return fail(stderr, err)
	}
	if !v.OK {
		fmt.Fprintln(stdout, "FAILED")
		return engine.ExitFatal
	}
	if rec, gerr := store.Get(projectID); gerr == nil {
		switch rec.MigrationStatus {
		case registry.StatusRegistered:
			rec.MigrationStatus = registry.StatusMigrated
			if serr := store.Save(ctx, rec); serr != nil {
				fmt.Fprintln(stderr, "koryph: warning: could not promote migration status:", serr)
			} else {
				fmt.Fprintln(stdout, "promoted migration_status: registered -> migrated")
			}
		case registry.StatusMigrated:
			// Promote to validated only on evidence of a green canary: the
			// latest run has at least one merged slot and no failures.
			if run, lerr := ledger.NewStore(rec.Root).LoadLatest(); lerr == nil {
				merged, bad := 0, 0
				for _, s := range run.Slots {
					switch s.Status {
					case ledger.SlotMerged:
						merged++
					case ledger.SlotFailed, ledger.SlotBlocked, ledger.SlotConflict:
						bad++
					}
				}
				if merged > 0 && bad == 0 {
					rec.MigrationStatus = registry.StatusValidated
					if serr := store.Save(ctx, rec); serr != nil {
						fmt.Fprintln(stderr, "koryph: warning: could not promote migration status:", serr)
					} else {
						fmt.Fprintf(stdout, "promoted migration_status: migrated -> validated (canary run %s: %d merged, 0 failed)\n", run.RunID, merged)
					}
				} else {
					fmt.Fprintf(stdout, "not promoted to validated: latest run %s has %d merged / %d failed-blocked-conflict slots (need >=1 merged, 0 bad)\n", run.RunID, merged, bad)
				}
			} else {
				fmt.Fprintln(stdout, "not promoted to validated: no canary run found (run `koryph run --project "+projectID+" --once --allow-unvalidated` and re-validate)")
			}
		}
	}
	fmt.Fprintln(stdout, "OK")
	return 0
}
