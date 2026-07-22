// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/procx"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/worktree"
)

func init() {
	registerCmd(command{
		name:    "ops",
		summary: "dispatch-free operator maintenance (reconcile dead runs)",
		run:     cmdOps,
		DocLinks: []string{
			"user-guide/running-waves.md",
		},
		subs: []command{
			{
				name:     "reconcile",
				summary:  "park zombie slots of a dead run blocked, release their leases, finalize the run",
				run:      cmdOpsReconcile,
				DocLinks: []string{"user-guide/running-waves.md"},
			},
		},
	})
}

// cmdOps dispatches the ops sub-verbs.
func cmdOps(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "ops", "dispatch-free operator maintenance", []subVerb{
			{"reconcile [--project ID] [--dry-run]", "park zombie slots of a dead run blocked, release their leases, finalize the run"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "reconcile":
		return cmdOpsReconcile(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown ops subcommand %q", sub))
	}
}

// cmdOpsReconcile is the dispatch-free counterpart to `koryph run --resume`
// (koryph-1es). A killed run loop can leave a run status=running with
// non-terminal slots whose agent pid is dead: nothing outside a fresh engine
// run ever revisits that state, the zombie is misleading in the TUI/status,
// the run never finalizes, and it can pin stale govern leases/demand. Unlike
// --resume, this never dispatches anything — it only classifies each
// non-terminal slot's pid and drives dead ones to a terminal `blocked` state,
// then finalizes the run once every slot is terminal.
func cmdOpsReconcile(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("ops reconcile", stderr)
	projectID := fs.String("project", "", "project id (default: the project containing the current directory)")
	dryRun := fs.Bool("dry-run", false, "report what would change without mutating the ledger or releasing leases")
	setUsage(fs, stdout,
		"reconcile a dead run — never dispatches; blocks dead-agent slots, releases their leases, finalizes the run",
		"[--project ID] [--dry-run]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, code := resolveProjectRecordCwd(stderr, store, *projectID, "ops reconcile")
	if code != 0 {
		return code
	}

	lstore := ledger.NewStore(rec.Root)
	run, err := lstore.LoadLatest()
	if err != nil {
		fmt.Fprintf(stdout, "ops reconcile: no runs found for %s — nothing to reconcile\n", rec.ProjectID)
		return 0
	}

	// A live engine still owns this project's run state — its own
	// resume/health-patrol path handles dead slots; reconciling out from
	// under it would race the single-writer discipline every ledger mutation
	// depends on. koryph.lock is held for the engine's entire lifetime (see
	// ledger.Store.RunLock), so a live holder here IS a live engine for this
	// project, regardless of which run it currently has open.
	if pid, alive, ok := lstore.LockHolder(); ok && alive {
		fmt.Fprintf(stdout, "ops reconcile: a live engine (pid %d) owns %s — nothing to reconcile\n", pid, rec.ProjectID)
		return 0
	}

	gov := newGovernStore()
	pool := poolKeyFor(rec)

	type transition struct {
		phaseID string
		from    string
		note    string
	}
	var (
		left        []string
		warnings    []string
		transitions []transition
	)

	ids := make([]string, 0, len(run.Slots))
	for id := range run.Slots {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		sl := run.Slots[id]
		if sl == nil || ledger.Terminal(sl.Status) {
			continue
		}

		if sl.PID > 0 && dispatch.Alive(sl.PID) {
			left = append(left, fmt.Sprintf("%s: pid %d alive (status=%s) — left alone", id, sl.PID, sl.Status))
			if w := pidReuseWarning(id, sl); w != "" {
				warnings = append(warnings, w)
			}
			continue
		}

		branch := sl.Branch
		if branch == "" {
			branch = worktree.BranchFor(id)
		}
		commits := sl.Commits
		if commits == 0 && branch != "" {
			if n, cerr := reconcileCommitCount(ctx, rec.Root, rec.DefaultBranch, branch); cerr == nil {
				commits = n
			}
		}
		note := fmt.Sprintf("reconciled: agent dead, loop gone; %d commits preserved on %s", commits, branch)
		from := sl.Status

		if *dryRun {
			transitions = append(transitions, transition{phaseID: id, from: from, note: note})
			continue
		}
		// Only record the transition (and count it toward finalization) once
		// the ledger write actually succeeds — a failed UpdateSlot must be
		// reported honestly as "still not terminal," not silently claimed as
		// blocked while the run finalizes over it.
		if uerr := lstore.UpdateSlot(run, id, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = note
			s.PID = 0
		}); uerr != nil {
			fmt.Fprintf(stderr, "ops reconcile: %s: %v\n", id, uerr)
			left = append(left, fmt.Sprintf("%s: could not update ledger (%v) — left alone", id, uerr))
			continue
		}
		transitions = append(transitions, transition{phaseID: id, from: from, note: note})
		if rerr := gov.Release(pool, rec.ProjectID, id); rerr != nil {
			fmt.Fprintf(stderr, "ops reconcile: %s: release lease: %v\n", id, rerr)
		}
	}

	for _, l := range left {
		fmt.Fprintln(stdout, l)
	}
	for _, w := range warnings {
		fmt.Fprintln(stdout, "warning: "+w)
	}
	verb := "reconciled"
	if *dryRun {
		verb = "would reconcile"
	}
	for _, tr := range transitions {
		fmt.Fprintf(stdout, "%s %s: %s -> blocked (%s)\n", verb, tr.phaseID, tr.from, tr.note)
	}

	if *dryRun {
		fmt.Fprintf(stdout, "ops reconcile (dry-run): %d slot(s) left alone, %d slot(s) would be blocked\n", len(left), len(transitions))
		return 0
	}

	if len(left) == 0 {
		if ferr := lstore.FinalizeRun(run); ferr != nil {
			fmt.Fprintf(stderr, "ops reconcile: finalize run: %v\n", ferr)
		} else {
			fmt.Fprintf(stdout, "ops reconcile: run %s finalized (status=%s)\n", run.RunID, run.Status)
		}
	} else {
		fmt.Fprintf(stdout, "ops reconcile: run %s still has %d live slot(s) — not finalized\n", run.RunID, len(left))
	}

	fmt.Fprintf(stdout, "ops reconcile: %d slot(s) left alone, %d slot(s) blocked\n", len(left), len(transitions))
	return 0
}

// poolKeyFor resolves the governor pool a project's leases are held under —
// mirrors runner.quotaName() (internal/engine/run.go): the resolved quota
// profile when set, else the account profile.
func poolKeyFor(rec *registry.Record) string {
	if rec.QuotaProfile != "" {
		return rec.QuotaProfile
	}
	return rec.AccountProfile
}

// reconcileCommitCount counts commits on branch beyond defaultBranch, from
// the primary checkout — the standalone counterpart to runner.commitCount
// (internal/engine/recover.go), usable outside a live engine.
func reconcileCommitCount(ctx context.Context, repoRoot, defaultBranch, branch string) (int, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: repoRoot, Name: "git",
		Args: []string{"rev-list", "--count", defaultBranch + ".." + branch},
	})
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(res.Stdout))
}

// pidReuseWarning cross-checks a live-per-kill(0) slot's pid start time
// against its DispatchedAt, surfacing the known kill(0) pid-reuse false
// positive: once the real agent has died and the OS eventually recycles its
// pid, kill(0) reports the new, unrelated process as "alive." Best-effort and
// platform-limited (procx.StartTime shells out to `ps`, whose etime
// granularity coarsens for long-elapsed processes) — an unreadable start time
// or DispatchedAt, or a gap under the slack window, is not flagged: a false
// negative here just leaves a slot reattach-eligible (today's behavior,
// unchanged); a false positive would wrongly cast doubt on a live agent.
func pidReuseWarning(id string, sl *ledger.Slot) string {
	if sl.DispatchedAt == "" {
		return ""
	}
	dispatched, err := time.Parse(time.RFC3339, sl.DispatchedAt)
	if err != nil {
		return ""
	}
	started, ok := procx.StartTime(sl.PID)
	if !ok {
		return ""
	}
	const slack = 5 * time.Minute
	if started.Sub(dispatched) > slack {
		return fmt.Sprintf("%s: pid %d's process start time (%s) is well after its dispatched_at (%s) — possible pid reuse (a recycled pid, not the original agent); verify manually before trusting the alive/reattach signal",
			id, sl.PID, started.UTC().Format(time.RFC3339), sl.DispatchedAt)
	}
	return ""
}
