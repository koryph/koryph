// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import "io"

// command is one node in the data-driven command tree. It is the single source
// of truth for the mux (run() dispatches through lookupCommand), for usage
// discovery, and for completion. A parent carries subs; a leaf carries none.
// run may be nil for a sub that only names a value (e.g. `completion bash`),
// in which case it contributes no flags to completion.
//
// DocLinks holds relative paths to docs pages that are conceptually relevant
// to this command (e.g. "user-guide/running-waves.md" for the run command).
// Paths are relative to the docs/ directory. The doc-generation tool
// (koryph __docgen) emits these as cross-links in docs/reference/cli.md.
// Commands with no DocLinks get a "TODO: add documentation links" warning in
// the generator output so coverage converges over time.
type command struct {
	name     string
	summary  string
	run      func([]string, io.Writer, io.Writer) int
	subs     []command
	DocLinks []string // relative paths under docs/ to cross-link pages

	// hidden keeps a still-dispatchable command out of the global usage()
	// listing, shell completion, and the generated CLI reference. Used for
	// back-compat aliases whose canonical form lives elsewhere (e.g. the
	// standalone agents/commands/rules installers, superseded by
	// `project install-assets`). lookupCommand still resolves it.
	hidden bool
}

// commandRegistry is the dynamic list of top-level koryph commands. Each
// command source file populates it from its own init() by calling registerCmd.
// The hidden __complete verb is handled directly in run() and intentionally
// absent here.
var commandRegistry []command

// registerCmd appends c to commandRegistry. It is called from init() functions
// in each command source file; registration order does not affect correctness
// (lookupCommand searches by name; completion candidates are sorted before
// output).
func registerCmd(c command) {
	commandRegistry = append(commandRegistry, c)
}

// lookupCommand returns the top-level command node with the given name, or nil.
func lookupCommand(name string) *command {
	for i := range commandRegistry {
		if commandRegistry[i].name == name {
			return &commandRegistry[i]
		}
	}
	return nil
}

// findSub returns c's subcommand with the given name, or nil.
func findSub(c *command, name string) *command {
	for i := range c.subs {
		if c.subs[i].name == name {
			return &c.subs[i]
		}
	}
	return nil
}
