// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/doctor"
	"github.com/koryph/koryph/internal/engine"
)

// cmdDoctor runs health checks and prints a human table (or JSON with --json).
// Without --project it checks the global ~/.koryph installation.
// With --project <id> it checks a specific registered project.
// Exit codes: 0 ok / 1 warnings / 2 errors.
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	jsonOut := fs.Bool("json", false, "emit the report as JSON instead of a table")
	fix := fs.Bool("fix", false, "remove zombie slot files and stale demand heartbeats")
	projectID := fs.String("project", "", "run project-scoped checks for the named project")
	if _, err := parseFlags(fs, args); err != nil {
		return engine.ExitUsage
	}

	var report *doctor.Report
	var err error
	if *projectID != "" {
		report, err = doctor.RunProject(doctor.ProjectOptions{
			ProjectID: *projectID,
			Fix:       *fix,
		})
	} else {
		report, err = doctor.Run(doctor.Options{Fix: *fix})
	}
	if err != nil {
		return fail(stderr, err)
	}

	if *jsonOut {
		if err := printJSON(stdout, report); err != nil {
			return fail(stderr, err)
		}
		return report.ExitCode()
	}

	printDoctorTable(stdout, report)
	return report.ExitCode()
}

// printDoctorTable renders the report as a human-readable table.
func printDoctorTable(w io.Writer, r *doctor.Report) {
	if r.Project != "" {
		fmt.Fprintf(w, "koryph doctor --project %s  root: %s  at: %s\n\n", r.Project, r.Home, r.At)
	} else {
		fmt.Fprintf(w, "koryph doctor  home: %s  at: %s\n\n", r.Home, r.At)
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tSTATUS\tMESSAGE")
	for _, f := range r.Findings {
		status := string(f.Level)
		if f.Fixed {
			status = "fixed"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Check, status, f.Message)
	}
	_ = tw.Flush()

	if r.FixedCount > 0 {
		fmt.Fprintf(w, "\n%d issue(s) fixed.\n", r.FixedCount)
	}

	code := r.ExitCode()
	switch code {
	case 0:
		fmt.Fprintln(w, "\nall checks passed.")
	case 1:
		fmt.Fprintln(w, "\nwarnings found (exit 1).")
	case 2:
		fmt.Fprintln(w, "\nerrors found (exit 2).")
	}
}
