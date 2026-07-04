// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/doctor"
)

// cmdDoctor runs health checks and prints a human table (or JSON with --json).
// Without --project it checks the global ~/.koryph installation.
// With --project <id> it checks a specific registered project.
// With --matrix it renders the integration matrix for the project at --root
//
//	(or the current working directory).
//
// Exit codes: 0 ok / 1 warnings / 2 errors.
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	jsonOut := fs.Bool("json", false, "emit the report as JSON instead of a table")
	fix := fs.Bool("fix", false, "auto-remediate: remove zombie slots/stale demand (global); install missing assets (project)")
	force := fs.Bool("force", false, "with --fix and --project: also overwrite stale asset files (default: only install missing)")
	projectID := fs.String("project", "", "run project-scoped checks for the named project")
	matrix := fs.Bool("matrix", false, "render the integration matrix for the project at --root (or current dir)")
	root := fs.String("root", ".", "project repository root for --matrix mode")
	setUsage(fs, stdout, "health check: layout, binaries, registry, governor, leases, quota, vaults, asset drift",
		"[--project ID] [--json] [--fix] [--force] [--matrix [--root PATH]]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	if *matrix {
		m, err := doctor.BuildMatrix(*root, doctor.MatrixOptions{})
		if err != nil {
			return fail(stderr, err)
		}
		if *jsonOut {
			if err := printJSON(stdout, m); err != nil {
				return fail(stderr, err)
			}
			return matrixExitCode(m)
		}
		printMatrixTable(stdout, m)
		return matrixExitCode(m)
	}

	var report *doctor.Report
	var err error
	if *projectID != "" {
		report, err = doctor.RunProject(doctor.ProjectOptions{
			ProjectID: *projectID,
			Fix:       *fix,
			Force:     *force,
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

// matrixExitCode maps matrix row statuses to an exit code:
// all ok → 0, any warn → 1, any missing → 2.
func matrixExitCode(m *doctor.Matrix) int {
	code := 0
	for _, row := range m.Rows {
		switch row.Status {
		case doctor.MatrixMissing:
			code = 2
		case doctor.MatrixWarn:
			if code < 1 {
				code = 1
			}
		}
	}
	return code
}

// printMatrixTable renders the integration matrix as a human-readable table.
func printMatrixTable(w io.Writer, m *doctor.Matrix) {
	fmt.Fprintf(w, "koryph doctor --matrix  root: %s  at: %s\n\n", m.Root, m.At)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tNAME\tSTATUS\tDETAIL\tSUGGESTION")
	for _, row := range m.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			row.Category, row.Name, string(row.Status), row.Detail, row.Suggestion)
	}
	_ = tw.Flush()

	code := matrixExitCode(m)
	switch code {
	case 0:
		fmt.Fprintln(w, "\nall integrations configured.")
	case 1:
		fmt.Fprintln(w, "\nsome integrations incomplete (exit 1).")
	case 2:
		fmt.Fprintln(w, "\nsome integrations missing (exit 2).")
	}
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
