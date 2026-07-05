// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/gc"
)

func init() {
	registerCmd(command{
		name:    "gc",
		summary: "apply data lifecycle policy: compress old run dirs, rotate audit logs",
		run:     cmdGC,
		DocLinks: []string{
			"user-guide/gc.md",
		},
	})
}

// cmdGC applies the retention policy for run phase-dirs and audit/runs logs.
//
//	koryph gc [--dry-run] [--project ID]
//
// Without --project, only the global artifact classes (audit.jsonl, runs.jsonl)
// are processed. With --project, run phase-dirs for that project are also
// processed.
//
// --dry-run reports what would be done without making any changes.
// Exit codes: 0 ok / 1 warnings or non-fatal errors.
func cmdGC(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("gc", stderr)
	dryRun := fs.Bool("dry-run", false, "report without making any changes")
	projectID := fs.String("project", "", "apply run-dirs gc for this project")
	jsonOut := fs.Bool("json", false, "emit the result as JSON")
	setUsage(fs, stdout,
		"apply data lifecycle policy: compress old run dirs, rotate audit logs",
		"[--dry-run] [--project ID] [--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	var repoRoot string
	if *projectID != "" {
		store, err := openStore(context.Background())
		if err != nil {
			return fail(stderr, err)
		}
		rec, err := store.Get(*projectID)
		if err != nil {
			return fail(stderr, err)
		}
		repoRoot = rec.Root
	}

	opts := gc.Options{
		RepoRoot: repoRoot,
		DryRun:   *dryRun,
	}

	res, err := gc.Run(opts)
	if err != nil {
		return fail(stderr, err)
	}

	if *jsonOut {
		if jerr := printJSON(stdout, res); jerr != nil {
			return fail(stderr, jerr)
		}
		return gcExitCode(res)
	}

	printGCTable(stdout, res)
	return gcExitCode(res)
}

// gcExitCode: 0 if no errors, 1 if any ClassResult has errors.
func gcExitCode(res *gc.Result) int {
	for _, c := range res.Classes {
		if len(c.Errors) > 0 {
			return 1
		}
	}
	return 0
}

// printGCTable renders a gc result as a human-readable table.
func printGCTable(w io.Writer, res *gc.Result) {
	dryTag := ""
	if res.DryRun {
		dryTag = " (dry-run)"
	}
	fmt.Fprintf(w, "koryph gc%s  at: %s\n\n", dryTag, res.At)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CLASS\tSCANNED MB\tCOMPRESSED\tDELETED\tSKIPPED\tRECLAIMED MB\tERRORS")
	for _, c := range res.Classes {
		fmt.Fprintf(tw, "%s\t%.1f\t%d\t%d\t%d\t%.1f\t%d\n",
			c.Class, c.ScannedMB, c.Compressed, c.Deleted, c.Skipped,
			c.ReclaimedMB, len(c.Errors))
	}
	_ = tw.Flush()

	total := res.TotalReclaimedMB()
	if res.DryRun {
		fmt.Fprintf(w, "\ndry-run: would reclaim %.1f MB\n", total)
	} else {
		fmt.Fprintf(w, "\nreclaimed %.1f MB\n", total)
	}

	// Print any errors.
	for _, c := range res.Classes {
		for _, e := range c.Errors {
			fmt.Fprintf(w, "gc warning [%s]: %s\n", c.Class, e)
		}
	}
}
