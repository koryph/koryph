// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import "github.com/koryph/koryph/internal/ledger"

const (
	runtimeTransientRetryBudget = 2
	budgetContinuationBudget    = 1
	turnContinuationBudget      = 2
)

// RetryBudgets are facts observed for the current typed lifecycle. They do not
// independently decide policy.
type RetryBudgets struct {
	CompletionRepairs   int
	CodeRepairs         int
	SemanticRepairs     int
	RecoveryAnalyses    int
	PostAnalysisRepairs int
	SecurityRepairs     int
	MechanicalRepairs   int
	TransientRetries    int
	MergeRevalidations  int
	BudgetContinuations int
	TurnContinuations   int
}

type RetryPolicyInput struct {
	Outcome CandidateOutcomeClass
	Budgets RetryBudgets

	// EvidenceChanged means a new gate/review/capability fingerprint is
	// present. An unchanged failure cannot consume another dispatch.
	EvidenceChanged bool
	// DeterministicRepairAvailable selects an engine-owned mechanical repair.
	DeterministicRepairAvailable bool
	// ModelCapabilityDiagnosed can only come from structured frontier recovery
	// analysis. Frontier implementation is additionally disabled by default.
	ModelCapabilityDiagnosed      bool
	FrontierImplementationEnabled bool
	// AttemptThrashing is a typed, measured budget-exhaustion fact: the
	// attempt consumed the configured high-token floor without a commit.
	// It suppresses even the otherwise-allowed warm continuation.
	AttemptThrashing bool
}

// DecideRetry is the one pure typed transition table. It has no runner,
// ledger, filesystem, model router, or note-string dependencies.
func DecideRetry(in RetryPolicyInput) RecoveryDecision {
	switch in.Outcome {
	case OutcomeCandidateReady:
		return decision(RecoveryEnterValidation, ModelConsequenceNone, "candidate-ready")

	case OutcomeCompletionContractMissing:
		if in.Budgets.CompletionRepairs < 1 {
			return decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "completion-repair")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "completion-repair-exhausted")

	case OutcomeCodeDefect:
		if !in.EvidenceChanged {
			return decision(RecoveryPark, ModelConsequenceNone, "code-repair-unchanged")
		}
		if in.Budgets.CodeRepairs < 1 {
			return decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "code-repair")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "code-repair-exhausted")

	case OutcomeSemanticDefect:
		if in.EvidenceChanged && in.Budgets.SemanticRepairs < 1 {
			return decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "semantic-repair")
		}
		return decision(RecoveryFrontierAnalysis, ModelConsequenceFrontierAnalysis, "persistent-semantic-analysis")

	case OutcomePersistentSemanticDefect:
		if in.Budgets.RecoveryAnalyses < 1 {
			return decision(RecoveryFrontierAnalysis, ModelConsequenceFrontierAnalysis, "persistent-semantic-analysis")
		}
		if in.EvidenceChanged && in.Budgets.PostAnalysisRepairs < 1 {
			return decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "post-analysis-repair")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "persistent-semantic-exhausted")

	case OutcomeSecurityDefect:
		if !in.EvidenceChanged {
			return decision(RecoveryPark, ModelConsequenceNone, "security-repair-unchanged")
		}
		if in.Budgets.SecurityRepairs < 1 {
			return decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "security-repair")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "security-repair-exhausted")

	case OutcomeCapabilityUnavailable:
		return decision(RecoveryEvidenceHold, ModelConsequenceNone, "capability-evidence-hold")

	case OutcomeRuntimeTransient:
		if in.Budgets.TransientRetries < runtimeTransientRetryBudget {
			return decision(RecoveryRetrySameTier, ModelConsequenceNone, "runtime-transient-retry")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "runtime-transient-exhausted")

	case OutcomeBudgetExhausted:
		if in.AttemptThrashing {
			return decision(RecoveryPark, ModelConsequenceNone, "budget-thrash-detected")
		}
		if in.Budgets.BudgetContinuations < budgetContinuationBudget {
			return decision(RecoveryRetrySameTier, ModelConsequenceNone, "budget-warm-continuation")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "budget-continuation-exhausted")

	case OutcomeTurnExhausted:
		if in.Budgets.TurnContinuations < turnContinuationBudget {
			return decision(RecoveryRetrySameTier, ModelConsequenceNone, "turn-fresh-continuation")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "turn-continuation-exhausted")

	case OutcomeMergeBaseMoved:
		if in.Budgets.MergeRevalidations < 1 {
			return decision(RecoveryRebaseRevalidate, ModelConsequenceNone, "merge-base-moved")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "merge-revalidation-exhausted")

	case OutcomeMechanical:
		if in.DeterministicRepairAvailable {
			return decision(RecoveryDeterministicFix, ModelConsequenceNone, "mechanical-deterministic-repair")
		}
		if in.Budgets.MechanicalRepairs < 1 {
			return decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "mechanical-targeted-repair")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "mechanical-repair-exhausted")

	case OutcomeOperatorStop:
		return decision(RecoveryPark, ModelConsequenceNone, "operator-stop")

	case OutcomeEngineInvariant:
		return decision(RecoveryOpenCircuit, ModelConsequenceNone, "engine-invariant")

	case OutcomeModelCapability:
		if in.ModelCapabilityDiagnosed && in.FrontierImplementationEnabled {
			return decision(RecoveryTargetedRepair, ModelConsequenceFrontierImplementation, "diagnosed-model-capability")
		}
		return decision(RecoveryPark, ModelConsequenceNone, "frontier-implementation-disabled-or-unproven")

	default:
		return decision(RecoveryOpenCircuit, ModelConsequenceNone, "unknown-outcome")
	}
}

func retryDecisionEvidenceUnchanged(d RecoveryDecision) bool {
	switch d.Reason {
	case "code-repair-unchanged", "security-repair-unchanged":
		return true
	default:
		return false
	}
}

func decision(action RecoveryAction, model ModelConsequence, reason string) RecoveryDecision {
	return RecoveryDecision{Action: action, Model: model, Reason: reason}
}

func retryBudgets(c ledger.RetryCounters) RetryBudgets {
	return RetryBudgets{
		CompletionRepairs: c.CompletionRepairs,
		CodeRepairs:       c.CodeRepairs, SemanticRepairs: c.SemanticRepairs,
		RecoveryAnalyses: c.RecoveryAnalyses, PostAnalysisRepairs: c.PostAnalysisRepairs,
		SecurityRepairs: c.SecurityRepairs, MechanicalRepairs: c.MechanicalRepairs,
		TransientRetries: c.TransientRetries, MergeRevalidations: c.MergeRevalidations,
		BudgetContinuations: c.BudgetContinuations, TurnContinuations: c.TurnContinuations,
	}
}
