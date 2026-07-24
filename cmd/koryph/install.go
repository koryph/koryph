// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/agentsmd"
	"github.com/koryph/koryph/internal/commands"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/personas"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/rules"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/scaffold"
)

func init() {
	registerCmd(command{
		name:    "commands",
		summary: "install canonical koryph workflows and runtime projections",
		run:     cmdCommands,
		// Superseded by `project install-assets commands`; hidden alias.
		hidden: true,
		DocLinks: []string{
			"user-guide/projects-and-accounts.md",
		},
		subs: []command{
			{
				name:     "install",
				summary:  "seed canonical commands and link a runtime projection",
				run:      cmdCommandsInstall,
				DocLinks: []string{"user-guide/projects-and-accounts.md"},
			},
		},
	})
	registerCmd(command{
		name:    "rules",
		summary: "install hook scripts + merge wiring",
		run:     cmdRules,
		// Superseded by `project install-assets rules`; hidden alias.
		hidden: true,
		DocLinks: []string{
			"user-guide/projects-and-accounts.md",
			"concepts/worktrees.md",
		},
		subs: []command{
			{
				name:     "install",
				summary:  "install hooks into a runtime's native configuration",
				run:      cmdRulesInstall,
				DocLinks: []string{"user-guide/projects-and-accounts.md"},
			},
		},
	})
}

// assetTargets is the canonical ordered set of koryph asset kinds that
// `project install-assets` (and `project add`) install. agentsmd comes first
// because it is the runtime-neutral file installed unconditionally; the others
// are capability-gated (see installAssetType's per-target notes).
var assetTargets = []string{"agentsmd", "agents", "commands", "rules"}

// cmdProjectInstallAssets is the canonical grouped installer: it (re)installs
// the koryph assets — canonical personas, workflows, and runtime-native hook
// wiring — that `project add` installs automatically. The
// optional positional target (agents|commands|rules|all, default all) narrows
// the set; --force overwrites differing files; --all installs into every
// registered project (the older --all-projects spelling still works as a
// hidden alias, koryph-b8g #18). The per-asset top-level verbs (agents
// install, commands install, rules install) remain as working aliases.
func cmdProjectInstallAssets(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project install-assets", stderr)
	force := fs.Bool("force", false, "overwrite existing assets whose content differs")
	allProjects := fs.Bool("all", false, "install into every registered project (registry-wide refresh)")
	fs.BoolVar(allProjects, "all-projects", false, "deprecated alias for --all")
	hideFlag(fs, "all-projects")
	setUsage(fs, stdout,
		"(re)install koryph assets (AGENTS.md, agents, commands & rules) — normally run automatically by `koryph project add`",
		"(<root> | --all) [agentsmd|agents|commands|rules|all] [--force]")
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
		case "agentsmd", "agents", "commands", "rules":
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
			return usageErr(stderr, "project install-assets: <root> and --all are mutually exclusive")
		}
		return installAssetsAllProjects(stdout, stderr, targets, *force)
	}
	if rootArg == "" {
		return usageErr(stderr, "project install-assets: <root> is required (or use --all)")
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
//
// Capability gating applies independently to every enabled runtime. Commands
// and personas are projected for all of them, not just default_runtime, so a
// project can be opened with Claude and Codex side by side while retaining one
// editable source under agents/ and commands/. Rules are rendered only for a
// runtime that declares native hook support; others rely on worktree isolation
// and merge-time protected-path refusal.
//
// agentsmd and agents are always installed: AGENTS.md is the runtime-neutral
// canonical instruction file, and personas render correctly for any runtime
// via InstallForRuntime.
func installAssetType(stdout, stderr io.Writer, root, target string, force bool) error {
	runtimeNames := installRuntimeNames(root)
	switch target {
	case "agentsmd":
		action, err := agentsmd.Install(root, force)
		if err != nil {
			return err
		}
		reportAgentsMD(stdout, stderr, action, force)
	case "agents":
		for _, runtimeName := range runtimeNames {
			results, untiered, err := personas.InstallForRuntime(root, force, runtimeName)
			if err != nil {
				return err
			}
			reportInstall(stdout, stderr, "agents ("+runtimeName+")", results, force)
			if len(untiered) > 0 {
				fmt.Fprintf(stderr, "koryph: note: %d persona(s) installed unchanged (no tier: frontmatter, or tier unmapped by runtime %s): %s\n",
					len(untiered), runtimeName, strings.Join(untiered, ", "))
			}
		}
	case "commands":
		for _, runtimeName := range runtimeNames {
			results, err := commands.InstallForRuntime(root, force, runtimeName)
			if err != nil {
				return err
			}
			reportInstall(stdout, stderr, "commands ("+runtimeName+")", results, force)
		}
	case "rules":
		for _, runtimeName := range runtimeNames {
			rt, ok := runtime.Default.Get(runtimeName)
			if !ok {
				return fmt.Errorf("runtime %q is not registered", runtimeName)
			}
			if !rt.Capabilities().Hooks {
				fmt.Fprintf(stdout, "rules install: skipped for %q (no native hooks; containment via worktree isolation + merge gate)\n", runtimeName)
				continue
			}
			hookResults, settings, err := rules.InstallForRuntime(root, force, runtimeName)
			if err != nil {
				return err
			}
			reportInstall(stdout, stderr, "hooks ("+runtimeName+")", hookResults, force)
			reportSettings(stdout, stderr, settings)
		}
	default:
		return fmt.Errorf("unknown asset target %q", target)
	}
	return nil
}

// installRuntimeNames selects every runtime whose projection belongs in this
// project. A missing or invalid config preserves historical Claude-only
// installation, rather than preventing recovery with `install-assets`.
func installRuntimeNames(root string) []string {
	cfg, err := project.Load(root)
	if err != nil {
		return []string{"claude"}
	}
	return cfg.EnabledRuntimeNames()
}

// cmdCommands dispatches the commands sub-verbs.
func cmdCommands(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "commands", "manage canonical koryph workflows and runtime-native links (normally run by `koryph project add`)", []subVerb{
			{"install <root> [--force]", "seed commands/ and install the selected runtime projection"},
			{"install --all [--force]", "install canonical workflows into every registered project"},
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
// merges the wiring into the configured runtime's native hook configuration.
func cmdRulesInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("rules install", stderr)
	force := fs.Bool("force", false, "overwrite differing hook scripts; rebuild an unparseable settings.json")
	setUsage(fs, stdout, "install hook scripts + merge native runtime hook wiring (normally run automatically by `koryph project add`)", "<root> [--force]")
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
	hookResults, settings, err := rules.InstallForRuntime(root, *force, resolveInstallRuntime(root, ""))
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
		fmt.Fprintln(stderr, "koryph: warning: native hook configuration is unparseable or has an incompatible shape — left unchanged; fix it or re-run with --force to rebuild.")
	}
}

// The former onboardInstallAgentsMD/onboardRules/onboardInstall helpers moved
// to internal/adopt (InstallAssets and friends) so `project add` and `koryph
// adopt` share one install path.

// cmdCommandsInstall seeds canonical commands/*.md and links it into the
// selected runtime's native workflow surface. With --all, each project uses
// its own configured runtime (the older --all-projects spelling still works
// as a hidden alias).
func cmdCommandsInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("commands install", stderr)
	force := fs.Bool("force", false, "overwrite existing commands whose content differs")
	allProjects := fs.Bool("all", false, "install into every registered project (registry-wide refresh)")
	runtimeName := fs.String("runtime", "", "target runtime name (default: <root>'s default_runtime, else claude)")
	fs.BoolVar(allProjects, "all-projects", false, "deprecated alias for --all")
	hideFlag(fs, "all-projects")
	setUsage(fs, stdout,
		"seed canonical commands/*.md and link a runtime projection (idempotent; normally run automatically by `koryph project add`)",
		"(<root> | --all) [--runtime NAME] [--force]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}

	if *allProjects {
		if len(pos) > 0 {
			return usageErr(stderr, "commands install: <root> and --all are mutually exclusive")
		}
		if *runtimeName != "" {
			return usageErr(stderr, "commands install: --runtime and --all are mutually exclusive")
		}
		return installAllProjects(stdout, stderr, "commands", *force,
			func(root string, force bool) ([]scaffold.Result, error) {
				return commands.InstallForRuntime(root, force, resolveInstallRuntime(root, ""))
			})
	}

	if len(pos) < 1 {
		return usageErr(stderr, "commands install: <root> is required (or use --all)")
	}
	root, err := filepath.Abs(pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	results, err := commands.InstallForRuntime(root, *force, resolveInstallRuntime(root, *runtimeName))
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

// reportAgentsMD reports the outcome of an AGENTS.md install step.
func reportAgentsMD(stdout, stderr io.Writer, action string, force bool) {
	switch action {
	case scaffold.ActionInstalled:
		fmt.Fprintln(stdout, "agentsmd install: installed AGENTS.md (koryph operating contract)")
	case scaffold.ActionOverwritten:
		fmt.Fprintln(stdout, "agentsmd install: overwritten AGENTS.md")
	case scaffold.ActionUnchanged:
		fmt.Fprintln(stdout, "agentsmd install: unchanged (already up-to-date)")
	case scaffold.ActionSkipped:
		fmt.Fprintln(stdout, "agentsmd install: skipped AGENTS.md (differs from template)")
		if !force {
			fmt.Fprintln(stderr, "koryph: warning: AGENTS.md already exists with different content — left unchanged; re-run with --force to overwrite.")
		}
	}
}
