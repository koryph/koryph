// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/koryph/koryph/internal/cockpit"
	"github.com/koryph/koryph/internal/registry"
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
//
// Project selection, in precedence order:
//   - --project ID       — that single project.
//   - --all-projects / -a — every registered project (aggregate cockpit).
//   - neither            — the project whose repo root contains the current
//     directory (a repo added to koryph, or any subdirectory of one). Outside
//     every registered root we cannot guess which project is meant, so we ask
//     the operator to name one rather than silently opening an unrelated
//     cockpit.
func cmdTUI(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("tui", stderr)
	projectID := fs.String("project", "", "project id (default: the project containing the current directory)")
	allProjects := fs.Bool("all-projects", false, "show every registered project (aggregate cockpit)")
	fs.BoolVar(allProjects, "a", false, "shorthand for --all-projects")
	readOnly := fs.Bool("read-only", false, "disable write actions (nudge, drain) — safe for shared/observer sessions")
	setUsage(fs, stdout, "interactive terminal cockpit — threads, queue, events, efficiency",
		"[--project ID | --all-projects] [--read-only]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	if *projectID != "" && *allProjects {
		return usageErr(stderr, "tui: --project and --all-projects are mutually exclusive")
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fail(stderr, fmt.Errorf("tui: resolve current directory: %w", err))
	}

	recs, code := resolveTUIProjects(stderr, store, *projectID, *allProjects, cwd)
	if code != 0 {
		return code
	}

	var providers []cockpit.Provider
	for _, rec := range recs {
		providers = append(providers, cockpit.NewLedgerProvider(rec.ProjectID, rec.Root, rec.AccountProfile))
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

// resolveTUIProjects chooses which project records the cockpit will display,
// from the flag selection and the current directory. On success it returns
// (records, 0); on any failure it writes guidance to stderr and returns
// (nil, exitcode). Split out from cmdTUI so the selection logic is unit-tested
// without launching the interactive program (which needs a TTY). cwd is passed
// explicitly rather than read here so tests can drive the default-resolution
// path deterministically.
func resolveTUIProjects(stderr io.Writer, store *registry.Store, projectID string, allProjects bool, cwd string) ([]*registry.Record, int) {
	switch {
	case projectID != "":
		rec, err := store.Get(projectID)
		if err != nil {
			return nil, fail(stderr, fmt.Errorf("tui: project %q not found: %w", projectID, err))
		}
		return []*registry.Record{rec}, 0

	case allProjects:
		recs, err := store.List()
		if err != nil {
			return nil, fail(stderr, err)
		}
		if len(recs) == 0 {
			fmt.Fprintln(stderr, "tui: no projects registered — run `koryph init` first")
			return nil, 1
		}
		return recs, 0

	default:
		rec, err := store.FindByPath(cwd)
		if err != nil {
			return nil, fail(stderr, err)
		}
		if rec == nil {
			return nil, tuiSelectProjectHint(stderr, store)
		}
		return []*registry.Record{rec}, 0
	}
}

// tuiSelectProjectHint reports that the current directory is outside every
// registered project and tells the operator how to pick one: name it with
// --project ID, or use --all-projects (-a) to view them all. The registered
// ids and roots are listed so a valid choice is one glance away. Returns the
// usage exit code.
func tuiSelectProjectHint(stderr io.Writer, store *registry.Store) int {
	fmt.Fprintln(stderr, "tui: the current directory is not inside a registered koryph project.")
	recs, err := store.List()
	if err != nil || len(recs) == 0 {
		return usageErr(stderr, "no projects registered — run `koryph init` first")
	}
	fmt.Fprintln(stderr, "registered projects:")
	tw := tabwriter.NewWriter(stderr, 0, 0, 2, ' ', 0)
	for _, rec := range recs {
		fmt.Fprintf(tw, "  %s\t%s\n", rec.ProjectID, rec.Root)
	}
	tw.Flush()
	return usageErr(stderr, "specify one with --project ID, or use --all-projects (-a) to view them all")
}
