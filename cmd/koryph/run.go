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
	project := fs.String("project", "", "project id (required)")
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
	dispatchMode := fs.String("dispatch-mode", "", "dispatch mode: wave|rolling (default: project config, else wave)")
	setUsage(fs, stdout, "execute one engine run over a project", "--project ID [flags]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *project == "" {
		return usageErr(stderr, "run: --project is required")
	}

	opts := engine.Options{
		ProjectID:        *project,
		Max:              *max,
		Once:             *once,
		DryRun:           *dryRun,
		Resume:           *resume,
		Parent:           *parent,
		Only:             *only,
		BudgetUSD:        *budget,
		DefaultModel:     *defaultModel,
		AutoMerge:        *autoMerge,
		Direct:           *direct,
		Review:           *review,
		Manual:           *manual,
		AllowAPISpend:    *allowAPISpend,
		AllowUnvalidated: *allowUnvalidated,
		NoBillingGuard:   *noBillingGuard,
		DispatchMode:     *dispatchMode,
		Out:              stdout,
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
				if sl.PID > 0 && dispatch.Alive(sl.PID) {
					e.LivePIDs++
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
	fmt.Fprintln(tw, "PROJECT\tMIGRATION\tACCOUNT\tRUN\tRUN-STATUS\tSLOTS\tLIVE")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			e.ProjectID, e.MigrationStatus, e.Account,
			orDash(e.RunID), orDash(e.RunStatus), slotSummary(e.Slots), e.LivePIDs)
	}
	tw.Flush()
	return 0
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
	project := fs.String("project", "", "project id (required)")
	asJSON := fs.Bool("json", false, "emit the run as JSON")
	setUsage(fs, stdout, "latest-run per-slot detail", "--project ID [--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *project == "" {
		return usageErr(stderr, "status: --project is required")
	}
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, err := store.Get(*project)
	if err != nil {
		return fail(stderr, err)
	}
	run, err := ledger.NewStore(rec.Root).LoadLatest()
	if err != nil {
		fmt.Fprintf(stdout, "%s: no runs yet\n", rec.ProjectID)
		return 0
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
	for _, k := range sortedSlotKeys(run.Slots) {
		sl := run.Slots[k]
		if sl == nil {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t$%.2f\t%d\t%s\t%s\n",
			sl.PhaseID, sl.Status, orDash(sl.Model), sl.CostUSD, sl.Attempts, orDash(sl.Branch), orDash(sl.Worktree))
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
