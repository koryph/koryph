// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
	"github.com/koryph/koryph/internal/worktree"
)

// gateRequeueNote marks a slot that already burned its single gate-failure
// requeue; a second gate failure blocks instead of looping.
const gateRequeueNote = "gate-failed requeue"

// mergeErrorRequeueNote marks a slot that already burned its single
// merge-error requeue; a second merge failure blocks instead of looping.
const mergeErrorRequeueNote = "merge-error requeue"

// commitStyleRequeueNote marks a slot bounced once for non-conventional commit
// subjects; a second commit-style failure blocks instead of looping.
const commitStyleRequeueNote = "commit-style requeue"

// mergeErrorRetryable reports whether a slot whose merge just errored should be
// requeued for one more attempt rather than blocked. A merge error is usually
// transient — the base moved, a push raced — and requeueSlot Force-rebases the
// landed branch onto current main before resuming, so the retry re-attempts the
// merge from a correct base. It is retried at most once (koryph-3fs).
func mergeErrorRetryable(sl *ledger.Slot) bool {
	return sl.Note != mergeErrorRequeueNote && sl.Attempts < ledger.MaxAttempts
}

// pollUntilIdle ticks every pollInterval until every slot in the run is
// terminal, waking early on SIGCHLD (koryph-2im.2). A ctx cancellation
// propagates so the caller can checkpoint.
//
// Dispatched agents are direct children of this process (that is why
// slotAlive's Wait4(WNOHANG) below works), so a child's exit raises SIGCHLD
// here immediately — completion latency drops from up-to-pollInterval to
// near-instant. It is a wake HINT only: short-lived git/bd children raise the
// same signal, so a wake does not mean "a slot finished," it means "go find
// out" — the poll pass below makes that call, same as a timer tick would.
// The channel is 1-buffered and non-blocking to send on, so a burst of
// exits (fanned-out git subprocesses, several agents landing at once)
// coalesces into one extra pass rather than one per signal. The timer
// remains the backstop: a missed or coalesced signal just falls back to the
// next tick, never to incorrectness.
func (r *runner) pollUntilIdle(ctx context.Context) error {
	interval := r.pollInterval()

	wake := r.wakeCh
	if wake == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGCHLD)
		defer signal.Stop(ch)
		wake = ch
	}

	tick := 0
	for {
		if r.activeCount() == 0 {
			return nil
		}
		probeProgress := false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
			tick++
			// Split probe cost (L3): the git rev-list progress probe is the
			// pricier subprocess, so it runs on the first timer tick and every
			// 3rd one thereafter — same freshness as a 30s poll, a fraction of
			// the churn at a 10s tick. Liveness/stuck detection below is
			// unaffected; it runs on every pass regardless.
			probeProgress = progressProbeDue(tick)
		case <-wake:
			// Signal wake: liveness is the point, so skip the progress probe —
			// the timer's own cadence keeps commit counts fresh regardless.
		}
		for _, id := range r.activePhaseIDs() {
			sl := r.run.Slots[id]
			if sl == nil || ledger.Terminal(sl.Status) {
				continue
			}
			r.pollSlot(ctx, sl, probeProgress)
		}
		// Flush this tick's batched live-slot progress in one write. Terminal
		// transitions (completeSlot) already persisted immediately; this only
		// commits the cheap commit-count / heartbeat refresh, which resume
		// recomputes from git anyway — so batching it costs no crash safety.
		_ = r.store.SaveRun(r.run)
	}
}

// progressProbeDue reports whether a timer-driven poll pass should run the
// git progress probe: tick 1, and every 3rd tick thereafter (1, 4, 7, ...).
// tick counts timer ticks only — signal-triggered passes never advance it and
// never call this (koryph-2im.2).
func progressProbeDue(tick int) bool {
	return tick%3 == 1
}

// pollSlot refreshes one slot: liveness (always), commit progress (only when
// probeProgress — see progressProbeDue), stuck detection, and — on death —
// completion handling.
func (r *runner) pollSlot(ctx context.Context, sl *ledger.Slot, probeProgress bool) {
	alive := slotAlive(sl.PID)

	// Batch per-tick progress in memory; pollUntilIdle flushes once per tick.
	if probeProgress {
		if commits, head, err := r.branchProgress(ctx, sl.Worktree); err == nil {
			r.store.MutateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
				s.Commits = commits
				s.LastCommit = head
			})
		}
	}

	if alive {
		status := ledger.SlotRunning
		if r.isStuck(ctx, sl) {
			status = ledger.SlotStuck
		}
		if sl.Status != status {
			r.store.MutateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.Status = status })
			if status == ledger.SlotStuck {
				r.progress("bead %s: stuck (no heartbeat or commit for >%ds); still polling", sl.PhaseID, r.opts.StuckSec)
			}
		}
		r.checkpointSlot(sl, "running")
		return
	}

	r.completeSlot(ctx, sl)
}

// slotAlive reports whether the agent process is still running. The engine is
// usually the direct parent of a dispatched (detached, released) agent, so a
// plain kill(pid,0) probe would report a zombie as alive forever — nobody
// waits on it. A non-blocking Wait4 reaps our own dead children first; for
// processes we did not parent (resumed runs) it fails with ECHILD and we fall
// back to the signal probe.
func slotAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	if err == nil {
		if wpid == pid {
			return false // reaped: definitively dead
		}
		if wpid == 0 {
			return true // our child, still running
		}
	}
	return dispatch.Alive(pid)
}

// completeSlot handles a dead agent: record cost, then either finish the
// candidate (review + merge policy), block, or requeue.
func (r *runner) completeSlot(ctx context.Context, sl *ledger.Slot) {
	if cost, ok := dispatch.ParseResultCost(sl.Stream); ok {
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.CostUSD = cost })
		model, size := sl.Model, r.sizeClass(sl.PhaseID)
		// Lock-guarded read-modify-write so concurrent runs on the same account
		// don't clobber each other's EWMA calibration (koryph-8iu.1).
		if cfg, err := quota.UpdateConfig(r.quotaName(), func(c *quota.Config) error {
			quota.Record(c, model, size, cost)
			return nil
		}); err == nil {
			r.quotaCfg = cfg
		}
	}

	summary := filepath.Join(r.store.PhaseDir(r.run.RunID, sl.PhaseID), "SUMMARY.md")
	if sl.Commits > 0 || fsx.Exists(summary) {
		r.finishCandidate(ctx, sl)
		return
	}

	if sl.Attempts >= ledger.MaxAttempts {
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = fmt.Sprintf("agent died with no commits; %d attempts exhausted", sl.Attempts)
		})
		r.checkpointSlot(sl, "blocked")
		r.releaseGlobalSlot(sl.PhaseID) // terminal
		r.progress("bead %s: blocked (died with no commits, %d attempts)", sl.PhaseID, sl.Attempts)
		return
	}
	r.requeueSlot(ctx, sl, "", "agent died with no commits")
}

// finishCandidate runs the configured post-implement pipeline stages, the
// optional review pass, and then applies the merge policy to a completed slot.
func (r *runner) finishCandidate(ctx context.Context, sl *ledger.Slot) {
	policy := r.mergePolicy(ctx, sl.EpicID)

	// --direct is the owner override: skip the PR flow and merge straight to the
	// default branch, even on a merge:pr epic. The push to a protected default
	// branch still requires the identity to hold a branch-protection bypass —
	// koryph does not gate on org role. A blocking review can still downgrade
	// this to manual below, so the safety path is not bypassed (koryph-ufy.5).
	if r.opts.Direct {
		policy = project.PolicyAuto
	}

	// Post-implement stages (docs, test, ...) run in the worktree before review
	// and merge (koryph-a14). A required stage failure blocks the slot —
	// never auto-merge past incomplete pipeline work.
	if ok, failed := r.runPipelineStages(ctx, sl); !ok {
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "pipeline stage failed: " + failed
		})
		r.checkpointSlot(sl, "stage-failed")
		r.releaseGlobalSlot(sl.PhaseID) // terminal
		r.progress("bead %s: blocked (pipeline stage %q failed)", sl.PhaseID, failed)
		return
	}

	if r.opts.Review {
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.Status = ledger.SlotReview })
		outPath := filepath.Join(r.store.PhaseDir(r.run.RunID, sl.PhaseID), "review.json")
		v := review.Review(ctx, review.Opts{
			RepoRoot:  r.rec.Root,
			Worktree:  sl.Worktree,
			Branch:    sl.Branch,
			Base:      r.rec.DefaultBranch,
			Persona:   modelroute.PersonaFor(modelroute.StageReview, r.cfg.Stages),
			Model:     modelroute.TierOpus,
			Profile:   r.profile,
			OutPath:   outPath,
			ClaudeBin: os.Getenv(envClaudeBin),
		})
		if v.Degraded {
			// Fail CLOSED: --review was explicitly requested, so a review we
			// could not obtain (even after in-reviewer retries) must never wave
			// the merge through. Block the slot and surface the reason rather
			// than silently auto-merging unreviewed work (koryph-b2h).
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
				s.Status = ledger.SlotBlocked
				s.Note = fmt.Sprintf("review degraded after %d attempt(s), NOT merged: %s", v.Attempts, v.Reason)
			})
			r.checkpointSlot(sl, "review-degraded")
			r.releaseGlobalSlot(sl.PhaseID) // terminal
			r.progress("bead %s: BLOCKED — review could not complete after %d attempt(s) (%s); refusing to auto-merge unreviewed work",
				sl.PhaseID, v.Attempts, v.Reason)
			return
		}
		if v.Blocking {
			if sl.ReviewIters < 2 {
				_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.ReviewIters++ })
				r.progress("bead %s: blocking review findings (iteration %d) — bouncing back to the implementer",
					sl.PhaseID, sl.ReviewIters)
				r.requeueSlot(ctx, sl, outPath, "blocking review findings")
				return
			}
			// Iterations exhausted: never auto-merge unresolved findings.
			policy = project.PolicyManual
			r.progress("bead %s: blocking findings persist after %d review iterations — forcing manual merge",
				sl.PhaseID, sl.ReviewIters)
		}
	}

	// merge_policy pr never touches the protected default branch directly:
	// push the branch and open a PR for a later fast-forward landing step.
	// This is the safe path for protected branches, so — unlike auto-merge —
	// it is not gated on --auto-merge (koryph-ufy.1).
	if policy == project.PolicyPR {
		r.openPRSlot(ctx, sl)
		return
	}

	if policy == project.PolicyAuto && (r.opts.AutoMerge || r.opts.Direct) {
		r.mergeSlot(ctx, sl)
		return
	}

	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.Status = ledger.SlotMergePending })
	r.checkpointSlot(sl, "merge-pending")
	r.releaseGlobalSlot(sl.PhaseID) // agent done; free the slot (operator merges)
	_ = r.adapter.Comment(ctx, sl.PhaseID,
		fmt.Sprintf("ready for merge: branch %s, run %s", sl.Branch, r.run.RunID))
	r.progress("bead %s: merge-pending (policy %s, branch %s) — merge via the CLI", sl.PhaseID, policy, sl.Branch)
}

// mergeSlot lands a review-clean candidate on the default branch.
func (r *runner) mergeSlot(ctx context.Context, sl *ledger.Slot) {
	res, err := merge.Merge(ctx, merge.Opts{
		RepoRoot:            r.rec.Root,
		Branch:              sl.Branch,
		DefaultBranch:       r.rec.DefaultBranch,
		Gate:                r.cfg.Gate,
		Extra:               r.cfg.ProtectedPaths,
		Push:                true, // merge itself skips push when no remote exists
		SlotOwner:           r.owner,
		SlotRetries:         3,
		Slot:                r.slotLocker(ctx),
		RequireSigned:       r.requireSigned(),
		RequireConventional: r.cfg.EnforceConventional(),
	})
	if err != nil {
		// A merge error is usually transient (base moved, push raced). Self-heal
		// by requeueing ONCE — requeueSlot Force-rebases the landed branch onto
		// current main and resumes — rather than stranding the bead. Only a
		// second failure blocks. Mirrors the gate-failed path below (koryph-3fs).
		if mergeErrorRetryable(sl) {
			r.progress("bead %s: merge error (%v) — requeueing once to retry the merge", sl.PhaseID, err)
			r.requeueSlot(ctx, sl, "", mergeErrorRequeueNote)
			return
		}
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "merge error after requeue: " + err.Error()
		})
		r.checkpointSlot(sl, "merge-error")
		r.releaseGlobalSlot(sl.PhaseID) // terminal
		r.progress("bead %s: blocked (merge error after requeue: %v)", sl.PhaseID, err)
		return
	}

	if res.Status == merge.StatusMerged {
		now := time.Now().UTC().Format(time.RFC3339)
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotMerged
			s.MergedAt = now
		})
		r.checkpointSlot(sl, "merged")
		_ = r.adapter.Close(ctx, sl.PhaseID, "merged: "+res.MergedSHA)
		_ = r.reg.Audit(registry.Event{
			Kind:      "merge",
			ProjectID: r.opts.ProjectID,
			Actor:     r.owner,
			Detail: map[string]string{
				"bead": sl.PhaseID, "branch": sl.Branch, "sha": res.MergedSHA,
			},
		})
		r.progress("bead %s: merged (%s)", sl.PhaseID, shortSHA(res.MergedSHA))
		r.releaseGlobalSlot(sl.PhaseID)
		return
	}

	// The gate-failed requeue keeps its slot and returns early; every other
	// (terminal) failure frees the global slot.
	if r.handleMergeFailure(ctx, sl, res) {
		return
	}
	r.releaseGlobalSlot(sl.PhaseID)
}

// handleMergeFailure records the non-success outcomes shared by the ff-merge
// and PR-open paths (gate-failed, conflict, protected, unsigned, and any
// unknown status). It returns true when it requeued the slot instead of
// blocking it (the gate-failed single retry): the caller must then return
// WITHOUT releasing the global slot, because a requeue keeps it.
func (r *runner) handleMergeFailure(ctx context.Context, sl *ledger.Slot, res merge.Result) (requeued bool) {
	switch res.Status {
	case merge.StatusGateFailed:
		if sl.Note != gateRequeueNote && sl.Attempts < ledger.MaxAttempts {
			r.progress("bead %s: gate failed after rebase — requeueing once", sl.PhaseID)
			r.requeueSlot(ctx, sl, "", gateRequeueNote)
			return true
		}
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "gate failed after requeue: " + tailOf(res.GateOutput, 400)
		})
		r.checkpointSlot(sl, "gate-failed")
		r.progress("bead %s: blocked (gate failed after requeue)", sl.PhaseID)

	case merge.StatusCommitStyle:
		if sl.Note != commitStyleRequeueNote && sl.Attempts < ledger.MaxAttempts {
			r.progress("bead %s: non-conventional commit subject(s) — bouncing to the implementer to reword",
				sl.PhaseID)
			r.requeueSlot(ctx, sl, "", commitStyleRequeueNote)
			return true
		}
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "commit-style failed after requeue: " + tailOf(res.GateOutput, 400)
		})
		r.checkpointSlot(sl, "commit-style")
		r.progress("bead %s: blocked (non-conventional commit subjects persist after requeue)", sl.PhaseID)

	case merge.StatusConflict:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotConflict
			s.Note = "rebase conflict: " + res.ConflictMD
		})
		r.checkpointSlot(sl, "conflict")
		r.progress("bead %s: rebase conflict (details: %s)", sl.PhaseID, res.ConflictMD)

	case merge.StatusProtected:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "protected paths touched: " + strings.Join(res.Protected, ", ")
		})
		r.checkpointSlot(sl, "protected")
		r.progress("bead %s: blocked (protected paths: %s)", sl.PhaseID, strings.Join(res.Protected, ", "))

	case merge.StatusUnsigned:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "merge refused: " + tailOf(res.GateOutput, 400)
		})
		r.checkpointSlot(sl, "unsigned")
		r.progress("bead %s: blocked (unsigned commits on %s — signing is required; nothing merged)",
			sl.PhaseID, sl.Branch)

	default:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotFailed
			s.Note = "merge status " + string(res.Status)
		})
		r.checkpointSlot(sl, "merge-"+string(res.Status))
		r.progress("bead %s: merge failed (%s)", sl.PhaseID, string(res.Status))
	}
	return false
}

// openPRSlot handles merge_policy pr: it reuses the merge preflight (protected
// paths, signing, sync, rebase, gate) and then pushes the branch and opens a
// pull request instead of fast-forward merging. The worktree/branch are kept
// for a later landing step, so the bead parks in pr-opened rather than merged.
func (r *runner) openPRSlot(ctx context.Context, sl *ledger.Slot) {
	iss := r.issueFor(ctx, sl)
	res, err := merge.Merge(ctx, merge.Opts{
		RepoRoot:            r.rec.Root,
		Branch:              sl.Branch,
		DefaultBranch:       r.rec.DefaultBranch,
		Gate:                r.cfg.Gate,
		Extra:               r.cfg.ProtectedPaths,
		SlotOwner:           r.owner,
		SlotRetries:         3,
		Slot:                r.slotLocker(ctx),
		RequireSigned:       r.requireSigned(),
		RequireConventional: r.cfg.EnforceConventional(),
		OpenPR:              true,
		KeepWorktree:        true, // the branch parks for a later landing step
		PRTitle:             prTitle(iss),
		PRBody:              prBody(iss, r.run.RunID),
	})
	if err != nil {
		// A push or gh error is usually config/auth (not a transient rebase
		// race), so block with the reason rather than looping. The branch is
		// kept, so a fixed remote/gh lets a --resume retry.
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "pr error: " + err.Error()
		})
		r.checkpointSlot(sl, "pr-error")
		r.releaseGlobalSlot(sl.PhaseID)
		r.progress("bead %s: blocked (pr error: %v)", sl.PhaseID, err)
		return
	}

	switch res.Status {
	case merge.StatusPROpened:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotPROpened
			s.Note = fmt.Sprintf("PR #%d opened: %s", res.PRNumber, res.PRURL)
		})
		r.checkpointSlot(sl, "pr-opened")
		_ = r.reg.Audit(registry.Event{
			Kind:      "pr-open",
			ProjectID: r.opts.ProjectID,
			Actor:     r.owner,
			Detail: map[string]string{
				"bead": sl.PhaseID, "branch": sl.Branch, "pr": res.PRURL,
			},
		})
		_ = r.adapter.Comment(ctx, sl.PhaseID,
			fmt.Sprintf("PR opened for merge: %s (branch %s, run %s)", res.PRURL, sl.Branch, r.run.RunID))
		r.progress("bead %s: pr-opened (%s, branch %s) — land the PR to complete", sl.PhaseID, res.PRURL, sl.Branch)
		r.releaseGlobalSlot(sl.PhaseID)

	case merge.StatusPRNoRemote:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "merge_policy pr requires a git remote; none configured"
		})
		r.checkpointSlot(sl, "pr-no-remote")
		r.releaseGlobalSlot(sl.PhaseID)
		r.progress("bead %s: blocked (merge_policy pr needs a git remote; branch %s kept)", sl.PhaseID, sl.Branch)

	case merge.StatusPRNoGH:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "merge_policy pr requires an authenticated gh CLI; unavailable"
		})
		r.checkpointSlot(sl, "pr-no-gh")
		r.releaseGlobalSlot(sl.PhaseID)
		r.progress("bead %s: blocked (merge_policy pr needs an authenticated gh; branch %s kept)", sl.PhaseID, sl.Branch)

	default:
		if r.handleMergeFailure(ctx, sl, res) {
			return
		}
		r.releaseGlobalSlot(sl.PhaseID)
	}
}

// prTitle renders a conventional-commit-shaped PR title from a bead: the type
// derives from the bead type (bug→fix, chore→chore, else feat), the scope is
// the bead id, and the subject is the bead title's first line.
func prTitle(iss beads.Issue) string {
	typ := "feat"
	switch iss.IssueType {
	case "bug":
		typ = "fix"
	case "chore":
		typ = "chore"
	}
	subject := iss.Title
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	if subject = strings.TrimSpace(subject); subject == "" {
		subject = iss.ID
	}
	return fmt.Sprintf("%s(%s): %s", typ, iss.ID, subject)
}

// prBody renders a PR body carrying the bead id, title, and acceptance text
// (the bead description), plus a note that the PR must land fast-forward-only.
func prBody(iss beads.Issue, runID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Bead **%s**: %s\n\n", iss.ID, iss.Title)
	if desc := strings.TrimSpace(iss.Description); desc != "" {
		b.WriteString(desc)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "---\nOpened by `koryph run` (merge_policy=pr, run %s). Land fast-forward-only; do not push straight to the default branch.\n", runID)
	return b.String()
}

// requeueSlot re-dispatches a slot: backoff, refresh the worktree onto current
// main, then the same dispatch flow with the branch HEAD as ResumeSHA and (when
// the manifest carries a session id and the worktree survives) a native session
// resume.
func (r *runner) requeueSlot(ctx context.Context, sl *ledger.Slot, reviewPath, why string) {
	attempt := sl.Attempts + 1
	r.progress("bead %s: requeueing, attempt %d (%s)", sl.PhaseID, attempt, why)
	r.backoffSleep(ctx, sl.Attempts)

	// Never re-run an agent against a checkout that predates a main-side fix
	// (koryph-137): rebuild a no-commit worktree from current main, or rebase
	// one with landed work onto the advanced base, before re-dispatch.
	r.refreshWorktreeForRequeue(ctx, sl)

	resumeSession := ""
	if m, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID); err == nil &&
		m.SessionID != "" && sl.Worktree != "" && fsx.Exists(sl.Worktree) {
		resumeSession = m.SessionID
	}

	r.dispatchBead(ctx, dispatchReq{
		issue:           r.issueFor(ctx, sl),
		epicID:          sl.EpicID,
		attempt:         attempt,
		resumeSHA:       r.branchHead(ctx, sl.Branch),
		resumeSessionID: resumeSession,
		reviewPath:      reviewPath,
		reviewIters:     sl.ReviewIters,
		note:            why,
	})
}

// refreshWorktreeForRequeue makes a requeued slot's worktree reflect current
// main before re-dispatch, closing the stale-checkout gap (koryph-137): a
// worktree created before a main-side fix must not carry the old checkout
// across attempts, because dispatchBead's worktree.Ensure attaches to an
// existing tree rather than rebuilding it.
//
//   - No commits to preserve → rebuild from the default branch: snapshot any
//     WIP for forensics, remove the worktree, and drop the branch so Ensure
//     recreates both from current main (a fresh checkout, re-bootstrapped).
//   - Landed commits → rebase the branch onto the advanced base via Refresh
//     (Force, so the requeue always re-bases regardless of drift threshold or
//     footprint overlap) and resume on top of current main.
//
// Every failure is non-fatal: the slot still re-dispatches onto whatever
// checkout survives, matching pre-137 behavior, with the reason logged.
func (r *runner) refreshWorktreeForRequeue(ctx context.Context, sl *ledger.Slot) {
	if sl.Worktree == "" || sl.Branch == "" {
		return
	}

	commits := sl.Commits
	if commits == 0 {
		if n, err := r.commitCount(ctx, sl.Branch); err == nil {
			commits = n
		}
	}

	if commits > 0 {
		res, err := worktree.Refresh(ctx, worktree.RefreshOpts{
			RepoRoot: r.rec.Root,
			Path:     sl.Worktree,
			Branch:   sl.Branch,
			Base:     r.rec.DefaultBranch,
			Force:    true, // a requeue always re-bases onto current main
		})
		if err != nil {
			r.progress("bead %s: requeue refresh error (resuming on existing checkout): %v", sl.PhaseID, err)
			return
		}
		switch res.Action {
		case "refreshed":
			r.progress("bead %s: worktree rebased onto %s before requeue (was %d behind)",
				sl.PhaseID, r.rec.DefaultBranch, res.Behind)
		case "conflict":
			r.progress("bead %s: requeue rebase conflict — resuming on the un-rebased branch (see CONFLICT.md)", sl.PhaseID)
		case "deferred-dirty":
			r.progress("bead %s: requeue refresh deferred (worktree dirty) — resuming on existing checkout", sl.PhaseID)
		}
		return
	}

	// No landed work: rebuild from current main so a stale checkout can never
	// survive the requeue.
	if fsx.Exists(sl.Worktree) {
		if patch, err := worktree.PatchSnapshot(ctx, sl.Worktree, r.store.PhaseDir(r.run.RunID, sl.PhaseID)); err == nil && patch != "" {
			r.progress("bead %s: captured WIP snapshot before worktree rebuild: %s", sl.PhaseID, patch)
		}
		if err := worktree.Remove(ctx, sl.Worktree, true); err != nil {
			r.progress("bead %s: requeue worktree rebuild skipped, remove failed (dispatch will attach existing): %v",
				sl.PhaseID, err)
			return
		}
	}
	// Drop the stale branch so Ensure recreates it from the default branch tip
	// rather than re-checking-out the old one.
	if err := worktree.DeleteBranch(ctx, r.rec.Root, sl.Branch); err != nil {
		r.progress("bead %s: requeue branch reset skipped (%v) — dispatch may attach the old tip", sl.PhaseID, err)
	}
}

// slotLocker returns a bd-backed merge mutex when the project has a
// <project>-merge-slot bead, else nil (no cross-process locking).
func (r *runner) slotLocker(ctx context.Context) merge.SlotLocker {
	slotID := r.opts.ProjectID + "-merge-slot"
	if _, err := r.adapter.Show(ctx, slotID); err != nil {
		return nil
	}
	return &bdSlotLocker{runner: r, slotID: slotID}
}

// bdSlotLocker satisfies merge.SlotLocker over the beads adapter's
// claim/release lease on the merge-slot bead.
type bdSlotLocker struct {
	runner *runner
	slotID string
}

// Acquire claims the merge-slot bead (3 retries with backoff).
func (l *bdSlotLocker) Acquire(ctx context.Context, owner string) error {
	return l.runner.adapter.MergeSlotAcquire(ctx, l.slotID, owner, 3)
}

// Release reopens the merge-slot bead.
func (l *bdSlotLocker) Release(ctx context.Context) error {
	return l.runner.adapter.MergeSlotRelease(ctx, l.slotID)
}

// checkpointSlot refreshes the slot's manifest v2 (execution state + head
// commit + attempt), rebuilding a minimal manifest when none exists yet.
func (r *runner) checkpointSlot(sl *ledger.Slot, execState string) {
	cur := r.run.Slots[sl.PhaseID]
	if cur == nil {
		cur = sl
	}
	m, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID)
	existed := err == nil
	if err != nil {
		m = &ledger.Manifest{
			ProjectID:       r.opts.ProjectID,
			BeadID:          cur.PhaseID,
			EpicID:          cur.EpicID,
			AccountProfile:  cur.AccountProfile,
			ClaudeConfigDir: cur.ClaudeConfigDir,
			SessionID:       cur.SessionID,
			SessionName:     cur.SessionName,
			Model:           cur.Model,
			WorktreePath:    cur.Worktree,
			Branch:          cur.Branch,
			BillingMode:     cur.BillingMode,
		}
	}
	// Skip the manifest read-modify-write when nothing recovery-relevant moved —
	// a quietly-running slot re-checkpoints identically every tick otherwise.
	if existed && m.ExecutionState == execState && m.HeadCommit == cur.LastCommit && m.Attempt == cur.Attempts {
		return
	}
	m.ExecutionState = execState
	m.HeadCommit = cur.LastCommit
	m.Attempt = cur.Attempts
	_ = r.store.SaveManifest(r.run.RunID, sl.PhaseID, m)
}

// isStuck reports whether an alive slot shows neither heartbeat nor commit
// progress within StuckSec. Informational only — polling continues.
func (r *runner) isStuck(ctx context.Context, sl *ledger.Slot) bool {
	threshold := time.Duration(r.opts.StuckSec) * time.Second

	if fi, err := os.Stat(sl.StatusPath); err == nil {
		if time.Since(fi.ModTime()) <= threshold {
			return false
		}
	} else if r.sinceDispatched(sl) <= threshold {
		return false
	}

	res, err := execx.Run(ctx, execx.Cmd{
		Dir: sl.Worktree, Name: "git", Args: []string{"log", "-1", "--format=%ct", "HEAD"},
	})
	if err == nil && res.ExitCode == 0 {
		if sec, perr := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64); perr == nil {
			return time.Since(time.Unix(sec, 0)) > threshold
		}
	}
	return r.sinceDispatched(sl) > threshold
}

// sinceDispatched is the age of the slot's dispatch (zero when unparseable).
func (r *runner) sinceDispatched(sl *ledger.Slot) time.Duration {
	t, err := time.Parse(time.RFC3339, sl.DispatchedAt)
	if err != nil {
		return 0
	}
	return time.Since(t)
}

// branchProgress counts commits ahead of the default branch and resolves the
// short HEAD inside a worktree.
func (r *runner) branchProgress(ctx context.Context, wtPath string) (int, string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: wtPath, Name: "git",
		Args: []string{"rev-list", "--count", r.rec.DefaultBranch + "..HEAD"},
	})
	if err != nil {
		return 0, "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		return 0, "", err
	}
	head := ""
	if hr, herr := execx.Run(ctx, execx.Cmd{
		Dir: wtPath, Name: "git", Args: []string{"rev-parse", "--short", "HEAD"},
	}); herr == nil && hr.ExitCode == 0 {
		head = strings.TrimSpace(hr.Stdout)
	}
	return n, head, nil
}

// sizeClass buckets a bead for the cost estimator, defaulting to M when the
// issue body is not in memory (e.g. an adopted slot).
func (r *runner) sizeClass(beadID string) string {
	if iss, ok := r.issues[beadID]; ok {
		return quota.SizeOf(len(iss.Description))
	}
	return "M"
}

// tailOf returns the last n bytes of s.
func tailOf(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
