// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/koryph/koryph/internal/ciinstall"
	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/forge/github"
	"github.com/koryph/koryph/internal/forge/gitlab"
	"github.com/koryph/koryph/internal/project"
)

func init() {
	registerCmd(command{
		name:    "ci",
		summary: "render and install forge-native CI pipeline assets",
		run:     cmdCI,
		DocLinks: []string{
			"user-guide/ci-setup.md",
		},
		subs: []command{
			{
				name:     "setup",
				summary:  "render and install CI assets into the project",
				run:      cmdCISetup,
				DocLinks: []string{"user-guide/ci-setup.md"},
			},
			{
				name:     "check",
				summary:  "report drift between installed CI assets and current Render output",
				run:      cmdCICheck,
				DocLinks: []string{"user-guide/ci-setup.md"},
			},
		},
	})
}

// cmdCI dispatches the ci sub-verbs.
func cmdCI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "ci", "render and install forge-native CI pipeline assets", []subVerb{
			{"setup --project ID [--kind gate|scanner|all] [--gate-cmd CMD]", "render and install CI assets (idempotent)"},
			{"check --project ID [--kind gate|scanner|all] [--gate-cmd CMD]", "report drift between installed assets and current render; exit 1 on drift"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "setup":
		return cmdCISetup(rest, stdout, stderr)
	case "check":
		return cmdCICheck(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown ci subcommand %q", sub))
	}
}

// cmdCISetup implements `koryph ci setup [--project ID] [--kind gate|scanner|all]
// [--gate-cmd CMD]`. It resolves the project's forge from koryph.project.json,
// renders the requested CI kinds via forge.CI().Render, and writes them to the
// forge-native paths. Never commits; prints commit guidance instead.
func cmdCISetup(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("ci setup", stderr)
	flagProject := fs.String("project", "", "project id")
	flagKind := fs.String("kind", "gate", "CI asset kind(s) to install: gate, scanner, or all")
	flagGateCmd := fs.String("gate-cmd", "", "override the gate command (default: make gate)")
	setUsage(fs, stdout,
		"render and install forge-native CI pipeline assets into the project (idempotent)",
		"[--project ID] [--kind gate|scanner|all] [--gate-cmd CMD]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	// Accept a bare positional as --project (same convention as release setup).
	posVal := ""
	if len(pos) > 0 {
		posVal = pos[0]
	}

	id, code := resolveProjectID(stderr, "ci setup", posVal, *flagProject)
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

	cfg, err := project.Load(rec.Root)
	if err != nil {
		return fail(stderr, err)
	}

	kinds, code := resolveKinds("ci setup", *flagKind, stderr)
	if code != 0 {
		return code
	}

	f, ok := forge.Default.Get(cfg.ResolvedForge())
	if !ok {
		return fail(stderr, fmt.Errorf("ci setup: unknown forge %q (check koryph.project.json)", cfg.ResolvedForge()))
	}

	// Build a provider with the gate-cmd override when specified.
	ci := buildCIService(f, cfg, *flagGateCmd)

	anyInstalled := false
	anyUnsupported := false
	for _, kind := range kinds {
		res, ierr := ciinstall.Install(rec.Root, cfg.ResolvedForge(), ci, kind)
		if ierr != nil {
			return fail(stderr, fmt.Errorf("ci setup: %w", ierr))
		}
		switch res.Action {
		case ciinstall.ActionInstalled:
			fmt.Fprintf(stdout, "  installed    %s\n", res.Path)
			anyInstalled = true
		case ciinstall.ActionUnchanged:
			fmt.Fprintf(stdout, "  unchanged    %s (already up-to-date)\n", res.Path)
		case ciinstall.ActionUnsupported:
			fmt.Fprintf(stdout, "  unsupported  %s (forge %q does not support kind %q yet)\n",
				kind, cfg.ResolvedForge(), kind)
			anyUnsupported = true
		}
	}

	if !anyInstalled && !anyUnsupported {
		fmt.Fprintln(stdout, "ci setup: all assets are already up-to-date — nothing to do.")
		return 0
	}

	// GitLab: print include guidance for fragments that were written.
	if cfg.ResolvedForge() == "gitlab" && anyInstalled {
		fmt.Fprintln(stdout, "\nGitLab: add the installed fragment(s) to .gitlab-ci.yml with:")
		for _, kind := range kinds {
			path, ok := ciinstall.KindPath("gitlab", kind)
			if ok {
				fmt.Fprintf(stdout, "  include:\n    - local: '%s'\n", path)
			}
		}
	}

	// Commit guidance — same UX as koryph release setup.
	fmt.Fprintln(stdout, "\nRemaining HUMAN steps:")
	fmt.Fprintln(stdout, "  1. Review the installed file(s) above.")
	fmt.Fprintln(stdout, "  2. git add <paths above> && git commit -s -m 'ci: install koryph CI assets'")
	if cfg.ResolvedForge() == "github" {
		fmt.Fprintln(stdout, "  3. Push and open a PR — GitHub will run the gate workflow on every push and PR.")
	} else if cfg.ResolvedForge() == "gitlab" {
		fmt.Fprintln(stdout, "  3. Add the include: entries to .gitlab-ci.yml, then push and open an MR.")
	}

	return 0
}

// cmdCICheck implements `koryph ci check [--project ID] [--kind gate|scanner|all]
// [--gate-cmd CMD]`. It compares installed CI assets against the current Render
// output and exits 1 when any asset is missing or has drifted.
func cmdCICheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("ci check", stderr)
	flagProject := fs.String("project", "", "project id")
	flagKind := fs.String("kind", "gate", "CI asset kind(s) to check: gate, scanner, or all")
	flagGateCmd := fs.String("gate-cmd", "", "override the gate command (default: make gate)")
	setUsage(fs, stdout,
		"report drift between installed CI pipeline assets and current render; exit 1 on drift",
		"[--project ID] [--kind gate|scanner|all] [--gate-cmd CMD]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	posVal := ""
	if len(pos) > 0 {
		posVal = pos[0]
	}

	id, code := resolveProjectID(stderr, "ci check", posVal, *flagProject)
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

	cfg, err := project.Load(rec.Root)
	if err != nil {
		return fail(stderr, err)
	}

	kinds, code := resolveKinds("ci check", *flagKind, stderr)
	if code != 0 {
		return code
	}

	f, ok := forge.Default.Get(cfg.ResolvedForge())
	if !ok {
		return fail(stderr, fmt.Errorf("ci check: unknown forge %q", cfg.ResolvedForge()))
	}

	ci := buildCIService(f, cfg, *flagGateCmd)

	hasDrift := false
	for _, kind := range kinds {
		res, cerr := ciinstall.Check(rec.Root, cfg.ResolvedForge(), ci, kind)
		if cerr != nil {
			return fail(stderr, fmt.Errorf("ci check: %w", cerr))
		}
		switch {
		case res.Action == ciinstall.ActionUnsupported:
			fmt.Fprintf(stdout, "  skip         %s (forge %q does not support kind %q)\n",
				kind, cfg.ResolvedForge(), kind)
		case res.HasDrift:
			fmt.Fprintf(stdout, "  DRIFT        %s\n", res.Path)
			hasDrift = true
		default:
			fmt.Fprintf(stdout, "  ok           %s\n", res.Path)
		}
	}

	if hasDrift {
		fmt.Fprintln(stderr, "koryph: ci check: drift detected — run `koryph ci setup --project "+id+"` to update")
		return 1
	}
	fmt.Fprintln(stdout, "ci check: all CI assets are current.")
	return 0
}

// resolveKinds expands the --kind flag value into a list of kind strings.
// "all" expands to ciinstall.AllKinds. Returns a non-nil list and code 0 on
// success, or a usage error exit code on an unrecognised value.
func resolveKinds(cmd, kindFlag string, stderr io.Writer) ([]string, int) {
	switch kindFlag {
	case "all":
		return ciinstall.AllKinds, 0
	case "gate", "scanner", "caller", "docs", "release":
		return []string{kindFlag}, 0
	default:
		return nil, usageErr(stderr, fmt.Sprintf("%s: unknown --kind value %q (want gate|scanner|all)", cmd, kindFlag))
	}
}

// buildCIService constructs a forge.CIService from the project's forge provider
// with optional gate-command override. For GitHub and GitLab providers, a
// project-scoped provider instance is built with the gate command wired; for
// other providers the base CI() service is returned unchanged.
func buildCIService(f forge.Forge, cfg *project.Config, gateCmd string) forge.CIService {
	// Nothing project-scoped to inject (no gate override, no per-project
	// copyright) → the base provider service, unchanged.
	if gateCmd == "" && cfg.Copyright == nil {
		return f.CI()
	}
	switch f.Name() {
	case "github":
		var opts []github.Option
		if gateCmd != "" {
			opts = append(opts, github.WithGateCommand(gateCmd))
		}
		if cfg.Release != nil {
			opts = append(opts, github.WithReleaseConfig(cfg.Release))
		}
		if cfg.Copyright != nil {
			opts = append(opts, github.WithCopyright(cfg.Copyright))
		}
		return github.New(opts...).CI()
	case "gitlab":
		var opts []gitlab.Option
		if gateCmd != "" {
			opts = append(opts, gitlab.WithGateCommand(gateCmd))
		}
		if cfg.Release != nil {
			opts = append(opts, gitlab.WithReleaseConfig(cfg.Release))
		}
		if cfg.Copyright != nil {
			opts = append(opts, gitlab.WithCopyright(cfg.Copyright))
		}
		return gitlab.New(opts...).CI()
	}
	return f.CI()
}
