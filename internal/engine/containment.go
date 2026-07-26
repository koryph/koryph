// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/ledger"
)

const (
	canaryContainmentGrace   = 2 * time.Second
	canaryIdentityProbeLimit = 2 * time.Second
	canaryTrackerUpdateLimit = 15 * time.Second

	deathReasonCanaryContainment = "native-canary-hard-stop-containment"
)

// containNativeCanaryHardStop is the synchronous containment barrier between
// cancellation and a native-canary engine return. Ordinary interruption never
// enters this path: its live slots remain resumable by design.
//
// Each live process is authenticated against the process-start identity
// captured at dispatch before the process group is signalled. A mismatch
// fails closed. Successful termination is checkpointed terminal before its
// machine lease is released, and both nonterminal slots and leases are
// re-counted before Complete may become true.
func (r *runner) containNativeCanaryHardStop() ContainmentResult {
	result := ContainmentResult{Required: true}
	if r.run == nil || r.store == nil {
		result.Error = "native-canary containment has no durable run"
		return result
	}

	active := r.activePhaseIDs()
	if r.run.Status != ledger.RunAborted {
		seen := make(map[string]bool, len(active))
		for _, id := range active {
			seen[id] = true
		}
		// A crash after terminal slot persistence but before lease release or
		// RunAborted must replay the remaining transaction. DeathReason is the
		// durable intent marker across that terminal boundary.
		for id, sl := range r.run.Slots {
			if sl != nil && sl.DeathReason == deathReasonCanaryContainment &&
				!seen[id] {
				active = append(active, id)
			}
		}
	}
	sort.Strings(active)
	owned := make(map[string]struct{}, len(active))
	for _, id := range active {
		owned[id] = struct{}{}
	}

	var containmentErrors []error
	for _, id := range active {
		sl := r.run.Slots[id]
		if sl == nil {
			continue
		}
		if err := r.containCanarySlot(sl); err != nil {
			containmentErrors = append(containmentErrors, fmt.Errorf("%s: %w", id, err))
		}
	}

	result.NonTerminalSlots, result.ActiveWorkers = r.remainingContainedWork(owned)
	result.RemainingLeases = r.remainingContainmentLeases(owned)
	if result.NonTerminalSlots == 0 && result.ActiveWorkers == 0 &&
		result.RemainingLeases == 0 && len(containmentErrors) == 0 {
		r.run.Status = ledger.RunAborted
		if err := r.store.SaveRun(r.run); err != nil {
			containmentErrors = append(containmentErrors,
				fmt.Errorf("persist contained run: %w", err))
		} else {
			result.Complete = true
			r.progress("native canary hard stop contained %d admitted slot(s); all workers reaped and leases released",
				len(active))
		}
	}
	if len(containmentErrors) > 0 {
		result.Error = errors.Join(containmentErrors...).Error()
	}
	if !result.Complete {
		r.progress("native canary hard-stop containment incomplete: active_workers=%d nonterminal_slots=%d remaining_leases=%d error=%s",
			result.ActiveWorkers, result.NonTerminalSlots, result.RemainingLeases, result.Error)
	}
	return result
}

func (r *runner) containCanarySlot(sl *ledger.Slot) error {
	if sl == nil {
		return errors.New("missing slot")
	}
	pendingNote := "native-canary hard stop: containment pending; branch and worktree preserved"
	if err := r.store.UpdateSlot(r.run, sl.PhaseID, func(current *ledger.Slot) {
		current.OutcomeClass = string(OutcomeOperatorStop)
		current.DeathReason = deathReasonCanaryContainment
		current.Note = pendingNote
	}); err != nil {
		return fmt.Errorf("persist containment intent: %w", err)
	}
	sl = r.run.Slots[sl.PhaseID]
	if err := r.checkpointCanaryContainment(sl, "canary-hard-stop-containment-pending"); err != nil {
		return fmt.Errorf("persist containment intent manifest: %w", err)
	}

	leaderAlive := sl.PID > 0 && slotAlive(sl.PID)
	groupAlive := sl.PID > 0 && dispatch.ProcessGroupAlive(sl.PID)
	if groupAlive && !leaderAlive {
		return fmt.Errorf(
			"process group %d remains live after its authenticated leader exited; refusing an unauthenticated group signal",
			sl.PID,
		)
	}
	if leaderAlive {
		if strings.TrimSpace(sl.VerifiedIdentity) == "" {
			return fmt.Errorf("live pid %d has no authenticated runtime identity", sl.PID)
		}
		if strings.TrimSpace(sl.ProcessIdentity) == "" {
			return fmt.Errorf("live pid %d has no process-start identity", sl.PID)
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), canaryIdentityProbeLimit)
		observed := r.processIdentity(probeCtx, sl.PID)
		cancel()
		if observed == "" {
			return fmt.Errorf("live pid %d process identity unavailable", sl.PID)
		}
		if observed != sl.ProcessIdentity {
			return fmt.Errorf("live pid %d process identity mismatch", sl.PID)
		}
		stop := r.containmentStop
		if stop == nil {
			stop = dispatch.StopGracefulThenForceAndReap
		}
		if err := stop(sl.PID, canaryContainmentGrace); err != nil {
			return fmt.Errorf("stop and reap authenticated pid %d: %w", sl.PID, err)
		}
		if slotAlive(sl.PID) || dispatch.ProcessGroupAlive(sl.PID) {
			return fmt.Errorf("authenticated process group %d remained live after stop/reap", sl.PID)
		}
	}

	note := "native-canary hard stop: worker contained; branch and worktree preserved"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := r.store.UpdateSlot(r.run, sl.PhaseID, func(current *ledger.Slot) {
		current.Status = ledger.SlotBlocked
		current.OutcomeClass = string(OutcomeOperatorStop)
		current.Note = note
		current.PID = 0
		current.ProcessIdentity = ""
		current.FinishedAt = now
	}); err != nil {
		return fmt.Errorf("persist terminal containment slot: %w", err)
	}
	sl = r.run.Slots[sl.PhaseID]
	if err := r.checkpointCanaryContainment(sl, "canary-hard-stop-contained"); err != nil {
		return fmt.Errorf("persist terminal containment manifest: %w", err)
	}

	if err := r.releaseContainmentLease(sl.PhaseID); err != nil {
		return err
	}

	if r.adapter != nil {
		trackerCtx, cancel := context.WithTimeout(context.Background(), canaryTrackerUpdateLimit)
		defer cancel()
		if err := r.adapter.SetStatus(trackerCtx, sl.PhaseID, "blocked"); err != nil {
			r.progress("bead %s: contained safely; tracker status reconciliation deferred: %v",
				sl.PhaseID, err)
			return nil
		}
		comment := fmt.Sprintf(
			"engine: native-canary hard stop contained the worker; branch %s and worktree %s are preserved (run %s)",
			sl.Branch, sl.Worktree, r.run.RunID,
		)
		if err := r.adapter.Comment(trackerCtx, sl.PhaseID, comment); err != nil {
			r.progress("bead %s: contained safely; tracker note reconciliation deferred: %v",
				sl.PhaseID, err)
		}
	}
	return nil
}

func (r *runner) releaseContainmentLease(phaseID string) error {
	owned := map[string]struct{}{phaseID: {}}
	for attempt := 0; attempt < 3; attempt++ {
		r.releaseGlobalSlot(phaseID)
		if r.remainingContainmentLeases(owned) == 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("machine lease remained after bounded release retries")
}

func (r *runner) checkpointCanaryContainment(sl *ledger.Slot, state string) error {
	if sl == nil {
		return errors.New("missing containment slot")
	}
	r.checkpointSlot(sl, state)
	manifest, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID)
	if err != nil {
		return err
	}
	if manifest.ExecutionState != state ||
		manifest.PID != sl.PID ||
		manifest.ProcessIdentity != sl.ProcessIdentity {
		return fmt.Errorf(
			"manifest state=%q pid=%d identity=%q, want state=%q pid=%d identity=%q",
			manifest.ExecutionState, manifest.PID, manifest.ProcessIdentity,
			state, sl.PID, sl.ProcessIdentity,
		)
	}
	return nil
}

func (r *runner) remainingContainedWork(owned map[string]struct{}) (nonterminal, live int) {
	for id := range owned {
		sl := r.run.Slots[id]
		if sl == nil || ledger.Terminal(sl.Status) {
			continue
		}
		nonterminal++
		if sl.PID > 0 && (slotAlive(sl.PID) || dispatch.ProcessGroupAlive(sl.PID)) {
			live++
		}
	}
	return nonterminal, live
}

func (r *runner) remainingContainmentLeases(owned map[string]struct{}) int {
	if r.gov == nil || len(owned) == 0 {
		return 0
	}
	_, leases, _, err := r.gov.Snapshot(r.poolKey())
	if err != nil {
		// A failed verification is not equivalent to absence. Return one
		// synthetic remainder so the publication barrier stays closed.
		return 1
	}
	remaining := 0
	for _, lease := range leases {
		if lease.Project != r.opts.ProjectID {
			continue
		}
		if _, ok := owned[lease.Bead]; ok {
			remaining++
		}
	}
	return remaining
}
