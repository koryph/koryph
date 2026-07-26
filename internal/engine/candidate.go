// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/plan"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/worktree"
	"golang.org/x/sys/unix"
)

const maxCompletionStatusBytes = 64 << 10

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
	outcome          CandidateOutcomeClass
	capabilityBlock  bool
	capability       string
	capabilityDetail string
	reason           string
}

func readCompletion(path string) (portableCompletion, error) {
	if path == "" {
		return portableCompletion{}, nil
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return portableCompletion{}, nil
		}
		return portableCompletion{}, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return portableCompletion{}, errors.New("open completion status")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return portableCompletion{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxCompletionStatusBytes {
		return portableCompletion{}, errors.New("completion status must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCompletionStatusBytes+1))
	if err != nil {
		return portableCompletion{}, err
	}
	if len(data) == 0 || len(data) > maxCompletionStatusBytes {
		return portableCompletion{}, errors.New("completion status must be a bounded regular file")
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

// assessCandidate validates the portable output contract before any pipeline,
// review, PR, or merge work. The invariant is deliberately outside every
// runtime adapter: Claude, Codex, and future runtimes all hand the engine the
// same branch, worktree, and status document.
func (r *runner) assessCandidate(ctx context.Context, sl *ledger.Slot) candidateAssessment {
	var reasons []string
	reportedBlock := false
	capabilityBlock := false
	structuredBlockMalformed := false
	capability := ""
	capabilityDetail := ""
	commits := 0
	clean := false
	engineInvariantFailure := false
	inputInvalidFailure := false
	workerDefect := false
	completionContractMissing := false

	dispatchValid := false
	var dispatch phasecontrol.DispatchContext
	manifest, manifestErr := r.store.LoadManifest(r.run.RunID, sl.PhaseID)
	if manifestErr != nil {
		engineInvariantFailure = true
		reasons = append(reasons, "dispatch manifest is missing or unreadable: "+manifestErr.Error())
	} else {
		dispatch = phasecontrol.DispatchContext{
			RunID: r.run.RunID, PhaseID: sl.PhaseID, Attempt: sl.Attempts,
			SessionID: sl.SessionID, BaseSHA: sl.DispatchBaseSHA,
		}
		expectedGeneration := phasecontrol.DispatchGeneration(dispatch)
		switch {
		case sl.DispatchBaseSHA == "" || sl.DispatchGeneration == "":
			engineInvariantFailure = true
			reasons = append(reasons, "slot is missing trusted dispatch identity")
		case sl.SessionID == "":
			engineInvariantFailure = true
			reasons = append(reasons, "slot dispatch session is missing")
		case manifest.BeadID != sl.PhaseID:
			engineInvariantFailure = true
			reasons = append(reasons, "dispatch manifest phase does not match slot")
		case manifest.Attempt != sl.Attempts:
			engineInvariantFailure = true
			reasons = append(reasons, "dispatch manifest attempt does not match slot")
		case manifest.SessionID != sl.SessionID:
			engineInvariantFailure = true
			reasons = append(reasons, "dispatch manifest session does not match slot")
		case manifest.BaseCommit != sl.DispatchBaseSHA:
			engineInvariantFailure = true
			reasons = append(reasons, "dispatch manifest base SHA does not match slot")
		case manifest.WorktreePath != sl.Worktree || manifest.Branch != sl.Branch:
			engineInvariantFailure = true
			reasons = append(reasons, "dispatch manifest worktree or branch does not match slot")
		case sl.DispatchGeneration != expectedGeneration:
			engineInvariantFailure = true
			reasons = append(reasons, "slot dispatch generation is invalid")
		case manifest.DispatchGeneration != sl.DispatchGeneration:
			engineInvariantFailure = true
			reasons = append(reasons, "dispatch manifest generation does not match slot")
		default:
			dispatchValid = true
		}
	}

	dirty, err := worktree.IsDirty(ctx, sl.Worktree)
	if err != nil {
		engineInvariantFailure = true
		reasons = append(reasons, "cannot verify worktree cleanliness: "+err.Error())
	} else if dirty {
		workerDefect = true
		reasons = append(reasons, "worktree has staged, unstaged, or untracked changes")
	} else {
		clean = true
	}
	var head string
	// A typed dispatch uses only the immutable base. branchProgress is retained
	// as a legacy diagnostic fallback when identity is missing, but its
	// current-default-branch count must never poison a valid old-base result.
	if dispatchValid {
		state, err := phasecontrol.InspectCandidate(ctx, sl.Worktree, dispatch.BaseSHA)
		if err != nil {
			engineInvariantFailure = true
			reasons = append(reasons, "cannot inspect candidate against dispatch base: "+err.Error())
		} else {
			commits, head, clean = state.CommitCount, state.SHA, state.Clean
			if commits == 0 {
				workerDefect = true
				reasons = append(reasons, "branch has no commits beyond the dispatch base")
			}
			if !clean && !dirty {
				workerDefect = true
				reasons = append(reasons, "worktree became dirty during candidate inspection")
			}
			observedHead := head
			if commits == 0 {
				// Preserve the ledger's established meaning: LastCommit is
				// the candidate commit beyond the dispatch base, not HEAD
				// itself when no candidate commit exists.
				observedHead = ""
			}
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
				s.Commits = commits
				s.LastCommit = observedHead
			})
			sl.Commits = commits
			sl.LastCommit = observedHead
		}
	} else {
		commits, head, err = r.branchProgress(ctx, sl.Worktree)
		if err != nil {
			engineInvariantFailure = true
			reasons = append(reasons, "cannot verify candidate commits: "+err.Error())
		} else {
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
				s.Commits = commits
				s.LastCommit = head
			})
			sl.Commits = commits
			sl.LastCommit = head
			if commits == 0 {
				workerDefect = true
				reasons = append(reasons, "branch has no commits beyond the dispatch base")
			}
		}
	}

	resultValid := false
	phaseDir := r.store.PhaseDir(r.run.RunID, sl.PhaseID)
	resultPath := phasecontrol.ResultPath(phaseDir)
	if _, err := fsx.ReadRegularConfined("SUMMARY.md", 1<<20, phaseDir); err != nil {
		workerDefect = true
		reasons = append(reasons, "completion summary is missing or unreadable: "+err.Error())
	}
	result, resultErr := phasecontrol.LoadResult(phaseDir)
	resultMissing := errors.Is(resultErr, os.ErrNotExist)
	var expectedCriteria []plan.Criterion
	switch {
	case resultMissing:
		completionContractMissing = true
		reasons = append(reasons, "terminal result manifest is missing")
	case resultErr != nil:
		workerDefect = true
		reasons = append(reasons, "terminal result manifest is malformed or unreadable: "+resultErr.Error())
	case !dispatchValid:
		reasons = append(reasons, "terminal result cannot be matched without a valid dispatch manifest")
	default:
		issue := r.issueFor(ctx, sl)
		expectedCriteria, err = plan.ParseStrictCriteria(issue.AcceptanceCriteria)
		if err != nil {
			inputInvalidFailure = true
			reasons = append(reasons, "issue acceptance criteria are not strict: "+err.Error())
			break
		}
		if err := phasecontrol.ValidateResult(result, phasecontrol.ValidationContext{
			PhaseDir:         phaseDir,
			Worktree:         sl.Worktree,
			Dispatch:         dispatch,
			CandidateSHA:     head,
			CommitCount:      commits,
			WorktreeClean:    clean,
			ExpectedCriteria: expectedCriteria,
		}); err != nil {
			workerDefect = true
			reasons = append(reasons, "terminal result manifest does not match live candidate: "+err.Error())
		} else {
			resultValid = true
		}
	}

	// status.json is a heartbeat and optional failure/capability advisory, not
	// a success owner. Once the current SHA-bound result validates, even a
	// stale "blocked" heartbeat is ignored. Without a valid result, explicit
	// failure status can still classify why the candidate must not finalize.
	if !resultValid && sl.StatusPath != "" {
		completion, err := readCompletion(sl.StatusPath)
		if err != nil {
			workerDefect = true
			reasons = append(reasons, "completion status is malformed or unreadable: "+err.Error())
		} else {
			switch strings.ToLower(completion.State) {
			case "blocked", "failed", "error", "cancelled", "canceled":
				reportedBlock = true
				workerDefect = true
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

	if len(reasons) > 0 {
		outcome := OutcomeCodeDefect
		if engineInvariantFailure {
			outcome = OutcomeEngineInvariant
		} else if inputInvalidFailure {
			outcome = OutcomeInputInvalid
		} else if capabilityBlock {
			outcome = OutcomeCapabilityUnavailable
		} else if completionContractMissing && !workerDefect && dispatchValid &&
			!reportedBlock && commits > 0 && clean {
			outcome = OutcomeCompletionContractMissing
		}
		retryable := false
		if outcome == OutcomeCompletionContractMissing && !structuredBlockMalformed {
			decision := DecideRetry(RetryPolicyInput{Outcome: outcome, Budgets: retryBudgets(sl.Retry)})
			retryable = decision.Action == RecoveryTargetedRepair
		}
		return candidateAssessment{
			// Only a clean committed candidate missing exactly its terminal
			// result receives the one same-tier completion repair. A dirty,
			// commitless, explicitly blocked, stale, or malformed candidate
			// parks with its work preserved.
			retryableBlock:   retryable,
			outcome:          outcome,
			capabilityBlock:  capabilityBlock,
			capability:       capability,
			capabilityDetail: capabilityDetail,
			reason:           strings.Join(reasons, "; "),
		}
	}
	if !resultValid {
		return candidateAssessment{outcome: OutcomeEngineInvariant, reason: "terminal result validation did not produce a verdict"}
	}
	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.CandidateGeneration = result.Generation
		s.CandidateResultPath = resultPath
		s.OutcomeClass = string(OutcomeCandidateReady)
	})
	sl.CandidateGeneration = result.Generation
	sl.CandidateResultPath = resultPath
	sl.OutcomeClass = string(OutcomeCandidateReady)
	return candidateAssessment{eligible: true, outcome: OutcomeCandidateReady}
}

// candidateEligible preserves the compact contract used by existing callers
// and tests; finishCandidate needs the richer assessment to distinguish a
// bounded, clean self-block from a terminally incomplete candidate.
func (r *runner) candidateEligible(ctx context.Context, sl *ledger.Slot) (bool, string) {
	a := r.assessCandidate(ctx, sl)
	return a.eligible, a.reason
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
	issue := r.issueFor(ctx, sl)
	hold, _, _ := r.store.LoadCapabilityHold(sl.PhaseID)
	evidence := r.capabilityEvidenceHash(ctx, issue, hold)
	if err := r.store.SetCapabilityHold(ledger.CapabilityHold{
		BeadID:       sl.PhaseID,
		Capability:   capability,
		EvidenceHash: evidence,
		RetryLimit:   capabilityRetryLimit,
	}); err != nil {
		r.progress("bead %s: warning: capability hold persistence failed: %v", sl.PhaseID, err)
	}
	r.checkpointSlot(sl, "capability-blocked")
	r.releaseGlobalSlot(sl.PhaseID)
	r.progress("ERROR: bead %s capability-blocked (%s)", sl.PhaseID, note)
	logCapabilityBlocked(r.run.RunID, r.opts.ProjectID, sl.PhaseID, capability, detail, evidence, sl.Model, sl.Attempts)
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
				"evidence":   evidence,
			},
		})
	}
}
