// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	doctorpkg "github.com/koryph/koryph/internal/doctor"
	"github.com/koryph/koryph/internal/engine"
	loopsupervisor "github.com/koryph/koryph/internal/loop"
	"github.com/koryph/koryph/internal/metrics"
)

func autonomyCLIInput() metrics.AutonomyInput {
	thresholds := metrics.DefaultAutonomyThresholds()
	thresholds.MinEligibleBeads = 1
	thresholds.MaxFrontierImplementation = 1
	var safety []metrics.SafetyEvidence
	for _, name := range metrics.RequiredSafetyInvariants {
		safety = append(safety, metrics.SafetyEvidence{Name: name, Passed: true})
	}
	candidate := strings.Repeat("c", 40)
	base := strings.Repeat("d", 40)
	return metrics.AutonomyInput{
		SchemaVersion: metrics.AutonomyInputSchema,
		ProjectID:     "demo", InstalledCommit: strings.Repeat("a", 40),
		BinaryVersion: "0.10.0", BuildIdentity: "fixture-build",
		ContractDigest: "sha256:" + strings.Repeat("b", 64),
		Cohort:         []string{"demo-1"},
		StartedAt:      "2026-07-25T10:00:00Z", EndedAt: "2026-07-25T10:05:00Z",
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
					ArtifactDigest: "sha256:" + strings.Repeat("1", 64),
					CandidateSHA:   candidate, BaseSHA: base,
					DiffDigest:       "sha256:" + strings.Repeat("2", 64),
					GateConfigDigest: "sha256:" + strings.Repeat("3", 64),
					CommandDigest:    "sha256:" + strings.Repeat("4", 64),
					EngineVersion:    "0.10.0", BuildIdentity: "fixture-build",
					CompletedAt: "2026-07-25T10:03:00Z",
				},
				ReviewEvidenceRefs: []metrics.ReviewEvidenceRef{{
					Kind: "general", ArtifactDigest: "sha256:" + strings.Repeat("5", 64),
					HistoryDigest: "sha256:" + strings.Repeat("6", 64),
					CandidateSHA:  candidate, BaseSHA: base,
					CompletedAt: "2026-07-25T10:04:00Z",
				}},
				TerminalEvidence: &metrics.TerminalEvidenceRef{
					ArtifactDigest: "sha256:" + strings.Repeat("7", 64),
					Generation:     strings.Repeat("8", 64),
					CandidateSHA:   candidate, BaseSHA: base,
					SummaryDigest:  "sha256:" + strings.Repeat("9", 64),
					EvidenceDigest: "sha256:" + strings.Repeat("a", 64),
					CompletedAt:    "2026-07-25T10:02:00Z",
				},
				Tokens: metrics.TokenEvidence{Semantics: metrics.TokenSemanticsDisjointV1},
			}},
		},
	}
}

func writeAutonomyCLIInput(t *testing.T, input metrics.AutonomyInput) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func autonomyRealTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAutonomyReportCommandPublishesAndReportsPassingCanary(t *testing.T) {
	input := writeAutonomyCLIInput(t, autonomyCLIInput())
	output := filepath.Join(autonomyRealTempDir(t), "canary", "report.json")
	var stdout, stderr bytes.Buffer
	code := cmdMetricsAutonomy([]string{"--input", input, "--out", output}, &stdout, &stderr)
	if code != engine.ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "passed=true") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	report, err := metrics.LoadAutonomyReport(output)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Decision.Passed || report.ProjectID != "demo" {
		t.Fatalf("report = %+v", report)
	}
}

func TestMetricsDispatchRoutesAutonomySubcommand(t *testing.T) {
	input := writeAutonomyCLIInput(t, autonomyCLIInput())
	output := filepath.Join(autonomyRealTempDir(t), "report.json")
	var stdout, stderr bytes.Buffer
	code := cmdMetricsDispatch(
		[]string{"autonomy", "--input", input, "--out", output},
		&stdout,
		&stderr,
	)
	if code != 0 || !strings.Contains(stdout.String(), "autonomy report:") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := metrics.LoadAutonomyReport(output); err != nil {
		t.Fatal(err)
	}
}

func TestAutonomyReportCommandPublishesFailureBeforeNonzeroExit(t *testing.T) {
	input := autonomyCLIInput()
	input.Evidence.IdleLedgerCreations = 1
	inputPath := writeAutonomyCLIInput(t, input)
	output := filepath.Join(autonomyRealTempDir(t), "report.json")
	var stdout, stderr bytes.Buffer
	code := cmdMetricsAutonomy([]string{"--input", inputPath, "--out", output}, &stdout, &stderr)
	if code != engine.ExitFatal {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	report, err := metrics.LoadAutonomyReport(output)
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision.Passed || !strings.Contains(stderr.String(), "posture failed") {
		t.Fatalf("failed report decision=%+v stderr=%q", report.Decision, stderr.String())
	}
}

func TestAutonomyReportCommandNeverOverwritesExistingEvidence(t *testing.T) {
	input := writeAutonomyCLIInput(t, autonomyCLIInput())
	output := filepath.Join(autonomyRealTempDir(t), "report.json")
	var out, errOut bytes.Buffer
	if code := cmdMetricsAutonomy([]string{"--input", input, "--out", output}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	before, _ := os.ReadFile(output)
	out.Reset()
	errOut.Reset()
	if code := cmdMetricsAutonomy([]string{"--input", input, "--out", output}, &out, &errOut); code != engine.ExitFatal {
		t.Fatalf("second code=%d stderr=%s", code, errOut.String())
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("existing report was overwritten")
	}
}

func TestAutonomyOfflineCommandCannotPoisonNativeReportPath(t *testing.T) {
	input := writeAutonomyCLIInput(t, autonomyCLIInput())
	root := autonomyRealTempDir(t)
	reserved := filepath.Join(root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath))
	var stdout, stderr bytes.Buffer
	code := cmdMetricsAutonomy(
		[]string{"--input", input, "--out", reserved}, &stdout, &stderr,
	)
	if code != engine.ExitUsage || !strings.Contains(stderr.String(), "reserved") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(reserved); !os.IsNotExist(err) {
		t.Fatalf("manual path poisoning created reserved report: %v", err)
	}

	if reservedNativeAutonomyReportPath(defaultOfflineAutonomyReportPath) {
		t.Fatal("offline default still aliases the reserved native report path")
	}
}

func TestAutonomyReportDefaultMedianThreshold(t *testing.T) {
	if got := time.Duration(metrics.DefaultAutonomyThresholds().MaxMedianLatencyMS) * time.Millisecond; got != 15*time.Minute {
		t.Fatalf("default median threshold = %s", got)
	}
}

func TestDoctorAutonomyFindingIsBoundToDurableCanaryState(t *testing.T) {
	root := autonomyRealTempDir(t)
	input := autonomyCLIInput()
	input.Thresholds = metrics.DefaultAutonomyThresholds()
	input.Cohort = nil
	input.Evidence.Attempts = nil
	template := autonomyCLIInput().Evidence.Attempts[0]
	for i := 0; i < input.Thresholds.MinEligibleBeads; i++ {
		id := fmt.Sprintf("demo-%02d", i+1)
		attempt := template
		attempt.BeadID = id
		attempt.PhaseID = id
		input.Cohort = append(input.Cohort, id)
		input.Evidence.Attempts = append(input.Evidence.Attempts, attempt)
	}
	path := doctorpkg.AutonomyReportPath(root)
	generated := time.Date(2026, 7, 25, 10, 5, 1, 0, time.UTC)
	autonomyReport, err := metrics.PublishAutonomyReport(path, input, generated)
	if err != nil {
		t.Fatal(err)
	}
	cohortDigest, err := metrics.AutonomyCohortDigest(input.Cohort)
	if err != nil {
		t.Fatal(err)
	}
	state := loopsupervisor.State{
		ProjectID: "demo",
		StartedAt: input.StartedAt,
		Canary: &loopsupervisor.CanaryState{
			Cohort:            append([]string(nil), input.Cohort...),
			CohortDigest:      cohortDigest,
			StartedAt:         input.StartedAt,
			InstalledCommit:   input.InstalledCommit,
			BinaryVersion:     input.BinaryVersion,
			BuildIdentity:     input.BuildIdentity,
			ContractDigest:    input.ContractDigest,
			ReportPath:        path,
			Decision:          "passed",
			ReportDigest:      autonomyReport.EvidenceDigest,
			ReportGeneratedAt: autonomyReport.GeneratedAt,
			PublishedAt:       generated.Add(time.Second).Format(time.RFC3339Nano),
		},
	}
	store := loopsupervisor.NewStore(root)
	if err := store.SaveState(state); err != nil {
		t.Fatal(err)
	}
	report := &doctorpkg.Report{Project: "demo", Home: root}
	appendAutonomyDoctorFinding(report, false)
	if len(report.Findings) != 1 || report.Findings[0].Level != doctorpkg.LevelOK {
		t.Fatalf("valid state-bound finding = %+v", report.Findings)
	}

	state.Canary.ReportDigest = "sha256:" + strings.Repeat("c", 64)
	if err := store.SaveState(state); err != nil {
		t.Fatal(err)
	}
	report.Findings = nil
	appendAutonomyDoctorFinding(report, false)
	if len(report.Findings) != 1 || report.Findings[0].Level != doctorpkg.LevelError ||
		!strings.Contains(report.Findings[0].Message, "expected release identity") {
		t.Fatalf("mismatched state finding = %+v", report.Findings)
	}
}

func TestDoctorAutonomyReportWithoutCanaryStateFailsClosed(t *testing.T) {
	root := autonomyRealTempDir(t)
	path := doctorpkg.AutonomyReportPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := &doctorpkg.Report{Project: "demo", Home: root}
	appendAutonomyDoctorFinding(report, false)
	if len(report.Findings) != 1 || report.Findings[0].Level != doctorpkg.LevelError ||
		!strings.Contains(report.Findings[0].Message, "durable supervisor state") {
		t.Fatalf("missing-state finding = %+v", report.Findings)
	}
}

func TestDoctorAutonomyRejectsSymlinkedAndDuplicateKeySupervisorState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		poison func(*testing.T, *loopsupervisor.Store)
	}{
		{
			name: "symlink",
			poison: func(t *testing.T, store *loopsupervisor.Store) {
				outside := filepath.Join(t.TempDir(), "supervisor.json")
				if err := os.WriteFile(outside, []byte(`{"schema_version":2}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, store.StatePath()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "duplicate key",
			poison: func(t *testing.T, store *loopsupervisor.Store) {
				if err := os.WriteFile(
					store.StatePath(),
					[]byte(`{"schema_version":2,"schema_version":2}`),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := autonomyRealTempDir(t)
			store := loopsupervisor.NewStore(root)
			if err := os.MkdirAll(filepath.Dir(store.StatePath()), 0o700); err != nil {
				t.Fatal(err)
			}
			tc.poison(t, store)
			report := &doctorpkg.Report{Project: "demo", Home: root}
			appendAutonomyDoctorFinding(report, true)
			if len(report.Findings) != 1 ||
				report.Findings[0].Level != doctorpkg.LevelError ||
				!strings.Contains(report.Findings[0].Message, "state is invalid") {
				t.Fatalf("poisoned state finding = %+v", report.Findings)
			}
		})
	}
}
