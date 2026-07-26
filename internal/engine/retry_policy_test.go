// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import "testing"

func TestDecideRetryTypedTransitionTable(t *testing.T) {
	tests := []struct {
		name string
		in   RetryPolicyInput
		want RecoveryDecision
	}{
		{"ready", RetryPolicyInput{Outcome: OutcomeCandidateReady}, decision(RecoveryEnterValidation, ModelConsequenceNone, "candidate-ready")},
		{"completion repair", RetryPolicyInput{Outcome: OutcomeCompletionContractMissing}, decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "completion-repair")},
		{"completion exhausted", RetryPolicyInput{Outcome: OutcomeCompletionContractMissing, Budgets: RetryBudgets{CompletionRepairs: 1}}, decision(RecoveryPark, ModelConsequenceNone, "completion-repair-exhausted")},
		{"code repair changed", RetryPolicyInput{Outcome: OutcomeCodeDefect, EvidenceChanged: true}, decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "code-repair")},
		{"code unchanged", RetryPolicyInput{Outcome: OutcomeCodeDefect}, decision(RecoveryPark, ModelConsequenceNone, "code-repair-unchanged")},
		{"code exhausted", RetryPolicyInput{Outcome: OutcomeCodeDefect, EvidenceChanged: true, Budgets: RetryBudgets{CodeRepairs: 1}}, decision(RecoveryPark, ModelConsequenceNone, "code-repair-exhausted")},
		{"semantic repair", RetryPolicyInput{Outcome: OutcomeSemanticDefect, EvidenceChanged: true}, decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "semantic-repair")},
		{"semantic analysis", RetryPolicyInput{Outcome: OutcomeSemanticDefect, Budgets: RetryBudgets{SemanticRepairs: 1}}, decision(RecoveryFrontierAnalysis, ModelConsequenceFrontierAnalysis, "persistent-semantic-analysis")},
		{"persistent analysis", RetryPolicyInput{Outcome: OutcomePersistentSemanticDefect}, decision(RecoveryFrontierAnalysis, ModelConsequenceFrontierAnalysis, "persistent-semantic-analysis")},
		{"post analysis standard", RetryPolicyInput{Outcome: OutcomePersistentSemanticDefect, EvidenceChanged: true, Budgets: RetryBudgets{RecoveryAnalyses: 1}}, decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "post-analysis-repair")},
		{"security repair", RetryPolicyInput{Outcome: OutcomeSecurityDefect, EvidenceChanged: true}, decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "security-repair")},
		{"security unchanged", RetryPolicyInput{Outcome: OutcomeSecurityDefect}, decision(RecoveryPark, ModelConsequenceNone, "security-repair-unchanged")},
		{"security exhausted", RetryPolicyInput{Outcome: OutcomeSecurityDefect, EvidenceChanged: true, Budgets: RetryBudgets{SecurityRepairs: 1}}, decision(RecoveryPark, ModelConsequenceNone, "security-repair-exhausted")},
		{"capability hold", RetryPolicyInput{Outcome: OutcomeCapabilityUnavailable}, decision(RecoveryEvidenceHold, ModelConsequenceNone, "capability-evidence-hold")},
		{"transient retry", RetryPolicyInput{Outcome: OutcomeRuntimeTransient}, decision(RecoveryRetrySameTier, ModelConsequenceNone, "runtime-transient-retry")},
		{"transient exhausted", RetryPolicyInput{Outcome: OutcomeRuntimeTransient, Budgets: RetryBudgets{TransientRetries: 2}}, decision(RecoveryPark, ModelConsequenceNone, "runtime-transient-exhausted")},
		{"budget warm continuation", RetryPolicyInput{Outcome: OutcomeBudgetExhausted}, decision(RecoveryRetrySameTier, ModelConsequenceNone, "budget-warm-continuation")},
		{"budget exhausted", RetryPolicyInput{Outcome: OutcomeBudgetExhausted, Budgets: RetryBudgets{BudgetContinuations: 1}}, decision(RecoveryPark, ModelConsequenceNone, "budget-continuation-exhausted")},
		{"budget thrashing", RetryPolicyInput{Outcome: OutcomeBudgetExhausted, AttemptThrashing: true}, decision(RecoveryPark, ModelConsequenceNone, "budget-thrash-detected")},
		{"turn fresh continuation", RetryPolicyInput{Outcome: OutcomeTurnExhausted}, decision(RecoveryRetrySameTier, ModelConsequenceNone, "turn-fresh-continuation")},
		{"turn second continuation", RetryPolicyInput{Outcome: OutcomeTurnExhausted, Budgets: RetryBudgets{TurnContinuations: 1}}, decision(RecoveryRetrySameTier, ModelConsequenceNone, "turn-fresh-continuation")},
		{"turn exhausted", RetryPolicyInput{Outcome: OutcomeTurnExhausted, Budgets: RetryBudgets{TurnContinuations: 2}}, decision(RecoveryPark, ModelConsequenceNone, "turn-continuation-exhausted")},
		{"base moved", RetryPolicyInput{Outcome: OutcomeMergeBaseMoved}, decision(RecoveryRebaseRevalidate, ModelConsequenceNone, "merge-base-moved")},
		{"base move exhausted", RetryPolicyInput{Outcome: OutcomeMergeBaseMoved, Budgets: RetryBudgets{MergeRevalidations: 1}}, decision(RecoveryPark, ModelConsequenceNone, "merge-revalidation-exhausted")},
		{"mechanical deterministic", RetryPolicyInput{Outcome: OutcomeMechanical, DeterministicRepairAvailable: true}, decision(RecoveryDeterministicFix, ModelConsequenceNone, "mechanical-deterministic-repair")},
		{"mechanical targeted", RetryPolicyInput{Outcome: OutcomeMechanical}, decision(RecoveryTargetedRepair, ModelConsequenceStandardImplementation, "mechanical-targeted-repair")},
		{"mechanical exhausted", RetryPolicyInput{Outcome: OutcomeMechanical, Budgets: RetryBudgets{MechanicalRepairs: 1}}, decision(RecoveryPark, ModelConsequenceNone, "mechanical-repair-exhausted")},
		{"stop", RetryPolicyInput{Outcome: OutcomeOperatorStop}, decision(RecoveryPark, ModelConsequenceNone, "operator-stop")},
		{"invariant", RetryPolicyInput{Outcome: OutcomeEngineInvariant}, decision(RecoveryOpenCircuit, ModelConsequenceNone, "engine-invariant")},
		{"unknown", RetryPolicyInput{Outcome: CandidateOutcomeClass("future")}, decision(RecoveryOpenCircuit, ModelConsequenceNone, "unknown-outcome")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideRetry(tc.in); got != tc.want {
				t.Fatalf("DecideRetry(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestUnchangedTripwireClassificationIsExact(t *testing.T) {
	for _, reason := range []string{"code-repair-unchanged", "security-repair-unchanged"} {
		if !retryDecisionEvidenceUnchanged(RecoveryDecision{Reason: reason}) {
			t.Errorf("%q was not classified as unchanged", reason)
		}
	}
	for _, reason := range []string{
		"code-repair-exhausted",
		"security-repair-exhausted",
		"something-unchanged-but-not-a-policy-verdict",
	} {
		if retryDecisionEvidenceUnchanged(RecoveryDecision{Reason: reason}) {
			t.Errorf("%q was incorrectly classified as unchanged", reason)
		}
	}
}

func TestFrontierImplementationRequiresBothDiagnosisAndOptIn(t *testing.T) {
	for _, tc := range []RetryPolicyInput{
		{Outcome: OutcomeModelCapability},
		{Outcome: OutcomeModelCapability, ModelCapabilityDiagnosed: true},
		{Outcome: OutcomeModelCapability, FrontierImplementationEnabled: true},
	} {
		if got := DecideRetry(tc); got.Model == ModelConsequenceFrontierImplementation {
			t.Fatalf("frontier implementation selected without diagnosis+opt-in: %+v", tc)
		}
	}
	got := DecideRetry(RetryPolicyInput{
		Outcome: OutcomeModelCapability, ModelCapabilityDiagnosed: true, FrontierImplementationEnabled: true,
	})
	if got.Model != ModelConsequenceFrontierImplementation || got.Action != RecoveryTargetedRepair {
		t.Fatalf("diagnosed opt-in = %+v", got)
	}
}

func TestNonCapabilityFailuresNeverSelectFrontierImplementation(t *testing.T) {
	for _, outcome := range []CandidateOutcomeClass{
		OutcomeCandidateReady, OutcomeCompletionContractMissing, OutcomeCodeDefect,
		OutcomeSemanticDefect, OutcomePersistentSemanticDefect, OutcomeSecurityDefect,
		OutcomeCapabilityUnavailable, OutcomeRuntimeTransient, OutcomeBudgetExhausted,
		OutcomeTurnExhausted, OutcomeMergeBaseMoved,
		OutcomeMechanical, OutcomeOperatorStop, OutcomeEngineInvariant,
	} {
		got := DecideRetry(RetryPolicyInput{
			Outcome: outcome, EvidenceChanged: true, DeterministicRepairAvailable: true,
			ModelCapabilityDiagnosed: true, FrontierImplementationEnabled: true,
		})
		if got.Model == ModelConsequenceFrontierImplementation {
			t.Fatalf("%s selected frontier implementation: %+v", outcome, got)
		}
	}
}
