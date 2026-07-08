// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/plan"
	"github.com/koryph/koryph/internal/project"
)

func init() {
	registerCmd(command{
		name:    "plan",
		summary: "plan and analyze the project bead corpus",
		run:     cmdPlan,
		DocLinks: []string{
			"concepts/beads.md",
			"concepts/footprints.md",
		},
		subs: []command{
			{
				name:     "audit",
				summary:  "read-only corpus conflict analysis: footprint gaps, non-dispatchable beads, parallel width",
				run:      cmdPlanAudit,
				DocLinks: []string{"concepts/beads.md", "concepts/footprints.md"},
			},
		},
	})
}

// cmdPlan dispatches the plan sub-verbs.
func cmdPlan(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "plan", "plan and analyze the project bead corpus", []subVerb{
			{"audit --project ID [--json]", "deterministic corpus conflict analysis (read-only)"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "audit":
		return cmdPlanAudit(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown plan subcommand %q", sub))
	}
}

// cmdPlanAudit runs the read-only corpus conflict analysis and prints a report.
func cmdPlanAudit(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("plan audit", stderr)
	projectFlag := fs.String("project", "", "project id (default: the project containing the current directory)")
	asJSON := fs.Bool("json", false, "emit the audit report as JSON (for agent consumption)")
	setUsage(fs, stdout,
		"deterministic corpus conflict analysis — footprint gaps, skip reasons, conflicting pairs, parallel width",
		"[<id> | --project ID] [--json]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	posVal := ""
	if len(pos) > 0 {
		posVal = pos[0]
	}
	projectID, code := mergeProjectID(stderr, "plan audit", posVal, *projectFlag)
	if code != 0 {
		return code
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, code := resolveProjectRecordCwd(stderr, store, projectID, "plan audit")
	if code != 0 {
		return code
	}

	cfg, err := project.Load(rec.Root)
	if err != nil {
		return fail(stderr, err)
	}

	bd := beads.New(rec.Root)
	if v := os.Getenv("KORYPH_BD_BIN"); v != "" {
		bd.Bin = v
	}
	if !bd.Available() {
		return fail(stderr, fmt.Errorf("bd is not available on PATH; plan audit requires the bd binary"))
	}

	// Load all open issues (full corpus, not just the ready frontier).
	issues, err := bd.List(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("bd list: %w", err))
	}

	// Load the dependency graph for dependency-order checks.
	deps, err := bd.DepDigraph(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("bd list --format digraph: %w", err))
	}

	report := plan.Audit(issues, deps, cfg)

	if *asJSON {
		if err := printJSON(stdout, report); err != nil {
			return fail(stderr, err)
		}
		return 0
	}

	printAuditReport(stdout, report)
	return 0
}

// printAuditReport renders a human-readable corpus audit to w.
func printAuditReport(w io.Writer, r *plan.AuditReport) {
	fmt.Fprintf(w, "koryph plan audit — project %s\n", r.ProjectID)
	fmt.Fprintf(w, "open issues: %d\n\n", r.TotalOpen)

	// --- Unlabeled -----------------------------------------------------------
	fmt.Fprintf(w, "UNLABELED (domain:unknown) — %d\n", len(r.Unlabeled))
	if len(r.Unlabeled) > 0 {
		fmt.Fprintln(w, "  These beads serialize: only one unknown runs per wave.")
		fmt.Fprintln(w, "  Fix: add area:* or fp:* labels.")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, it := range r.Unlabeled {
			fmt.Fprintf(tw, "  %s\t%s\t(%s)\n", it.ID, truncate(it.Title, 60), it.IssueType)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	// --- Non-dispatchable ----------------------------------------------------
	fmt.Fprintf(w, "NON-DISPATCHABLE — %d\n", len(r.NonDispatch))
	if len(r.NonDispatch) > 0 {
		fmt.Fprintln(w, "  These will never dispatch as-is (structural type or label problem).")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, it := range r.NonDispatch {
			fmt.Fprintf(tw, "  %s\t%s\t→ %s\n", it.ID, truncate(it.Title, 50), it.Reason)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	// --- Conflicting pairs ---------------------------------------------------
	fmt.Fprintf(w, "DEPENDENCY-UNORDERED CONFLICTING PAIRS — %d\n", len(r.Conflicts))
	if len(r.Conflicts) > 0 {
		fmt.Fprintln(w, "  These pairs would block each other in the scheduler when both are ready.")
		fmt.Fprintln(w, "  Fix: add dependency ordering, use fp:read:* for read-only touches, or split.")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, cp := range r.Conflicts {
			fmt.Fprintf(tw, "  %s × %s\t[%s]\t(%s)\n",
				cp.A.ID, cp.B.ID,
				strings.Join(cp.SharedTokens, ", "),
				cp.Mode)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	// --- Width ---------------------------------------------------------------
	fmt.Fprintf(w, "PARALLEL WIDTH\n")
	fmt.Fprintf(w, "  current:   %d  (greedy, current footprint labels)\n", r.ParallelWidth.Current)
	fmt.Fprintf(w, "  potential: %d  (if all unlabeled beads were re-labeled)\n", r.ParallelWidth.Potential)
	fmt.Fprintln(w)

	// --- Stats ---------------------------------------------------------------
	fmt.Fprintf(w, "CORPUS STATS\n")
	fmt.Fprintf(w, "  refactor-core: %d  (authored on main; never loop-dispatched)\n", r.Stats.RefactorCore)
	fmt.Fprintf(w, "  no-dispatch:   %d  (manually deferred; remove label to re-enable)\n", r.Stats.NoDispatch)
	fmt.Fprintln(w)

	// Summary line
	problems := len(r.Unlabeled) + len(r.Conflicts)
	if problems == 0 && len(r.NonDispatch) == 0 {
		fmt.Fprintln(w, "✓ No corpus parallelism issues detected.")
	} else {
		fmt.Fprintf(w, "→ %d labeling/conflict issue(s) detected; %d non-dispatchable bead(s).\n",
			problems, len(r.NonDispatch))
		fmt.Fprintln(w, "  Run with --json for machine-readable output (e.g. for koryph-replan).")
	}
}

// truncate shortens s to at most n runes, appending "…" when truncated.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}
