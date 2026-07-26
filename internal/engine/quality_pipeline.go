// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/review"
	"github.com/koryph/koryph/internal/version"
)

func (r *runner) gateEvidencePath(sl *ledger.Slot) string {
	if sl != nil && strings.TrimSpace(sl.GateEvidencePath) != "" {
		return sl.GateEvidencePath
	}
	return ""
}

func (r *runner) finalizationEvidenceDir(sl *ledger.Slot) string {
	return r.store.EvidenceDir(r.run.RunID, sl.PhaseID)
}

func finalizationArtifactID(generation finalizationGeneration) string {
	raw := strings.Join([]string{
		generation.phaseID,
		generation.stage,
		fmt.Sprintf("%d", generation.attempt),
		generation.branch,
		generation.dispatchGeneration,
		fmt.Sprintf("%d", generation.revalidation),
		generation.candidateSHA,
		generation.baseSHA,
		generation.evidenceKey,
		generation.evidenceDigest,
	}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum[:12])
}

func (r *runner) gateEvidencePathFor(sl *ledger.Slot, generation finalizationGeneration) string {
	return filepath.Join(
		r.finalizationEvidenceDir(sl),
		"gate-evidence-"+finalizationArtifactID(generation)+".json",
	)
}

func (r *runner) loadGateEvidence(sl *ledger.Slot) (*merge.GateEvidence, error) {
	if sl == nil || strings.TrimSpace(sl.GateEvidencePath) == "" ||
		strings.TrimSpace(sl.GateEvidenceDigest) == "" {
		return nil, errors.New("trusted gate evidence path or digest is absent")
	}
	evidence, digest, err := r.readGateEvidence(sl)
	if err != nil {
		return nil, err
	}
	if digest != strings.TrimSpace(sl.GateEvidenceDigest) {
		return nil, errors.New("gate evidence digest does not match the trusted ledger")
	}
	return evidence, nil
}

func (r *runner) readGateEvidence(sl *ledger.Slot) (*merge.GateEvidence, string, error) {
	if sl == nil || strings.TrimSpace(sl.GateEvidencePath) == "" {
		return nil, "", errors.New("gate evidence path is absent")
	}
	read, err := fsx.ReadRegularConfined(
		sl.GateEvidencePath, 64<<10, r.finalizationEvidenceDir(sl),
	)
	if err != nil {
		return nil, "", err
	}
	dec := json.NewDecoder(bytes.NewReader(read.Data))
	dec.DisallowUnknownFields()
	var evidence merge.GateEvidence
	if err := dec.Decode(&evidence); err != nil {
		return nil, "", err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, "", errors.New("gate evidence contains trailing JSON")
		}
		return nil, "", err
	}
	if err := merge.ValidateGateEvidence(&evidence); err != nil {
		return nil, "", err
	}
	return &evidence, "sha256:" + read.Digest, nil
}

func exactBuildIdentity() (string, error) {
	return version.BuildIdentity(EngineVersion)
}

func (r *runner) qualityMergeOpts(sl *ledger.Slot) (merge.Opts, error) {
	buildIdentity, err := exactBuildIdentity()
	if err != nil {
		return merge.Opts{}, err
	}
	return merge.Opts{
		RepoRoot:      r.rec.Root,
		Branch:        sl.Branch,
		DefaultBranch: r.rec.DefaultBranch,
		// Validation and worker full-gate commands must share one
		// ProcessGuard namespace. Only verdict/evidence artifacts live in the
		// separate engine-private directory.
		ValidationPhaseDir:  r.store.PhaseDir(r.run.RunID, sl.PhaseID),
		Gate:                append([]string(nil), r.cfg.Gate...),
		Extra:               append([]string(nil), r.cfg.ProtectedPaths...),
		RequireSigned:       r.requireSigned(),
		RequireConventional: r.cfg.EnforceConventional(),
		Reconcilers:         mergeReconcilers(r.cfg),
		Prepare:             append([]string(nil), r.cfg.MergePrepare...),
		EngineVersion:       EngineVersion,
		BuildIdentity:       buildIdentity,
		EvidencePath:        r.gateEvidencePath(sl),
		KeepWorktree:        true,
	}, nil
}

// validateSlot prepares and gates the candidate without holding the default
// branch merge slot. The result is persisted before any semantic review starts.
func (r *runner) validateSlot(ctx context.Context, sl *ledger.Slot) {
	opts, err := r.qualityMergeOpts(sl)
	if err != nil {
		r.parkTypedRecovery(ctx, sl, OutcomeEngineInvariant,
			"identify running validation binary: "+err.Error())
		return
	}
	opts.ValidateOnly = true
	generation := generationForStage(sl, finalizationGate)
	evidencePath := r.gateEvidencePathFor(sl, generation)
	opts.EvidencePath = evidencePath
	opts.SlotOwner = r.owner
	opts.SlotRetries = 3
	opts.Slot = r.slotLocker(ctx)
	if sl.FinalizationQueuedAt == "" {
		sl.FinalizationQueuedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.Status = ledger.SlotMerging
		s.FinalizationStage = finalizationGate
		if s.FinalizationQueuedAt == "" {
			s.FinalizationQueuedAt = sl.FinalizationQueuedAt
		}
		s.GateEvidencePath = evidencePath
		s.GateEvidenceDigest = ""
	})
	sl.GateEvidencePath = evidencePath
	sl.GateEvidenceDigest = ""
	r.checkpointSlot(sl, finalizationGate)
	r.submitFinalization(ctx, finalizationJob{
		generation: generation,
		stage:      finalizationGate,
		mergeOpts:  opts,
	})
}

func (r *runner) applyGateResult(ctx context.Context, sl *ledger.Slot, res merge.Result, err error) {
	if err != nil {
		r.parkTypedRecovery(ctx, sl, OutcomeRuntimeTransient,
			"authoritative validation infrastructure retries exhausted: "+err.Error())
		return
	}
	if res.Status != merge.StatusValidated {
		if r.handleMergeFailure(ctx, sl, res) {
			return
		}
		r.releaseGlobalSlot(sl.PhaseID)
		return
	}
	persisted, evidenceDigest, evidenceErr := r.readGateEvidence(sl)
	if evidenceErr != nil || !sameGateEvidence(persisted, res.Evidence) {
		if evidenceErr == nil {
			evidenceErr = errors.New("persisted evidence does not match validation result")
		}
		r.parkTypedRecovery(ctx, sl, OutcomeEngineInvariant,
			"authenticate authoritative gate evidence: "+evidenceErr.Error())
		return
	}
	if err := r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.GateEvidenceDigest = evidenceDigest
	}); err != nil {
		r.parkTypedRecovery(ctx, sl, OutcomeEngineInvariant,
			"persist authoritative gate evidence digest: "+err.Error())
		return
	}
	sl.GateEvidenceDigest = evidenceDigest
	if !r.finalizationEvidenceCurrent(
		ctx, sl, generationForEvidence(sl, persisted, finalizationGate),
	) {
		r.validateSlot(ctx, sl)
		return
	}
	r.progress("bead %s: authoritative gate passed for candidate %s on base %s",
		sl.PhaseID, shortSHA(res.Evidence.CandidateSHA), shortSHA(res.Evidence.BaseSHA))
	if r.opts.Review {
		r.startReview(ctx, sl)
		return
	}
	policy := r.mergePolicy(ctx, sl.EpicID)
	if r.opts.Direct {
		policy = project.PolicyAuto
	}
	r.finishAfterReview(ctx, sl, policy)
}

func sameGateEvidence(a, b *merge.GateEvidence) bool {
	return a != nil && b != nil &&
		a.Schema == b.Schema &&
		a.CandidateSHA == b.CandidateSHA &&
		a.BaseSHA == b.BaseSHA &&
		a.DiffDigest == b.DiffDigest &&
		a.GateConfigDigest == b.GateConfigDigest &&
		a.CommandDigest == b.CommandDigest &&
		a.EngineVersion == b.EngineVersion &&
		a.BuildIdentity == b.BuildIdentity &&
		a.CompletedAt.Equal(b.CompletedAt)
}

func gateEvidenceKey(e *merge.GateEvidence) string {
	if e == nil {
		return ""
	}
	return strings.Join([]string{
		e.Schema, e.CandidateSHA, e.BaseSHA, e.DiffDigest,
		e.GateConfigDigest, e.CommandDigest, e.EngineVersion, e.BuildIdentity,
		e.CompletedAt.UTC().Format(time.RFC3339Nano),
	}, "\x00")
}

// revalidationTargetKey identifies the live inputs that an engine-owned
// revalidation would consume. It deliberately includes both refs, the exact
// binary identity, and the merge refusal class. That permits any number of
// genuinely distinct base advances without sending work back to a model, while
// turning an identical repeated refusal into a visible engine-invariant block
// instead of an unbounded gate loop.
func (r *runner) revalidationTargetKey(
	ctx context.Context,
	sl *ledger.Slot,
	res merge.Result,
) (string, error) {
	if r.rec == nil || sl == nil || sl.Worktree == "" || sl.Branch == "" {
		return "", errors.New("revalidation target is missing repository or candidate identity")
	}
	base, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: r.rec.Root, Name: "git",
		Args: []string{"rev-parse", "--verify", r.rec.DefaultBranch + "^{commit}"},
	})
	if err != nil {
		return "", fmt.Errorf("resolve revalidation base: %w", err)
	}
	candidate, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: sl.Worktree, Name: "git",
		Args: []string{"rev-parse", "--verify", sl.Branch + "^{commit}"},
	})
	if err != nil {
		return "", fmt.Errorf("resolve revalidation candidate: %w", err)
	}
	buildIdentity, err := exactBuildIdentity()
	if err != nil {
		return "", err
	}
	raw := strings.Join([]string{
		strings.TrimSpace(base.Stdout),
		strings.TrimSpace(candidate.Stdout),
		buildIdentity,
		strings.TrimSpace(res.GateOutput),
	}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

// gateFailureTarget identifies one failed validation target and the candidate
// paths that target actually changes. Broad repository gates can fail because
// of ambient default-branch or infrastructure state. The engine uses this
// identity to confirm a failure once without a model, then uses changedPaths
// to keep any eventual coding repair inside the candidate's task boundary.
func (r *runner) gateFailureTarget(
	ctx context.Context,
	sl *ledger.Slot,
) (key string, changedPaths []string, err error) {
	if r.rec == nil || sl == nil || sl.Worktree == "" || sl.Branch == "" {
		return "", nil, errors.New("gate failure target is missing repository or candidate identity")
	}
	base, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: r.rec.Root, Name: "git",
		Args: []string{"rev-parse", "--verify", r.rec.DefaultBranch + "^{commit}"},
	})
	if err != nil {
		return "", nil, fmt.Errorf("resolve failed-gate base: %w", err)
	}
	candidate, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: sl.Worktree, Name: "git",
		Args: []string{"rev-parse", "--verify", sl.Branch + "^{commit}"},
	})
	if err != nil {
		return "", nil, fmt.Errorf("resolve failed-gate candidate: %w", err)
	}
	baseSHA := strings.TrimSpace(base.Stdout)
	candidateSHA := strings.TrimSpace(candidate.Stdout)
	mergeBase, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: sl.Worktree, Name: "git",
		Args: []string{"merge-base", baseSHA, candidateSHA},
	})
	if err != nil {
		return "", nil, fmt.Errorf("resolve failed-gate merge base: %w", err)
	}
	diff, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: sl.Worktree, Name: "git",
		Args: []string{
			"diff", "--name-only", "--no-renames",
			strings.TrimSpace(mergeBase.Stdout) + "..." + candidateSHA,
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("list failed-gate candidate paths: %w", err)
	}
	buildIdentity, err := exactBuildIdentity()
	if err != nil {
		return "", nil, err
	}
	raw := strings.Join([]string{
		baseSHA,
		candidateSHA,
		buildIdentity,
		"authoritative-gate-failed",
	}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("sha256:%x", sum[:]), strings.Fields(diff.Stdout), nil
}

// gateFailureTouchesCandidate is intentionally evidence-based rather than a
// semantic guess. Go and most build tools report either the changed file or
// its package/directory. A failure that names neither stays in the control
// plane; a task worker is never asked to repair unrelated repository state.
func gateFailureTouchesCandidate(output string, changedPaths []string) bool {
	output = strings.ToLower(strings.ReplaceAll(output, "\\", "/"))
	for _, changed := range changedPaths {
		changed = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(changed, "\\", "/")))
		if changed == "" {
			continue
		}
		if containsGatePath(output, changed) {
			return true
		}
		if cut := strings.LastIndex(changed, "/"); cut > 0 &&
			containsGatePath(output, changed[:cut]) {
			return true
		}
	}
	return false
}

func containsGatePath(output, path string) bool {
	for offset := 0; offset < len(output); {
		found := strings.Index(output[offset:], path)
		if found < 0 {
			return false
		}
		start := offset + found
		end := start + len(path)
		beforeOK := start == 0 || !gatePathIdentifierByte(output[start-1])
		afterOK := end == len(output) || !gatePathIdentifierByte(output[end])
		if beforeOK && afterOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func gatePathIdentifierByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

func (r *runner) finalizationEvidenceCurrent(
	ctx context.Context,
	sl *ledger.Slot,
	generation finalizationGeneration,
) bool {
	evidence, err := r.loadGateEvidence(sl)
	if err != nil ||
		evidence.CandidateSHA != generation.candidateSHA ||
		evidence.BaseSHA != generation.baseSHA ||
		gateEvidenceKey(evidence) != generation.evidenceKey ||
		strings.TrimSpace(sl.GateEvidenceDigest) != generation.evidenceDigest {
		return false
	}
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: sl.Worktree, Name: "git",
		Args: []string{"rev-parse", "--verify", sl.Branch + "^{commit}"},
	})
	return err == nil && strings.TrimSpace(res.Stdout) == generation.candidateSHA
}

func (r *runner) evidenceForLanding(ctx context.Context, sl *ledger.Slot) (*merge.GateEvidence, bool) {
	evidence, err := r.loadGateEvidence(sl)
	if err == nil {
		return evidence, true
	}
	r.parkTypedRecovery(ctx, sl, OutcomeEngineInvariant,
		fmt.Sprintf("load authoritative gate evidence: %v", err))
	return nil, false
}

func (r *runner) resumeGateEvidence(ctx context.Context, sl *ledger.Slot) (*merge.GateEvidence, bool) {
	evidence, err := r.loadGateEvidence(sl)
	if err != nil {
		return nil, false
	}
	opts, err := r.qualityMergeOpts(sl)
	if err != nil {
		return nil, false
	}
	current, err := merge.EvidenceCurrent(ctx, opts, evidence)
	if err == nil && current {
		return evidence, true
	}
	// Recovery after a partial landing is still evidence-authenticated. The
	// local default may already contain CandidateSHA even though publication
	// failed or the engine died before applying the result; landValidated will
	// resume only the missing push/cleanup transaction.
	if landed, ok := r.alreadyLandedSHA(ctx, sl); ok &&
		landed == evidence.CandidateSHA {
		return evidence, true
	}
	return evidence, false
}

var highRiskReviewPrefixes = []string{
	".github/",
	"hooks/",
	"internal/account/",
	"internal/auth",
	"internal/commandguard/",
	"internal/merge/",
	"internal/signing/",
	"internal/vault/",
}

func (r *runner) securityReviewRequired(
	ctx context.Context,
	sl *ledger.Slot,
	verdict review.Verdict,
) (bool, string) {
	if verdict.SecurityReviewRequired {
		reason := strings.TrimSpace(verdict.SecurityEvidence)
		if reason == "" {
			reason = "general reviewer requested security review"
		}
		return true, reason
	}
	issue := r.issueFor(ctx, sl)
	for _, label := range issue.Labels {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "security-risk", "risk:security", "security":
			return true, "explicit " + label + " label"
		}
	}
	evidence, ok := r.evidenceForLanding(ctx, sl)
	if !ok {
		return true, "authoritative gate evidence became unavailable"
	}
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: sl.Worktree, Name: "git",
		Args: []string{"diff", "--name-only", evidence.BaseSHA + "..." + evidence.CandidateSHA},
	})
	if err != nil {
		// Fail closed through the security lane when risk classification cannot
		// inspect the already-gated diff.
		return true, "high-risk path classification failed: " + err.Error()
	}
	prefixes := append([]string(nil), highRiskReviewPrefixes...)
	if r.cfg != nil {
		prefixes = append(prefixes, r.cfg.EffectiveReview().HighRiskPaths...)
	}
	for _, raw := range strings.Split(res.Stdout, "\n") {
		path := strings.ToLower(strings.TrimSpace(raw))
		for _, prefix := range prefixes {
			prefix = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(prefix, "\\", "/")))
			if riskPathMatches(path, prefix) {
				return true, "high-risk path " + strings.TrimSpace(raw)
			}
		}
	}
	return false, ""
}

func riskPathMatches(path, prefix string) bool {
	path = strings.Trim(strings.TrimSpace(strings.ReplaceAll(path, "\\", "/")), "/")
	prefix = strings.Trim(strings.TrimSpace(strings.ReplaceAll(prefix, "\\", "/")), "/")
	return path != "" && prefix != "" &&
		(path == prefix || strings.HasPrefix(path, prefix+"/"))
}
