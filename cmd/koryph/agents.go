// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/personas"
)

// cmdAgents dispatches the agents sub-verbs.
func cmdAgents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, "usage: koryph agents <install> ...")
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
// untouched unless --force is passed.
func cmdAgentsInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents install", stderr)
	force := fs.Bool("force", false, "overwrite existing personas whose content differs")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return engine.ExitUsage
	}
	if len(pos) < 1 {
		return usageErr(stderr, "agents install: <root> is required")
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
