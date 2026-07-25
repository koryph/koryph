// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/textx"
	"github.com/koryph/koryph/internal/worktree"
)

var (
	buildValidationEvidence   = buildGateEvidenceFromCheckpoint
	persistValidationEvidence = persistGateEvidence
)

// validateOnly captures one immutable base under the short merge slot, then
// releases the slot before it mutates only the candidate worktree and runs the
// expensive gate exactly once.
func validateOnly(
	ctx context.Context,
	o Opts,
	wt *worktree.Info,
	def string,
) (Result, error) {
	checkpoint := o.ValidationCheckpoint
	if checkpoint != nil {
		current, err := validationCheckpointCurrent(ctx, o, wt, checkpoint)
		if err != nil {
			return Result{Status: StatusError, ValidationCheckpoint: checkpoint},
				fmt.Errorf("authenticate successful-gate checkpoint: %w", err)
		}
		if !current {
			return Result{
				Status:     StatusEvidenceStale,
				GateOutput: "successful-gate checkpoint candidate/base/config changed",
			}, nil
		}
	} else {
		if o.Slot == nil {
			return Result{Status: StatusError}, errors.New("authoritative validation requires the merge slot for base capture")
		}
		baseSHA, err := captureValidationBase(ctx, o, def)
		if err != nil {
			return Result{Status: StatusError}, err
		}
		if res, ok, err := preflight(ctx, o, wt, baseSHA); !ok {
			return res, err
		}
		if dirty, err := candidateWorktreeDirty(ctx, wt.Path); err != nil {
			return Result{Status: StatusError}, err
		} else if dirty {
			return Result{
				Status:     StatusDirty,
				GateOutput: "candidate worktree became dirty before validation; work preserved",
			}, nil
		}

		var reconciled []string
		var reconcileRounds int
		if anc, err := gitRun(ctx, wt.Path, "merge-base", "--is-ancestor", baseSHA, "HEAD"); err != nil {
			return Result{Status: StatusError}, err
		} else if anc.ExitCode != 0 {
			rb, err := gitRun(ctx, wt.Path, "rebase", baseSHA)
			if err != nil {
				return Result{Status: StatusError}, err
			}
			if rb.ExitCode != 0 {
				healed, paths, rounds, err := reconcileRebase(ctx, wt.Path, o.Reconcilers)
				if err != nil {
					_, _ = gitRun(ctx, wt.Path, "rebase", "--abort")
					return Result{Status: StatusError}, err
				}
				if !healed {
					_, _ = gitRun(ctx, wt.Path, "rebase", "--abort")
					mdPath := filepath.Join(wt.Path, conflictBreadcrumb)
					_ = fsx.WriteAtomic(mdPath,
						[]byte(worktree.ConflictMarkdown(o.Branch, baseSHA, rb.Stdout+rb.Stderr)),
						0o644)
					return Result{Status: StatusConflict, ConflictMD: mdPath}, nil
				}
				reconciled, reconcileRounds = paths, rounds
			}
		}

		prepared := false
		if len(o.Prepare) > 0 {
			p, ok, out, err := runMergePrepare(ctx, wt.Path, baseSHA, o.Prepare)
			if err != nil {
				return Result{Status: StatusError}, err
			}
			if !ok {
				_, _ = gitRun(ctx, wt.Path, "checkout", "--", ".")
				return Result{Status: StatusGateFailed, GateOutput: textx.Tail(out, gateOutputCap)}, nil
			}
			prepared = p
		}

		checkpoint, err = buildValidationCheckpoint(
			ctx, o, wt, baseSHA, reconciled, reconcileRounds, prepared,
		)
		if err != nil {
			return Result{Status: StatusError}, fmt.Errorf("prepare successful-gate checkpoint: %w", err)
		}
		if !o.SkipGate && len(o.Gate) > 0 {
			ok, out, infraErr := runGate(ctx, wt.Path, o.ValidationPhaseDir, o.Gate)
			if infraErr != nil {
				return Result{Status: StatusError, GateOutput: textx.Tail(out, gateOutputCap)},
					fmt.Errorf("run authoritative gate infrastructure: %w", infraErr)
			}
			if !ok {
				_, _ = gitRun(ctx, wt.Path, "checkout", "--", ".")
				return Result{Status: StatusGateFailed, GateOutput: textx.Tail(out, gateOutputCap)}, nil
			}
		}
		checkpoint.CompletedAt = time.Now().UTC()
	}

	// Everything below this line consumes an authenticated successful-gate
	// checkpoint. Infrastructure failure returns it to the finalization lane,
	// which retries only this tail and never reruns the expensive gate.
	current, err := validationCheckpointCurrent(ctx, o, wt, checkpoint)
	if err != nil {
		return Result{Status: StatusError, ValidationCheckpoint: checkpoint},
			fmt.Errorf("authenticate successful-gate checkpoint after gate: %w", err)
	}
	if !current {
		return Result{
			Status:     StatusEvidenceStale,
			GateOutput: "candidate or configuration changed during authoritative gate",
		}, nil
	}
	if o.RequireSigned {
		bad, err := verifySignatures(ctx, wt.Path, checkpoint.BaseSHA, o.Branch)
		if err != nil {
			return Result{Status: StatusError, ValidationCheckpoint: checkpoint}, err
		}
		if len(bad) > 0 {
			return Result{
				Status: StatusUnsigned,
				GateOutput: "unsigned or unverifiable commits after validation rebase:\n" +
					strings.Join(bad, "\n"),
				ValidationCheckpoint: checkpoint,
			}, nil
		}
	}
	evidence, err := buildValidationEvidence(ctx, o, wt, checkpoint)
	if err != nil {
		return Result{Status: StatusError, ValidationCheckpoint: checkpoint}, err
	}
	if err := persistValidationEvidence(o.EvidencePath, evidence); err != nil {
		return Result{Status: StatusError, ValidationCheckpoint: checkpoint},
			fmt.Errorf("persist gate evidence: %w", err)
	}
	return Result{
		Status: StatusValidated, Evidence: evidence,
		Reconciled: checkpoint.Reconciled, ReconcileRounds: checkpoint.ReconcileRounds,
		Prepared: checkpoint.Prepared, ValidationCheckpoint: checkpoint,
	}, nil
}

func captureValidationBase(ctx context.Context, o Opts, def string) (string, error) {
	hasRemote, err := remoteExists(ctx, o.RepoRoot)
	if err != nil {
		return "", err
	}
	if hasRemote {
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.RepoRoot, Name: "git", Args: []string{"fetch", "origin", def},
		}); err != nil {
			return "", err
		}
	}
	if err := o.Slot.Acquire(ctx, o.SlotOwner); err != nil {
		return "", fmt.Errorf("acquire merge slot for validation base: %w", err)
	}
	defer func() { _ = o.Slot.Release(ctx) }()

	if _, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.RepoRoot, Name: "git", Args: []string{"checkout", def},
	}); err != nil {
		return "", err
	}
	if hasRemote {
		ff, err := gitRun(ctx, o.RepoRoot, "merge", "--ff-only", "origin/"+def)
		if err != nil {
			return "", err
		}
		if ff.ExitCode != 0 {
			return "", fmt.Errorf("local %s cannot fast-forward to captured origin/%s: %s",
				def, def, strings.TrimSpace(textx.Tail(ff.Stderr, 400)))
		}
	}
	return gitCommit(ctx, o.RepoRoot, def)
}
