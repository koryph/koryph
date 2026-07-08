// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/registry"
)

// resolveProjectRecord resolves the single registry Record a command should act
// on, applying the same default-from-cwd rule as `koryph tui`:
//
//   - explicit id (from --project, or a positional merged in by the caller) →
//     store.Get(id)
//   - empty id → the registered project whose repo root contains cwd
//     (store.FindByPath); a repo added to koryph, or any subdirectory of one,
//     resolves to that project.
//   - neither → a usage error that lists the registered projects and tells the
//     operator to name one or cd into a project.
//
// It returns (record, 0) on success, or (nil, exitcode) after writing guidance
// to stderr. cmd is the command name used in messages (e.g. "roster"); cwd is
// passed explicitly (rather than read here) so tests drive the default-
// resolution path deterministically. This is the shared resolver every
// project-scoped command uses so `--project` is optional whenever the operator
// is already inside the project's tree. Callers that read cwd from the process
// should use resolveProjectRecordCwd.
func resolveProjectRecord(stderr io.Writer, store *registry.Store, id, cwd, cmd string) (*registry.Record, int) {
	if id != "" {
		rec, err := store.Get(id)
		if err != nil {
			return nil, fail(stderr, fmt.Errorf("%s: project %q not found: %w", cmd, id, err))
		}
		return rec, 0
	}

	rec, err := store.FindByPath(cwd)
	if err != nil {
		return nil, fail(stderr, err)
	}
	if rec == nil {
		return nil, projectSelectHint(stderr, store, cmd)
	}
	return rec, 0
}

// resolveProjectRecordCwd is resolveProjectRecord with cwd read from the
// process. It is the form command handlers call; the cwd-explicit variant
// exists for deterministic tests.
func resolveProjectRecordCwd(stderr io.Writer, store *registry.Store, id, cmd string) (*registry.Record, int) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fail(stderr, fmt.Errorf("%s: resolve current directory: %w", cmd, err))
	}
	return resolveProjectRecord(stderr, store, id, cwd, cmd)
}

// mergeProjectID reconciles a positional project id with a --project flag for
// commands that accept the id either way, returning the chosen id. Unlike
// resolveProjectID it does NOT require one: an empty result means "neither was
// given" and signals the caller to fall back to cwd resolution (via
// resolveProjectRecordCwd). A conflict — both set to different values — is a
// usage error.
func mergeProjectID(stderr io.Writer, cmd, posVal, flagVal string) (string, int) {
	if posVal != "" && flagVal != "" && posVal != flagVal {
		return "", usageErr(stderr,
			cmd+": positional <id> "+posVal+" and --project "+flagVal+" conflict; pass only one")
	}
	if flagVal != "" {
		return flagVal, 0
	}
	return posVal, 0
}

// projectSelectHint reports that no project could be resolved — neither a
// --project flag nor the current directory identifies one — and returns the
// usage exit code. When any projects are registered it lists their ids and
// roots so a valid choice is one glance away. The final line always contains
// "--project is required" so callers share one recognizable phrasing.
func projectSelectHint(stderr io.Writer, store *registry.Store, cmd string) int {
	recs, err := store.List()
	if err == nil && len(recs) > 0 {
		fmt.Fprintf(stderr, "%s: the current directory is not inside a registered koryph project.\n", cmd)
		fmt.Fprintln(stderr, "registered projects:")
		tw := tabwriter.NewWriter(stderr, 0, 0, 2, ' ', 0)
		for _, rec := range recs {
			fmt.Fprintf(tw, "  %s\t%s\n", rec.ProjectID, rec.Root)
		}
		tw.Flush()
	}
	return usageErr(stderr,
		cmd+": --project is required — name one with --project ID, or run inside a registered project directory")
}
