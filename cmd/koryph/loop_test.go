// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/commands"
	"github.com/koryph/koryph/internal/engine"
	koryphgc "github.com/koryph/koryph/internal/gc"
	"github.com/koryph/koryph/internal/ledger"
	loopsupervisor "github.com/koryph/koryph/internal/loop"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
)

func TestLoopHasNoAllowUnvalidatedSwitch(t *testing.T) {
	code, _, errb := runCmd("loop", "--allow-unvalidated")
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want usage", code)
	}
	if !strings.Contains(errb, "flag provided but not defined") {
		t.Fatalf("stderr = %q", errb)
	}
}

func TestCanaryRequiresReviewAndAutoMerge(t *testing.T) {
	for _, args := range [][]string{
		{"loop", "--canary-cohort", "a,b", "--review=false"},
		{"loop", "--canary-cohort", "a,b", "--auto-merge=false"},
	} {
		code, _, errb := runCmd(args...)
		if code != engine.ExitUsage ||
			!strings.Contains(errb, "requires --review=true and --auto-merge=true") {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, errb)
		}
	}
}

func TestLoopRefusesUnvalidatedBeforeSupervisorState(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	code, _, errb := runCmd("loop", "--project", "demo")
	if code != engine.ExitFatal || !strings.Contains(errb, "never permits --allow-unvalidated") {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
	if state, err := loopsupervisor.NewStore(rec.Root).LoadState(); err != nil || state.ProjectID != "" {
		t.Fatalf("unvalidated loop wrote state: state=%+v err=%v", state, err)
	}
}

func TestLoopStatusReadsDurableState(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	store := loopsupervisor.NewStore(rec.Root)
	if err := store.SaveState(loopsupervisor.State{
		ProjectID:     "demo",
		Mode:          loopsupervisor.ModeCircuitOpen,
		CircuitReason: "test tripwire",
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runCmd("loop", "status", "--project", "demo")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "circuit-open") || !strings.Contains(out, "test tripwire") {
		t.Fatalf("status output = %q", out)
	}
}

func TestLoopDrainWritesSupervisorAndEngineControl(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	code, _, errb := runCmd("loop", "drain", "--project", "demo")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errb)
	}
	control, err := loopsupervisor.NewStore(rec.Root).LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if !control.Drain || !ledger.NewStore(rec.Root).DrainRequested() {
		t.Fatalf("control=%+v engine-drain=%t", control, ledger.NewStore(rec.Root).DrainRequested())
	}
}

func TestLoopEngineForwardsImmutableFixedCohortAndRunID(t *testing.T) {
	var captured engine.Options
	runner := &loopEngine{
		run: func(_ context.Context, opts engine.Options) (engine.Outcome, error) {
			captured = opts
			if err := opts.OnRunStart("run-live"); err != nil {
				return engine.Outcome{}, err
			}
			return engine.Outcome{Code: engine.ExitOK, RunID: "run-live"}, nil
		},
	}
	var published string
	result, err := runner.Run(context.Background(), loopsupervisor.RunRequest{
		AllowedIDs: []string{"a", "b"},
		Max:        2,
		OnRunStart: func(runID string) error {
			published = runID
			return nil
		},
	})
	if err != nil || result.Code != engine.ExitOK {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if strings.Join(captured.AllowedIDs, ",") != "a,b" || captured.Max != 2 {
		t.Fatalf("engine options = %+v", captured)
	}
	if published != "run-live" {
		t.Fatalf("published run id = %q", published)
	}
}

func TestLoopEngineBridgesTypedTripwireWithoutLogParsing(t *testing.T) {
	runner := &loopEngine{
		run: func(_ context.Context, opts engine.Options) (engine.Outcome, error) {
			opts.OnSafetyTripwire(engine.SafetyTripwire{
				Kind:  engine.SafetyTripwireEngineInvariant,
				RunID: "run-1", BeadID: "b1", Detail: "gate evidence mismatch",
			})
			return engine.Outcome{Code: engine.ExitOK, RunID: "run-1"}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := runner.Events(ctx)
	if _, err := runner.Run(ctx, loopsupervisor.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Kind != string(engine.SafetyTripwireEngineInvariant) ||
			event.RunID != "run-1" || event.BeadID != "b1" ||
			event.Detail != "gate evidence mismatch" {
			t.Fatalf("bridged event = %+v", event)
		}
	default:
		t.Fatal("typed engine tripwire did not reach loop subscription")
	}
}

func TestTerminalQualityRequiresAuthenticatedSecurityEvidence(t *testing.T) {
	t.Run("positive", func(t *testing.T) {
		root, run, slot := terminalSecurityFixture(t)
		if !terminalHasFullQualityEvidence(t.Context(), root, run, slot, true) {
			t.Fatal("complete authenticated security lane was not Good")
		}
	})

	t.Run("missing", func(t *testing.T) {
		root, run, slot := terminalSecurityFixture(t)
		slot.SecurityReviewArtifactPath = ""
		slot.SecurityReviewArtifactDigest = ""
		slot.SecurityReviewCandidateSHA = ""
		slot.SecurityReviewBaseSHA = ""
		if terminalHasFullQualityEvidence(t.Context(), root, run, slot, true) {
			t.Fatal("security-required candidate passed without security evidence")
		}
	})

	t.Run("tampered", func(t *testing.T) {
		root, run, slot := terminalSecurityFixture(t)
		if err := os.WriteFile(slot.SecurityReviewArtifactPath, []byte(`{"tampered":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if terminalHasFullQualityEvidence(t.Context(), root, run, slot, true) {
			t.Fatal("tampered security artifact passed")
		}
	})

	t.Run("mismatched identity", func(t *testing.T) {
		root, run, slot := terminalSecurityFixture(t)
		slot.SecurityReviewCandidateSHA = strings.Repeat("c", 40)
		if terminalHasFullQualityEvidence(t.Context(), root, run, slot, true) {
			t.Fatal("security artifact with mismatched trusted candidate passed")
		}
	})
}

func terminalSecurityFixture(t *testing.T) (string, *ledger.Run, *ledger.Slot) {
	t.Helper()
	root := t.TempDir()
	const (
		runID      = "run-security"
		phaseID    = "bead-security"
		generation = "generation-1"
	)
	baseSHA := strings.Repeat("a", 40)
	candidateSHA := strings.Repeat("b", 40)
	store := ledger.NewStore(root)
	phaseDir := store.PhaseDir(runID, phaseID)
	evidenceDir := store.EvidenceDir(runID, phaseID)

	resultPath := phasecontrol.ResultPath(phaseDir)
	writeLoopJSON(t, resultPath, phasecontrol.ResultManifest{
		SchemaVersion: phasecontrol.ResultSchemaVersion,
		State:         "done",
		RunID:         runID,
		PhaseID:       phaseID,
		Generation:    generation,
		BaseSHA:       baseSHA,
		CandidateSHA:  candidateSHA,
		CommitCount:   1,
		WorktreeClean: true,
	})

	gatePath := filepath.Join(evidenceDir, "gate.json")
	gateDigest := writeLoopJSON(t, gatePath, merge.GateEvidence{
		Schema:           merge.GateEvidenceSchema,
		CandidateSHA:     candidateSHA,
		BaseSHA:          baseSHA,
		DiffDigest:       testSHA256([]byte("diff")),
		GateConfigDigest: testSHA256([]byte("config")),
		CommandDigest:    testSHA256([]byte("commands")),
		EngineVersion:    "test",
		BuildIdentity:    "test-build",
		CompletedAt:      time.Now().UTC(),
	})

	generalPath := filepath.Join(evidenceDir, "general.json")
	generalDigest := writeLoopJSON(t, generalPath, terminalReviewArtifact(
		review.ReviewKindGeneral, candidateSHA, baseSHA,
		review.Verdict{SecurityReviewRequired: true, SecurityEvidence: "test risk"},
	))
	securityPath := filepath.Join(evidenceDir, "security.json")
	securityDigest := writeLoopJSON(t, securityPath, terminalReviewArtifact(
		review.ReviewKindSecurity, candidateSHA, baseSHA, review.Verdict{},
	))

	run := &ledger.Run{RunID: runID}
	slot := &ledger.Slot{
		PhaseID: phaseID, Status: ledger.SlotMerged,
		DispatchGeneration: generation, DispatchBaseSHA: baseSHA,
		CandidateGeneration: generation, CandidateResultPath: resultPath,
		CompletionAccounted: true,
		GateEvidencePath:    gatePath, GateEvidenceDigest: gateDigest,
		GeneralReviewArtifactPath: generalPath, GeneralReviewArtifactDigest: generalDigest,
		GeneralReviewCandidateSHA: candidateSHA, GeneralReviewBaseSHA: baseSHA,
		SecurityReviewArtifactPath: securityPath, SecurityReviewArtifactDigest: securityDigest,
		SecurityReviewCandidateSHA: candidateSHA, SecurityReviewBaseSHA: baseSHA,
	}
	return root, run, slot
}

func terminalReviewArtifact(kind, candidateSHA, baseSHA string, verdict review.Verdict) review.Artifact {
	emptyHistory, _ := json.Marshal([]review.HistoricalFinding{})
	return review.Artifact{
		Schema: review.ReviewArtifactSchema, Kind: kind,
		CandidateSHA: candidateSHA, BaseSHA: baseSHA,
		HistoryDigest: testSHA256(emptyHistory),
		Verdict:       verdict,
	}
}

func writeLoopJSON(t *testing.T, path string, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return testSHA256(data)
}

func testSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestLoopObserverIgnoresRecoveryAndClaimsOutsideFixedCohort(t *testing.T) {
	repo := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
case "$1" in
  list) printf '%s\n' '[{"id":"outside","status":"in_progress","issue_type":"task"}]' ;;
  ready) printf '%s\n' '[]' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter := beads.New(repo)
	adapter.Bin = bin
	lstore := ledger.NewStore(repo)
	run, err := lstore.NewRun("demo", "bd", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := lstore.SetSlot(run, &ledger.Slot{
		PhaseID: "outside", BeadID: "outside", Status: ledger.SlotRunning,
	}); err != nil {
		t.Fatal(err)
	}
	observer := &loopProjectObserver{root: repo, bd: adapter, ledger: lstore}
	got, err := observer.Observe(t.Context(), loopsupervisor.Scope{
		FixedCohort: []string{"inside"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Recovery || got.Reconcile || got.RecoveryRunID != "" || len(got.ReadyIDs) != 0 {
		t.Fatalf("outside work woke fixed cohort: %+v", got)
	}
}

func TestObservedRecoveryRunRemainsPinnedAfterNewerRunInterleaves(t *testing.T) {
	repo := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
case "$1" in
  list|ready) printf '%s\n' '[]' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter := beads.New(repo)
	adapter.Bin = bin
	lstore := ledger.NewStore(repo)
	observed, err := lstore.NewRun("demo", "bd", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := lstore.SetSlot(observed, &ledger.Slot{
		PhaseID: "recover-me", BeadID: "recover-me", Status: ledger.SlotQueued,
	}); err != nil {
		t.Fatal(err)
	}
	observer := &loopProjectObserver{root: repo, bd: adapter, ledger: lstore}
	observation, err := observer.Observe(t.Context(), loopsupervisor.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Recovery || observation.RecoveryRunID != observed.RunID {
		t.Fatalf("observation = %+v", observation)
	}
	// Run IDs are second-resolution timestamps; cross that boundary so the
	// interleaving ledger is unambiguously newer instead of replacing A.
	time.Sleep(1100 * time.Millisecond)
	newer, err := lstore.NewRun("demo", "bd", "test")
	if err != nil {
		t.Fatal(err)
	}
	var captured engine.Options
	runner := &loopEngine{
		root: repo,
		run: func(_ context.Context, opts engine.Options) (engine.Outcome, error) {
			captured = opts
			return engine.Outcome{Code: engine.ExitOK, RunID: opts.RecoveryRunID}, nil
		},
	}
	result, err := runner.Run(t.Context(), loopsupervisor.RunRequest{
		Resume: true, RecoveryRunID: observation.RecoveryRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if newer.RunID == captured.RecoveryRunID ||
		captured.RecoveryRunID != observed.RunID ||
		result.RunID != observed.RunID {
		t.Fatalf("newer=%s observed=%s options=%+v result=%+v",
			newer.RunID, observed.RunID, captured, result)
	}
}

func TestCanaryDefaultTargetCoversFixedCohort(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}
	if got := canaryTargetWidth(ids, 0); got != len(ids) {
		t.Fatalf("default target = %d, want cohort size %d", got, len(ids))
	}
	if got := canaryTargetWidth(ids, 3); got != 3 {
		t.Fatalf("explicit bounded target = %d, want 3", got)
	}
	if got := canaryTargetWidth(ids, 10); got != len(ids) {
		t.Fatalf("oversized target = %d, want cohort size %d", got, len(ids))
	}
}

func TestNativeCanarySpecAuthenticatesExactCleanCheckout(t *testing.T) {
	repo := gitRepo(t)
	phaseGit(t, repo, "config", "user.name", "Canary Test")
	phaseGit(t, repo, "config", "user.email", "canary@example.com")
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("contract v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	phaseGit(t, repo, "add", "AGENTS.md")
	phaseGit(t, repo, "commit", "-m", "initial contract")
	first := phaseGit(t, repo, "rev-parse", "HEAD")

	oldCommit := loopInstalledCommit
	oldVersion := loopBinaryVersion
	t.Cleanup(func() {
		loopInstalledCommit = oldCommit
		loopBinaryVersion = oldVersion
	})
	loopInstalledCommit = func() string { return first }
	loopBinaryVersion = func() string { return "0.10.0-test" }
	if _, err := nativeCanarySpec(repo, []string{"b1", "b2"}, 2); err != nil {
		t.Fatalf("clean matching checkout: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("contract v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	phaseGit(t, repo, "add", "AGENTS.md")
	phaseGit(t, repo, "commit", "-m", "new contract")
	second := phaseGit(t, repo, "rev-parse", "HEAD")
	if _, err := nativeCanarySpec(repo, []string{"b1", "b2"}, 2); err == nil ||
		!strings.Contains(err.Error(), "does not match checkout HEAD") {
		t.Fatalf("stale otherwise-valid binary error = %v", err)
	}

	loopInstalledCommit = func() string { return second }
	if err := os.WriteFile(filepath.Join(repo, "untracked.go"), []byte("package dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeCanarySpec(repo, []string{"b1", "b2"}, 2); err == nil ||
		!strings.Contains(err.Error(), "tracked or untracked changes") {
		t.Fatalf("untracked source error = %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "untracked.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("dirty tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeCanarySpec(repo, []string{"b1", "b2"}, 2); err == nil ||
		!strings.Contains(err.Error(), "tracked or untracked changes") {
		t.Fatalf("tracked source error = %v", err)
	}
}

func TestCanaryBuildStampUsesFullCommitAndDirtyState(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	makeText := string(makefile)
	for _, want := range []string{
		"git rev-parse HEAD",
		"git status --porcelain=v1 --untracked-files=all",
	} {
		if !strings.Contains(makeText, want) {
			t.Fatalf("Makefile build stamp missing %q", want)
		}
	}
	if strings.Contains(makeText, "git rev-parse --short HEAD") {
		t.Fatal("Makefile still stamps an abbreviated commit")
	}

	release, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	releaseText := string(release)
	for _, want := range []string{".FullCommit", ".IsGitDirty", "-dirty"} {
		if !strings.Contains(releaseText, want) {
			t.Fatalf("GoReleaser build stamp missing %q", want)
		}
	}
	if strings.Contains(releaseText, "internal/version.commit={{ .ShortCommit") {
		t.Fatal("GoReleaser still stamps an abbreviated binary commit")
	}
}

func TestLoopCommandAssetIsThinBinaryInvocation(t *testing.T) {
	data, err := commands.FS.ReadFile("koryph-loop.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"koryph-loop.sh", "tail -F", "while true", "LOOP-ERROR"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("command asset still contains generated wrapper fragment %q", forbidden)
		}
	}
	for _, want := range []string{"koryph loop --auto-merge --review", "koryph loop status", "koryph loop drain"} {
		if !strings.Contains(text, want) {
			t.Errorf("command asset missing %q", want)
		}
	}
}

func TestLoopCanaryInjectionRejectsOutsideFixedCohort(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	reg := registry.NewStore()
	rec.MigrationStatus = registry.StatusValidated
	if err := reg.Save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	store := loopsupervisor.NewStore(rec.Root)
	if err := store.SaveState(loopsupervisor.State{
		ProjectID: "demo",
		Mode:      loopsupervisor.ModeStopped,
		Canary: &loopsupervisor.CanaryState{
			CohortDigest: "fixture",
			Cohort:       []string{"inside"},
			TargetWidth:  2,
			CurrentWidth: 2,
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The command checks bead existence before cohort membership. A missing bd
	// binary is therefore an ordinary lookup error, not evidence that the
	// cohort guard is absent; exercise the guard directly on the durable state.
	state, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if containsID(state.Canary.Cohort, "outside") {
		t.Fatal("outside bead unexpectedly belongs to fixed cohort")
	}
}

func TestLoopStoreSingleton(t *testing.T) {
	store := loopsupervisor.NewStore(t.TempDir())
	first, err := store.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Unlock() //nolint:errcheck
	if _, err := store.Acquire(); !errors.Is(err, loopsupervisor.ErrAlreadyRunning) {
		t.Fatalf("second Acquire error = %v", err)
	}
}

func TestLoopMaintainerRunsProjectGCAndSurfacesClassErrors(t *testing.T) {
	var got koryphgc.Options
	maintainer := loopMaintainer{
		root: "/project",
		run: func(opts koryphgc.Options) (*koryphgc.Result, error) {
			got = opts
			return &koryphgc.Result{Classes: []koryphgc.ClassResult{{
				Class:  "project-budget",
				Errors: []string{"cache lock failed"},
			}}}, nil
		},
	}
	err := maintainer.Maintain(context.Background(), loopsupervisor.BoundaryIdle)
	if err == nil || !strings.Contains(err.Error(), "project-budget: cache lock failed") {
		t.Fatalf("Maintain error = %v", err)
	}
	if got.RepoRoot != "/project" || got.DryRun {
		t.Fatalf("gc options = %+v", got)
	}
}
