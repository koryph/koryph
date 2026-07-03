// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"fmt"
	"sort"
)

// Recovery decision actions (mirrors the Decision.Action contract).
const (
	ActionSkip          = "skip"
	ActionReattach      = "reattach"
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
	Alive       func(pid int) bool
	CommitCount func(branch string) (int, error)
}

// Classify computes one recovery Decision per slot, implementing the contract
// documented in types.go. Precedence per slot:
//
//  1. terminal status            → skip
//  2. Attempts >= MaxAttempts    → blocked   (checked before liveness)
//  3. PID > 0 and Alive(PID)     → reattach
//  4. dead and commits > 0       → requeue-resume (reason names last commit)
//  5. dead and no commits        → requeue-fresh
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

		case sl.Attempts >= MaxAttempts:
			out = append(out, Decision{
				PhaseID: id,
				Action:  ActionBlocked,
				Reason:  fmt.Sprintf("attempts %d >= max %d", sl.Attempts, MaxAttempts),
			})

		case sl.PID > 0 && p.Alive != nil && p.Alive(sl.PID):
			out = append(out, Decision{
				PhaseID: id,
				Action:  ActionReattach,
				Reason:  fmt.Sprintf("pid %d alive", sl.PID),
			})

		default:
			out = append(out, classifyDead(id, sl, p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PhaseID < out[j].PhaseID })
	return out
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
