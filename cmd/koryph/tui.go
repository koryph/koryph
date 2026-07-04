// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/koryph/koryph/internal/cockpit"
	"github.com/koryph/koryph/internal/tui"
)

func init() {
	registerCmd(command{
		name:    "tui",
		summary: "interactive terminal cockpit (threads, queue, events)",
		run:     cmdTUI,
		// DocLinks intentionally empty until docs/user-guide/tui.md exists
		// (koryph-9af.6): the reference generator publishes these as links,
		// design docs are excluded from the published book, and zensical
		// --strict fails the docs build on any dead link.
		DocLinks: nil,
	})
}

// cmdTUI launches the interactive terminal cockpit.
func cmdTUI(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("tui", stderr)
	projectID := fs.String("project", "", "project id (default: all registered projects)")
	readOnly := fs.Bool("read-only", false, "disable write actions (nudge, drain) — safe for shared/observer sessions")
	setUsage(fs, stdout, "interactive terminal cockpit — threads, queue, events, efficiency",
		"[--project ID] [--read-only]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}

	// Build the list of providers. If --project is given, use only that one;
	// otherwise include every registered project.
	var providers []cockpit.Provider

	if *projectID != "" {
		rec, err := store.Get(*projectID)
		if err != nil {
			return fail(stderr, fmt.Errorf("tui: project %q not found: %w", *projectID, err))
		}
		providers = append(providers, cockpit.NewLedgerProvider(rec.ProjectID, rec.Root, rec.AccountProfile))
	} else {
		recs, err := store.List()
		if err != nil {
			return fail(stderr, err)
		}
		for _, rec := range recs {
			providers = append(providers, cockpit.NewLedgerProvider(rec.ProjectID, rec.Root, rec.AccountProfile))
		}
	}

	if len(providers) == 0 {
		fmt.Fprintln(stderr, "tui: no projects registered — run `koryph init` first")
		return 1
	}

	app := tui.NewApp(providers, *readOnly)
	p := tea.NewProgram(app,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := p.Run(); err != nil {
		return fail(stderr, fmt.Errorf("tui: %w", err))
	}
	return 0
}
