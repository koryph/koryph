// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
)

func init() {
	registerCmd(command{
		name:    "run",
		summary: "execute one engine run over a project",
		run:     cmdRun,
		DocLinks: []string{
			"user-guide/running-waves.md",
			"concepts/rolling-dispatch.md",
			"concepts/beads.md",
		},
	})
	registerCmd(command{
		name:    "board",
		summary: "one-line-per-project run overview",
		run:     cmdBoard,
		DocLinks: []string{
			"user-guide/running-waves.md",
		},
	})
	registerCmd(command{
		name:    "status",
		summary: "latest-run per-slot detail",
		run:     cmdStatus,
		DocLinks: []string{
			"user-guide/running-waves.md",
		},
	})
}

// cmdRun executes one engine run over a project.
func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("run", stderr)
	project := fs.String("project", "", "project id (default: the project containing the current directory)")
	once := fs.Bool("once", false, "run exactly one wave")
	max := fs.Int("max", 0, "wave width cap (0 = project/engine default)")
	parent := fs.String("parent", "", "epic scope for the bd frontier")
	only := fs.String("only", "", "dispatch only this specific ready bead id")
	budget := fs.Float64("budget", 0, "per-run cost ceiling in USD (0 = unlimited)")
	defaultModel := fs.String("default-model", "", "model for label-less beads")
	autoMerge := fs.Bool("auto-merge", false, "allow auto-merge for merge:auto items")
	direct := fs.Bool("direct", false, "owner override: skip PRs and merge straight to the default branch (needs branch-protection bypass)")
	dryRun := fs.Bool("dry-run", false, "plan and print without dispatching")
	resume := fs.Bool("resume", false, "classify and re-dispatch the latest run first")
	review := fs.Bool("review", false, "post-implementation review pass before merge")
	allowAPISpend := fs.Bool("allow-api-spend", false, "permit api-key billing at governor stop")
	allowUnvalidated := fs.Bool("allow-unvalidated", false, "permit runs on non-validated projects")
	manual := fs.Bool("manual", false, "single manual dispatch semantics (quota-exempt)")
	noBillingGuard := fs.Bool("no-billing-guard", false, "disable quota throttling for this run (usage still measured; billing stays subscription)")
	// No backticks in this usage string: flag.UnquoteUsage treats any backtick
	// span as the flag's value-name, which would make this bool render with a
	// bogus multi-word argument placeholder.
	requireCalibration := fs.Bool("require-calibration", false, "refuse to dispatch while the quota governor is uncalibrated (koryph-grz); run 'koryph quota calibrate' first")
	dispatchMode := fs.String("dispatch-mode", "", "dispatch mode: wave|rolling (default: project config, else wave)")
	setGroupedUsage(fs, stdout, "execute one engine run over a project", "[--project ID] [flags]", []flagGroup{
		{title: "CORE", names: []string{"project", "once", "max", "parent", "only", "dispatch-mode"}},
		{title: "LAND & REVIEW", names: []string{"auto-merge", "review", "direct", "resume", "dry-run", "default-model"}},
		{title: "BUDGET & SAFETY OVERRIDES", names: []string{"budget", "manual", "no-billing-guard", "require-calibration", "allow-api-spend", "allow-unvalidated"}},
	})
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	// Default the project to the one containing the current directory when
	// --project is omitted (matches every other project-scoped command). The
	// engine validates an explicit id itself, so we only touch the registry to
	// resolve the cwd default.
	projectID := *project
	if projectID == "" {
		store, err := openStore(context.Background())
		if err != nil {
			return fail(stderr, err)
		}
		rec, code := resolveProjectRecordCwd(stderr, store, "", "run")
		if code != 0 {
			return code
		}
		projectID = rec.ProjectID
	}

	opts := engine.Options{
		ProjectID:          projectID,
		Max:                *max,
		Once:               *once,
		DryRun:             *dryRun,
		Resume:             *resume,
		Parent:             *parent,
		Only:               *only,
		BudgetUSD:          *budget,
		DefaultModel:       *defaultModel,
		AutoMerge:          *autoMerge,
		Direct:             *direct,
		Review:             *review,
		Manual:             *manual,
		AllowAPISpend:      *allowAPISpend,
		AllowUnvalidated:   *allowUnvalidated,
		NoBillingGuard:     *noBillingGuard,
		RequireCalibration: *requireCalibration,
		DispatchMode:       *dispatchMode,
		Out:                stdout,
	}
	outcome, err := engine.Run(context.Background(), opts)
	if err != nil {
		fmt.Fprintln(stderr, "koryph run:", err)
	}
	code := outcome.Code
	if code == 0 && err != nil {
		code = engine.ExitFatal
	}
	return code
}

// boardEntry is one project's line on the board.
type boardEntry struct {
	ProjectID       string         `json:"project_id"`
	MigrationStatus string         `json:"migration_status"`
	Account         string         `json:"account"`
	RunID           string         `json:"run_id,omitempty"`
	RunStatus       string         `json:"run_status,omitempty"`
	Slots           map[string]int `json:"slots,omitempty"`
	LivePIDs        int            `json:"live_pids"`
	// Zombies is the count of non-terminal slots (running/dispatching/review/
	// stuck) whose recorded PID is no longer alive — the running:N/LIVE:0
	// mismatch (koryph-k6o) called out explicitly instead of left for the
	// operator to notice by comparing SLOTS and LIVE by hand.
	Zombies int `json:"zombies"`
}

// cmdBoard prints a one-line-per-project run overview.
func cmdBoard(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("board", stderr)
	asJSON := fs.Bool("json", false, "emit the board as JSON")
	setUsage(fs, stdout, "one-line-per-project run overview", "[--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	recs, err := store.List()
	if err != nil {
		return fail(stderr, err)
	}

	entries := make([]boardEntry, 0, len(recs))
	for _, rec := range recs {
		e := boardEntry{
			ProjectID:       rec.ProjectID,
			MigrationStatus: rec.MigrationStatus,
			Account:         rec.AccountProfile,
			Slots:           map[string]int{},
		}
		if run, lerr := ledger.NewStore(rec.Root).LoadLatest(); lerr == nil && run != nil {
			e.RunID = run.RunID
			e.RunStatus = run.Status
			for _, sl := range run.Slots {
				if sl == nil {
					continue
				}
				e.Slots[sl.Status]++
				switch {
				case sl.PID > 0 && dispatch.Alive(sl.PID):
					e.LivePIDs++
				case zombieSlot(sl, dispatch.Alive):
					// Non-terminal (still "running" work per the ledger) but the
					// pid is dead: the exact mismatch this column exists to
					// surface loudly instead of leaving SLOTS/LIVE to be
					// compared by hand.
					e.Zombies++
				}
			}
		}
		entries = append(entries, e)
	}

	if *asJSON {
		if err := printJSON(stdout, entries); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "no projects registered")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tMIGRATION\tACCOUNT\tRUN\tRUN-STATUS\tSLOTS\tLIVE\tZOMBIES")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			e.ProjectID, e.MigrationStatus, e.Account,
			orDash(e.RunID), orDash(e.RunStatus), slotSummary(e.Slots), e.LivePIDs, zombieCell(e.Zombies))
	}
	tw.Flush()
	return 0
}

// zombieCell renders the board's ZOMBIES column: "-" when clean, else a loud
// "⚠ N" so a running:N/LIVE:0 mismatch (koryph-k6o) can't be missed by eyeballing
// SLOTS against LIVE.
func zombieCell(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("⚠ %d", n)
}

// zombieSlot reports whether sl is non-terminal (the ledger still records it
// as live work) but its recorded pid is no longer alive per the probe —
// dead process, live status. Shared by koryph board and koryph status
// (koryph-k6o) so both surfaces agree on exactly what counts as a zombie.
// alive is dispatch.Alive in production; tests inject a deterministic stub.
func zombieSlot(sl *ledger.Slot, alive func(int) bool) bool {
	return sl != nil && sl.PID > 0 && !ledger.Terminal(sl.Status) && !alive(sl.PID)
}

// slotSummary renders a compact "status:count" summary, or "-" when empty.
func slotSummary(slots map[string]int) string {
	if len(slots) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	// Stable order for readability.
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, slots[k]))
	}
	return strings.Join(parts, " ")
}

// cmdStatus prints the latest-run per-slot detail for one project.
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	project := fs.String("project", "", "project id (default: the project containing the current directory)")
	asJSON := fs.Bool("json", false, "emit the run as JSON")
	frontier := fs.Bool("frontier", false, "show the last wave's per-candidate dispatch verdict instead of the slot table")
	setUsage(fs, stdout, "latest-run per-slot detail", "[--project ID] [--json] [--frontier]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, code := resolveProjectRecordCwd(stderr, store, *project, "status")
	if code != 0 {
		return code
	}
	run, err := ledger.NewStore(rec.Root).LoadLatest()
	if err != nil {
		fmt.Fprintf(stdout, "%s: no runs yet\n", rec.ProjectID)
		return 0
	}
	if *frontier {
		return printFrontier(stdout, stderr, rec.ProjectID, run, *asJSON)
	}
	if *asJSON {
		if err := printJSON(stdout, run); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "project %s  run %s  status %s  wave %d\n", rec.ProjectID, run.RunID, run.Status, run.Wave)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PHASE\tSTATUS\tMODEL\tCOST\tATTEMPTS\tBRANCH\tWORKTREE")
	zombies := 0
	for _, k := range sortedSlotKeys(run.Slots) {
		sl := run.Slots[k]
		if sl == nil {
			continue
		}
		status := sl.Status
		// Best-effort, read-only liveness probe (koryph-k6o): a non-terminal
		// slot with a dead recorded pid is rendered as a distinct "zombie"
		// state instead of the plain persisted status, which otherwise reads
		// identically to a genuinely live slot.
		if zombieSlot(sl, dispatch.Alive) {
			status = sl.Status + " (dead pid — zombie)"
			zombies++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t$%.2f\t%d\t%s\t%s\n",
			sl.PhaseID, status, orDash(sl.Model), sl.CostUSD, sl.Attempts, orDash(sl.Branch), orDash(sl.Worktree))
	}
	tw.Flush()
	if zombies > 0 {
		fmt.Fprintf(stdout, "\n⚠ %d slot(s) marked as live work with a dead pid — the exit was never processed. Reconcile: koryph stop then koryph merge if a slot stays stuck.\n", zombies)
	}
	return 0
}

// printFrontier renders the last wave's per-candidate dispatch verdict (D7/D9):
// every ready bead the scheduler considered and why it was dispatched, deferred,
// or skipped — the full set with full reasons, never the "+N more" truncation of
// the live progress log. bd-dependency-blocked beads are upstream of the ready
// frontier and are not part of a wave, so they do not appear here.
func printFrontier(stdout, stderr io.Writer, projectID string, run *ledger.Run, asJSON bool) int {
	fr := run.Frontier
	if asJSON {
		if err := printJSON(stdout, fr); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if fr == nil || len(fr.Entries) == 0 {
		fmt.Fprintf(stdout, "%s: no frontier recorded yet (no wave built this run, or the run predates frontier capture)\n", projectID)
		return 0
	}
	var disp, def, blk, skp int
	for _, e := range fr.Entries {
		switch e.Verdict {
		case "dispatched":
			disp++
		case "deferred":
			def++
		case "blocked":
			blk++
		case "skipped":
			skp++
		}
	}
	fmt.Fprintf(stdout, "project %s  run %s  wave %d  frontier @ %s\n", projectID, run.RunID, fr.Wave, fr.At)
	fmt.Fprintf(stdout, "  %d dispatched · %d deferred · %d blocked · %d skipped\n\n", disp, def, blk, skp)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BEAD\tVERDICT\tREASON\tTITLE")
	for _, e := range fr.Entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.BeadID, e.Verdict, orDash(e.Reason), e.Title)
	}
	tw.Flush()
	return 0
}

// sortedSlotKeys returns the slot keys of run in stable order.
func sortedSlotKeys(slots map[string]*ledger.Slot) []string {
	keys := make([]string, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}
