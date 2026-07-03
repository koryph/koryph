// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/sched"
)

// rosterSlot is one entry in the MERGED or RUNNING group.
type rosterSlot struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// MERGED fields
	MergeCommit string `json:"merge_commit,omitempty"`
	// RUNNING fields
	Model   string `json:"model,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Age     string `json:"age,omitempty"`
}

// rosterItem is one entry in the QUEUED group.
type rosterItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// rosterDeferred is one entry in the DEFERRED group.
type rosterDeferred struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// rosterOutput is the full --json payload.
type rosterOutput struct {
	ProjectID string           `json:"project_id"`
	RunID     string           `json:"run_id"`
	RunStatus string           `json:"run_status"`
	Wave      int              `json:"wave"`
	Merged    []rosterSlot     `json:"merged"`
	Running   []rosterSlot     `json:"running"`
	Queued    []rosterItem     `json:"queued"`
	Deferred  []rosterDeferred `json:"deferred"`
}

// cmdRoster prints a human-readable bead roster grouped by lifecycle state for
// one project's (latest or specified) run.
func cmdRoster(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("roster", stderr)
	projectID := fs.String("project", "", "project id (required)")
	runID := fs.String("run", "", "run id (default: latest)")
	asJSON := fs.Bool("json", false, "emit roster as JSON")
	if _, err := parseFlags(fs, args); err != nil {
		return engine.ExitUsage
	}
	if *projectID == "" {
		return usageErr(stderr, "roster: --project is required")
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, err := store.Get(*projectID)
	if err != nil {
		return fail(stderr, err)
	}

	ls := ledger.NewStore(rec.Root)
	var run *ledger.Run
	if *runID != "" {
		run, err = ls.LoadRun(*runID)
	} else {
		run, err = ls.LoadLatest()
	}
	if err != nil {
		fmt.Fprintf(stdout, "%s: no runs found\n", rec.ProjectID)
		return 0
	}

	// Load project config for sched.BuildWave (tolerates missing config).
	cfg, _ := project.Load(rec.Root)

	// Open the beads adapter for title lookups and the ready frontier.
	bd := beads.New(rec.Root)

	// -------------------------------------------------------------------------
	// Classify ledger slots into MERGED and RUNNING.
	// -------------------------------------------------------------------------
	now := time.Now().UTC()
	merged := []rosterSlot{}
	running := []rosterSlot{}

	// We'll batch-look up titles for all slots that have a BeadID.
	titles := map[string]string{} // beadID → title cache
	if bd.Available() {
		for _, sl := range run.Slots {
			if sl == nil || sl.BeadID == "" {
				continue
			}
			if _, seen := titles[sl.BeadID]; seen {
				continue
			}
			if iss, serr := bd.Show(ctx, sl.BeadID); serr == nil {
				titles[sl.BeadID] = iss.Title
			}
		}
	}

	slotTitle := func(sl *ledger.Slot) string {
		if t, ok := titles[sl.BeadID]; ok {
			return t
		}
		if sl.BeadID != "" {
			return sl.BeadID
		}
		return sl.PhaseID
	}

	for _, k := range sortedSlotKeys(run.Slots) {
		sl := run.Slots[k]
		if sl == nil {
			continue
		}
		switch sl.Status {
		case ledger.SlotMerged, ledger.SlotDone, ledger.SlotPROpened:
			merged = append(merged, rosterSlot{
				ID:          sl.PhaseID,
				Title:       slotTitle(sl),
				Status:      sl.Status,
				MergeCommit: sl.LastCommit,
			})
		default:
			rs := rosterSlot{
				ID:      sl.PhaseID,
				Title:   slotTitle(sl),
				Status:  sl.Status,
				Model:   sl.Model,
				Attempt: sl.Attempts,
				PID:     sl.PID,
			}
			if sl.DispatchedAt != "" && dispatch.Alive(sl.PID) {
				if t, perr := time.Parse(time.RFC3339, sl.DispatchedAt); perr == nil {
					rs.Age = humanAge(now.Sub(t))
				}
			}
			running = append(running, rs)
		}
	}

	// -------------------------------------------------------------------------
	// Build the wave to separate QUEUED from DEFERRED.
	// -------------------------------------------------------------------------
	// Always initialize to non-nil so --json emits [] rather than null.
	queued := []rosterItem{}
	deferred := []rosterDeferred{}

	if bd.Available() {
		issues, rerr := bd.Ready(ctx, beads.ReadyOpts{})
		if rerr == nil {
			// Active IDs from non-terminal slots.
			activeIDs := map[string]bool{}
			for id, sl := range run.Slots {
				if sl != nil && !ledger.Terminal(sl.Status) {
					activeIDs[id] = true
				}
			}

			// childLister: treat errors as "no open children" (same as engine).
			childLister := func(id string) (bool, error) {
				kids, lerr := bd.ListChildren(ctx, id)
				if lerr != nil {
					return false, nil
				}
				for _, k := range kids {
					if k.Status != "closed" && k.Status != "done" {
						return true, nil
					}
				}
				return false, nil
			}

			w, werr := sched.BuildWave(ctx, issues, cfg, sched.Opts{
				ActiveIDs: activeIDs,
			}, childLister)
			if werr == nil {
				for _, item := range w.Items {
					queued = append(queued, rosterItem{
						ID:    item.Issue.ID,
						Title: item.Issue.Title,
					})
				}
				// Merge Deferred and Skipped into one DEFERRED group.
				allDeferred := append(w.Deferred, w.Skipped...)
				sort.Slice(allDeferred, func(i, j int) bool {
					return allDeferred[i].ID < allDeferred[j].ID
				})
				for _, r := range allDeferred {
					deferred = append(deferred, rosterDeferred{
						ID:     r.ID,
						Title:  r.Title,
						Reason: r.Reason,
					})
				}
			}
		}
	}

	out := rosterOutput{
		ProjectID: rec.ProjectID,
		RunID:     run.RunID,
		RunStatus: run.Status,
		Wave:      run.Wave,
		Merged:    merged,
		Running:   running,
		Queued:    queued,
		Deferred:  deferred,
	}

	if *asJSON {
		return printRosterJSON(stdout, stderr, out)
	}
	printRosterHuman(stdout, out)
	return 0
}

// printRosterJSON emits the roster as indented JSON.
func printRosterJSON(stdout, stderr io.Writer, out rosterOutput) int {
	if err := printJSON(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// printRosterHuman renders the four lifecycle groups in a scannable format.
func printRosterHuman(w io.Writer, out rosterOutput) {
	fmt.Fprintf(w, "project %s  run %s  status %s  wave %d\n\n",
		out.ProjectID, out.RunID, out.RunStatus, out.Wave)

	// MERGED
	fmt.Fprintf(w, "MERGED (%d)\n", len(out.Merged))
	if len(out.Merged) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, s := range out.Merged {
			commit := orDash(shortSHA(s.MergeCommit))
			fmt.Fprintf(tw, "  %s\t%s\t[%s]\t%s\n", s.ID, truncateTitle(s.Title), s.Status, commit)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	// RUNNING
	fmt.Fprintf(w, "RUNNING (%d)\n", len(out.Running))
	if len(out.Running) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, s := range out.Running {
			model := orDash(s.Model)
			age := orDash(s.Age)
			pid := ""
			if s.PID > 0 {
				pid = fmt.Sprintf("pid %d", s.PID)
			}
			line := fmt.Sprintf("  %s\t%s\t[%s]\tmodel %s\tattempt %d\t%s",
				s.ID, truncateTitle(s.Title), s.Status, model, s.Attempt, age)
			if pid != "" {
				line += "\t" + pid
			}
			fmt.Fprintln(tw, line)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	// QUEUED
	fmt.Fprintf(w, "QUEUED (%d)\n", len(out.Queued))
	if len(out.Queued) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, q := range out.Queued {
			fmt.Fprintf(tw, "  %s\t%s\n", q.ID, truncateTitle(q.Title))
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	// DEFERRED
	fmt.Fprintf(w, "DEFERRED (%d)\n", len(out.Deferred))
	if len(out.Deferred) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, d := range out.Deferred {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", d.ID, truncateTitle(d.Title), d.Reason)
		}
		tw.Flush()
	}
}

// shortSHA truncates a commit SHA to 12 hex chars for display.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// humanAge converts a duration to a short human-readable string.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh%dm", h, m)
	}
}
