// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testAutonomyThresholds() AutonomyThresholds {
	thresholds := DefaultAutonomyThresholds()
	thresholds.MinEligibleBeads = 2
	thresholds.MinFirstReviewPassRate = 0.50
	thresholds.MaxRetryDispatchRate = 0.50
	thresholds.MaxFrontierImplementation = 0.50
	thresholds.MaxArtifactBytes = 1 << 20
	thresholds.HardArtifactBytes = 2 << 20
	return thresholds
}

func testSafetyEvidence() []SafetyEvidence {
	out := make([]SafetyEvidence, 0, len(RequiredSafetyInvariants))
	for _, name := range RequiredSafetyInvariants {
		out = append(out, SafetyEvidence{Name: name, Passed: true})
	}
	return out
}

func testTokens() TokenEvidence {
	return TokenEvidence{
		Semantics: TokenSemanticsDisjointV1,
		Input:     10, Output: 5, CacheRead: 100, CacheCreation: 2, Total: 117,
		ProviderTotalInput: 110,
	}
}

func digest(char string) string {
	return "sha256:" + strings.Repeat(char, 64)
}

func testGateRef(candidate, base, completed string) *GateEvidenceRef {
	return &GateEvidenceRef{
		ArtifactDigest: digest("a"), CandidateSHA: candidate, BaseSHA: base,
		DiffDigest: digest("b"), GateConfigDigest: digest("c"), CommandDigest: digest("d"),
		EngineVersion: "0.10.0", BuildIdentity: "fixture-build", CompletedAt: completed,
	}
}

func testReviewRef(candidate, base, completed string) ReviewEvidenceRef {
	return ReviewEvidenceRef{
		Kind: "general", ArtifactDigest: digest("e"), HistoryDigest: digest("f"),
		CandidateSHA: candidate, BaseSHA: base, CompletedAt: completed,
	}
}

func testTerminalRef(candidate, base, completed string) *TerminalEvidenceRef {
	return &TerminalEvidenceRef{
		ArtifactDigest: digest("1"), Generation: strings.Repeat("a", 64),
		CandidateSHA: candidate, BaseSHA: base,
		SummaryDigest: digest("2"), EvidenceDigest: digest("3"), CompletedAt: completed,
	}
}

func testAutonomyInput() AutonomyInput {
	base := strings.Repeat("1", 40)
	firstCandidate := strings.Repeat("2", 40)
	secondCandidate := strings.Repeat("3", 40)
	otherCandidate := strings.Repeat("4", 40)
	return AutonomyInput{
		SchemaVersion: AutonomyInputSchema,
		ProjectID:     "koryph", InstalledCommit: strings.Repeat("a", 40),
		BinaryVersion: "0.10.0", BuildIdentity: "fixture-build", ContractDigest: digest("b"),
		RegistryIdentityDigest: digest("c"),
		Cohort:                 []string{"b2", "b1"},
		StartedAt:              "2026-07-25T10:00:00Z", EndedAt: "2026-07-25T10:12:00Z",
		Thresholds: testAutonomyThresholds(),
		Evidence: AutonomyEvidence{
			Safety:    testSafetyEvidence(),
			Artifacts: ArtifactEvidence{ActiveBytes: 1000, RetainedFailureBytes: 100, ReclaimableBytes: 300},
			Pressure:  []PressureEvidence{{At: "2026-07-25T10:01:00Z", Level: "normal", AvailableMB: 8192}},
			Attempts: []AutonomyAttempt{
				{
					BeadID: "b1", RunID: "r1", PhaseID: "b1", Attempt: 1,
					ModelTier: "standard", DispatchedAt: "2026-07-25T10:00:00Z",
					Outcome:      TypedOutcome{Kind: "code-defect", Terminal: false},
					Gate:         StageTiming{Reached: true, QueueMS: 1000, ServiceMS: 2000},
					GateEvidence: testGateRef(firstCandidate, base, "2026-07-25T10:02:00Z"),
					Tokens:       testTokens(),
				},
				{
					BeadID: "b1", RunID: "r1", PhaseID: "b1", Attempt: 2,
					ModelTier: "standard", DispatchedAt: "2026-07-25T10:03:00Z",
					TerminalAt:     "2026-07-25T10:12:00Z",
					Outcome:        TypedOutcome{Kind: "merged", Terminal: true, Correct: true},
					Retry:          &RetryEvidence{Kind: "code-defect", PriorEvidenceDigest: digest("1"), EvidenceDigest: digest("2")},
					Review:         ReviewEvidence{Reached: true, Attempts: 2, FirstPass: false},
					Gate:           StageTiming{Reached: true, QueueMS: 2000, ServiceMS: 3000},
					SemanticReview: StageTiming{Reached: true, QueueMS: 3000, ServiceMS: 4000},
					Merge:          StageTiming{Reached: true, QueueMS: 4000, ServiceMS: 5000},
					GateEvidence:   testGateRef(secondCandidate, base, "2026-07-25T10:08:00Z"),
					ReviewEvidenceRefs: []ReviewEvidenceRef{
						testReviewRef(secondCandidate, base, "2026-07-25T10:09:00Z"),
					},
					TerminalEvidence: testTerminalRef(secondCandidate, base, "2026-07-25T10:07:00Z"),
					Tokens:           testTokens(),
				},
				{
					BeadID: "b2", RunID: "r1", PhaseID: "b2", Attempt: 1,
					ModelTier: "frontier", FrontierJustification: "model-capability",
					DispatchedAt: "2026-07-25T10:01:00Z", TerminalAt: "2026-07-25T10:11:00Z",
					Outcome:        TypedOutcome{Kind: "merged", Terminal: true, Correct: true},
					Review:         ReviewEvidence{Reached: true, Attempts: 1, FirstPass: true},
					Gate:           StageTiming{Reached: true, QueueMS: 1000, ServiceMS: 1000},
					SemanticReview: StageTiming{Reached: true, QueueMS: 1000, ServiceMS: 2000},
					Merge:          StageTiming{Reached: true, QueueMS: 1000, ServiceMS: 1000},
					GateEvidence:   testGateRef(otherCandidate, base, "2026-07-25T10:07:00Z"),
					ReviewEvidenceRefs: []ReviewEvidenceRef{
						testReviewRef(otherCandidate, base, "2026-07-25T10:08:00Z"),
					},
					TerminalEvidence: testTerminalRef(otherCandidate, base, "2026-07-25T10:06:00Z"),
					ProcessEvents: []ProcessEvidence{
						{
							Event: "start", Class: "full-gate", Signature: strings.Repeat("a", 24),
							Generation: strings.Repeat("b", 32), At: "2026-07-25T10:08:00Z",
						},
						{
							Event: "complete", Class: "full-gate", Signature: strings.Repeat("a", 24),
							Generation: strings.Repeat("b", 32), At: "2026-07-25T10:09:00Z", DurationMS: 2000,
						},
					},
					Tokens: testTokens(),
				},
			},
		},
	}
}

func TestBuildAutonomyReportComputesExactSLOsAndSeparateStages(t *testing.T) {
	report, err := BuildAutonomyReport(testAutonomyInput(), time.Date(2026, 7, 25, 10, 13, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Decision.Passed {
		t.Fatalf("decision = %+v", report.Decision)
	}
	if got := report.Cohort; len(got) != 2 || got[0] != "b1" || got[1] != "b2" {
		t.Fatalf("normalized cohort = %v", got)
	}
	m := report.Metrics
	if m.AutonomousCompletion != (RatioMetric{Numerator: 2, Denominator: 2, Rate: 1}) {
		t.Errorf("completion = %+v", m.AutonomousCompletion)
	}
	if m.FirstReviewPass.Numerator != 1 || m.FirstReviewPass.Denominator != 2 || m.FirstReviewPass.Rate != 0.5 {
		t.Errorf("first review = %+v", m.FirstReviewPass)
	}
	if m.RetryDispatches.Numerator != 1 || m.RetryDispatches.Denominator != 3 ||
		m.RetryDispatches.Rate != 1.0/3.0 {
		t.Errorf("retries = %+v", m.RetryDispatches)
	}
	if m.FrontierImplementation.Numerator != 1 || m.FrontierImplementation.Denominator != 3 {
		t.Errorf("frontier = %+v", m.FrontierImplementation)
	}
	if m.DispatchToTerminal.Count != 2 || m.DispatchToTerminal.MedianMS != (11*time.Minute).Milliseconds() ||
		m.DispatchToTerminal.P95MS != (12*time.Minute).Milliseconds() {
		t.Errorf("latency = %+v", m.DispatchToTerminal)
	}
	if m.Gate.Queue.Count != 3 || m.Gate.Service.Count != 3 ||
		m.Review.Queue.Count != 2 || m.Review.Service.Count != 2 ||
		m.Merge.Queue.Count != 2 || m.Merge.Service.Count != 2 {
		t.Errorf("stage timing counts gate=%+v review=%+v merge=%+v", m.Gate, m.Review, m.Merge)
	}
	if m.Tokens.Total != 3*117 || m.Tokens.ProviderTotalInput != 3*110 {
		t.Errorf("tokens = %+v", m.Tokens)
	}
	if report.CohortDigest == "" || report.EvidenceDigest == "" {
		t.Fatal("report identity digests are empty")
	}
	if err := ValidateAutonomyReport(report); err != nil {
		t.Fatalf("validate report: %v", err)
	}
}

func TestAutonomyEvidenceReferencesRequireOneExactCandidateTuple(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*AutonomyInput)
		want   string
	}{
		{
			name: "review mismatch",
			mutate: func(input *AutonomyInput) {
				input.Evidence.Attempts[1].ReviewEvidenceRefs[0].CandidateSHA = strings.Repeat("9", 40)
			},
			want: "review-gate-tuple",
		},
		{
			name: "terminal mismatch",
			mutate: func(input *AutonomyInput) {
				input.Evidence.Attempts[1].TerminalEvidence.BaseSHA = strings.Repeat("8", 40)
			},
			want: "terminal-gate-tuple",
		},
		{
			name: "missing gate",
			mutate: func(input *AutonomyInput) {
				input.Evidence.Attempts[1].GateEvidence = nil
			},
			want: "missing-gate",
		},
		{
			name: "foreign binary",
			mutate: func(input *AutonomyInput) {
				input.Evidence.Attempts[1].GateEvidence.BuildIdentity = "foreign-build"
			},
			want: "gate-build-identity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := testAutonomyInput()
			tc.mutate(&input)
			report, err := BuildAutonomyReport(
				input, time.Date(2026, 7, 25, 10, 13, 0, 0, time.UTC),
			)
			if err != nil {
				t.Fatal(err)
			}
			if report.Decision.Passed ||
				!containsSubstring(report.Decision.SafetyViolations, tc.want) {
				t.Fatalf("decision = %+v, want %q", report.Decision, tc.want)
			}
		})
	}
}

func TestCorrectTerminalOutcomeRequiresItsFinalizationLane(t *testing.T) {
	input := testAutonomyInput()
	input.Evidence.Attempts[1].Merge = StageTiming{}
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.AutonomousCompletion.Numerator != 1 ||
		!containsString(report.Decision.SafetyViolations, "terminal-lane-invalid:b1:2") {
		t.Fatalf("missing merge lane counted as completion: metrics=%+v decision=%+v",
			report.Metrics, report.Decision)
	}

	input = testAutonomyInput()
	attempt := &input.Evidence.Attempts[1]
	attempt.Outcome.Kind = "pr-opened"
	attempt.PR = attempt.Merge
	attempt.Merge = StageTiming{}
	report, err = BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.AutonomousCompletion.Numerator != 2 ||
		containsString(report.Decision.SafetyViolations, "terminal-lane-invalid:b1:2") ||
		report.Metrics.Merge.Queue.Count != 2 {
		t.Fatalf("valid PR lane was not counted: metrics=%+v decision=%+v",
			report.Metrics, report.Decision)
	}
}

func TestExternalCapabilityExclusionsArePredeclaredAndMisclassificationFails(t *testing.T) {
	input := testAutonomyInput()
	input.Thresholds.MinEligibleBeads = 1
	input.Evidence.Exclusions = []AutonomyExclusion{{
		BeadID: "b2", Kind: ExclusionExternalCapability, Predeclared: true, Detail: "hardware lab",
	}}
	input.Evidence.Attempts[2].ModelTier = "standard"
	input.Evidence.Attempts[2].FrontierJustification = ""
	input.Evidence.Attempts[2].Outcome = TypedOutcome{
		Kind: "capability-hold", Terminal: true, BlockKind: ExclusionExternalCapability, Capability: "hardware-lab",
	}
	input.Evidence.Attempts[1].Review.Attempts = 1
	input.Evidence.Attempts[1].Review.FirstPass = true
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Decision.Passed || report.Metrics.EligibleBeads != 1 ||
		report.Metrics.ExcludedExternalHolds != 1 || report.Metrics.AutonomousCompletion.Denominator != 1 {
		t.Fatalf("valid exclusion report metrics=%+v decision=%+v", report.Metrics, report.Decision)
	}

	input.Evidence.Exclusions = nil
	input.Thresholds.MinEligibleBeads = 2
	report, err = BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision.Passed || report.Metrics.MisclassifiedCapabilityBlocks != 1 ||
		!containsString(report.Decision.SafetyViolations, "misclassified-capability-block") {
		t.Fatalf("misclassified hold was hidden: metrics=%+v decision=%+v", report.Metrics, report.Decision)
	}
}

func TestExternalCapabilityExclusionRequiresTerminalHoldEvidence(t *testing.T) {
	input := testAutonomyInput()
	input.Thresholds.MinEligibleBeads = 1
	input.Evidence.Exclusions = []AutonomyExclusion{{
		BeadID: "b2", Kind: ExclusionExternalCapability, Predeclared: true, Detail: "hardware lab",
	}}
	input.Evidence.Attempts = input.Evidence.Attempts[:2]
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision.Passed ||
		!containsString(report.Decision.SafetyViolations, "missing-attempts:b2") {
		t.Fatalf("zero-attempt exclusion bypassed terminal hold: %+v", report.Decision)
	}
}

func TestAutonomySafetyTripwiresFailClosed(t *testing.T) {
	input := testAutonomyInput()
	input.Evidence.IdleLedgerCreations = 1
	input.Evidence.Safety = input.Evidence.Safety[1:] // required invariant omitted
	input.Evidence.Attempts[1].Retry.EvidenceDigest = input.Evidence.Attempts[1].Retry.PriorEvidenceDigest
	start := input.Evidence.Attempts[2].ProcessEvents[0]
	reuse := start
	reuse.Event = "reuse"
	reuse.At = "2026-07-25T10:08:30Z"
	reuse.Duplicate = true
	input.Evidence.Attempts[2].ProcessEvents = append(
		input.Evidence.Attempts[2].ProcessEvents[:1],
		reuse,
		input.Evidence.Attempts[2].ProcessEvents[1],
	)
	input.Evidence.Attempts[2].FrontierJustification = ""
	input.Evidence.Attempts[2].Tokens.Total++
	input.Evidence.Pressure = append(input.Evidence.Pressure, PressureEvidence{
		At: "2026-07-25T10:05:00Z", Level: "critical", Persistent: true,
	})
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"idle-ledger-creation", "unchanged-retry", "duplicate-broad-command",
		"unjustified-frontier-implementation", "token-semantics-inconsistent",
		"persistent-critical-pressure", "safety-invariant-missing:candidate-contract",
	} {
		if !containsString(report.Decision.SafetyViolations, want) {
			t.Errorf("missing violation %q in %v", want, report.Decision.SafetyViolations)
		}
	}
	if report.Decision.Passed {
		t.Fatal("unsafe report passed")
	}
}

func TestAutonomyReportWriteIsAtomicAndImmutable(t *testing.T) {
	report, err := BuildAutonomyReport(testAutonomyInput(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tempRoot, "canary", "report.json")
	if err := WriteAutonomyReport(path, report); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAutonomyReport(path, report); !errors.Is(err, ErrAutonomyReportExists) {
		t.Fatalf("second write error = %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("immutable report changed after second write")
	}
	loaded, err := LoadAutonomyReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.EvidenceDigest != report.EvidenceDigest {
		t.Fatal("loaded report differs")
	}
}

func TestAutonomyReportRejectsTamperedDerivedDecision(t *testing.T) {
	report, err := BuildAutonomyReport(testAutonomyInput(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	report.Decision.Passed = false
	if err := ValidateAutonomyReport(report); err == nil || !strings.Contains(err.Error(), "derived metrics or decision") {
		t.Fatalf("tamper validation error = %v", err)
	}
}

func TestDecodeAutonomyInputRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	input := testAutonomyInput()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"project_id"`), []byte(`"surprise":true,"project_id"`), 1)
	if _, err := DecodeAutonomyInput(bytes.NewReader(raw)); err == nil {
		t.Fatal("unknown input field accepted")
	}
	raw, _ = json.Marshal(input)
	if _, err := DecodeAutonomyInput(bytes.NewReader(append(raw, []byte(`{}`)...))); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestExpectedAutonomyReportPinsReleaseIdentityAndFreshness(t *testing.T) {
	input := testAutonomyInput()
	generated := time.Date(2026, 7, 25, 10, 12, 1, 0, time.UTC)
	report, err := BuildAutonomyReport(input, generated)
	if err != nil {
		t.Fatal(err)
	}
	expected := AutonomyReportExpectation{
		ProjectID:              input.ProjectID,
		InstalledCommit:        input.InstalledCommit,
		BinaryVersion:          input.BinaryVersion,
		BuildIdentity:          input.BuildIdentity,
		ContractDigest:         input.ContractDigest,
		RegistryIdentityDigest: input.RegistryIdentityDigest,
		Cohort:                 input.Cohort,
		CohortDigest:           report.CohortDigest,
		Thresholds:             input.Thresholds,
		CanaryStartedAt:        input.StartedAt,
		EvidenceDigest:         report.EvidenceDigest,
		GeneratedAt:            report.GeneratedAt,
		FreshAt:                generated.Add(time.Minute),
		MaxAge:                 time.Hour,
		MaxFutureSkew:          0,
	}
	if err := ValidateAutonomyReportExpected(report, expected); err != nil {
		t.Fatalf("expected report rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*AutonomyReportExpectation)
	}{
		{"project", func(e *AutonomyReportExpectation) { e.ProjectID = "foreign" }},
		{"commit", func(e *AutonomyReportExpectation) { e.InstalledCommit = strings.Repeat("c", 40) }},
		{"version", func(e *AutonomyReportExpectation) { e.BinaryVersion = "0.10.1" }},
		{"registry identity", func(e *AutonomyReportExpectation) { e.RegistryIdentityDigest = digest("d") }},
		{"build", func(e *AutonomyReportExpectation) { e.BuildIdentity = "foreign-build" }},
		{"contract", func(e *AutonomyReportExpectation) { e.ContractDigest = digest("c") }},
		{"cohort", func(e *AutonomyReportExpectation) {
			e.Cohort = []string{"b1", "b3"}
			e.CohortDigest, _ = AutonomyCohortDigest(e.Cohort)
		}},
		{"threshold", func(e *AutonomyReportExpectation) { e.Thresholds.MinEligibleBeads++ }},
		{"start", func(e *AutonomyReportExpectation) { e.CanaryStartedAt = "2026-07-25T10:00:01Z" }},
		{"evidence digest", func(e *AutonomyReportExpectation) { e.EvidenceDigest = digest("c") }},
		{"generation", func(e *AutonomyReportExpectation) {
			e.GeneratedAt = generated.Add(time.Second).Format(time.RFC3339Nano)
		}},
		{"stale", func(e *AutonomyReportExpectation) {
			e.FreshAt = generated.Add(2 * time.Hour)
			e.MaxAge = time.Hour
		}},
		{"future", func(e *AutonomyReportExpectation) { e.FreshAt = generated.Add(-time.Second) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := expected
			tc.mutate(&changed)
			if err := ValidateAutonomyReportExpected(report, changed); err == nil {
				t.Fatal("foreign or stale report accepted")
			}
		})
	}
	if err := ValidateAutonomyReportExpected(report, AutonomyReportExpectation{}); err == nil {
		t.Fatal("missing release expectation accepted")
	}
}

func TestAutonomyReportPathsRejectSymlinksAndSpecialFiles(t *testing.T) {
	report, err := BuildAutonomyReport(testAutonomyInput(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(root, "real")
	realPath := filepath.Join(realParent, "report.json")
	if err := WriteAutonomyReport(realPath, report); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realParent, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAutonomyReport(filepath.Join(alias, "report.json")); err == nil {
		t.Fatal("load followed a symlinked parent")
	}
	if err := WriteAutonomyReport(filepath.Join(alias, "new.json"), report); err == nil {
		t.Fatal("write followed a symlinked parent")
	}
	finalLink := filepath.Join(root, "report-link.json")
	if err := os.Symlink(realPath, finalLink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAutonomyReport(finalLink); err == nil {
		t.Fatal("load followed a final symlink")
	}
	if err := WriteAutonomyReport(finalLink, report); err == nil {
		t.Fatal("write accepted a final symlink")
	}
	fifo := filepath.Join(root, "report.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAutonomyReport(fifo); err == nil {
		t.Fatal("load accepted a FIFO")
	}
	if err := WriteAutonomyReport(fifo, report); err == nil {
		t.Fatal("write accepted a FIFO")
	}
}

func TestAutonomyVocabulariesAndTruthCombinationsFailClosed(t *testing.T) {
	input := testAutonomyInput()
	input.Evidence.Attempts[2].ModelTier = "future-tier"
	input.Evidence.Attempts[2].Outcome = TypedOutcome{Kind: "future-outcome", Terminal: true, Correct: true}
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision.Passed ||
		report.Metrics.AutonomousCompletion.Numerator != 1 ||
		!containsString(report.Decision.SafetyViolations, "model-tier-invalid:b2:1") ||
		!containsString(report.Decision.SafetyViolations, "outcome-invalid:b2:1:unknown-kind") {
		t.Fatalf("unknown vocabulary escaped: metrics=%+v decision=%+v", report.Metrics, report.Decision)
	}

	input = testAutonomyInput()
	input.Thresholds.MinEligibleBeads = 1
	input.Evidence.Exclusions = []AutonomyExclusion{{
		BeadID: "b2", Kind: ExclusionExternalCapability, Predeclared: true, Detail: "lab",
	}}
	input.Evidence.Attempts[2].Outcome = TypedOutcome{
		Kind: "capability-unavailable", BlockKind: ExclusionExternalCapability, Capability: "lab",
	}
	input.Evidence.Attempts[2].TerminalAt = ""
	report, err = BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision.Passed ||
		!containsString(report.Decision.SafetyViolations, "excluded-without-external-hold:b2") {
		t.Fatalf("noncanonical capability hold accepted: %+v", report.Decision)
	}
}

func TestAutonomyIntegerArithmeticRejectsOverflowAndNegativeProviderInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*AutonomyInput)
	}{
		{"negative provider", func(input *AutonomyInput) {
			input.Evidence.Attempts[0].Tokens.ProviderTotalInput = -1
		}},
		{"attempt overflow", func(input *AutonomyInput) {
			input.Evidence.Attempts[0].Tokens = TokenEvidence{
				Semantics: TokenSemanticsDisjointV1,
				Input:     math.MaxInt64, Output: 1, Total: math.MaxInt64,
			}
		}},
		{"aggregate overflow", func(input *AutonomyInput) {
			input.Evidence.Attempts[0].Tokens = TokenEvidence{
				Semantics: TokenSemanticsDisjointV1, Input: math.MaxInt64, Total: math.MaxInt64,
			}
			input.Evidence.Attempts[1].Tokens = TokenEvidence{
				Semantics: TokenSemanticsDisjointV1, Input: 1, Total: 1,
			}
		}},
		{"stage aggregate overflow", func(input *AutonomyInput) {
			input.Evidence.Attempts[0].Gate = StageTiming{Reached: true, QueueMS: math.MaxInt64}
			input.Evidence.Attempts[1].Gate = StageTiming{Reached: true, QueueMS: 1}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := testAutonomyInput()
			tc.mutate(&input)
			if _, err := BuildAutonomyReport(input, time.Now()); err == nil {
				t.Fatal("invalid integer arithmetic accepted")
			}
		})
	}
}

func TestReachedStageDistributionsKeepZeroAndMedianRoundsUp(t *testing.T) {
	input := testAutonomyInput()
	input.Evidence.Attempts[0].Gate = StageTiming{Reached: true}
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.Gate.Queue.Count != 3 || report.Metrics.Gate.Service.Count != 3 {
		t.Fatalf("reached zero sample disappeared: %+v", report.Metrics.Gate)
	}
	got := distribution([]int64{1, 2})
	if got.MedianMS != 2 {
		t.Fatalf("even median = %d, want conservative ceiling 2", got.MedianMS)
	}
	got = distribution([]int64{math.MaxInt64, 1})
	if got.TotalMS != math.MaxInt64 {
		t.Fatalf("overflowing distribution total = %d, want saturation", got.TotalMS)
	}
	input = testAutonomyInput()
	input.Evidence.Attempts[0].Gate.Reached = false
	report, err = BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(report.Decision.SafetyViolations, "stage-reach-invalid:b1:1") {
		t.Fatalf("unreached nonzero timing accepted: %+v", report.Decision)
	}
}

func TestDecodeAutonomyInputRejectsDuplicateObjectKeys(t *testing.T) {
	raw, err := json.Marshal(testAutonomyInput())
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"input":10`), []byte(`"input":10,"input":11`), 1)
	if _, err := DecodeAutonomyInput(bytes.NewReader(raw)); err == nil ||
		!strings.Contains(err.Error(), "duplicate JSON object key") {
		t.Fatalf("duplicate-key error = %v", err)
	}
}

func TestAutonomyJSONRejectsNoncanonicalCaseAliases(t *testing.T) {
	inputRaw, err := json.Marshal(testAutonomyInput())
	if err != nil {
		t.Fatal(err)
	}
	inputRaw = bytes.Replace(inputRaw, []byte(`"project_id"`), []byte(`"Project_ID"`), 1)
	if _, err := DecodeAutonomyInput(bytes.NewReader(inputRaw)); err == nil ||
		!strings.Contains(err.Error(), "noncanonical JSON object key") {
		t.Fatalf("input alias error = %v", err)
	}

	report, err := BuildAutonomyReport(testAutonomyInput(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reportRaw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	reportRaw = bytes.Replace(reportRaw, []byte(`"project_id"`), []byte(`"Project_ID"`), 1)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "report.json")
	if err := os.WriteFile(path, reportRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAutonomyReport(path); err == nil ||
		!strings.Contains(err.Error(), "noncanonical JSON object key") {
		t.Fatalf("report alias error = %v", err)
	}
}

func TestAutonomyEvidenceTimestampsAreBoundedAndOrdered(t *testing.T) {
	input := testAutonomyInput()
	input.Evidence.Attempts[0].DispatchedAt = "2026-07-25T09:59:59Z"
	input.Evidence.Attempts[2].TerminalAt = "2026-07-25T10:12:01Z"
	input.Evidence.Attempts[2].ProcessEvents = []ProcessEvidence{
		{Event: "complete", Class: "broad", Signature: "one", Generation: "g1", At: "2026-07-25T10:02:00Z"},
		{Event: "complete", Class: "broad", Signature: "two", Generation: "g2", At: "2026-07-25T10:01:30Z"},
	}
	input.Evidence.Pressure = append(input.Evidence.Pressure, PressureEvidence{
		At: "2026-07-25T09:59:00Z", Level: "normal",
	})
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"attempt-dispatch-time:b1:1",
		"attempt-terminal-time:b2:1",
		"process-evidence-time:b2:1",
		"pressure-evidence-invalid",
	} {
		if !containsString(report.Decision.SafetyViolations, want) {
			t.Errorf("missing %q in %v", want, report.Decision.SafetyViolations)
		}
	}
	if _, err := BuildAutonomyReport(testAutonomyInput(),
		time.Date(2026, 7, 25, 10, 11, 0, 0, time.UTC)); err == nil {
		t.Fatal("generation before canary end accepted")
	}
}

func TestAutonomyCrossStageTemporalOrderFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AutonomyAttempt)
		want   string
	}{
		{
			name: "candidate completion after gate",
			mutate: func(attempt *AutonomyAttempt) {
				attempt.TerminalEvidence.CompletedAt = "2026-07-25T10:08:01Z"
			},
			want: "gate-time",
		},
		{
			name: "gate before dispatch",
			mutate: func(attempt *AutonomyAttempt) {
				attempt.GateEvidence.CompletedAt = "2026-07-25T10:02:59Z"
			},
			want: "gate-time",
		},
		{
			name: "general review before gate",
			mutate: func(attempt *AutonomyAttempt) {
				attempt.ReviewEvidenceRefs[0].CompletedAt = "2026-07-25T10:07:59Z"
			},
			want: "general-review-order",
		},
		{
			name: "required security before general",
			mutate: func(attempt *AutonomyAttempt) {
				attempt.ReviewEvidenceRefs[0].SecurityRequired = true
				security := attempt.ReviewEvidenceRefs[0]
				security.Kind = "security"
				security.ArtifactDigest = digest("7")
				security.HistoryDigest = digest("8")
				security.CompletedAt = "2026-07-25T10:08:59Z"
				attempt.ReviewEvidenceRefs = append(attempt.ReviewEvidenceRefs, security)
			},
			want: "security-review-order",
		},
		{
			name: "terminal before review",
			mutate: func(attempt *AutonomyAttempt) {
				attempt.TerminalAt = "2026-07-25T10:08:59Z"
			},
			want: "terminal-order",
		},
		{
			name: "review ref without queue service stage",
			mutate: func(attempt *AutonomyAttempt) {
				attempt.SemanticReview = StageTiming{}
				attempt.Review = ReviewEvidence{}
			},
			want: "review-stage-unreached",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := testAutonomyInput()
			attempt := &input.Evidence.Attempts[1]
			tc.mutate(attempt)
			report, err := BuildAutonomyReport(input, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			want := "evidence-reference-invalid:b1:2:" + tc.want
			if !containsString(report.Decision.SafetyViolations, want) {
				t.Fatalf("missing %q in %v", want, report.Decision.SafetyViolations)
			}
		})
	}
}

func TestDuplicateBroadExecutionsAreInferredFromIdentity(t *testing.T) {
	input := testAutonomyInput()
	event := input.Evidence.Attempts[2].ProcessEvents[0]
	event.At = "2026-07-25T10:09:01Z"
	event.Duplicate = false
	input.Evidence.Attempts[2].ProcessEvents = append(
		input.Evidence.Attempts[2].ProcessEvents,
		event,
	)
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.DuplicateBroadCommands != 1 ||
		!containsString(report.Decision.SafetyViolations, "duplicate-broad-command") {
		t.Fatalf("duplicate execution was not inferred: metrics=%+v decision=%+v",
			report.Metrics, report.Decision)
	}
}

func TestBroadCommandLifecycleAndArtifactEvidenceFailClosed(t *testing.T) {
	input := testAutonomyInput()
	input.Evidence.Attempts[2].ProcessEvents =
		input.Evidence.Attempts[2].ProcessEvents[1:]
	input.Evidence.Artifacts.ReclaimableBytes = 901
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"process-evidence-lifecycle:" + strings.Repeat("a", 24) + ":" + strings.Repeat("b", 32),
		"artifact-evidence-invalid",
	} {
		if !containsString(report.Decision.SafetyViolations, want) {
			t.Errorf("missing %q in %v", want, report.Decision.SafetyViolations)
		}
	}
}

func TestBroadCommandCompletionCannotPrecedeStart(t *testing.T) {
	input := testAutonomyInput()
	input.Evidence.Attempts[2].ProcessEvents[0].Event = "complete"
	input.Evidence.Attempts[2].ProcessEvents[1].Event = "start"
	report, err := BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := "process-evidence-lifecycle:" +
		strings.Repeat("a", 24) + ":" + strings.Repeat("b", 32)
	if report.Decision.Passed ||
		!containsString(report.Decision.SafetyViolations, want) {
		t.Fatalf("complete-before-start lifecycle passed: %+v", report.Decision)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
