// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/commands"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/personas"
	"github.com/koryph/koryph/internal/rules"
	"github.com/koryph/koryph/internal/scaffold"
)

// assetTargets is the canonical ordered set of koryph asset kinds that
// `project install-assets` (and `project add`) install.
var assetTargets = []string{"agents", "commands", "rules"}

// cmdProjectInstallAssets is the canonical grouped installer: it (re)installs
// the koryph assets — fallback personas, koryph-* slash commands, and the hook
// scripts + settings wiring — that `project add` installs automatically. The
// optional positional target (agents|commands|rules|all, default all) narrows
// the set; --force overwrites differing files; --all-projects installs into
// every registered project. The per-asset top-level verbs (agents install,
// commands install, rules install) remain as working aliases.
func cmdProjectInstallAssets(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project install-assets", stderr)
	force := fs.Bool("force", false, "overwrite existing assets whose content differs")
	allProjects := fs.Bool("all-projects", false, "install into every registered project (registry-wide refresh)")
	setUsage(fs, stdout,
		"(re)install koryph assets (agents, commands & rules) — normally run automatically by `koryph project add`",
		"(<root> | --all-projects) [agents|commands|rules|all] [--force]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}

	// Classify positionals: a known target keyword selects the asset set;
	// anything else is the <root> path. A path never collides with a keyword.
	targets := assetTargets
	rootArg := ""
	for _, p := range pos {
		switch p {
		case "all":
			targets = assetTargets
		case "agents", "commands", "rules":
			targets = []string{p}
		default:
			if rootArg != "" {
				return usageErr(stderr, "project install-assets: unexpected extra argument "+p)
			}
			rootArg = p
		}
	}

	if *allProjects {
		if rootArg != "" {
			return usageErr(stderr, "project install-assets: <root> and --all-projects are mutually exclusive")
		}
		return installAssetsAllProjects(stdout, stderr, targets, *force)
	}
	if rootArg == "" {
		return usageErr(stderr, "project install-assets: <root> is required (or use --all-projects)")
	}
	root, err := filepath.Abs(rootArg)
	if err != nil {
		return fail(stderr, err)
	}
	anyFailed := false
	for _, t := range targets {
		if ierr := installAssetType(stdout, stderr, root, t, *force); ierr != nil {
			fmt.Fprintf(stderr, "koryph: %s install failed: %v\n", t, ierr)
			anyFailed = true
		}
	}
	if anyFailed {
		return engine.ExitFatal
	}
	return 0
}

// installAssetsAllProjects installs the selected asset targets into every
// registered project, printing a per-project header. Best-effort: a failure in
// one project warns and continues; the exit code is 1 if any install failed.
func installAssetsAllProjects(stdout, stderr io.Writer, targets []string, force bool) int {
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
	anyFailed := false
	for _, rec := range recs {
		fmt.Fprintf(stdout, "== %s ==\n", rec.ProjectID)
		for _, t := range targets {
			if ierr := installAssetType(stdout, stderr, rec.Root, t, force); ierr != nil {
				fmt.Fprintf(stderr, "koryph: warning: %s: %s install failed: %v\n", rec.ProjectID, t, ierr)
				anyFailed = true
			}
		}
	}
	if anyFailed {
		return 1
	}
	return 0
}

// installAssetType installs one asset kind into root and reports it via the
// shared installer reporters. It returns an error only on a hard failure; a
// differing/skipped file is a warning surfaced by the reporter, not an error.
func installAssetType(stdout, stderr io.Writer, root, target string, force bool) error {
	switch target {
	case "agents":
		results, err := personas.Install(root, force)
		if err != nil {
			return err
		}
		reportInstall(stdout, stderr, "agents", results, force)
	case "commands":
		results, err := commands.Install(root, force)
		if err != nil {
			return err
		}
		reportInstall(stdout, stderr, "commands", results, force)
	case "rules":
		hookResults, settings, err := rules.Install(root, force)
		if err != nil {
			return err
		}
		reportInstall(stdout, stderr, "hooks", hookResults, force)
		reportSettings(stdout, stderr, settings)
	default:
		return fmt.Errorf("unknown asset target %q", target)
	}
	return nil
}

// cmdCommands dispatches the commands sub-verbs.
func cmdCommands(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "commands", "manage the koryph-* Claude slash commands in a project (normally run by `koryph project add`)", []subVerb{
			{"install <root> [--force]", "install koryph-* slash commands into <root>/.claude/commands"},
			{"install --all-projects [--force]", "install koryph-* slash commands into every registered project"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return cmdCommandsInstall(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown commands subcommand %q", sub))
	}
}

// cmdRules dispatches the rules sub-verbs.
func cmdRules(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "rules", "manage the koryph hook scripts + settings wiring in a project (normally run by `koryph project add`)", []subVerb{
			{"install <root> [--force]", "install hook scripts + merge hook/permission wiring into settings.json"},
		})
		return 0
	}
	switch args[0] {
	case "install":
		return cmdRulesInstall(args[1:], stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown rules subcommand %q", args[0]))
	}
}

// cmdRulesInstall installs the koryph hook scripts into <root>/hooks and
// merges the hook + permission wiring into <root>/.claude/settings.json.
func cmdRulesInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("rules install", stderr)
	force := fs.Bool("force", false, "overwrite differing hook scripts; rebuild an unparseable settings.json")
	setUsage(fs, stdout, "install hook scripts + merge hook/permission wiring into <root>/.claude/settings.json (normally run automatically by `koryph project add`)", "<root> [--force]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) < 1 {
		return usageErr(stderr, "rules install: <root> is required")
	}
	root, err := filepath.Abs(pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	hookResults, settings, err := rules.Install(root, *force)
	if err != nil {
		return fail(stderr, err)
	}
	reportInstall(stdout, stderr, "hooks", hookResults, *force)
	reportSettings(stdout, stderr, settings)
	return 0
}

// reportSettings prints the settings.json merge outcome, warning on a skip.
func reportSettings(stdout, stderr io.Writer, action string) {
	switch action {
	case rules.SettingsCreated:
		fmt.Fprintln(stdout, "settings.json: created with koryph hooks + permissions")
	case rules.SettingsMerged:
		fmt.Fprintln(stdout, "settings.json: merged koryph hooks + permissions (existing keys preserved)")
	case rules.SettingsUnchanged:
		fmt.Fprintln(stdout, "settings.json: already wired (no change)")
	case rules.SettingsSkipped:
		fmt.Fprintln(stderr, "koryph: warning: .claude/settings.json is unparseable or has an incompatible shape — left unchanged; fix it or re-run with --force to rebuild.")
	}
}

// onboardRules installs the enforcement rules during `project add` (best-effort:
// warnings, never a failure).
func onboardRules(stderr io.Writer, root string) {
	hookResults, settings, err := rules.Install(root, false)
	if err != nil {
		fmt.Fprintf(stderr, "koryph: warning: could not install rules: %v\n", err)
		return
	}
	if n := scaffold.Count(hookResults, scaffold.ActionInstalled); n > 0 {
		fmt.Fprintf(stderr, "koryph: installed %d hook script(s) into hooks/\n", n)
	}
	if c := scaffold.Conflicts(hookResults); len(c) > 0 {
		fmt.Fprintf(stderr, "koryph: warning: %d hook script(s) differ, left unchanged: %s — run `koryph rules install <root> --force`\n",
			len(c), strings.Join(c, ", "))
	}
	switch settings {
	case rules.SettingsCreated:
		fmt.Fprintln(stderr, "koryph: wrote .claude/settings.json (koryph hooks + permissions)")
	case rules.SettingsMerged:
		fmt.Fprintln(stderr, "koryph: merged koryph hooks + permissions into .claude/settings.json")
	case rules.SettingsSkipped:
		fmt.Fprintln(stderr, "koryph: warning: .claude/settings.json unparseable/incompatible — left unchanged (fix it or `koryph rules install --force`)")
	}
}

// cmdCommandsInstall writes the koryph-* Claude slash commands into
// <root>/.claude/commands. Identical files are a no-op; content that differs is
// left untouched unless --force is passed. With --all-projects, installs into
// every registered project instead of a single <root>.
func cmdCommandsInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("commands install", stderr)
	force := fs.Bool("force", false, "overwrite existing commands whose content differs")
	allProjects := fs.Bool("all-projects", false, "install into every registered project (registry-wide refresh)")
	setUsage(fs, stdout,
		"install koryph-* Claude slash commands into <root>/.claude/commands (idempotent; normally run automatically by `koryph project add`)",
		"(<root> | --all-projects) [--force]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}

	if *allProjects {
		if len(pos) > 0 {
			return usageErr(stderr, "commands install: <root> and --all-projects are mutually exclusive")
		}
		return installAllProjects(stdout, stderr, "commands", *force,
			func(root string, force bool) ([]scaffold.Result, error) {
				return commands.Install(root, force)
			})
	}

	if len(pos) < 1 {
		return usageErr(stderr, "commands install: <root> is required (or use --all-projects)")
	}
	root, err := filepath.Abs(pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	results, err := commands.Install(root, *force)
	if err != nil {
		return fail(stderr, err)
	}
	reportInstall(stdout, stderr, "commands", results, *force)
	return 0
}

// installAllProjects calls the given install function for every project in the
// registry, prints a per-project summary, and returns 0 if all installs
// succeed or 1 if any project install fails (best-effort: failures do not stop
// processing of remaining projects). label is used in status messages.
func installAllProjects(stdout, stderr io.Writer, label string, force bool,
	install func(root string, force bool) ([]scaffold.Result, error)) int {
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

	anyFailed := false
	for _, rec := range recs {
		results, ierr := install(rec.Root, force)
		if ierr != nil {
			fmt.Fprintf(stderr, "koryph: warning: %s: %s install failed: %v\n", rec.ProjectID, label, ierr)
			anyFailed = true
			continue
		}
		installed := scaffold.Count(results, scaffold.ActionInstalled)
		overwritten := scaffold.Count(results, scaffold.ActionOverwritten)
		unchanged := scaffold.Count(results, scaffold.ActionUnchanged)
		skipped := scaffold.Count(results, scaffold.ActionSkipped)
		fmt.Fprintf(stdout, "%s: %d installed, %d overwritten, %d unchanged, %d skipped\n",
			rec.ProjectID, installed, overwritten, unchanged, skipped)
		if conflicts := scaffold.Conflicts(results); len(conflicts) > 0 && !force {
			fmt.Fprintf(stderr, "koryph: warning: %s: %d %s differ, left unchanged: %s\n",
				rec.ProjectID, len(conflicts), label, strings.Join(conflicts, ", "))
		}
	}
	if anyFailed {
		return 1
	}
	return 0
}

// onboardInstall runs one scaffolding install during `project add`. It never
// fails onboarding (a warning at most), reports how many assets were newly
// installed, and surfaces any differing files that were left untouched.
func onboardInstall(stderr io.Writer, label string, install func() ([]scaffold.Result, error)) {
	results, err := install()
	if err != nil {
		fmt.Fprintf(stderr, "koryph: warning: could not install %s: %v\n", label, err)
		return
	}
	if n := scaffold.Count(results, scaffold.ActionInstalled); n > 0 {
		fmt.Fprintf(stderr, "koryph: installed %d %s into .claude/%s\n", n, label, label)
	}
	if conflicts := scaffold.Conflicts(results); len(conflicts) > 0 {
		fmt.Fprintf(stderr,
			"koryph: warning: %d %s already exist with different content, left unchanged: %s\n",
			len(conflicts), label, strings.Join(conflicts, ", "))
		fmt.Fprintf(stderr, "koryph: run `koryph %s install <root> --force` to update them.\n", label)
	}
}

// reportInstall prints one line per asset and a summary, then — when a
// non-force run left differing files untouched — warns and points at --force.
// Installing is best-effort: a skip is a warning, not a failure.
func reportInstall(stdout, stderr io.Writer, label string, results []scaffold.Result, force bool) {
	for _, r := range results {
		switch r.Action {
		case scaffold.ActionInstalled:
			fmt.Fprintf(stdout, "  installed    %s\n", r.Name)
		case scaffold.ActionOverwritten:
			fmt.Fprintf(stdout, "  overwritten  %s\n", r.Name)
		case scaffold.ActionUnchanged:
			fmt.Fprintf(stdout, "  unchanged    %s\n", r.Name)
		case scaffold.ActionSkipped:
			fmt.Fprintf(stdout, "  skipped      %s (differs from installed)\n", r.Name)
		}
	}
	fmt.Fprintf(stdout, "%s install: %d installed, %d overwritten, %d unchanged, %d skipped\n",
		label,
		scaffold.Count(results, scaffold.ActionInstalled),
		scaffold.Count(results, scaffold.ActionOverwritten),
		scaffold.Count(results, scaffold.ActionUnchanged),
		scaffold.Count(results, scaffold.ActionSkipped))
	if conflicts := scaffold.Conflicts(results); len(conflicts) > 0 && !force {
		fmt.Fprintf(stderr,
			"koryph: warning: %d %s already exist with different content and were left unchanged: %s\n",
			len(conflicts), label, strings.Join(conflicts, ", "))
		fmt.Fprintln(stderr, "koryph: re-run with --force to overwrite them.")
	}
}
