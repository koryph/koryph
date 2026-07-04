// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/koryph/koryph/internal/personas"
	"github.com/koryph/koryph/internal/scaffold"
)

// cmdAgents dispatches the agents sub-verbs.
func cmdAgents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "agents", "manage the fallback koryph personas in a project", []subVerb{
			{"install <root> [--force]", "install fallback personas into <root>/.claude/agents"},
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
// every registered project instead of a single <root>.
func cmdAgentsInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents install", stderr)
	force := fs.Bool("force", false, "overwrite existing personas whose content differs")
	allProjects := fs.Bool("all-projects", false, "install into every registered project (registry-wide refresh)")
	setUsage(fs, stdout, "install fallback personas into <root>/.claude/agents (idempotent)",
		"(<root> | --all-projects) [--force]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}

	if *allProjects {
		if len(pos) > 0 {
			return usageErr(stderr, "agents install: <root> and --all-projects are mutually exclusive")
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

	results, err := personas.Install(root, *force)
	if err != nil {
		return fail(stderr, err)
	}
	reportInstall(stdout, stderr, "agents", results, *force)
	return 0
}
