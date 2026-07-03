// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/commands"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/personas"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/scaffold"
)

// cmdProject dispatches the project sub-verbs.
func cmdProject(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "project", "onboard and manage registered projects", []subVerb{
			{"add <root> --account P --identity EMAIL [flags]", "register a project (inspect + register + scaffold)"},
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
	setUsage(fs, stdout, "register a project (inspect + register + scaffold adapter + install agents, commands & rules)",
		"<root> --account <personal|work> --identity EMAIL [flags]")
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

	// Install the koryph scaffolding so the project enforces koryph
	// semantics whether `koryph` is run explicitly or implied by a prompt:
	// fallback personas (.claude/agents) and the koryph-* slash commands
	// (.claude/commands). Idempotent; differing files are left untouched
	// (re-run `koryph agents|commands install --force` to update).
	onboardInstall(stderr, "agents", func() ([]scaffold.Result, error) { return personas.Install(root, false) })
	onboardInstall(stderr, "commands", func() ([]scaffold.Result, error) { return commands.Install(root, false) })
	onboardRules(stderr, root)

	if err := printJSON(stdout, rec); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdProjectList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project list", stderr)
	setUsage(fs, stdout, "list managed projects (id, account, status, root)", "")
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
