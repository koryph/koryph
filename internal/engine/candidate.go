// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/worktree"
)

// portableCompletion is the runtime-neutral status.json subset agents update
// through KORYPH_STATUS_PATH. Unknown fields remain forwards-compatible.
type portableCompletion struct {
	State string `json:"state"`
}

type candidateAssessment struct {
	eligible       bool
	retryableBlock bool
	reason         string
}

func completionState(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var status portableCompletion
	if err := json.Unmarshal(data, &status); err != nil {
		return "", err
	}
	return strings.TrimSpace(status.State), nil
}

// assessCandidate validates the portable output contract before any pipeline,
// review, PR, or merge work. The invariant is deliberately outside every
// runtime adapter: Claude, Codex, and future runtimes all hand the engine the
// same branch, worktree, and status document.
func (r *runner) assessCandidate(ctx context.Context, sl *ledger.Slot) candidateAssessment {
	var reasons []string
	reportedBlock := false
	commits := 0
	clean := false

	if sl.StatusPath != "" {
		state, err := completionState(sl.StatusPath)
		if err != nil {
			reasons = append(reasons, "completion status is malformed or unreadable: "+err.Error())
		} else {
			switch strings.ToLower(state) {
			case "blocked", "failed", "error", "cancelled", "canceled":
				reportedBlock = true
				reasons = append(reasons, "agent reported completion state "+state)
			}
		}
	}

	commits, head, err := r.branchProgress(ctx, sl.Worktree)
	if err != nil {
		reasons = append(reasons, "cannot verify candidate commits: "+err.Error())
	} else {
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Commits = commits
			s.LastCommit = head
		})
		sl.Commits = commits
		sl.LastCommit = head
		if commits == 0 {
			reasons = append(reasons, "branch has no commits beyond the dispatch base")
		}
	}

	dirty, err := worktree.IsDirty(ctx, sl.Worktree)
	if err != nil {
		reasons = append(reasons, "cannot verify worktree cleanliness: "+err.Error())
	} else if dirty {
		reasons = append(reasons, "worktree has staged, unstaged, or untracked changes")
	} else {
		clean = true
	}

	if len(reasons) > 0 {
		return candidateAssessment{
			retryableBlock: reportedBlock && commits > 0 && clean && sl.Attempts < ledger.MaxAttempts,
			reason:         strings.Join(reasons, "; "),
		}
	}
	return candidateAssessment{eligible: true}
}

// candidateEligible preserves the compact contract used by existing callers
// and tests; finishCandidate needs the richer assessment to distinguish a
// bounded, clean self-block from a terminally incomplete candidate.
func (r *runner) candidateEligible(ctx context.Context, sl *ledger.Slot) (bool, string) {
	a := r.assessCandidate(ctx, sl)
	return a.eligible, a.reason
}

func (r *runner) parkIncompleteCandidate(ctx context.Context, sl *ledger.Slot, reason string) {
	note := fmt.Sprintf("candidate is not mergeable: %s — branch/worktree preserved for recovery", reason)
	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.Status = ledger.SlotBlocked
		s.Note = note
	})
	r.checkpointSlot(sl, "candidate-incomplete")
	r.releaseGlobalSlot(sl.PhaseID)
	r.progress("bead %s: blocked (%s)", sl.PhaseID, note)
	r.auditBlocked(ctx, sl, "candidate-incomplete", reason)
}
