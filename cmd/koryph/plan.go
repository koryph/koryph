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
		summary: "deterministic corpus conflict analysis and scoped epic quality gate",
		run:     cmdPlan,
		DocLinks: []string{
			"concepts/beads.md",
			"concepts/footprints.md",
		},
		subs: []command{
			{
				name:    "audit",
				summary: "read-only corpus conflict analysis and scoped epic quality gate",
				run:     cmdPlan,
				// koryph-b8g #24: 'plan' is a single-child noun group;
				// flattened so 'plan [<id>] [--json]' is the primary form.
				// The two-word 'plan audit ...' still works — hidden so it
				// doesn't clutter help/completion/docgen.
				hidden:   true,
				DocLinks: []string{"concepts/beads.md", "concepts/footprints.md"},
			},
		},
	})
}

// cmdPlan implements `koryph plan [<id> | --project ID] [--json]` — the
// read-only corpus conflict analysis. koryph-b8g #24: 'plan' was a
// single-child noun group ('plan audit ...'); flattened so the project id is
// a direct argument. The two-word 'plan audit ...' form still works as a
// hidden alias.
func cmdPlan(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "audit" {
		args = args[1:]
	}
	fs := newFlagSet("plan", stderr)
	projectFlag := fs.String("project", "", "project id (default: the project containing the current directory)")
	epicID := fs.String("epic", "", "scope analysis and quality checks to one epic")
	strict := fs.Bool("strict", false, "exit non-zero when the scoped epic has quality or scheduling errors")
	asJSON := fs.Bool("json", false, "emit the audit report as JSON (for agent consumption)")
	setUsage(fs, stdout,
		"deterministic corpus conflict analysis and scoped epic quality gate",
		"[<id> | --project ID] [--epic ID] [--strict] [--json]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	posVal := ""
	if len(pos) > 0 {
		posVal = pos[0]
	}
	projectID, code := mergeProjectID(stderr, "plan", posVal, *projectFlag)
	if code != 0 {
		return code
	}
	if *strict && *epicID == "" {
		return usageErr(stderr, "plan: --strict requires --epic")
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, code := resolveProjectRecordCwd(stderr, store, projectID, "plan")
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
		return fail(stderr, fmt.Errorf("bd is not available on PATH; plan requires the bd binary"))
	}

	// Load the dependency graph for dependency-order checks.
	deps, err := bd.DepDigraph(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("bd list --format digraph: %w", err))
	}

	var report *plan.AuditReport
	if *epicID != "" {
		epic, err := bd.Show(ctx, *epicID)
		if err != nil {
			return fail(stderr, fmt.Errorf("bd show %s: %w", *epicID, err))
		}
		children, err := bd.ListChildrenAll(ctx, *epicID)
		if err != nil {
			return fail(stderr, fmt.Errorf("bd list --parent %s: %w", *epicID, err))
		}
		report = plan.AuditEpic(epic, children, deps, cfg, rec.Root)
	} else {
		// Load all open issues (full corpus, not just the ready frontier).
		issues, err := bd.List(ctx)
		if err != nil {
			return fail(stderr, fmt.Errorf("bd list: %w", err))
		}
		report = plan.Audit(issues, deps, cfg)
	}

	if *asJSON {
		if err := printJSON(stdout, report); err != nil {
			return fail(stderr, err)
		}
	} else {
		printAuditReport(stdout, report)
	}
	if *strict && report.StrictFailure() {
		return 1
	}
	return 0
}

// printAuditReport renders a human-readable corpus audit to w.
func printAuditReport(w io.Writer, r *plan.AuditReport) {
	fmt.Fprintf(w, "koryph plan — project %s\n", r.ProjectID)
	if r.EpicID != "" {
		fmt.Fprintf(w, "epic: %s\n", r.EpicID)
	}
	fmt.Fprintf(w, "open issues: %d\n\n", r.TotalOpen)

	// --- Scoped quality findings --------------------------------------------
	if r.EpicID != "" {
		errors, warnings := 0, 0
		for _, finding := range r.Quality {
			if finding.Severity == "error" {
				errors++
			} else {
				warnings++
			}
		}
		fmt.Fprintf(w, "QUALITY GATE — %d error(s), %d warning(s)\n", errors, warnings)
		for _, finding := range r.Quality {
			fmt.Fprintf(w, "  %s  %s  [%s] %s\n",
				strings.ToUpper(finding.Severity), finding.IssueID, finding.Code, finding.Message)
			if finding.Remediation != "" {
				fmt.Fprintf(w, "    fix: %s\n", finding.Remediation)
			}
		}
		fmt.Fprintln(w)
	}

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

	// --- Derived-artifact co-footprint risks ---------------------------------
	fmt.Fprintf(w, "DERIVED-ARTIFACT CO-FOOTPRINT RISKS — %d\n", len(r.DerivedArtifactRisks))
	if len(r.DerivedArtifactRisks) > 0 {
		fmt.Fprintln(w, "  These pairs both touch a checked-in derived artifact (migrations lockfile,")
		fmt.Fprintln(w, "  secrets baseline) but are write-disjoint, so they may co-dispatch and collide")
		fmt.Fprintln(w, "  at merge. Fix: share a write token or order them; declare merge_reconcilers.")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, da := range r.DerivedArtifactRisks {
			fmt.Fprintf(tw, "  %s × %s\t(%s)\n", da.A.ID, da.B.ID, da.Keyword)
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
	problems := len(r.Unlabeled) + len(r.Conflicts) + len(r.DerivedArtifactRisks)
	for _, finding := range r.Quality {
		if finding.Severity == "error" {
			problems++
		}
	}
	if problems == 0 && len(r.NonDispatch) == 0 {
		if r.EpicID != "" {
			fmt.Fprintln(w, "✓ Epic passes deterministic planning quality checks.")
		} else {
			fmt.Fprintln(w, "✓ No corpus parallelism issues detected.")
		}
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
