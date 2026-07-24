// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/worktree"
)

// resume classifies the latest run and re-adopts it: alive agents are
// reattached (polling continues), dead-with-work and dead-without-work slots
// are re-dispatched per ledger.Classify, and exhausted slots are blocked.
// It reports false (fresh run) when there is no latest run or everything in
// it is terminal.
func (r *runner) resume(ctx context.Context) (bool, error) {
	latest, err := r.store.LoadLatest()
	if err != nil {
		return false, nil // no latest run → fresh
	}

	decisions := ledger.Classify(latest, ledger.Probe{
		AliveSlot: func(sl *ledger.Slot) bool {
			return r.slotProcessMatches(ctx, sl)
		},
		CommitCount: func(branch string) (int, error) {
			return r.commitCount(ctx, branch)
		},
	})

	r.run = latest
	adopted := false
	for _, d := range decisions {
		sl := latest.Slots[d.PhaseID]
		if sl == nil {
			continue
		}
		switch d.Action {
		case ledger.ActionSkip:
			// terminal — leave as recorded

		case ledger.ActionReattach:
			// Adopt as active in this run: keep the slot running and let the
			// poll loop pick it up.
			r.progress("resume: reattaching to %s (%s)", d.PhaseID, d.Reason)
			_ = r.store.UpdateSlot(latest, d.PhaseID, func(s *ledger.Slot) {
				s.Status = ledger.SlotRunning
			})
			adopted = true

		case ledger.ActionRequeueResume, ledger.ActionRequeueFresh:
			// Park the stalled bead in the resume backlog instead of
			// re-dispatching it here (koryph-bzf). The old path fired
			// requeueSlot for EVERY classified bead at once, respecting neither
			// this run's effective width (capacity is only enforced later, in
			// the loop) nor the global governor's acquire admission — so
			// resuming a run that stalled at a higher thread count blew straight
			// past a lowered --max. drainResumeBacklog promotes these into live
			// dispatches at each scheduling boundary, capped at the effective
			// width and gated exactly like frontier work, while still routing
			// through requeueSlot so per-attempt continuity (resume session,
			// WIP snapshot, accumulated cost/attempts) is preserved.
			_ = r.store.UpdateSlot(latest, d.PhaseID, func(s *ledger.Slot) {
				s.Status = ledger.SlotQueued
				// The agent this slot last ran under is dead (that is what
				// classified it requeue, not reattach). Clear the stale PID so a
				// quota hard-stop's interruptActiveSlots cannot SIGTERM a
				// recycled PID, and a second --resume cannot mistake a recycled
				// PID for a live agent and falsely reattach.
				s.PID = 0
				s.Note = "resume: " + d.Reason + " (queued; awaiting a free slot under the current width)"
			})
			r.progress("resume: queued %s for width-gated re-dispatch (%s)", d.PhaseID, d.Reason)
			adopted = true

		case ledger.ActionBlocked:
			_ = r.store.UpdateSlot(latest, d.PhaseID, func(s *ledger.Slot) {
				s.Status = ledger.SlotBlocked
				s.Note = d.Reason
			})
			// Reconcile the bd claim: an exhausted slot classified terminal on
			// resume was leaving the bead in_progress with no live agent
			// (koryph-84yu). Mark it blocked-with-reason so it is visible, not a
			// silent strand invisible to every future frontier.
			r.reconcileBlockedBead(ctx, sl, "attempts exhausted on resume: "+d.Reason)
			r.progress("resume: %s blocked (%s)", d.PhaseID, d.Reason)
		}
	}

	if !adopted {
		// Nothing to carry forward. Close out the old run if it was left
		// "running" with only terminal slots (the stale-running fix), then
		// let the caller start fresh.
		_ = r.store.FinalizeRun(latest)
		r.run = nil
		return false, nil
	}

	latest.Status = ledger.RunRunning
	_ = r.store.SaveRun(latest)
	return true, nil
}

// drainResumeBacklog promotes stalled beads parked in the resume backlog
// (SlotQueued, by resume()) into live dispatches, in deterministic order, until
// the effective width is full or the backlog empties (koryph-bzf). It is the
// width-honoring counterpart to the pre-koryph-bzf resume(), which re-dispatched
// every stalled bead at once and so ignored a lowered --max. Each promotion is
// gated through the global concurrency governor exactly like a frontier
// dispatch (acquireGlobalSlot), and re-dispatched via requeueSlot so per-attempt
// continuity (resume session, WIP snapshot, accumulated cost/attempts) is
// preserved. A no-op whenever the backlog is empty (every non-resume boundary)
// or dispatch is not allowed this boundary. Callers detect "backlog held back"
// via liveActiveCount()==0 && len(queuedResumeIDs())>0 after the call (wave-mode
// pacing) rather than a return code, so a resource-skipped backlog paces the
// same as a cap-broken one.
func (r *runner) drainResumeBacklog(ctx context.Context, width int, allowDispatch bool) {
	if !allowDispatch {
		return
	}
	for _, id := range r.queuedResumeIDs() {
		if r.liveActiveCount() >= width {
			return
		}
		sl := r.run.Slots[id]
		if sl == nil {
			continue
		}
		// A bead the operator retired while the run was down must LEAVE the
		// backlog rather than spin here forever — requeueSlot would keep bailing
		// on the same closed bead and it would stay SlotQueued, re-tried every
		// boundary. beadClosedMidFlight marks it SlotBlocked (dropping it out of
		// queuedResumeIDs) and releases any slot, so a plain continue suffices.
		if r.beadClosedMidFlight(ctx, id) {
			continue
		}
		switch r.acquireGlobalSlot(id, sl.Resources, sl.MemReserveMB) {
		case admitSkip:
			continue // a per-bead resource is at capacity — a lighter backlog bead may still fit
		case admitBreak:
			return // machine-wide (pool cap / memory floor) — stop draining this boundary
		}
		r.requeueSlot(ctx, sl, "", resumeRequeueNote)
		// requeueSlot replaces the slot with a freshly dispatched one; if it
		// bailed without dispatching (bead closed mid-flight, run budget park)
		// the slot is still SlotQueued and we hold an unpaired global slot —
		// release it and stop rather than loop on a bead we cannot place.
		if cur := r.run.Slots[id]; cur != nil && cur.Status == ledger.SlotQueued {
			r.releaseGlobalSlot(id)
			return
		}
	}
}

// reconcileOrphans is the fresh-start safety net (koryph-47n). A plain
// `koryph run` (no --resume) adopts nothing from a prior run, so a bead left
// in_progress by a killed or interrupted run would otherwise stay leased and be
// excluded from `bd ready` forever. Before the first wave, reopen each orphan —
// a non-terminal slot in the latest run whose agent pid is dead — so this run
// re-dispatches it: drop the stale global lease, clean up a CLEAN worktree, and
// reset the bead to open. A dirty worktree or a branch with landed commits is
// preserved (its work belongs to --resume) and the bead is left untouched.
func (r *runner) reconcileOrphans(ctx context.Context) {
	latest, err := r.store.LoadLatest()
	if err != nil || latest == nil {
		return
	}
	reopened := 0
	for id, sl := range latest.Slots {
		if sl == nil || ledger.Terminal(sl.Status) || slotAlive(sl.PID) {
			continue
		}
		// Orphan: a non-terminal slot whose agent is gone. Drop any stale
		// global lease (also pruned lazily by the governor, but be explicit).
		r.releaseGlobalSlot(id)

		if kept, why := r.orphanWorktreeKept(ctx, sl); kept {
			r.progress("reconcile: %s left in place (%s) — recover its work with --resume", id, why)
			continue
		}
		if serr := r.adapter.SetStatus(ctx, id, "open"); serr != nil {
			r.progress("reconcile: could not reopen orphan %s: %v", id, serr)
			continue
		}
		_ = r.store.UpdateSlot(latest, id, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "reconciled: reopened as orphan of a dead run (koryph-47n)"
		})
		reopened++
		r.progress("reconcile: reopened orphaned in_progress bead %s (dead run %s)", id, latest.RunID)
	}
	if reopened > 0 {
		_ = r.store.SaveRun(latest)
	}
}

// orphanWorktreeKept decides whether a reconciled orphan's worktree must be
// preserved. It returns (true, reason) — leave it, do not reopen the bead —
// when the tree is dirty (never auto-remove uncommitted work) or the branch
// carries landed commits (that work belongs to --resume). Otherwise it removes
// the clean, commitless worktree and its branch and returns (false, "") so the
// caller reopens the bead for a fresh re-dispatch.
func (r *runner) orphanWorktreeKept(ctx context.Context, sl *ledger.Slot) (bool, string) {
	wt := sl.Worktree
	if wt == "" || !fsx.Exists(wt) {
		return false, "" // no worktree — safe to reopen
	}
	if dirty, derr := worktree.IsDirty(ctx, wt); derr == nil && dirty {
		return true, "dirty worktree"
	}
	branch := sl.Branch
	if branch == "" {
		branch = worktree.BranchFor(sl.PhaseID)
	}
	if n, cerr := r.commitCount(ctx, branch); cerr == nil && n > 0 {
		return true, fmt.Sprintf("%d unmerged commit(s) on %s", n, branch)
	}
	// Clean and commitless — discard so the bead re-dispatches from a fresh tree.
	_ = worktree.Remove(ctx, wt, false)
	_ = worktree.DeleteBranch(ctx, r.rec.Root, branch)
	return false, ""
}

// reconcileBlockedBead writes a terminally-blocked slot's state back to bd so a
// dead/faulted/stopped slot never strands its claim in_progress with no live
// agent (koryph-84yu). It sets the bead's bd status to "blocked": excluded from
// `bd ready` exactly like the in_progress claim it replaces — so a needs-
// attention block is never auto-redispatched — but VISIBLE, not silently
// dropped from every future frontier the way an unreconciled in_progress claim
// is. An operator (or the stale-claim patrol) can find it via `bd list --status
// blocked` and resolve it. The appended note names the run, attempt count, and
// any preserved uncommitted worktree so the operator can recover WIP before
// reopening (the i3b.10 strand, where real doctor auth.go work was only saved
// because a human happened to notice the orphaned worktree).
//
// Best-effort: a bd failure is logged, never fatal — the loop must not wedge on
// a tracker hiccup. A nil adapter (unit tests that never wire a WorkSource) is
// a no-op so the terminal-block paths stay panic-free.
func (r *runner) reconcileBlockedBead(ctx context.Context, sl *ledger.Slot, reason string) {
	if sl == nil || r.adapter == nil {
		return
	}
	// Idempotence: once the tracker already reflects the terminal block, a
	// repeated poll/reconcile must not append duplicate comments or rewrite
	// state. The ledger remains the detailed reason source.
	if issue, err := r.adapter.Show(ctx, sl.PhaseID); err == nil && issue.Status == "blocked" {
		return
	}
	runID := ""
	if r.run != nil {
		runID = r.run.RunID
	}
	note := fmt.Sprintf("engine: blocked without merge — %s (run %s, %d attempt(s), model %s)",
		reason, runID, sl.Attempts, blockedModelDesc(sl))
	if wt := sl.Worktree; wt != "" && fsx.Exists(wt) {
		if dirty, derr := worktree.IsDirty(ctx, wt); derr == nil && dirty {
			note += fmt.Sprintf("; uncommitted WIP preserved in worktree %s — recover it before reopening", wt)
		}
	}
	if err := r.adapter.SetStatus(ctx, sl.PhaseID, "blocked"); err != nil {
		r.progress("bead %s: could not reconcile stranded claim to blocked: %v", sl.PhaseID, err)
		return
	}
	_ = r.adapter.Comment(ctx, sl.PhaseID, note)
}

// issueFor recovers the bead behind a slot: the in-memory wave item first,
// then bd show, then a minimal synthetic issue (id-only) so a requeue can
// still compile a prompt.
func (r *runner) issueFor(ctx context.Context, sl *ledger.Slot) beads.Issue {
	if iss, ok := r.issues[sl.PhaseID]; ok {
		return iss
	}
	if iss, err := r.adapter.Show(ctx, sl.PhaseID); err == nil && iss.ID != "" {
		r.issues[sl.PhaseID] = iss
		return iss
	}
	return beads.Issue{ID: sl.PhaseID, Title: sl.PhaseID, Labels: []string{}}
}

// commitCount counts commits on branch beyond the default branch, from the
// primary checkout.
func (r *runner) commitCount(ctx context.Context, branch string) (int, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: r.rec.Root, Name: "git",
		Args: []string{"rev-list", "--count", r.rec.DefaultBranch + ".." + branch},
	})
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(res.Stdout))
}

// branchHead resolves a branch tip in the primary checkout ("" on error).
func (r *runner) branchHead(ctx context.Context, branch string) string {
	res, err := execx.Run(ctx, execx.Cmd{
		Dir: r.rec.Root, Name: "git", Args: []string{"rev-parse", branch},
	})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}
