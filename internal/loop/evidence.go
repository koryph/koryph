// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package loop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	koryphgc "github.com/koryph/koryph/internal/gc"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/metrics"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/resmon"
	"github.com/koryph/koryph/internal/review"
	"github.com/koryph/koryph/internal/strictjson"
)

// NativeCanaryPublisher derives the report exclusively from product-owned
// supervisor state, engine ledgers, manifests, and immutable artifacts.
type NativeCanaryPublisher struct {
	RepoRoot   string
	Ledger     *ledger.Store
	Thresholds metrics.AutonomyThresholds
	Now        func() time.Time
}

func (p NativeCanaryPublisher) Publish(
	_ context.Context,
	request CanaryPublicationRequest,
) (CanaryPublication, error) {
	root, err := filepath.Abs(strings.TrimSpace(p.RepoRoot))
	if err != nil || root == "" {
		return CanaryPublication{}, errors.New("loop: autonomy publisher requires a repository root")
	}
	fixedPath := filepath.Join(root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath))
	if filepath.Clean(request.Canary.ReportPath) != fixedPath {
		return CanaryPublication{}, fmt.Errorf("loop: autonomy report path %s is not fixed path %s",
			request.Canary.ReportPath, fixedPath)
	}
	store := p.Ledger
	if store == nil {
		store = ledger.NewStore(root)
	}
	thresholds := p.Thresholds
	if thresholds.MinEligibleBeads == 0 {
		thresholds = metrics.DefaultAutonomyThresholds()
	}
	thresholdDigest, err := metrics.AutonomyThresholdsDigest(thresholds)
	if err != nil || thresholdDigest != request.Canary.AutonomyPolicyDigest {
		return CanaryPublication{}, errors.New("loop: autonomy publisher policy differs from admitted canary generation")
	}
	evidence, err := collectNativeCanaryEvidence(root, store, request)
	if err != nil {
		return CanaryPublication{}, err
	}
	input := metrics.AutonomyInput{
		SchemaVersion:          metrics.AutonomyInputSchema,
		ProjectID:              request.ProjectID,
		InstalledCommit:        request.Canary.InstalledCommit,
		BinaryVersion:          request.Canary.BinaryVersion,
		BuildIdentity:          request.Canary.BuildIdentity,
		ContractDigest:         request.Canary.ContractDigest,
		RegistryIdentityDigest: request.Canary.RegistryIdentityDigest,
		GenerationDigest:       request.Canary.GenerationDigest,
		ExecutionPolicyDigest:  request.Canary.ExecutionPolicyDigest,
		Cohort:                 append([]string(nil), request.Canary.Cohort...),
		StartedAt:              request.StartedAt,
		EndedAt:                request.EndedAt,
		Thresholds:             thresholds,
		Evidence:               evidence,
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	report, err := metrics.PublishAutonomyReport(fixedPath, input, now)
	if errors.Is(err, metrics.ErrAutonomyReportExists) {
		// Publication may have completed immediately before a supervisor crash.
		// Accept the existing bytes only when rebuilding from current native
		// evidence at its original generation time produces the exact digest.
		existing, loadErr := metrics.LoadAutonomyReport(fixedPath)
		if loadErr != nil {
			return CanaryPublication{}, fmt.Errorf("loop: existing autonomy report is invalid: %w", loadErr)
		}
		generated, parseErr := time.Parse(time.RFC3339Nano, existing.GeneratedAt)
		if parseErr != nil {
			return CanaryPublication{}, parseErr
		}
		expected, buildErr := metrics.BuildAutonomyReport(input, generated)
		if buildErr != nil {
			return CanaryPublication{}, buildErr
		}
		if expected.EvidenceDigest != existing.EvidenceDigest {
			return CanaryPublication{}, errors.New("loop: existing autonomy report does not match native canary evidence")
		}
		report = existing
		err = nil
	}
	if err != nil {
		return CanaryPublication{}, err
	}
	decision := "failed"
	if report.Decision.Passed {
		decision = "passed"
	}
	return CanaryPublication{
		Path: fixedPath, Digest: report.EvidenceDigest, Decision: decision,
		GeneratedAt: report.GeneratedAt,
	}, nil
}

type nativeAttempt struct {
	runID string
	slot  ledger.Slot
}

type safetySet map[string]metrics.SafetyEvidence

func collectNativeCanaryEvidence(
	root string,
	store *ledger.Store,
	request CanaryPublicationRequest,
) (metrics.AutonomyEvidence, error) {
	safety := make(safetySet, len(metrics.RequiredSafetyInvariants))
	for _, name := range metrics.RequiredSafetyInvariants {
		safety[name] = metrics.SafetyEvidence{Name: name, Passed: true}
	}
	cohort := make(map[string]bool, len(request.Canary.Cohort))
	for _, id := range request.Canary.Cohort {
		cohort[id] = true
	}

	var attempts []nativeAttempt
	idleRuns := 0
	for _, runID := range request.Canary.RunIDs {
		run, err := store.LoadRun(runID)
		if err != nil {
			return metrics.AutonomyEvidence{}, fmt.Errorf("loop: load canary run %s: %w", runID, err)
		}
		if run.ProjectID != request.ProjectID {
			return metrics.AutonomyEvidence{}, fmt.Errorf("loop: canary run %s belongs to project %s", runID, run.ProjectID)
		}
		if len(run.Slots) == 0 {
			idleRuns++
		}
		for _, snapshot := range run.AttemptHistory {
			if cohort[snapshot.Slot.BeadID] || cohort[snapshot.Slot.PhaseID] {
				attempts = append(attempts, nativeAttempt{runID: runID, slot: snapshot.Slot})
			}
		}
		for _, sl := range run.Slots {
			if sl != nil && (cohort[sl.BeadID] || cohort[sl.PhaseID]) {
				attempts = append(attempts, nativeAttempt{runID: runID, slot: ledgerSlotCopy(*sl)})
			}
		}
		if run.TokenSemantics != ledger.CurrentTokenSemantics {
			failSafety(safety, "candidate-contract", "run "+runID+" uses legacy token semantics")
		}
	}
	sort.SliceStable(attempts, func(i, j int) bool {
		left, right := attempts[i], attempts[j]
		leftID, rightID := attemptBeadID(left.slot), attemptBeadID(right.slot)
		if leftID != rightID {
			return leftID < rightID
		}
		if left.slot.Attempts != right.slot.Attempts {
			return left.slot.Attempts < right.slot.Attempts
		}
		return left.runID < right.runID
	})

	seen := make(map[string]string)
	highest := make(map[string]int)
	for _, attempt := range attempts {
		id := attemptBeadID(attempt.slot)
		if attempt.slot.Attempts > highest[id] {
			highest[id] = attempt.slot.Attempts
		}
		key := fmt.Sprintf("%s\x00%d", id, attempt.slot.Attempts)
		identity := attempt.runID + "\x00" + attempt.slot.DispatchGeneration +
			"\x00" + attempt.slot.SessionID
		if prior, ok := seen[key]; ok && prior != identity {
			failSafety(safety, "resume-without-redispatch",
				fmt.Sprintf("duplicate logical attempt %s/%d", id, attempt.slot.Attempts))
		}
		seen[key] = identity
	}

	var normalized []metrics.AutonomyAttempt
	previousByBead := make(map[string]nativeAttempt)
	previousTokens := make(map[string]metrics.TokenEvidence)
	for index, attempt := range attempts {
		sl := attempt.slot
		id := attemptBeadID(sl)
		if id == "" || sl.Attempts <= 0 {
			failSafety(safety, "candidate-contract", "attempt identity is incomplete")
			continue
		}
		final := sl.Attempts == highest[id]
		terminal, hasTerminal := request.Canary.Terminal[id]
		outcome := metrics.TypedOutcome{Kind: canonicalAttemptOutcome(sl.OutcomeClass, false)}
		terminalAt := ""
		if final && hasTerminal {
			outcome.Kind = canonicalAttemptOutcome(terminal.Status, true)
			outcome.Terminal = true
			outcome.Correct = terminal.Good
			terminalAt = firstNonempty(sl.MergedAt, sl.FinishedAt, sl.UpdatedAt, request.EndedAt)
			if !terminal.Good {
				failSafety(safety, "candidate-contract", id+" lacks complete terminal quality evidence")
				failSafety(safety, "acceptance-criteria-satisfied", id+" did not pass terminal review")
			}
		}
		if final && hasTerminal && sl.OutcomeClass == "capability-unavailable" {
			outcome.Kind = metrics.OutcomeCapabilityHold
			outcome.BlockKind = metrics.ExclusionExternalCapability
			outcome.Capability = "capability-unavailable"
		}

		currentTokens := cumulativeTokens(sl)
		tokens, tokensOK := tokenDelta(currentTokens, previousTokens[id])
		if !tokensOK {
			failSafety(safety, "candidate-contract", id+" token counters regressed or overflowed")
		}
		previousTokens[id] = currentTokens

		reviewEvidence, generalReviewRef, securityRequired, reviewOK :=
			authenticatedReviewEvidence(
				store, attempt.runID, sl, sl.FinalizationTimings.Review.CompletedAt,
			)
		if !reviewOK && (sl.GeneralReviewArtifactPath != "" || (final && terminal.Good)) {
			failSafety(safety, "acceptance-criteria-satisfied", id+" review artifact is absent or unauthenticated")
		}
		var reviewRefs []metrics.ReviewEvidenceRef
		if reviewOK {
			reviewRefs = append(reviewRefs, generalReviewRef)
		}
		securityRef, securityOK := authenticatedSecurityReviewEvidence(
			store, attempt.runID, sl, sl.FinalizationTimings.SecurityReview.CompletedAt,
		)
		securityPresent := sl.SecurityReviewArtifactPath != "" ||
			sl.SecurityReviewArtifactDigest != "" ||
			sl.SecurityReviewCandidateSHA != "" || sl.SecurityReviewBaseSHA != ""
		if securityOK {
			reviewRefs = append(reviewRefs, securityRef)
		} else if securityPresent || (reviewOK && securityRequired) {
			failSafety(safety, "acceptance-criteria-satisfied", id+" security review artifact is unauthenticated")
		}
		gateRef, gateOK := authenticatedGateEvidence(store, attempt.runID, sl, request.Canary)
		if !gateOK && (sl.GateEvidencePath != "" || final) {
			failSafety(safety, "gate-candidate-tuple", id+" gate evidence is absent or unauthenticated")
			failSafety(safety, "signature-and-dco", id+" lacks authenticated gate evidence")
			failSafety(safety, "protected-path-refusal", id+" lacks authenticated gate evidence")
			failSafety(safety, "no-regex-mutation", id+" lacks authenticated deterministic gate evidence")
		}
		terminalRef, terminalOK := authenticatedTerminalManifest(store, attempt.runID, sl)
		if final && terminal.Good && !terminalOK {
			failSafety(safety, "candidate-contract", id+" terminal manifest does not match its dispatch")
		}

		nextDispatch := ""
		for j := index + 1; j < len(attempts); j++ {
			if attemptBeadID(attempts[j].slot) == id {
				nextDispatch = attempts[j].slot.DispatchedAt
				break
			}
		}
		processEvents, processOK := attemptProcessEvidence(store, attempt.runID, sl, nextDispatch)
		if !processOK {
			failSafety(safety, "single-broad-command", id+" command event evidence is malformed")
		}
		for _, event := range processEvents {
			if event.Duplicate {
				failSafety(safety, "single-broad-command", id+" repeated a broad command")
			}
		}
		if final && terminal.Good && !hasBroadCommandEvidence(processEvents) {
			failSafety(safety, "single-broad-command", id+" lacks authenticated broad-command lifecycle evidence")
		}

		modelTier, modelOK := normalizedModelTier(sl.ModelTier)
		if !modelOK {
			failSafety(safety, "candidate-contract", id+" model is outside the closed tier vocabulary")
		}
		if !knownAttemptOutcome(sl.OutcomeClass, false) {
			failSafety(safety, "candidate-contract", id+" outcome is outside the closed vocabulary")
		}
		if final && hasTerminal && !knownAttemptOutcome(terminal.Status, true) {
			failSafety(safety, "candidate-contract", id+" terminal outcome is outside the closed vocabulary")
		}
		frontierJustification := ""
		if modelTier == metrics.ModelTierFrontier && strings.Contains(sl.ModelWhy, "model-capability") {
			frontierJustification = "model-capability"
		}
		item := metrics.AutonomyAttempt{
			BeadID: id, RunID: attempt.runID, PhaseID: sl.PhaseID, Attempt: sl.Attempts,
			ModelTier: modelTier, ModelActual: sl.ModelActual,
			FrontierJustification: frontierJustification,
			DispatchedAt:          sl.DispatchedAt,
			TerminalAt:            terminalAt,
			Outcome:               outcome,
			Review:                reviewEvidence,
			Gate:                  stageTiming(sl.FinalizationTimings.Gate),
			SemanticReview: addStageTimings(
				stageTiming(sl.FinalizationTimings.Review),
				stageTiming(sl.FinalizationTimings.SecurityReview),
			),
			Merge:              stageTiming(sl.FinalizationTimings.Merge),
			PR:                 stageTiming(sl.FinalizationTimings.PR),
			ReviewEvidenceRefs: reviewRefs,
			ProcessEvents:      processEvents,
			Tokens:             tokens,
		}
		if gateOK {
			item.GateEvidence = &gateRef
		}
		if terminalOK {
			item.TerminalEvidence = &terminalRef
		}
		if previous, ok := previousByBead[id]; ok {
			priorClass := strings.TrimSpace(previous.slot.OutcomeClass)
			if !knownPriorFailureClass(priorClass) {
				failSafety(safety, "candidate-contract",
					id+" retry lacks an authenticated explicit prior failure class")
				failSafety(safety, "capability-no-redispatch",
					id+" retry provenance is missing its prior failure class")
			} else {
				item.Retry = &metrics.RetryEvidence{
					Kind:                canonicalAttemptOutcome(priorClass, false),
					PriorEvidenceDigest: nativeAttemptDigest(previous),
					EvidenceDigest:      nativeAttemptDigest(attempt),
				}
				if item.Retry.PriorEvidenceDigest == item.Retry.EvidenceDigest {
					failSafety(safety, "capability-no-redispatch", id+" retry evidence did not change")
				}
			}
		}
		previousByBead[id] = attempt
		normalized = append(normalized, item)
	}

	contractRead, err := fsx.ReadRegularConfined(filepath.Join(root, "AGENTS.md"), 1<<20, root)
	if err != nil || "sha256:"+contractRead.Digest != request.Canary.ContractDigest {
		failSafety(safety, "candidate-contract", "AGENTS.md digest changed during the canary")
	}
	runIDs := make(map[string]bool, len(request.Canary.RunIDs))
	for _, runID := range request.Canary.RunIDs {
		runIDs[runID] = true
	}
	pressureRuns := make(map[string]bool, len(request.Canary.Pressure))
	for _, pressure := range request.Canary.Pressure {
		if !runIDs[pressure.RunID] || pressureRuns[pressure.RunID] {
			failSafety(safety, "host-pressure-containment", "pressure sample has missing, foreign, or duplicate run identity")
		}
		pressureRuns[pressure.RunID] = true
		if pressure.Level == "critical" && pressure.Persistent {
			failSafety(safety, "host-pressure-containment", "persistent critical memory pressure")
		}
	}
	if len(pressureRuns) != len(runIDs) {
		failSafety(safety, "host-pressure-containment", "one or more engine runs lack a durable pressure sample")
	}
	applyHardStopSafety(safety, request.Canary.HardStop)
	inventoryAt, err := parseEvidenceTime(request.EndedAt)
	if err != nil {
		return metrics.AutonomyEvidence{}, fmt.Errorf("loop: artifact inventory timestamp: %w", err)
	}
	artifacts, artifactOK, err := nativeArtifactEvidence(
		store.KoryphRoot, request.Canary.ReportPath, inventoryAt,
	)
	if err != nil {
		return metrics.AutonomyEvidence{}, err
	}
	if !artifactOK {
		failSafety(safety, "candidate-contract", "artifact inventory contains an unsafe type or overflow")
	}
	safetyEvidence := make([]metrics.SafetyEvidence, 0, len(metrics.RequiredSafetyInvariants))
	for _, name := range metrics.RequiredSafetyInvariants {
		safetyEvidence = append(safetyEvidence, safety[name])
	}
	pressureEvidence := make([]metrics.PressureEvidence, 0, len(request.Canary.Pressure))
	for _, sample := range request.Canary.Pressure {
		pressureEvidence = append(pressureEvidence, metrics.PressureEvidence{
			At: sample.At, Level: sample.Level, Persistent: sample.Persistent,
			AvailableMB: sample.AvailableMB,
		})
	}
	return metrics.AutonomyEvidence{
		Attempts: normalized, Safety: safetyEvidence, Pressure: pressureEvidence,
		Artifacts: artifacts, IdleLedgerCreations: idleRuns,
	}, nil
}

func ledgerSlotCopy(sl ledger.Slot) ledger.Slot {
	sl.BeadLabels = append([]string(nil), sl.BeadLabels...)
	sl.Resources = append([]string(nil), sl.Resources...)
	return sl
}

func attemptBeadID(sl ledger.Slot) string {
	return firstNonempty(strings.TrimSpace(sl.BeadID), strings.TrimSpace(sl.PhaseID))
}

func cumulativeTokens(sl ledger.Slot) metrics.TokenEvidence {
	return metrics.TokenEvidence{
		Semantics: metrics.TokenSemanticsDisjointV1,
		Input:     sl.InputTokens, Output: sl.OutputTokens,
		CacheRead: sl.CacheReadTokens, CacheCreation: sl.CacheCreationTokens,
		ProviderTotalInput: sl.ProviderTotalInputTokens,
	}
}

func tokenDelta(current, prior metrics.TokenEvidence) (metrics.TokenEvidence, bool) {
	for _, value := range []int64{
		current.Input, current.Output, current.CacheRead, current.CacheCreation,
		current.ProviderTotalInput, prior.Input, prior.Output, prior.CacheRead,
		prior.CacheCreation, prior.ProviderTotalInput,
	} {
		if value < 0 {
			return metrics.TokenEvidence{Semantics: "invalid"}, false
		}
	}
	values := [4]int64{
		current.Input - prior.Input,
		current.Output - prior.Output,
		current.CacheRead - prior.CacheRead,
		current.CacheCreation - prior.CacheCreation,
	}
	if current.Input < prior.Input || current.Output < prior.Output ||
		current.CacheRead < prior.CacheRead || current.CacheCreation < prior.CacheCreation ||
		current.ProviderTotalInput < prior.ProviderTotalInput {
		return metrics.TokenEvidence{Semantics: "invalid"}, false
	}
	total := int64(0)
	for _, value := range values {
		if value < 0 || value > 0 && total > int64(^uint64(0)>>1)-value {
			return metrics.TokenEvidence{Semantics: "invalid"}, false
		}
		total += value
	}
	return metrics.TokenEvidence{
		Semantics: metrics.TokenSemanticsDisjointV1,
		Input:     values[0], Output: values[1],
		CacheRead: values[2], CacheCreation: values[3],
		Total: total, ProviderTotalInput: current.ProviderTotalInput - prior.ProviderTotalInput,
	}, true
}

func normalizedModelTier(tier string) (string, bool) {
	tier = strings.ToLower(strings.TrimSpace(tier))
	switch tier {
	case metrics.ModelTierLight:
		return metrics.ModelTierLight, true
	case metrics.ModelTierStandard:
		return metrics.ModelTierStandard, true
	case metrics.ModelTierFrontier:
		return metrics.ModelTierFrontier, true
	default:
		return metrics.ModelTierStandard, false
	}
}

func canonicalAttemptOutcome(value string, terminal bool) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if terminal {
		switch value {
		case ledger.SlotMerged:
			return "merged"
		case ledger.SlotDone:
			return "done"
		case ledger.SlotPROpened:
			return "pr-opened"
		case ledger.SlotBlocked:
			return "blocked"
		case ledger.SlotConflict:
			return "conflict"
		case ledger.SlotFailed:
			return "failed"
		default:
			return "failed"
		}
	}
	switch value {
	case "candidate-ready", "completion-contract-missing", "code-defect",
		"semantic-defect", "persistent-semantic-defect", "security-defect",
		"runtime-transient", "budget-exhausted",
		"turn-exhausted", "merge-base-moved", "commit-style-mechanical",
		"operator-stop-drain", "engine-invariant", "model-capability":
		return value
	case "capability-unavailable":
		return "model-capability"
	default:
		return ""
	}
}

func knownAttemptOutcome(value string, terminal bool) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if terminal {
		switch value {
		case ledger.SlotMerged, ledger.SlotDone, ledger.SlotPROpened,
			ledger.SlotBlocked, ledger.SlotConflict, ledger.SlotFailed:
			return true
		default:
			return false
		}
	}
	switch value {
	case "candidate-ready", "completion-contract-missing", "code-defect",
		"semantic-defect", "persistent-semantic-defect", "security-defect",
		"capability-unavailable", "runtime-transient", "budget-exhausted",
		"turn-exhausted", "merge-base-moved", "commit-style-mechanical",
		"operator-stop-drain", "engine-invariant", "model-capability":
		return true
	default:
		return false
	}
}

func knownPriorFailureClass(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value != "" && value != "candidate-ready" && knownAttemptOutcome(value, false)
}

func authenticatedTerminalManifest(
	store *ledger.Store,
	runID string,
	sl ledger.Slot,
) (metrics.TerminalEvidenceRef, bool) {
	phaseDir := filepath.Join(store.KoryphRoot, runID, sl.PhaseID)
	read, err := fsx.ReadRegularConfined(phasecontrol.ResultPath(phaseDir), 8<<20, phaseDir)
	if err != nil {
		return metrics.TerminalEvidenceRef{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(read.Data))
	decoder.DisallowUnknownFields()
	var result phasecontrol.ResultManifest
	if err := decoder.Decode(&result); err != nil {
		return metrics.TerminalEvidenceRef{}, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return metrics.TerminalEvidenceRef{}, false
	}
	if result.SchemaVersion != phasecontrol.ResultSchemaVersion ||
		result.State != "done" || result.RunID != runID ||
		result.PhaseID != sl.PhaseID || result.Attempt != sl.Attempts ||
		result.Generation != sl.DispatchGeneration ||
		result.Generation != sl.CandidateGeneration ||
		result.BaseSHA != sl.DispatchBaseSHA || result.CandidateSHA != sl.LastCommit ||
		result.CommitCount <= 0 || !result.WorktreeClean {
		return metrics.TerminalEvidenceRef{}, false
	}
	if _, err := parseEvidenceTime(result.CompletedAt); err != nil {
		return metrics.TerminalEvidenceRef{}, false
	}
	roots := []string{phaseDir}
	if sl.Worktree != "" {
		roots = append(roots, sl.Worktree)
	}
	summary, err := fsx.ReadRegularConfined(result.SummaryPath, 8<<20, roots...)
	if err != nil || summary.Digest != result.SummaryDigest {
		return metrics.TerminalEvidenceRef{}, false
	}
	evidenceRead, err := fsx.ReadRegularConfined(result.EvidencePath, 8<<20, roots...)
	if err != nil || evidenceRead.Digest != result.EvidenceDigest {
		return metrics.TerminalEvidenceRef{}, false
	}
	evidenceDecoder := json.NewDecoder(bytes.NewReader(evidenceRead.Data))
	evidenceDecoder.DisallowUnknownFields()
	var evidence phasecontrol.Evidence
	if err := evidenceDecoder.Decode(&evidence); err != nil {
		return metrics.TerminalEvidenceRef{}, false
	}
	if err := evidenceDecoder.Decode(&trailing); !errors.Is(err, io.EOF) ||
		len(evidence.Acceptance) == 0 {
		return metrics.TerminalEvidenceRef{}, false
	}
	left, _ := json.Marshal(evidence)
	right, _ := json.Marshal(result.Evidence)
	if !bytes.Equal(left, right) {
		return metrics.TerminalEvidenceRef{}, false
	}
	return metrics.TerminalEvidenceRef{
		ArtifactDigest: "sha256:" + read.Digest,
		Generation:     result.Generation,
		CandidateSHA:   result.CandidateSHA,
		BaseSHA:        result.BaseSHA,
		SummaryDigest:  "sha256:" + summary.Digest,
		EvidenceDigest: "sha256:" + evidenceRead.Digest,
		CompletedAt:    result.CompletedAt,
	}, true
}

func authenticatedGateEvidence(
	store *ledger.Store,
	runID string,
	sl ledger.Slot,
	canary CanaryState,
) (metrics.GateEvidenceRef, bool) {
	if sl.GateEvidencePath == "" || sl.GateEvidenceDigest == "" {
		return metrics.GateEvidenceRef{}, false
	}
	read, err := fsx.ReadRegularConfined(
		sl.GateEvidencePath, 64<<10, store.EvidenceDir(runID, sl.PhaseID),
	)
	if err != nil || "sha256:"+read.Digest != sl.GateEvidenceDigest {
		return metrics.GateEvidenceRef{}, false
	}
	decoder := json.NewDecoder(strings.NewReader(string(read.Data)))
	decoder.DisallowUnknownFields()
	var evidence merge.GateEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return metrics.GateEvidenceRef{}, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return metrics.GateEvidenceRef{}, false
	}
	if merge.ValidateGateEvidence(&evidence) != nil ||
		evidence.EngineVersion != canary.BinaryVersion ||
		evidence.BuildIdentity != canary.BuildIdentity {
		return metrics.GateEvidenceRef{}, false
	}
	if sl.GeneralReviewCandidateSHA != "" &&
		evidence.CandidateSHA != sl.GeneralReviewCandidateSHA {
		return metrics.GateEvidenceRef{}, false
	}
	if sl.GeneralReviewBaseSHA != "" && evidence.BaseSHA != sl.GeneralReviewBaseSHA {
		return metrics.GateEvidenceRef{}, false
	}
	return metrics.GateEvidenceRef{
		ArtifactDigest:   sl.GateEvidenceDigest,
		CandidateSHA:     evidence.CandidateSHA,
		BaseSHA:          evidence.BaseSHA,
		DiffDigest:       evidence.DiffDigest,
		GateConfigDigest: evidence.GateConfigDigest,
		CommandDigest:    evidence.CommandDigest,
		EngineVersion:    evidence.EngineVersion,
		BuildIdentity:    evidence.BuildIdentity,
		CompletedAt:      evidence.CompletedAt.UTC().Format(time.RFC3339Nano),
	}, true
}

func authenticatedReviewEvidence(
	store *ledger.Store,
	runID string,
	sl ledger.Slot,
	completedAt string,
) (metrics.ReviewEvidence, metrics.ReviewEvidenceRef, bool, bool) {
	if sl.GeneralReviewArtifactPath == "" || sl.GeneralReviewArtifactDigest == "" {
		return metrics.ReviewEvidence{}, metrics.ReviewEvidenceRef{}, false, false
	}
	read, err := fsx.ReadRegularConfined(
		sl.GeneralReviewArtifactPath, 1<<20, store.EvidenceDir(runID, sl.PhaseID),
	)
	if err != nil || "sha256:"+read.Digest != sl.GeneralReviewArtifactDigest {
		return metrics.ReviewEvidence{}, metrics.ReviewEvidenceRef{}, false, false
	}
	artifact, err := review.ParseArtifact(read.Data, review.ReviewKindGeneral)
	if err != nil {
		return metrics.ReviewEvidence{}, metrics.ReviewEvidenceRef{}, false, false
	}
	if artifact.CandidateSHA != sl.GeneralReviewCandidateSHA ||
		artifact.BaseSHA != sl.GeneralReviewBaseSHA {
		return metrics.ReviewEvidence{}, metrics.ReviewEvidenceRef{}, false, false
	}
	completed, err := parseEvidenceTime(completedAt)
	if err != nil {
		return metrics.ReviewEvidence{}, metrics.ReviewEvidenceRef{}, false, false
	}
	attempts := artifact.Verdict.Attempts
	if attempts <= 0 {
		attempts = 1
	}
	return metrics.ReviewEvidence{
			Reached: true, Attempts: attempts,
			FirstPass: sl.ReviewIters <= 1 && !artifact.Verdict.Blocking && !artifact.Verdict.Degraded,
		}, metrics.ReviewEvidenceRef{
			Kind: review.ReviewKindGeneral, ArtifactDigest: sl.GeneralReviewArtifactDigest,
			HistoryDigest: artifact.HistoryDigest,
			CandidateSHA:  artifact.CandidateSHA, BaseSHA: artifact.BaseSHA,
			CompletedAt:      completed.UTC().Format(time.RFC3339Nano),
			SecurityRequired: artifact.Verdict.SecurityReviewRequired,
		}, artifact.Verdict.SecurityReviewRequired, true
}

func authenticatedSecurityReviewEvidence(
	store *ledger.Store,
	runID string,
	sl ledger.Slot,
	completedAt string,
) (metrics.ReviewEvidenceRef, bool) {
	if sl.SecurityReviewArtifactPath == "" || sl.SecurityReviewArtifactDigest == "" {
		return metrics.ReviewEvidenceRef{}, false
	}
	read, err := fsx.ReadRegularConfined(
		sl.SecurityReviewArtifactPath, 1<<20, store.EvidenceDir(runID, sl.PhaseID),
	)
	if err != nil || "sha256:"+read.Digest != sl.SecurityReviewArtifactDigest {
		return metrics.ReviewEvidenceRef{}, false
	}
	artifact, err := review.ParseArtifact(read.Data, review.ReviewKindSecurity)
	if err != nil || artifact.CandidateSHA != sl.SecurityReviewCandidateSHA ||
		artifact.BaseSHA != sl.SecurityReviewBaseSHA {
		return metrics.ReviewEvidenceRef{}, false
	}
	completed, err := parseEvidenceTime(completedAt)
	if err != nil {
		return metrics.ReviewEvidenceRef{}, false
	}
	return metrics.ReviewEvidenceRef{
		Kind: review.ReviewKindSecurity, ArtifactDigest: sl.SecurityReviewArtifactDigest,
		HistoryDigest: artifact.HistoryDigest,
		CandidateSHA:  artifact.CandidateSHA, BaseSHA: artifact.BaseSHA,
		CompletedAt: completed.UTC().Format(time.RFC3339Nano),
	}, true
}

func attemptProcessEvidence(
	store *ledger.Store,
	runID string,
	sl ledger.Slot,
	nextDispatch string,
) ([]metrics.ProcessEvidence, bool) {
	phaseDir := filepath.Join(store.KoryphRoot, runID, sl.PhaseID)
	path := filepath.Join(phaseDir, ".koryph-command", "events.jsonl")
	read, err := fsx.ReadRegularConfined(path, 8<<20, phaseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	start, startErr := parseEvidenceTime(sl.DispatchedAt)
	end, endErr := parseEvidenceTime(nextDispatch)
	if startErr != nil {
		return nil, false
	}
	if nextDispatch == "" {
		endErr = errors.New("open interval")
	}
	var out []metrics.ProcessEvidence
	scanner := bufio.NewScanner(bytes.NewReader(read.Data))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var event resmon.CommandEvent
		if err := strictjson.Decode(scanner.Bytes(), &event); err != nil || event.At.IsZero() ||
			event.Schema != "koryph.command-event/v1" {
			return nil, false
		}
		if event.At.Before(start) || (endErr == nil && !event.At.Before(end)) {
			continue
		}
		out = append(out, metrics.ProcessEvidence{
			Event: event.Event, Class: string(event.Class),
			Signature: event.Signature, Generation: event.Generation,
			At:        event.At.UTC().Format(time.RFC3339Nano),
			PeakRSSKB: event.PeakRSSKB, CPUSeconds: event.CPUSeconds,
			DurationMS: event.DurationMS,
			Duplicate:  event.Event == "reuse" || event.Event == "denial",
		})
	}
	return out, scanner.Err() == nil
}

func stageTiming(e ledger.StageTimingEvidence) metrics.StageTiming {
	queued, qErr := parseEvidenceTime(e.QueuedAt)
	started, sErr := parseEvidenceTime(e.StartedAt)
	completed, cErr := parseEvidenceTime(e.CompletedAt)
	if qErr != nil || sErr != nil || cErr != nil ||
		started.Before(queued) || completed.Before(started) {
		return metrics.StageTiming{}
	}
	return metrics.StageTiming{
		Reached:   true,
		QueueMS:   started.Sub(queued).Milliseconds(),
		ServiceMS: completed.Sub(started).Milliseconds(),
	}
}

func addStageTimings(a, b metrics.StageTiming) metrics.StageTiming {
	return metrics.StageTiming{
		Reached:   a.Reached || b.Reached,
		QueueMS:   a.QueueMS + b.QueueMS,
		ServiceMS: a.ServiceMS + b.ServiceMS,
	}
}

func parseEvidenceTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, errors.New("empty time")
	}
	return time.Parse(time.RFC3339Nano, value)
}

func nativeAttemptDigest(attempt nativeAttempt) string {
	raw, _ := json.Marshal(struct {
		BeadID                      string `json:"bead_id"`
		OutcomeClass                string `json:"outcome_class"`
		GateEvidenceDigest          string `json:"gate_evidence_digest"`
		GeneralReviewArtifactDigest string `json:"general_review_artifact_digest"`
		SecurityArtifactDigest      string `json:"security_review_artifact_digest"`
		GeneralReviewCandidateSHA   string `json:"general_review_candidate_sha"`
		GeneralReviewBaseSHA        string `json:"general_review_base_sha"`
		SecurityReviewCandidateSHA  string `json:"security_review_candidate_sha"`
		SecurityReviewBaseSHA       string `json:"security_review_base_sha"`
	}{
		BeadID: attemptBeadID(attempt.slot), OutcomeClass: attempt.slot.OutcomeClass,
		GateEvidenceDigest:          attempt.slot.GateEvidenceDigest,
		GeneralReviewArtifactDigest: attempt.slot.GeneralReviewArtifactDigest,
		SecurityArtifactDigest:      attempt.slot.SecurityReviewArtifactDigest,
		GeneralReviewCandidateSHA:   attempt.slot.GeneralReviewCandidateSHA,
		GeneralReviewBaseSHA:        attempt.slot.GeneralReviewBaseSHA,
		SecurityReviewCandidateSHA:  attempt.slot.SecurityReviewCandidateSHA,
		SecurityReviewBaseSHA:       attempt.slot.SecurityReviewBaseSHA,
	})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hasBroadCommandEvidence(events []metrics.ProcessEvidence) bool {
	lifecycles := make(map[string]uint8)
	for _, event := range events {
		if event.Class != "broad" && event.Class != "full-gate" {
			continue
		}
		key := event.Signature + "\x00" + event.Generation
		switch event.Event {
		case "start":
			lifecycles[key] |= 1
		case "complete":
			lifecycles[key] |= 2
		}
	}
	for _, lifecycle := range lifecycles {
		if lifecycle == 3 {
			return true
		}
	}
	return false
}

func nativeArtifactEvidence(
	root, exclude string,
	inventoryAt time.Time,
) (metrics.ArtifactEvidence, bool, error) {
	var total int64
	var retainedFailure int64
	var compactEvidence int64
	valid := true
	maxInt64 := int64(^uint64(0) >> 1)
	retainedRoots, failureRetention := nativeRetainedFailureRoots(root)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if filepath.Clean(path) == filepath.Clean(exclude) {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type() != 0 {
			valid = false
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			valid = false
			return nil
		}
		size := info.Size()
		if size > maxInt64-total {
			total = maxInt64
			valid = false
		} else {
			total += size
		}
		compact := nativeCompactArtifact(root, path)
		if compact {
			if size > maxInt64-compactEvidence {
				compactEvidence = maxInt64
				valid = false
			} else {
				compactEvidence += size
			}
		} else if nativeRetainedFailureArtifact(
			path, info, retainedRoots, failureRetention, inventoryAt,
		) {
			if size > maxInt64-retainedFailure {
				retainedFailure = maxInt64
				valid = false
			} else {
				retainedFailure += size
			}
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return metrics.ArtifactEvidence{}, true, nil
	}
	return metrics.ArtifactEvidence{
		ActiveBytes: total, CompactEvidenceBytes: compactEvidence,
		RetainedFailureBytes: retainedFailure,
	}, valid, err
}

func nativeRetainedFailureRoots(root string) (map[string]bool, time.Duration) {
	repoRoot := filepath.Dir(filepath.Dir(root))
	store := ledger.NewStore(repoRoot)
	if filepath.Clean(store.KoryphRoot) != filepath.Clean(root) {
		return nil, 30 * 24 * time.Hour
	}
	retention := 30 * 24 * time.Hour
	if cfg, err := koryphgc.LoadConfig(repoRoot); err == nil &&
		cfg.ProjectBudget.FailureRetainDays > 0 {
		retention = time.Duration(cfg.ProjectBudget.FailureRetainDays) * 24 * time.Hour
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, retention
	}
	roots := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		run, err := store.LoadRun(entry.Name())
		if err != nil || run == nil || run.RunID != entry.Name() {
			continue
		}
		for phaseID, slot := range run.Slots {
			if slot == nil || !ledger.Terminal(slot.Status) ||
				slot.PhaseID != phaseID || phaseID == "" || phaseID == "." ||
				filepath.Base(phaseID) != phaseID ||
				slot.Status == ledger.SlotMerged || slot.Status == ledger.SlotPROpened ||
				slot.Status == ledger.SlotDone {
				continue
			}
			for _, candidate := range []string{
				store.PhaseDir(run.RunID, slot.PhaseID),
				filepath.Join(store.KoryphRoot, run.RunID, ".engine-evidence", slot.PhaseID),
				filepath.Join(store.KoryphRoot, run.RunID, ".evidence", slot.PhaseID),
				filepath.Join(store.KoryphRoot, run.RunID, "evidence", slot.PhaseID),
			} {
				if pathWithin(candidate, root) {
					roots[filepath.Clean(candidate)] = true
				}
			}
		}
	}
	return roots, retention
}

func nativeRetainedFailureArtifact(
	path string,
	info os.FileInfo,
	roots map[string]bool,
	retention time.Duration,
	inventoryAt time.Time,
) bool {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if roots[current] {
			return retention > 0 && inventoryAt.Sub(info.ModTime()) < retention
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func nativeCompactArtifact(root, path string) bool {
	if strings.HasSuffix(strings.ToLower(filepath.Base(path)), ".evidence.tar.gz") {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return nativeCompactEvidencePath(rel)
}

func nativeCompactEvidencePath(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || strings.HasPrefix(rel, "../") ||
		strings.Contains("/"+rel+"/", "/.runtime-scratch/") {
		return false
	}
	base := filepath.Base(rel)
	switch base {
	case "ledger.json", "manifest.json", "result.json", "SUMMARY.md", "runtime-final.md",
		"review.json", "review-envelope.json", "review-degraded.json",
		"events.jsonl", "pressure-state.json", "supervisor.json", "alerts.jsonl":
		return true
	}
	return strings.HasSuffix(base, ".tail") ||
		strings.HasPrefix(base, "gate-evidence-") ||
		strings.HasPrefix(base, "general-review-") ||
		strings.HasPrefix(base, "security-review-") ||
		strings.Contains("/"+rel, "/.evidence/") ||
		strings.Contains("/"+rel, "/evidence/") ||
		strings.Contains(rel, "/.engine-evidence/")
}

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func failSafety(safety safetySet, name, detail string) {
	current := safety[name]
	current.Name = name
	current.Passed = false
	if current.Detail == "" {
		current.Detail = detail
	} else if !strings.Contains(current.Detail, detail) {
		current.Detail += "; " + detail
	}
	safety[name] = current
}

func applyHardStopSafety(safety safetySet, reason string) {
	kind := strings.TrimSpace(reason)
	if before, _, ok := strings.Cut(kind, ":"); ok {
		kind = strings.TrimSpace(before)
	}
	if kind == "" {
		return
	}
	failSafety(safety, "candidate-contract", "canary hard stop: "+kind)
	switch kind {
	case "unchanged-retry":
		failSafety(safety, "capability-no-redispatch", "canary hard stop: "+kind)
	case "duplicate-broad-command":
		failSafety(safety, "single-broad-command", "canary hard stop: "+kind)
	case "host-memory-pressure":
		failSafety(safety, "host-pressure-containment", "canary hard stop: "+kind)
	case "cohort-admission-violation":
		failSafety(safety, "resume-without-redispatch", "canary hard stop: "+kind)
	}
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
