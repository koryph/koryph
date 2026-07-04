// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/personas"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/scaffold"
)

// cmdAgents dispatches the agents sub-verbs.
func cmdAgents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "agents", "manage the fallback koryph personas in a project (normally run by `koryph project add`)", []subVerb{
			{"install <root> [--runtime NAME] [--force]", "install fallback personas into <root>/.claude/agents"},
			{"install --all-projects [--force]", "install fallback personas into every registered project"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return cmdAgentsInstall(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown agents subcommand %q", sub))
	}
}

// cmdAgentsInstall writes the fallback personas into <root>/.claude/agents.
// Identical files are a no-op; a persona whose content differs is left
// untouched unless --force is passed. With --all-projects, installs into
// every registered project instead of a single <root> (always the verbatim
// claude rendering — --runtime is not supported alongside --all-projects
// today, koryph-v8u.12).
//
// --runtime renders each persona's frontmatter `model:` pin for a target
// runtime other than claude (koryph-v8u.12; see
// internal/personas.InstallForRuntime). Unset defaults to <root>'s
// koryph.project.json default_runtime when one is present and readable,
// else "claude" — so a project onboarded under a non-claude default_runtime
// gets correctly-rendered personas without the operator having to pass the
// flag by hand.
func cmdAgentsInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents install", stderr)
	force := fs.Bool("force", false, "overwrite existing personas whose content differs")
	allProjects := fs.Bool("all-projects", false, "install into every registered project (registry-wide refresh)")
	runtimeName := fs.String("runtime", "", "target runtime name (default: <root>'s default_runtime, else \"claude\")")
	setUsage(fs, stdout,
		"install fallback personas into <root>/.claude/agents (idempotent; normally run automatically by `koryph project add`)",
		"(<root> | --all-projects) [--runtime NAME] [--force]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}

	if *allProjects {
		if len(pos) > 0 {
			return usageErr(stderr, "agents install: <root> and --all-projects are mutually exclusive")
		}
		if *runtimeName != "" {
			return usageErr(stderr, "agents install: --runtime and --all-projects are mutually exclusive")
		}
		return installAllProjects(stdout, stderr, "agents", *force,
			func(root string, force bool) ([]scaffold.Result, error) {
				return personas.Install(root, force)
			})
	}

	if len(pos) < 1 {
		return usageErr(stderr, "agents install: <root> is required (or use --all-projects)")
	}
	root, err := filepath.Abs(pos[0])
	if err != nil {
		return fail(stderr, err)
	}

	results, untiered, err := personas.InstallForRuntime(root, *force, resolveInstallRuntime(root, *runtimeName))
	if err != nil {
		return fail(stderr, err)
	}
	reportInstall(stdout, stderr, "agents", results, *force)
	if len(untiered) > 0 {
		fmt.Fprintf(stderr, "koryph: note: %d persona(s) installed unchanged (no tier: frontmatter, or tier unmapped by the target runtime): %s\n",
			len(untiered), strings.Join(untiered, ", "))
	}
	return 0
}

// resolveInstallRuntime picks the target runtime for a persona install
// (koryph-v8u.12): an explicit --runtime flag always wins; absent that, a
// readable koryph.project.json's default_runtime; absent BOTH, "claude" —
// InstallForRuntime itself already treats "" as "claude", so this mainly
// exists to consult the project config when the flag is not given. A
// missing/unreadable config (e.g. before `project add` has scaffolded one)
// is silently treated as "claude", matching every pre-koryph-v8u.12 caller.
func resolveInstallRuntime(root, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if cfg, err := project.Load(root); err == nil && cfg.DefaultRuntime != "" {
		return cfg.DefaultRuntime
	}
	return ""
}
