// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

// CandidateOutcomeClass is the closed, typed vocabulary consumed by recovery
// policy. Free-form notes remain observability only and never select a retry.
type CandidateOutcomeClass string

const (
	OutcomeCandidateReady            CandidateOutcomeClass = "candidate-ready"
	OutcomeCompletionContractMissing CandidateOutcomeClass = "completion-contract-missing"
	OutcomeCodeDefect                CandidateOutcomeClass = "code-defect"
	OutcomeSemanticDefect            CandidateOutcomeClass = "semantic-defect"
	OutcomePersistentSemanticDefect  CandidateOutcomeClass = "persistent-semantic-defect"
	OutcomeSecurityDefect            CandidateOutcomeClass = "security-defect"
	OutcomeCapabilityUnavailable     CandidateOutcomeClass = "capability-unavailable"
	OutcomeRuntimeTransient          CandidateOutcomeClass = "runtime-transient"
	OutcomeBudgetExhausted           CandidateOutcomeClass = "budget-exhausted"
	OutcomeTurnExhausted             CandidateOutcomeClass = "turn-exhausted"
	OutcomeMergeBaseMoved            CandidateOutcomeClass = "merge-base-moved"
	OutcomeMechanical                CandidateOutcomeClass = "commit-style-mechanical"
	OutcomeOperatorStop              CandidateOutcomeClass = "operator-stop-drain"
	OutcomeEngineInvariant           CandidateOutcomeClass = "engine-invariant"
	// OutcomeModelCapability is emitted only by structured frontier recovery
	// analysis after every non-model cause has been excluded.
	OutcomeModelCapability CandidateOutcomeClass = "model-capability"
)

type RecoveryAction string

const (
	RecoveryEnterValidation  RecoveryAction = "enter-validation"
	RecoveryTargetedRepair   RecoveryAction = "targeted-repair"
	RecoveryRetrySameTier    RecoveryAction = "retry-same-tier"
	RecoveryFrontierAnalysis RecoveryAction = "frontier-recovery-analysis"
	RecoveryRebaseRevalidate RecoveryAction = "rebase-revalidate"
	RecoveryDeterministicFix RecoveryAction = "deterministic-repair"
	RecoveryEvidenceHold     RecoveryAction = "evidence-hold"
	RecoveryPark             RecoveryAction = "park"
	RecoveryOpenCircuit      RecoveryAction = "open-circuit"
)

type ModelConsequence string

const (
	ModelConsequenceNone                   ModelConsequence = "none"
	ModelConsequenceStandardImplementation ModelConsequence = "standard-implementation"
	ModelConsequenceFrontierAnalysis       ModelConsequence = "frontier-analysis"
	ModelConsequenceFrontierImplementation ModelConsequence = "frontier-implementation"
)

// RecoveryDecision is a pure policy result. Reason is a stable token, never
// parsed prose.
type RecoveryDecision struct {
	Action RecoveryAction
	Model  ModelConsequence
	Reason string
}
