// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/metrics"
)

func TestNativeCanaryPublisherDerivesAttemptsAndReplaysCreateOnce(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	contract := []byte("# repository contract\n")
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), contract, 0o644); err != nil {
		t.Fatal(err)
	}
	contractSum := sha256.Sum256(contract)
	store := ledger.NewStore(root)
	run, err := store.NewRun("demo", "bd", "0.10.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlot(run, &ledger.Slot{
		PhaseID: "b1", BeadID: "b1", Attempts: 1,
		DispatchGeneration: "generation-1",
		DispatchedAt:       "2026-07-25T12:00:00Z",
		Status:             ledger.SlotRunning,
		OutcomeClass:       "code-defect",
		InputTokens:        10, OutputTokens: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlot(run, &ledger.Slot{
		PhaseID: "b1", BeadID: "b1", Attempts: 2,
		DispatchGeneration: "generation-2",
		DispatchedAt:       "2026-07-25T12:01:00Z",
		FinishedAt:         "2026-07-25T12:02:00Z",
		Status:             ledger.SlotFailed,
		OutcomeClass:       "code-defect",
		InputTokens:        25, OutputTokens: 5,
	}); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(
		root, ".plan-logs", "koryph", "canary", "autonomous-loop-reliability.json",
	)
	thresholds := metrics.DefaultAutonomyThresholds()
	thresholds.MinEligibleBeads = 1
	publisher := NativeCanaryPublisher{
		RepoRoot: root, Ledger: store, Thresholds: thresholds,
		Now: func() time.Time {
			return time.Date(2026, 7, 25, 12, 3, 0, 0, time.UTC)
		},
	}
	request := CanaryPublicationRequest{
		ProjectID: "demo",
		StartedAt: "2026-07-25T12:00:00Z",
		EndedAt:   "2026-07-25T12:03:00Z",
		Canary: CanaryState{
			Cohort: []string{"b1"}, CohortDigest: cohortDigest([]string{"b1"}),
			InstalledCommit: strings.Repeat("a", 40), BinaryVersion: "0.10.0",
			BuildIdentity:  "fixture-build",
			ContractDigest: "sha256:" + hex.EncodeToString(contractSum[:]),
			ReportPath:     reportPath,
			RunIDs:         []string{run.RunID},
			Terminal: map[string]TerminalOutcome{
				"b1": {BeadID: "b1", Status: ledger.SlotFailed, Good: false},
			},
			Pressure: []PressureSample{{
				RunID: run.RunID, At: "2026-07-25T12:02:30Z",
				Level: "normal", AvailableMB: 4096,
			}},
		},
	}

	first, err := publisher.Publish(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Decision != "failed" || first.Digest == "" || first.GeneratedAt == "" {
		t.Fatalf("publication = %+v", first)
	}
	report, err := metrics.LoadAutonomyReport(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Evidence.Attempts) != 2 ||
		report.Evidence.Attempts[1].Retry == nil ||
		report.Evidence.Attempts[0].Tokens.Total != 12 ||
		report.Evidence.Attempts[1].Tokens.Total != 18 {
		t.Fatalf("native attempt evidence = %+v", report.Evidence.Attempts)
	}

	second, err := publisher.Publish(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Digest != first.Digest || second.GeneratedAt != first.GeneratedAt {
		t.Fatalf("idempotent publication changed: first=%+v second=%+v", first, second)
	}
}

func TestNativeCanaryEvidenceRejectsSameAttemptRelaunch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	contract := []byte("# repository contract\n")
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), contract, 0o644); err != nil {
		t.Fatal(err)
	}
	contractSum := sha256.Sum256(contract)
	store := ledger.NewStore(root)
	run, err := store.NewRun("demo", "bd", "0.10.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{"generation-1", "generation-2"} {
		if err := store.SetSlot(run, &ledger.Slot{
			PhaseID: "b1", BeadID: "b1", Attempts: 1,
			DispatchGeneration: generation,
			DispatchedAt:       "2026-07-25T12:00:00Z",
			FinishedAt:         "2026-07-25T12:01:00Z",
			Status:             ledger.SlotFailed,
			OutcomeClass:       "code-defect",
		}); err != nil {
			t.Fatal(err)
		}
	}
	request := CanaryPublicationRequest{
		ProjectID: "demo",
		StartedAt: "2026-07-25T12:00:00Z",
		EndedAt:   "2026-07-25T12:02:00Z",
		Canary: CanaryState{
			Cohort:          []string{"b1"},
			CohortDigest:    cohortDigest([]string{"b1"}),
			InstalledCommit: strings.Repeat("a", 40),
			BinaryVersion:   "0.10.0",
			BuildIdentity:   "fixture-build",
			ContractDigest:  "sha256:" + hex.EncodeToString(contractSum[:]),
			RunIDs:          []string{run.RunID},
			Terminal: map[string]TerminalOutcome{
				"b1": {BeadID: "b1", Status: ledger.SlotFailed},
			},
			Pressure: []PressureSample{{
				RunID: "foreign-run", At: "2026-07-25T12:01:30Z",
				Level: "normal", AvailableMB: 4096,
			}},
		},
	}
	evidence, err := collectNativeCanaryEvidence(root, store, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Attempts) != 2 {
		t.Fatalf("attempts = %+v, want both physical launches", evidence.Attempts)
	}
	for _, item := range evidence.Safety {
		if item.Name == "resume-without-redispatch" {
			if item.Passed {
				t.Fatalf("same-number relaunch passed safety: %+v", item)
			}
		}
		if item.Name == "host-pressure-containment" && item.Passed {
			t.Fatalf("foreign pressure sample passed safety: %+v", item)
		}
	}
	if safetyPassed(evidence.Safety, "resume-without-redispatch") {
		t.Fatal("resume-without-redispatch safety evidence is missing or passing")
	}
	if safetyPassed(evidence.Safety, "host-pressure-containment") {
		t.Fatal("host-pressure-containment safety evidence is missing or passing")
	}
}

func TestRetryReasonComesFromPriorFailedAttempt(t *testing.T) {
	root := t.TempDir()
	contract := []byte("# repository contract\n")
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), contract, 0o644); err != nil {
		t.Fatal(err)
	}
	contractSum := sha256.Sum256(contract)
	store := ledger.NewStore(root)
	run, err := store.NewRun("demo", "bd", "0.10.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range []*ledger.Slot{
		{
			PhaseID: "b1", BeadID: "b1", Attempts: 1, ModelTier: metrics.ModelTierStandard,
			DispatchGeneration: strings.Repeat("1", 64),
			DispatchedAt:       "2026-07-25T12:00:00Z", FinishedAt: "2026-07-25T12:01:00Z",
			Status: ledger.SlotFailed, OutcomeClass: "code-defect",
		},
		{
			PhaseID: "b1", BeadID: "b1", Attempts: 2, ModelTier: metrics.ModelTierStandard,
			DispatchGeneration: strings.Repeat("2", 64),
			DispatchedAt:       "2026-07-25T12:01:00Z", FinishedAt: "2026-07-25T12:02:00Z",
			Status: ledger.SlotMerged, OutcomeClass: "candidate-ready",
		},
	} {
		if err := store.SetSlot(run, slot); err != nil {
			t.Fatal(err)
		}
	}
	request := CanaryPublicationRequest{
		ProjectID: "demo", StartedAt: "2026-07-25T12:00:00Z", EndedAt: "2026-07-25T12:03:00Z",
		Canary: CanaryState{
			Cohort: []string{"b1"}, CohortDigest: cohortDigest([]string{"b1"}),
			InstalledCommit: strings.Repeat("a", 40), BinaryVersion: "0.10.0",
			BuildIdentity:  "fixture-build",
			ContractDigest: "sha256:" + hex.EncodeToString(contractSum[:]),
			RunIDs:         []string{run.RunID},
			Terminal: map[string]TerminalOutcome{
				"b1": {BeadID: "b1", Status: ledger.SlotMerged, Good: true},
			},
			Pressure: []PressureSample{{
				RunID: run.RunID, At: "2026-07-25T12:02:30Z", Level: "normal",
			}},
		},
	}
	evidence, err := collectNativeCanaryEvidence(root, store, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Attempts) != 2 || evidence.Attempts[1].Retry == nil ||
		evidence.Attempts[1].Outcome.Kind != ledger.SlotMerged ||
		evidence.Attempts[1].Retry.Kind != "code-defect" {
		t.Fatalf("retry semantics = %+v", evidence.Attempts)
	}
}

func TestRetryEvidenceRejectsMissingPriorOutcomeClass(t *testing.T) {
	root := t.TempDir()
	contract := []byte("# repository contract\n")
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), contract, 0o644); err != nil {
		t.Fatal(err)
	}
	contractSum := sha256.Sum256(contract)
	store := ledger.NewStore(root)
	run, err := store.NewRun("demo", "bd", "0.10.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range []*ledger.Slot{
		{
			PhaseID: "b1", BeadID: "b1", Attempts: 1, ModelTier: metrics.ModelTierStandard,
			DispatchGeneration: strings.Repeat("1", 64),
			DispatchedAt:       "2026-07-25T12:00:00Z", FinishedAt: "2026-07-25T12:01:00Z",
			Status: ledger.SlotFailed,
		},
		{
			PhaseID: "b1", BeadID: "b1", Attempts: 2, ModelTier: metrics.ModelTierStandard,
			DispatchGeneration: strings.Repeat("2", 64),
			DispatchedAt:       "2026-07-25T12:01:00Z", FinishedAt: "2026-07-25T12:02:00Z",
			Status: ledger.SlotFailed, OutcomeClass: "code-defect",
		},
	} {
		if err := store.SetSlot(run, slot); err != nil {
			t.Fatal(err)
		}
	}
	request := CanaryPublicationRequest{
		ProjectID: "demo", StartedAt: "2026-07-25T12:00:00Z", EndedAt: "2026-07-25T12:03:00Z",
		Canary: CanaryState{
			Cohort: []string{"b1"}, CohortDigest: cohortDigest([]string{"b1"}),
			InstalledCommit: strings.Repeat("a", 40), BinaryVersion: "0.10.0",
			BuildIdentity:  "fixture-build",
			ContractDigest: "sha256:" + hex.EncodeToString(contractSum[:]),
			RunIDs:         []string{run.RunID},
			Terminal: map[string]TerminalOutcome{
				"b1": {BeadID: "b1", Status: ledger.SlotFailed},
			},
			Pressure: []PressureSample{{
				RunID: run.RunID, At: "2026-07-25T12:02:30Z", Level: "normal",
			}},
		},
	}
	evidence, err := collectNativeCanaryEvidence(root, store, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Attempts) != 2 || evidence.Attempts[1].Retry != nil {
		t.Fatalf("missing prior class produced retry evidence: %+v", evidence.Attempts)
	}
	if safetyPassed(evidence.Safety, "candidate-contract") ||
		safetyPassed(evidence.Safety, "capability-no-redispatch") {
		t.Fatalf("missing prior class passed safety: %+v", evidence.Safety)
	}
}

func TestNativeAttemptDigestExcludesDispatchIdentityAndTime(t *testing.T) {
	first := nativeAttempt{runID: "run-1", slot: ledger.Slot{
		PhaseID: "b1", BeadID: "b1", Attempts: 2, SessionID: "session-1",
		DispatchGeneration: "generation-1", DispatchedAt: "2026-07-25T12:00:00Z",
		FinishedAt: "2026-07-25T12:01:00Z", OutcomeClass: "code-defect",
		GeneralReviewArtifactDigest: "sha256:" + strings.Repeat("a", 64),
	}}
	second := first
	second.runID = "run-2"
	second.slot.SessionID = "session-2"
	second.slot.DispatchGeneration = "generation-2"
	second.slot.DispatchedAt = "2026-07-25T13:00:00Z"
	second.slot.FinishedAt = "2026-07-25T13:01:00Z"
	if nativeAttemptDigest(first) != nativeAttemptDigest(second) {
		t.Fatal("dispatch metadata changed semantic retry evidence digest")
	}
	second.slot.OutcomeClass = "semantic-defect"
	if nativeAttemptDigest(first) == nativeAttemptDigest(second) {
		t.Fatal("semantic outcome change did not change retry evidence digest")
	}
}

func TestCommandEvidenceRejectsSymlinkAndUnknownSchema(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := ledger.NewStore(root)
	phaseDir := store.PhaseDir("run-1", "b1")
	commandDir := filepath.Join(phaseDir, ".koryph-command")
	if err := os.MkdirAll(commandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(commandDir, "events.jsonl")
	if err := os.Symlink(outside, events); err != nil {
		t.Fatal(err)
	}
	slot := ledger.Slot{
		PhaseID: "b1", DispatchedAt: "2026-07-25T12:00:00Z",
	}
	if _, ok := attemptProcessEvidence(store, "run-1", slot, ""); ok {
		t.Fatal("symlinked command evidence was accepted")
	}
	if err := os.Remove(events); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(events, []byte(`{"schema":"unknown","event":"start","at":"2026-07-25T12:00:01Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := attemptProcessEvidence(store, "run-1", slot, ""); ok {
		t.Fatal("unknown command event schema was accepted")
	}
	duplicateSchema := `{"schema":"unknown","schema":"koryph.command-event/v1",` +
		`"event":"start","at":"2026-07-25T12:00:01Z","class":"broad"}`
	if err := os.WriteFile(events, []byte(duplicateSchema+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := attemptProcessEvidence(store, "run-1", slot, ""); ok {
		t.Fatal("duplicate-key command event was accepted through last-wins decoding")
	}
	caseAlias := `{"schema":"koryph.command-event/v1","event":"start","Event":"complete",` +
		`"at":"2026-07-25T12:00:01Z","class":"broad"}`
	if err := os.WriteFile(events, []byte(caseAlias+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := attemptProcessEvidence(store, "run-1", slot, ""); ok {
		t.Fatal("case-fold command event alias was accepted")
	}
}

func TestNativeVocabularyAndTokenArithmeticFailClosed(t *testing.T) {
	if tier, ok := normalizedModelTier(""); ok || tier != metrics.ModelTierStandard {
		t.Fatalf("missing tier = %q/%t, want canonical standard plus invalid marker", tier, ok)
	}
	if _, ok := normalizedModelTier("gpt-5.6-terra"); ok {
		t.Fatal("ambiguous concrete model was accepted as a runtime-neutral tier")
	}
	if knownAttemptOutcome("future-outcome", false) {
		t.Fatal("unknown outcome was accepted")
	}
	current := metrics.TokenEvidence{
		Input: int64(^uint64(0) >> 1), Output: 1,
	}
	if got, ok := tokenDelta(current, metrics.TokenEvidence{}); ok ||
		got.Semantics != "invalid" {
		t.Fatalf("overflowing token delta = %+v/%t", got, ok)
	}
}

func TestHardStopAlwaysFailsNativeSafety(t *testing.T) {
	safety := make(safetySet)
	for _, name := range metrics.RequiredSafetyInvariants {
		safety[name] = metrics.SafetyEvidence{Name: name, Passed: true}
	}
	applyHardStopSafety(safety, "duplicate-broad-command: phase b1")
	if safety["candidate-contract"].Passed || safety["single-broad-command"].Passed {
		t.Fatalf("hard-stop safety = %+v", safety)
	}
}

func TestNativeArtifactEvidenceSubtractsOnlyLedgerClassifiedFailures(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	repo := t.TempDir()
	store := ledger.NewStore(repo)
	run, err := store.NewRun("demo", "bd", "0.10.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range []*ledger.Slot{
		{PhaseID: "failed", BeadID: "failed", Attempts: 1, Status: ledger.SlotFailed},
		{PhaseID: "merged", BeadID: "merged", Attempts: 1, Status: ledger.SlotMerged},
	} {
		if err := store.SetSlot(run, slot); err != nil {
			t.Fatal(err)
		}
	}
	failedPayload := []byte(strings.Repeat("f", 101))
	mergedPayload := []byte(strings.Repeat("m", 103))
	abusiveMarker := []byte(strings.Repeat("r", 107))
	compactPayload := []byte(strings.Repeat("c", 109))
	failedArtifact := filepath.Join(store.PhaseDir(run.RunID, "failed"), "runtime-cache", "entry")
	for path, payload := range map[string][]byte{
		failedArtifact: failedPayload,
		filepath.Join(store.PhaseDir(run.RunID, "merged"), "success.log"): mergedPayload,
		filepath.Join(store.PhaseDir(run.RunID, "merged"), ".retain"):     abusiveMarker,
		filepath.Join(
			store.KoryphRoot, run.RunID, ".engine-evidence", "merged",
			"gate-evidence-fixture.json",
		): compactPayload,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	evidence, valid, err := nativeArtifactEvidence(store.KoryphRoot, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("regular confined inventory was marked invalid")
	}
	if evidence.RetainedFailureBytes != int64(len(failedPayload)) {
		t.Fatalf("retained failure bytes = %d, want %d; arbitrary .retain must not count",
			evidence.RetainedFailureBytes, len(failedPayload))
	}
	if evidence.CompactEvidenceBytes < int64(len(compactPayload)) {
		t.Fatalf("compact evidence bytes = %d, want at least %d",
			evidence.CompactEvidenceBytes, len(compactPayload))
	}
	if evidence.ActiveBytes-evidence.RetainedFailureBytes-evidence.CompactEvidenceBytes <
		int64(len(mergedPayload)+len(abusiveMarker)) {
		t.Fatalf("eligible bytes hid non-failure evidence: %+v", evidence)
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(failedArtifact, old, old); err != nil {
		t.Fatal(err)
	}
	aged, valid, err := nativeArtifactEvidence(store.KoryphRoot, "", time.Now())
	if err != nil || !valid {
		t.Fatalf("aged inventory = %+v valid=%t err=%v", aged, valid, err)
	}
	if aged.RetainedFailureBytes != 0 ||
		aged.ActiveBytes-aged.CompactEvidenceBytes < int64(len(failedPayload)) {
		t.Fatalf("aged failure artifact was hidden from eligible bytes: %+v", aged)
	}
}

func safetyPassed(items []metrics.SafetyEvidence, name string) bool {
	for _, item := range items {
		if item.Name == name {
			return item.Passed
		}
	}
	return true
}

func TestStageTimingRequiresAuthenticatedCompleteInterval(t *testing.T) {
	complete := stageTiming(ledger.StageTimingEvidence{
		QueuedAt:    "2026-07-25T12:00:00Z",
		StartedAt:   "2026-07-25T12:00:01Z",
		CompletedAt: "2026-07-25T12:00:03Z",
	})
	if !complete.Reached || complete.QueueMS != 1000 || complete.ServiceMS != 2000 {
		t.Fatalf("complete timing = %+v", complete)
	}
	partial := stageTiming(ledger.StageTimingEvidence{
		QueuedAt:  "2026-07-25T12:00:00Z",
		StartedAt: "2026-07-25T12:00:01Z",
	})
	if partial.Reached || partial.QueueMS != 0 || partial.ServiceMS != 0 {
		t.Fatalf("partial timing inferred reachability: %+v", partial)
	}
	combined := addStageTimings(partial, complete)
	if !combined.Reached || combined.QueueMS != 1000 || combined.ServiceMS != 2000 {
		t.Fatalf("combined timing = %+v", combined)
	}
}
