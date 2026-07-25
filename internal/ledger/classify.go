// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"fmt"
	"sort"
)

// RunDead reports whether run is a phantom "running" run — its status is
// RunRunning but no live engine owns it — the RUN-level analog of the
// dead-agent zombie SLOT check (koryph-k6o). engineAlive must be true only when
// a live engine still holds this project's koryph.lock (Store.LockHolder ok &&
// alive, or Store.LockPID probed through the caller's own liveness func).
//
// The gate is deliberately run.Status == RunRunning, NOT a general
// !terminal-run check: paused-quota and hard-stop-quota runs also lack a live
// engine but are intentionally parked awaiting resume, not crashed, so they
// must render as themselves — only a status=running run whose engine is gone is
// the phantom a killed engine (SIGKILL, no finalization) leaves behind, frozen
// at running with nothing left to revisit it. Read-only: this never mutates the
// run; the remediation is a separate, explicit `koryph ops reconcile`.
func RunDead(run *Run, engineAlive bool) bool {
	return run != nil && run.Status == RunRunning && !engineAlive
}

// Recovery decision actions (mirrors the Decision.Action contract).
const (
	ActionSkip          = "skip"
	ActionReattach      = "reattach"
	ActionFinalize      = "finalize"
	ActionRequeueResume = "requeue-resume"
	ActionRequeueFresh  = "requeue-fresh"
	ActionBlocked       = "blocked"
)

// Probe supplies the external signals Classify needs but cannot compute on its
// own: process liveness and per-branch commit counts. Either func may be nil.
// A nil Alive means no PID can be confirmed alive (every slot is treated as
// dead). A nil CommitCount disables the branch fallback (only the slot's
// recorded Commits count).
type Probe struct {
	Alive func(pid int) bool
	// AliveSlot is the identity-aware liveness probe used when a caller can
	// authenticate more than a numeric PID. When supplied it takes precedence
	// over Alive, allowing resume to reject a recycled PID safely.
	AliveSlot   func(*Slot) bool
	CommitCount func(branch string) (int, error)
	// CompletionReady reports whether a dead slot has durable evidence that
	// implementation finished and should resume candidate finalization rather
	// than launch another coding agent.
	CompletionReady func(*Slot) bool
}

// Classify computes one recovery Decision per slot, implementing the contract
// documented in types.go. Precedence per slot:
//
//  1. terminal status            → skip
//  2. PID > 0 and Alive(PID)     → reattach
//  3. dead + completion ready    → finalize (even at the attempt ceiling)
//  4. Attempts >= MaxAttempts    → blocked
//  5. dead and commits > 0       → requeue-resume (reason names last commit)
//  6. dead and no commits        → requeue-fresh
//
// Commits are taken from slot.Commits, falling back to CommitCount(branch) only
// when the slot records zero commits and has a branch. SlotStuck is not
// terminal, so it flows through the liveness check like a running slot.
// Output is sorted by PhaseID for deterministic results.
func Classify(run *Run, p Probe) []Decision {
	if run == nil {
		return nil
	}
	out := make([]Decision, 0, len(run.Slots))
	for key, sl := range run.Slots {
		if sl == nil {
			continue
		}
		id := sl.PhaseID
		if id == "" {
			id = key
		}

		switch {
		case Terminal(sl.Status):
			out = append(out, Decision{PhaseID: id, Action: ActionSkip, Reason: "terminal: " + sl.Status})

		case sl.PID > 0 && p.AliveSlot != nil && p.AliveSlot(sl):
			out = append(out, Decision{
				PhaseID: id,
				Action:  ActionReattach,
				Reason:  fmt.Sprintf("pid %d authenticated alive", sl.PID),
			})

		case sl.PID > 0 && p.AliveSlot == nil && p.Alive != nil && p.Alive(sl.PID):
			out = append(out, Decision{
				PhaseID: id,
				Action:  ActionReattach,
				Reason:  fmt.Sprintf("pid %d alive", sl.PID),
			})

		case completionReady(sl, p):
			out = append(out, Decision{
				PhaseID: id,
				Action:  ActionFinalize,
				Reason:  "dead with completion-ready candidate state",
			})

		case sl.Attempts >= MaxAttempts:
			out = append(out, Decision{
				PhaseID: id,
				Action:  ActionBlocked,
				Reason:  fmt.Sprintf("attempts %d >= max %d", sl.Attempts, MaxAttempts),
			})

		default:
			out = append(out, classifyDead(id, sl, p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PhaseID < out[j].PhaseID })
	return out
}

// completionReady prevents advisory artifacts from becoming a recovery
// transition. Every state, including a previously recorded
// review/merge/finalizing state, needs the typed dispatch/result stamps and
// the caller's fresh live validation. Status alone is never an adoption
// credential.
func completionReady(sl *Slot, p Probe) bool {
	return sl.DispatchBaseSHA != "" &&
		sl.DispatchGeneration != "" &&
		p.CompletionReady != nil &&
		p.CompletionReady(sl)
}

// classifyDead handles a slot whose process is gone (or never confirmed alive):
// resume from existing commits if there are any, otherwise requeue fresh.
func classifyDead(id string, sl *Slot, p Probe) Decision {
	commits := sl.Commits
	if commits == 0 && sl.Branch != "" && p.CommitCount != nil {
		if n, err := p.CommitCount(sl.Branch); err == nil {
			commits = n
		}
	}
	if commits > 0 {
		reason := fmt.Sprintf("dead with %d commit(s)", commits)
		if sl.LastCommit != "" {
			reason += ", last " + sl.LastCommit
		}
		return Decision{PhaseID: id, Action: ActionRequeueResume, Reason: reason}
	}
	return Decision{PhaseID: id, Action: ActionRequeueFresh, Reason: "dead, no commits"}
}
