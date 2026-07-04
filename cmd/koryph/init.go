// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/paths"
)

func init() {
	registerCmd(command{
		name:    "init",
		summary: "create ~/.koryph, verify tools on PATH, print next steps",
		run:     cmdInit,
	})
}

// cmdInit creates ~/.koryph (idempotent), verifies required tools on PATH,
// and prints next steps so a fresh collaborator can start immediately.
func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("init", stderr)
	setUsage(fs, stdout, "create ~/.koryph, verify tools on PATH, print next steps (idempotent)", "")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	home := paths.KoryphHome()

	// Initialize the registry. Init is idempotent: re-running it produces no
	// extra commits and leaves all existing records intact.
	if _, err := openStore(ctx); err != nil {
		return fail(stderr, err)
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "home\t%s\tok\n", home)
	tw.Flush()

	// Verify tools on PATH; print found-path or "not found". Missing claude/bd
	// are warnings — the command still exits 0 so new collaborators can
	// bootstrap incrementally.
	fmt.Fprintln(stdout)
	missing := false
	tw = tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, tool := range []string{"git", "claude", "bd"} {
		if p, err := exec.LookPath(tool); err == nil {
			fmt.Fprintf(tw, "%s\t%s\tok\n", tool, p)
		} else {
			fmt.Fprintf(tw, "%s\t-\tnot found\n", tool)
			missing = true
		}
	}
	tw.Flush()

	fmt.Fprint(stdout, initNextSteps)
	if missing {
		fmt.Fprintln(stdout, "note: install missing tools before running `koryph project add`.")
	}
	return 0
}

// initNextSteps is the human-readable guide printed after a successful init.
const initNextSteps = `
Next steps:
  1. Register your first project:
       koryph project add <path/to/repo> --account personal --identity you@example.com

  2. Run the pre-dispatch gate:
       koryph validate <project-id>

  3. Start a run:
       koryph run --project <project-id>

`
