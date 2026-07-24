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
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/worktree"
)

// portableCompletion is the runtime-neutral status.json subset agents update
// through KORYPH_STATUS_PATH. Unknown fields remain forwards-compatible.
type portableCompletion struct {
	State      string `json:"state"`
	BlockKind  string `json:"block_kind,omitempty"`
	Capability string `json:"capability,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type candidateAssessment struct {
	eligible         bool
	retryableBlock   bool
	capabilityBlock  bool
	capability       string
	capabilityDetail string
	reason           string
}

func readCompletion(path string) (portableCompletion, error) {
	if path == "" {
		return portableCompletion{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return portableCompletion{}, nil
		}
		return portableCompletion{}, err
	}
	var status portableCompletion
	if err := json.Unmarshal(data, &status); err != nil {
		return portableCompletion{}, err
	}
	status.State = strings.TrimSpace(status.State)
	status.BlockKind = strings.TrimSpace(status.BlockKind)
	status.Capability = strings.TrimSpace(status.Capability)
	status.Detail = phasecontrol.SanitizeDetail(status.Detail)
	return status, nil
}

func completionState(path string) (string, error) {
	status, err := readCompletion(path)
	return status.State, err
}

// assessCandidate validates the portable output contract before any pipeline,
// review, PR, or merge work. The invariant is deliberately outside every
// runtime adapter: Claude, Codex, and future runtimes all hand the engine the
// same branch, worktree, and status document.
func (r *runner) assessCandidate(ctx context.Context, sl *ledger.Slot) candidateAssessment {
	var reasons []string
	reportedBlock := false
	genericHostSelfBlock := false
	capabilityBlock := false
	structuredBlockMalformed := false
	capability := ""
	capabilityDetail := ""
	commits := 0
	clean := false

	if sl.StatusPath != "" {
		completion, err := readCompletion(sl.StatusPath)
		if err != nil {
			reasons = append(reasons, "completion status is malformed or unreadable: "+err.Error())
		} else {
			switch strings.ToLower(completion.State) {
			case "blocked", "failed", "error", "cancelled", "canceled":
				reportedBlock = true
				genericHostSelfBlock = strings.EqualFold(completion.State, "blocked")
				reasons = append(reasons, "agent reported completion state "+completion.State)
				switch completion.BlockKind {
				case "":
				case "capability":
					if err := phasecontrol.ValidateCapability(completion.Capability); err != nil {
						structuredBlockMalformed = true
						reasons = append(reasons, "capability block is malformed: "+err.Error())
					} else {
						capabilityBlock = true
						capability = completion.Capability
						capabilityDetail = obs.RedactValue(completion.Detail)
						reasons = append(reasons, "host capability "+capability+" is unavailable")
					}
				default:
					structuredBlockMalformed = true
					reasons = append(reasons, "completion block_kind is unsupported: "+completion.BlockKind)
				}
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
			// A generic clean self-block receives exactly one same-tier retry so
			// the updated worker contract can correct it to a structured host
			// capability block. A second generic block is terminal: another
			// dispatch cannot add classification evidence and must not reach the
			// final frontier escalation.
			retryableBlock:   genericHostSelfBlock && reportedBlock && !capabilityBlock && !structuredBlockMalformed && commits > 0 && clean && sl.Attempts == 1,
			capabilityBlock:  capabilityBlock,
			capability:       capability,
			capabilityDetail: capabilityDetail,
			reason:           strings.Join(reasons, "; "),
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

func (r *runner) parkCapabilityBlock(ctx context.Context, sl *ledger.Slot, capability, detail string) {
	capability = strings.TrimSpace(capability)
	detail = obs.RedactValue(phasecontrol.SanitizeDetail(detail))
	note := fmt.Sprintf(
		"host capability %s is unavailable: %s — no coding-agent retry or model escalation; branch/worktree preserved",
		capability, detail,
	)
	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.Status = ledger.SlotBlocked
		s.Note = note
	})
	r.checkpointSlot(sl, "capability-blocked")
	r.releaseGlobalSlot(sl.PhaseID)
	r.capabilityBlocked = true
	r.capabilityBlockBead = sl.PhaseID
	r.progress("ERROR: bead %s capability-blocked (%s)", sl.PhaseID, note)
	logCapabilityBlocked(r.run.RunID, r.opts.ProjectID, sl.PhaseID, capability, detail, sl.Model, sl.Attempts)
	r.reconcileBlockedBead(ctx, sl, "capability "+capability+": "+detail)
	if r.reg != nil {
		_ = r.reg.Audit(registry.Event{
			Kind:      "capability-blocked",
			ProjectID: r.opts.ProjectID,
			Actor:     r.owner,
			Detail: map[string]string{
				"bead":       sl.PhaseID,
				"capability": capability,
				"detail":     detail,
				"branch":     sl.Branch,
			},
		})
	}
}
