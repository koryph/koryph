// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/metrics"
)

func doctorAutonomyInput() metrics.AutonomyInput {
	thresholds := metrics.DefaultAutonomyThresholds()
	thresholds.MinEligibleBeads = 1
	var safety []metrics.SafetyEvidence
	for _, name := range metrics.RequiredSafetyInvariants {
		safety = append(safety, metrics.SafetyEvidence{Name: name, Passed: true})
	}
	candidate := strings.Repeat("c", 40)
	base := strings.Repeat("d", 40)
	digest := func(char string) string { return "sha256:" + strings.Repeat(char, 64) }
	return metrics.AutonomyInput{
		SchemaVersion: metrics.AutonomyInputSchema,
		ProjectID:     "demo", InstalledCommit: strings.Repeat("a", 40),
		BinaryVersion: "0.10.0", BuildIdentity: "fixture-build",
		ContractDigest:         "sha256:" + strings.Repeat("b", 64),
		RegistryIdentityDigest: digest("c"),
		Cohort:                 []string{"demo-1"},
		StartedAt:              "2026-07-25T10:00:00Z", EndedAt: "2026-07-25T10:05:00Z",
		Thresholds: thresholds,
		Evidence: metrics.AutonomyEvidence{
			Safety:    safety,
			Artifacts: metrics.ArtifactEvidence{ActiveBytes: 1024},
			Attempts: []metrics.AutonomyAttempt{{
				BeadID: "demo-1", RunID: "run-1", PhaseID: "demo-1", Attempt: 1,
				ModelTier: "standard", DispatchedAt: "2026-07-25T10:00:00Z",
				TerminalAt:     "2026-07-25T10:05:00Z",
				Outcome:        metrics.TypedOutcome{Kind: "merged", Terminal: true, Correct: true},
				Review:         metrics.ReviewEvidence{Reached: true, Attempts: 1, FirstPass: true},
				Gate:           metrics.StageTiming{Reached: true, QueueMS: 100, ServiceMS: 200},
				SemanticReview: metrics.StageTiming{Reached: true, QueueMS: 300, ServiceMS: 400},
				Merge:          metrics.StageTiming{Reached: true, QueueMS: 500, ServiceMS: 600},
				GateEvidence: &metrics.GateEvidenceRef{
					ArtifactDigest: digest("1"), CandidateSHA: candidate, BaseSHA: base,
					DiffDigest: digest("2"), GateConfigDigest: digest("3"), CommandDigest: digest("4"),
					EngineVersion: "0.10.0", BuildIdentity: "fixture-build",
					CompletedAt: "2026-07-25T10:03:00Z",
				},
				ReviewEvidenceRefs: []metrics.ReviewEvidenceRef{{
					Kind: "general", ArtifactDigest: digest("5"), HistoryDigest: digest("6"),
					CandidateSHA: candidate, BaseSHA: base,
					CompletedAt: "2026-07-25T10:04:00Z",
				}},
				TerminalEvidence: &metrics.TerminalEvidenceRef{
					ArtifactDigest: digest("7"), Generation: strings.Repeat("8", 64),
					CandidateSHA: candidate, BaseSHA: base,
					SummaryDigest: digest("9"), EvidenceDigest: digest("a"),
					CompletedAt: "2026-07-25T10:02:00Z",
				},
				Tokens: metrics.TokenEvidence{Semantics: metrics.TokenSemanticsDisjointV1},
			}},
		},
	}
}

func writeDoctorAutonomyReport(t *testing.T, input metrics.AutonomyInput) string {
	t.Helper()
	report, err := metrics.BuildAutonomyReport(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tempRoot, "report.json")
	if err := metrics.WriteAutonomyReport(path, report); err != nil {
		t.Fatal(err)
	}
	return path
}

func doctorAutonomyExpectation(
	t *testing.T,
	path string,
	input metrics.AutonomyInput,
) metrics.AutonomyReportExpectation {
	t.Helper()
	report, err := metrics.LoadAutonomyReport(path)
	if err != nil {
		t.Fatal(err)
	}
	return metrics.AutonomyReportExpectation{
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
		FreshAt:                time.Now().Add(time.Minute),
		MaxAge:                 time.Hour,
		MaxFutureSkew:          0,
	}
}

func TestCheckAutonomyReportPassesOnlyValidGreenReport(t *testing.T) {
	input := doctorAutonomyInput()
	path := writeDoctorAutonomyReport(t, input)
	finding := CheckAutonomyReport(path, doctorAutonomyExpectation(t, path, input))
	if finding.Level != LevelOK || finding.Check != checkNameAutonomyCanary ||
		!strings.Contains(finding.Message, "completion 100.0%") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestCheckAutonomyReportFailsAnySafetyOrSLOMiss(t *testing.T) {
	input := doctorAutonomyInput()
	input.Evidence.IdleLedgerCreations = 1
	input.Thresholds.MaxMedianLatencyMS = (2 * time.Minute).Milliseconds()
	input.Thresholds.MaxP95LatencyMS = (3 * time.Minute).Milliseconds()
	path := writeDoctorAutonomyReport(t, input)
	finding := CheckAutonomyReport(path, doctorAutonomyExpectation(t, path, input))
	if finding.Level != LevelError ||
		!strings.Contains(finding.Message, "idle-ledger-creation") ||
		!strings.Contains(finding.Message, "slo:median-dispatch-to-terminal-ms") ||
		!strings.Contains(finding.Message, "slo:p95-dispatch-to-terminal-ms") {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestCheckAutonomyReportFailsMissingAndTamperedEvidence(t *testing.T) {
	input := doctorAutonomyInput()
	validPath := writeDoctorAutonomyReport(t, input)
	expected := doctorAutonomyExpectation(t, validPath, input)
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missing := CheckAutonomyReport(filepath.Join(tempRoot, "missing.json"), expected)
	if missing.Level != LevelError || !strings.Contains(missing.Message, "missing") {
		t.Fatalf("missing finding = %+v", missing)
	}
	path := validPath
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"passed": true`, `"passed": false`, 1))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tampered := CheckAutonomyReport(path, expected)
	if tampered.Level != LevelError || !strings.Contains(tampered.Message, "invalid") {
		t.Fatalf("tampered finding = %+v", tampered)
	}
}

func TestCheckAutonomyReportRequiresExactLiveReleaseExpectation(t *testing.T) {
	input := doctorAutonomyInput()
	path := writeDoctorAutonomyReport(t, input)
	if finding := CheckAutonomyReport(path); finding.Level != LevelError ||
		!strings.Contains(finding.Message, "expectation is missing") {
		t.Fatalf("expectation-free report passed: %+v", finding)
	}
	expected := doctorAutonomyExpectation(t, path, input)
	expected.ProjectID = "foreign"
	if finding := CheckAutonomyReport(path, expected); finding.Level != LevelError ||
		!strings.Contains(finding.Message, "invalid") {
		t.Fatalf("foreign report passed: %+v", finding)
	}
}

func TestAutonomyReportPathIsFixedUnderProject(t *testing.T) {
	got := AutonomyReportPath("/repo")
	want := filepath.Join("/repo", ".plan-logs", "koryph", "canary", "autonomous-loop-reliability.json")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
