// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/commandguard"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/resmon"
	"github.com/koryph/koryph/internal/review"
	"github.com/koryph/koryph/internal/runtime"
)

func installTestGateEvidence(
	t *testing.T,
	r *runner,
	sl *ledger.Slot,
	evidence merge.GateEvidence,
) {
	t.Helper()
	generation := generationForStage(sl, finalizationGate)
	generation.candidateSHA = evidence.CandidateSHA
	generation.baseSHA = evidence.BaseSHA
	path := r.gateEvidencePathFor(sl, generation)
	raw, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	sl.GateEvidencePath = path
	sl.GateEvidenceDigest = fmt.Sprintf("sha256:%x", sum[:])
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatal(err)
	}
}

func TestMovedBasesRevalidateWithoutModelAndIdenticalTargetTripsCircuit(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.cfg = &project.Config{}
	r.opts = Options{ProjectID: "proj"}

	lane := &finalizationLane{}
	lane.gates.ready = sync.NewCond(&lane.gates.mu)
	r.finalizer = lane
	stale := merge.Result{
		Status:     merge.StatusEvidenceStale,
		GateOutput: "default branch moved after validation",
	}

	if !r.handleMergeFailure(t.Context(), sl, stale) {
		t.Fatal("first moved base must enter engine-owned revalidation")
	}
	if got := r.run.Slots[sl.PhaseID]; got.Retry.MergeRevalidations != 1 ||
		got.LastRevalidationKey == "" || got.Status != ledger.SlotMerging {
		t.Fatalf("first revalidation slot = %+v", got)
	}

	writeFile(t, r.rec.Root+"/second-base.txt", "second\n", 0o644)
	runGit(t, r.rec.Root, "add", "second-base.txt")
	runGit(t, r.rec.Root, "commit", "--no-verify", "-m", "chore(test): second base")
	if !r.handleMergeFailure(t.Context(), sl, stale) {
		t.Fatal("a distinct moved base must remain schedulable")
	}
	got := r.run.Slots[sl.PhaseID]
	if got.Retry.MergeRevalidations != 2 || got.Status != ledger.SlotMerging {
		t.Fatalf("second revalidation slot = %+v", got)
	}
	if len(lane.gates.queue) != 2 {
		t.Fatalf("queued validations = %d, want 2", len(lane.gates.queue))
	}

	if r.handleMergeFailure(t.Context(), sl, stale) {
		t.Fatal("an identical repeated target must trip the invariant circuit")
	}
	got = r.run.Slots[sl.PhaseID]
	if got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeEngineInvariant) ||
		got.Retry.MergeRevalidations != 2 {
		t.Fatalf("repeated-target slot = %+v", got)
	}
	if r.dispatched != 0 {
		t.Fatalf("model dispatches = %d, want 0", r.dispatched)
	}
}

func TestDuplicateBroadCommandAlwaysBlocksCandidateAndOnlyExplicitlyTripsCircuit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit bool
	}{
		{name: "canonical slot-local"},
		{name: "explicit hard stop", explicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, sl, _ := candidateFixture(t)
			r.adapter = &fakeSource{}
			r.reg = registry.NewStore()
			r.opts.NativeCanary = true
			if tc.explicit {
				r.hardStopKinds = safetyTripwireSet([]SafetyTripwireKind{
					SafetyTripwireDuplicateCommand,
				})
			}
			sibling := &ledger.Slot{
				PhaseID: "sibling", BeadID: "sibling", Status: ledger.SlotRunning,
			}
			if err := r.store.SetSlot(r.run, sibling); err != nil {
				t.Fatal(err)
			}
			commandDir := filepath.Join(
				r.store.PhaseDir(r.run.RunID, sl.PhaseID), ".koryph-command",
			)
			if err := os.MkdirAll(commandDir, 0o700); err != nil {
				t.Fatal(err)
			}
			event := `{"schema":"koryph.command-event/v1","event":"start",` +
				`"at":"2026-07-25T12:00:01Z","class":"broad","signature":"same"}`
			writeFile(t, filepath.Join(commandDir, "events.jsonl"), event+"\n"+event+"\n", 0o600)
			var events []SafetyTripwire
			r.opts.OnSafetyTripwire = func(event SafetyTripwire) {
				events = append(events, event)
			}

			r.finishAssessedCandidate(t.Context(), sl, candidateAssessment{
				eligible: true,
				outcome:  OutcomeCandidateReady,
			})

			if got := r.run.Slots[sl.PhaseID]; got == nil ||
				got.Status != ledger.SlotBlocked ||
				got.OutcomeClass != string(OutcomeCodeDefect) {
				t.Fatalf("duplicate candidate = %+v, want blocked code defect", got)
			}
			if tc.explicit {
				if len(events) != 1 || events[0].Kind != SafetyTripwireDuplicateCommand ||
					!r.safetyTripwireFired || r.dispatchCircuitReason == "" {
					t.Fatalf("explicit duplicate hard stop = events=%+v fired=%t reason=%q",
						events, r.safetyTripwireFired, r.dispatchCircuitReason)
				}
			} else {
				if len(events) != 0 || r.safetyTripwireFired || r.dispatchCircuitReason != "" {
					t.Fatalf("slot-local duplicate opened circuit: events=%+v fired=%t reason=%q",
						events, r.safetyTripwireFired, r.dispatchCircuitReason)
				}
				if got := r.run.Slots[sibling.PhaseID]; got == nil ||
					got.Status != ledger.SlotRunning {
					t.Fatalf("sibling = %+v, want still running", got)
				}
			}
		})
	}
}

func TestEngineValidationSharesWorkerProcessGuardAndStartsGateOnce(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "guarded.txt"), "guarded\n", 0o644)
	runGit(t, wt, "add", "guarded.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): guarded")
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts = Options{ProjectID: "proj"}
	realKoryphRoot, err := filepath.EvalSymlinks(r.store.KoryphRoot)
	if err != nil {
		t.Fatal(err)
	}
	r.store.KoryphRoot = realKoryphRoot

	fakeBin := t.TempDir()
	starts := filepath.Join(t.TempDir(), "starts")
	makePath := filepath.Join(fakeBin, "make")
	writeFile(t, makePath,
		fmt.Sprintf("#!/bin/sh\necho start >>%q\nsleep 1\nexit 0\n", starts), 0o755)
	r.cfg = &project.Config{Gate: []string{makePath + " gate-agent"}}

	opts, err := r.qualityMergeOpts(sl)
	if err != nil {
		t.Fatal(err)
	}
	phaseDir := r.store.PhaseDir(r.run.RunID, sl.PhaseID)
	if opts.ValidationPhaseDir != phaseDir {
		t.Fatalf("validation guard dir = %q, want worker phase %q",
			opts.ValidationPhaseDir, phaseDir)
	}
	opts.ValidateOnly = true
	opts.EvidencePath = r.gateEvidencePathFor(sl, generationForStage(sl, finalizationGate))
	opts.SlotOwner = "test-validator"
	opts.SlotRetries = 1
	opts.Slot = r.slotLocker(t.Context())

	type outcome struct {
		result merge.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := merge.Merge(t.Context(), opts)
		done <- outcome{result: result, err: err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(starts); err == nil &&
			strings.Contains(string(data), "start") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for engine validation gate start")
		}
		time.Sleep(20 * time.Millisecond)
	}

	workerProcess := exec.Command("/bin/sleep", "5")
	workerProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := workerProcess.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := resmon.CurrentCommandIdentity(t.Context(), workerProcess.Process.Pid)
	if err != nil {
		_ = workerProcess.Process.Signal(syscall.SIGTERM)
		_ = workerProcess.Wait()
		t.Fatal(err)
	}
	guard := commandguard.NewProcessGuard(phaseDir)
	reuse, err := guard.Acquire(t.Context(), commandguard.ProcessGuardRequest{
		Argv: []string{"make", "gate"}, Role: commandguard.CommandRoleWorker,
		Identity: identity,
	})
	_ = workerProcess.Process.Signal(syscall.SIGTERM)
	_ = workerProcess.Wait()
	if err != nil || reuse.Action != commandguard.ProcessGuardReuse ||
		reuse.Existing.Role != commandguard.CommandRoleValidation {
		t.Fatalf("worker guard reuse = (%+v, %v)", reuse, err)
	}
	reused, err := guard.Wait(t.Context(), reuse)
	if err != nil || reused.ExitCode != 0 {
		t.Fatalf("worker reused validation = (%+v, %v)", reused, err)
	}
	got := <-done
	if got.err != nil || got.result.Status != merge.StatusValidated {
		t.Fatalf("engine validation = (%+v, %v)", got.result, got.err)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "start") != 1 {
		t.Fatalf("actual gate starts = %q, %v; want one", data, err)
	}
}

func TestExhaustedGateAndLandingInfrastructureNeverDispatchModels(t *testing.T) {
	for _, stage := range []string{finalizationGate, finalizationMerge, finalizationPR} {
		t.Run(stage, func(t *testing.T) {
			r, sl, _ := candidateFixture(t)
			r.adapter = &fakeSource{}
			r.reg = registry.NewStore()
			r.opts = Options{ProjectID: "proj"}
			sl.Status = ledger.SlotMerging
			sl.FinalizationStage = stage
			if stage == finalizationGate {
				r.applyGateResult(t.Context(), sl, merge.Result{}, fmt.Errorf("fetch failed"))
			} else if stage == finalizationPR {
				r.applyPRResult(t.Context(), sl, merge.Result{}, fmt.Errorf("push failed"))
			} else {
				r.applyMergeResult(t.Context(), sl, merge.Result{Status: merge.StatusMerged}, fmt.Errorf("push failed"))
			}
			if r.dispatched != 0 {
				t.Fatalf("model dispatches = %d, want zero", r.dispatched)
			}
			got := r.run.Slots[sl.PhaseID]
			if got.Status != ledger.SlotBlocked ||
				got.OutcomeClass != string(OutcomeRuntimeTransient) {
				t.Fatalf("infrastructure exhaustion slot = %+v", got)
			}
		})
	}
}

func TestActualValidationGuardInfrastructureRetriesWithoutModel(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "guard-infra.txt"), "candidate\n", 0o644)
	runGit(t, wt, "add", "guard-infra.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): guard infra")
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts = Options{ProjectID: "proj"}

	// ProcessGuard deliberately rejects any symlink component. Point the
	// otherwise-real worker phase namespace through one to exercise a genuine
	// guard infrastructure error from merge.runGateCommand, not a mocked
	// mergeFn error.
	realRoot, err := filepath.EvalSymlinks(r.store.KoryphRoot)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "ledger-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	r.store.KoryphRoot = alias

	starts := filepath.Join(t.TempDir(), "starts")
	makePath := filepath.Join(t.TempDir(), "make")
	writeFile(t, makePath,
		fmt.Sprintf("#!/bin/sh\necho start >>%q\nexit 0\n", starts), 0o755)
	r.cfg = &project.Config{Gate: []string{makePath + " gate-agent"}}
	opts, err := r.qualityMergeOpts(sl)
	if err != nil {
		t.Fatal(err)
	}
	opts.ValidateOnly = true
	opts.EvidencePath = r.gateEvidencePathFor(sl, generationForStage(sl, finalizationGate))
	opts.SlotOwner = "test-validator"
	opts.SlotRetries = 1
	opts.Slot = r.slotLocker(t.Context())
	sl.Status = ledger.SlotMerging
	sl.FinalizationStage = finalizationGate
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatal(err)
	}

	oldBackoff := finalizationRetryBackoff
	finalizationRetryBackoff = time.Millisecond
	t.Cleanup(func() { finalizationRetryBackoff = oldBackoff })
	lane := &finalizationLane{ctx: t.Context(), mergeFn: merge.Merge}
	result := lane.execute(finalizationJob{
		generation: generationForStage(sl, finalizationGate),
		stage:      finalizationGate,
		mergeOpts:  opts,
	})
	if result.err == nil || result.infraTries != finalizationInfraAttempts ||
		!strings.Contains(result.err.Error(), "prepare validation owner lease") {
		t.Fatalf("actual guard result = %+v, want %d bounded infrastructure attempts",
			result, finalizationInfraAttempts)
	}
	r.applyFinalizationResult(t.Context(), result)
	if r.dispatched != 0 {
		t.Fatalf("model dispatches = %d, want zero", r.dispatched)
	}
	if got := r.run.Slots[sl.PhaseID]; got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeRuntimeTransient) {
		t.Fatalf("guard infrastructure exhaustion slot = %+v", got)
	}
	if data, err := os.ReadFile(starts); err == nil && len(data) > 0 {
		t.Fatalf("real gate started despite guard failure: %q", data)
	}
}

func TestPostGateEvidencePersistenceRetriesWithoutRegatingOrModel(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "post-gate.txt"), "candidate\n", 0o644)
	runGit(t, wt, "add", "post-gate.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): post gate")
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts = Options{ProjectID: "proj"}
	realKoryphRoot, err := filepath.EvalSymlinks(r.store.KoryphRoot)
	if err != nil {
		t.Fatal(err)
	}
	r.store.KoryphRoot = realKoryphRoot

	starts := filepath.Join(t.TempDir(), "starts")
	makePath := filepath.Join(t.TempDir(), "make")
	writeFile(t, makePath,
		fmt.Sprintf("#!/bin/sh\necho start >>%q\nexit 0\n", starts), 0o755)
	r.cfg = &project.Config{Gate: []string{makePath + " gate-agent"}}
	opts, err := r.qualityMergeOpts(sl)
	if err != nil {
		t.Fatal(err)
	}
	opts.ValidateOnly = true
	opts.EvidencePath = r.gateEvidencePathFor(sl, generationForStage(sl, finalizationGate))
	// A directory at the final evidence filename produces a genuine atomic
	// persistence error after the gate has succeeded.
	if err := os.MkdirAll(opts.EvidencePath, 0o700); err != nil {
		t.Fatal(err)
	}
	opts.SlotOwner = "test-validator"
	opts.SlotRetries = 1
	opts.Slot = r.slotLocker(t.Context())
	sl.Status = ledger.SlotMerging
	sl.FinalizationStage = finalizationGate
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatal(err)
	}

	oldBackoff := finalizationRetryBackoff
	finalizationRetryBackoff = time.Millisecond
	t.Cleanup(func() { finalizationRetryBackoff = oldBackoff })
	lane := &finalizationLane{ctx: t.Context(), mergeFn: merge.Merge}
	result := lane.execute(finalizationJob{
		generation: generationForStage(sl, finalizationGate),
		stage:      finalizationGate,
		mergeOpts:  opts,
	})
	if result.err == nil || result.infraTries != finalizationInfraAttempts ||
		!strings.Contains(result.err.Error(), "persist gate evidence") {
		t.Fatalf("post-gate persistence result = %+v", result)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "start") != 1 {
		t.Fatalf("authoritative gate starts = %q, %v; want exactly one", data, err)
	}
	r.applyFinalizationResult(t.Context(), result)
	if r.dispatched != 0 {
		t.Fatalf("model dispatches = %d, want zero", r.dispatched)
	}
	if got := r.run.Slots[sl.PhaseID]; got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeRuntimeTransient) {
		t.Fatalf("post-gate infrastructure exhaustion slot = %+v", got)
	}
}

func TestPartialPushAdvancedOriginRevalidatesWithoutModel(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "partial.txt"), "candidate\n", 0o644)
	runGit(t, wt, "add", "partial.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): partial push")
	originalCandidate := strings.TrimSpace(runGit(t, wt, "rev-parse", "HEAD"))
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts = Options{ProjectID: "proj", AutoMerge: true}

	remoteRoot := t.TempDir()
	runGit(t, remoteRoot, "init", "--bare", "-q", "-b", "main", "origin.git")
	bare := filepath.Join(remoteRoot, "origin.git")
	runGit(t, r.rec.Root, "remote", "add", "origin", bare)
	runGit(t, r.rec.Root, "push", "-q", "-u", "origin", "main")

	starts := filepath.Join(t.TempDir(), "gate-starts")
	r.cfg = &project.Config{
		Gate:        []string{fmt.Sprintf("echo gate >>%q", starts)},
		MergePolicy: project.PolicyAuto,
	}
	hook := filepath.Join(bare, "hooks", "pre-receive")
	writeFile(t, hook, "#!/bin/sh\necho push unavailable >&2\nexit 1\n", 0o755)

	oldBackoff := finalizationRetryBackoff
	finalizationRetryBackoff = time.Millisecond
	t.Cleanup(func() { finalizationRetryBackoff = oldBackoff })
	r.validateSlot(t.Context(), sl)
	got := r.run.Slots[sl.PhaseID]
	if got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeRuntimeTransient) ||
		got.FinalizationStage != finalizationMerge {
		t.Fatalf("partial-push exhaustion slot = %+v", got)
	}
	if main := strings.TrimSpace(runGit(t, r.rec.Root, "rev-parse", "main")); main != originalCandidate {
		t.Fatalf("local default after partial push = %s, want candidate %s", main, originalCandidate)
	}
	if r.dispatched != 0 {
		t.Fatalf("model dispatches after partial push = %d, want zero", r.dispatched)
	}

	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	externalRoot := t.TempDir()
	runGit(t, externalRoot, "clone", "-q", bare, "external")
	external := filepath.Join(externalRoot, "external")
	runGit(t, external, "config", "user.email", "external@example.com")
	runGit(t, external, "config", "user.name", "External Writer")
	runGit(t, external, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(external, "remote.txt"), "remote\n", 0o644)
	runGit(t, external, "add", "remote.txt")
	runGit(t, external, "commit", "--no-verify", "-m", "chore(test): advance origin")
	advancedRemote := strings.TrimSpace(runGit(t, external, "rev-parse", "HEAD"))
	runGit(t, external, "push", "-q", "origin", "main")

	r.resumeFinalization(t.Context(), got)
	got = r.run.Slots[sl.PhaseID]
	if got.Status != ledger.SlotMerged {
		t.Fatalf("advanced-origin recovery slot = %+v", got)
	}
	if got.Retry.MergeRevalidations != 1 {
		t.Fatalf("merge revalidations = %d, want 1", got.Retry.MergeRevalidations)
	}
	if r.dispatched != 0 {
		t.Fatalf("model dispatches = %d, want zero", r.dispatched)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "gate") != 2 {
		t.Fatalf("gate starts = %q, %v; want one per old/new tuple", data, err)
	}
	localMain := strings.TrimSpace(runGit(t, r.rec.Root, "rev-parse", "main"))
	remoteMain := strings.Fields(runGit(t, r.rec.Root, "ls-remote", "origin", "refs/heads/main"))
	if len(remoteMain) == 0 || remoteMain[0] != localMain ||
		localMain == originalCandidate || localMain == advancedRemote {
		t.Fatalf("final refs local=%s remote=%v original=%s advanced=%s",
			localMain, remoteMain, originalCandidate, advancedRemote)
	}
}

func TestReviewRoutingUsesStandardGeneralAndFrontierOnlyForSecurity(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	r.cfg = &project.Config{}
	r.cfg.DefaultEquivalent = "frontier:xhigh"
	r.opts = Options{
		ProjectID: "proj", Review: true,
		DefaultModel: runtime.CodexModelMap[runtime.TierFrontier],
	}
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	sl.Runtime = "codex"
	writeFile(t, wt+"/candidate.txt", "candidate\n", 0o644)
	runGit(t, wt, "add", "candidate.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): add work")
	_ = completeCandidate(t, r, sl)
	candidate := strings.TrimSpace(runGit(t, sl.Worktree, "rev-parse", sl.Branch))
	base := strings.TrimSpace(runGit(t, r.rec.Root, "rev-parse", r.rec.DefaultBranch))
	buildIdentity, err := exactBuildIdentity()
	if err != nil {
		t.Fatal(err)
	}
	evidence := merge.GateEvidence{
		Schema: merge.GateEvidenceSchema, CandidateSHA: candidate, BaseSHA: base,
		DiffDigest:       "sha256:" + strings.Repeat("1", 64),
		GateConfigDigest: "sha256:" + strings.Repeat("2", 64),
		CommandDigest:    "sha256:" + strings.Repeat("3", 64), EngineVersion: EngineVersion,
		BuildIdentity: buildIdentity, CompletedAt: time.Now().UTC(),
	}
	installTestGateEvidence(t, r, sl, evidence)

	lane := &finalizationLane{}
	lane.reviews.ready = sync.NewCond(&lane.reviews.mu)
	r.finalizer = lane
	r.startReview(t.Context(), sl)
	r.startSecurityReview(t.Context(), sl, "test risk")

	if len(lane.reviews.queue) != 2 {
		t.Fatalf("queued reviews = %d, want general and security", len(lane.reviews.queue))
	}
	general, security := lane.reviews.queue[0], lane.reviews.queue[1]
	if general.stage != finalizationReview ||
		general.reviewOpts.Persona != "koryph-reviewer" ||
		general.reviewOpts.Model != runtime.CodexModelMap[runtime.TierStandard] {
		t.Fatalf("general review route = stage %q persona %q model %q",
			general.stage, general.reviewOpts.Persona, general.reviewOpts.Model)
	}
	if security.stage != finalizationSecurityReview ||
		security.reviewOpts.Persona != "koryph-security-reviewer" ||
		security.reviewOpts.Model != runtime.CodexModelMap[runtime.TierFrontier] {
		t.Fatalf("security review route = stage %q persona %q model %q",
			security.stage, security.reviewOpts.Persona, security.reviewOpts.Model)
	}
}

func TestFinalizationArtifactsAreGenerationSpecific(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	first := generationForStage(sl, finalizationGate)
	firstGate := r.gateEvidencePathFor(sl, first)

	sl.Retry.MergeRevalidations++
	second := generationForStage(sl, finalizationGate)
	secondGate := r.gateEvidencePathFor(sl, second)
	if firstGate == secondGate {
		t.Fatalf("gate artifact path reused across revalidation: %s", firstGate)
	}

}

func TestDegradedSecurityReviewFailsClosed(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts = Options{ProjectID: "proj"}
	sl.Status = ledger.SlotReview

	r.applyReviewResult(t.Context(), sl, review.Verdict{
		Degraded: true, Attempts: 2, Reason: "review runtime unavailable",
	}, true)

	got := r.run.Slots[sl.PhaseID]
	if got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeRuntimeTransient) ||
		!strings.Contains(got.Note, "security review degraded") {
		t.Fatalf("degraded security slot = %+v", got)
	}
}

func TestOnlyConfinedDigestMatchedReviewArtifactsArePromoted(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts = Options{ProjectID: "proj"}
	candidate := strings.TrimSpace(runGit(t, sl.Worktree, "rev-parse", sl.Branch))
	base := strings.TrimSpace(runGit(t, r.rec.Root, "rev-parse", r.rec.DefaultBranch))
	buildIdentity, err := exactBuildIdentity()
	if err != nil {
		t.Fatal(err)
	}
	installTestGateEvidence(t, r, sl, merge.GateEvidence{
		Schema: merge.GateEvidenceSchema, CandidateSHA: candidate, BaseSHA: base,
		DiffDigest:       "sha256:" + strings.Repeat("1", 64),
		GateConfigDigest: "sha256:" + strings.Repeat("2", 64),
		CommandDigest:    "sha256:" + strings.Repeat("3", 64),
		EngineVersion:    EngineVersion, BuildIdentity: buildIdentity,
		CompletedAt: time.Now().UTC(),
	})
	evidenceDir := r.finalizationEvidenceDir(sl)
	path := evidenceDir + "/general-review-test.json"
	emptyHistory := sha256.Sum256([]byte("[]"))
	artifact := review.Artifact{
		Schema: review.ReviewArtifactSchema, Kind: review.ReviewKindGeneral,
		CandidateSHA: candidate, BaseSHA: base,
		HistoryDigest: fmt.Sprintf("sha256:%x", emptyHistory[:]),
		History:       []review.HistoricalFinding{},
		Verdict:       review.Verdict{Blocking: false},
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	verdict := review.Verdict{
		ArtifactPath: path, ArtifactDigest: fmt.Sprintf("sha256:%x", sum[:]),
	}
	if !r.acceptReviewArtifact(t.Context(), sl, verdict, false) {
		t.Fatal("valid confined review artifact was rejected")
	}
	if got := r.run.Slots[sl.PhaseID].GeneralReviewArtifactPath; got != path {
		t.Fatalf("promoted review path = %q, want %q", got, path)
	}
	if got := r.run.Slots[sl.PhaseID]; got.GeneralReviewArtifactDigest == "" ||
		got.GeneralReviewCandidateSHA != candidate || got.GeneralReviewBaseSHA != base {
		t.Fatalf("promoted review identity = %+v", got)
	}

	verdict.ArtifactDigest = "sha256:wrong"
	if r.acceptReviewArtifact(t.Context(), sl, verdict, false) {
		t.Fatal("digest-mismatched review artifact was promoted")
	}
	if got := r.run.Slots[sl.PhaseID]; got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeEngineInvariant) {
		t.Fatalf("digest mismatch slot = %+v", got)
	}
}

func TestTamperedPriorReviewArtifactIsRejectedBeforeReuse(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.opts = Options{ProjectID: "proj"}
	candidate := strings.TrimSpace(runGit(t, sl.Worktree, "rev-parse", sl.Branch))
	base := strings.TrimSpace(runGit(t, r.rec.Root, "rev-parse", r.rec.DefaultBranch))
	path := r.finalizationEvidenceDir(sl) + "/prior.json"
	emptyHistory := sha256.Sum256([]byte("[]"))
	raw, err := json.Marshal(review.Artifact{
		Schema: review.ReviewArtifactSchema, Kind: review.ReviewKindGeneral,
		CandidateSHA: candidate, BaseSHA: base,
		HistoryDigest: fmt.Sprintf("sha256:%x", emptyHistory[:]),
		History:       []review.HistoricalFinding{},
		Verdict:       review.Verdict{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	sl.GeneralReviewArtifactPath = path
	sl.GeneralReviewArtifactDigest = fmt.Sprintf("sha256:%x", sum[:])
	sl.GeneralReviewCandidateSHA = candidate
	sl.GeneralReviewBaseSHA = base
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := r.authenticatedPriorReviewArtifact(t.Context(), sl, false); ok {
		t.Fatal("tampered prior review artifact was accepted")
	}
	if got := r.run.Slots[sl.PhaseID]; got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeEngineInvariant) {
		t.Fatalf("tampered prior slot = %+v", got)
	}
}

func TestTamperedGateEvidenceIsRejectedOnResumeAndLanding(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.cfg = &project.Config{}
	candidate := strings.TrimSpace(runGit(t, sl.Worktree, "rev-parse", sl.Branch))
	base := strings.TrimSpace(runGit(t, r.rec.Root, "rev-parse", r.rec.DefaultBranch))
	buildIdentity, err := exactBuildIdentity()
	if err != nil {
		t.Fatal(err)
	}
	evidence := merge.GateEvidence{
		Schema: merge.GateEvidenceSchema, CandidateSHA: candidate, BaseSHA: base,
		DiffDigest:       "sha256:" + strings.Repeat("1", 64),
		GateConfigDigest: "sha256:" + strings.Repeat("2", 64),
		CommandDigest:    "sha256:" + strings.Repeat("3", 64),
		EngineVersion:    EngineVersion, BuildIdentity: buildIdentity,
		CompletedAt: time.Now().UTC(),
	}
	installTestGateEvidence(t, r, sl, evidence)
	raw, err := os.ReadFile(sl.GateEvidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sl.GateEvidencePath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.resumeGateEvidence(t.Context(), sl); ok {
		t.Fatal("tampered gate evidence resumed")
	}
	if _, ok := r.evidenceForLanding(t.Context(), sl); ok {
		t.Fatal("tampered gate evidence admitted for landing")
	}
	if got := r.run.Slots[sl.PhaseID]; got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeEngineInvariant) {
		t.Fatalf("tampered gate slot = %+v", got)
	}
}

func TestFinalizationGenerationBindsCandidateBaseAndExactEvidence(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	candidate := strings.TrimSpace(runGit(t, sl.Worktree, "rev-parse", sl.Branch))
	base := strings.TrimSpace(runGit(t, r.rec.Root, "rev-parse", r.rec.DefaultBranch))
	buildIdentity, err := exactBuildIdentity()
	if err != nil {
		t.Fatal(err)
	}
	evidence := merge.GateEvidence{
		Schema: merge.GateEvidenceSchema, CandidateSHA: candidate, BaseSHA: base,
		DiffDigest:       "sha256:" + strings.Repeat("1", 64),
		GateConfigDigest: "sha256:" + strings.Repeat("2", 64),
		CommandDigest:    "sha256:" + strings.Repeat("3", 64),
		EngineVersion:    EngineVersion, BuildIdentity: buildIdentity,
		CompletedAt: time.Now().UTC(),
	}
	installTestGateEvidence(t, r, sl, evidence)
	generation := generationForEvidence(sl, &evidence, finalizationReview)
	if !r.finalizationEvidenceCurrent(t.Context(), sl, generation) {
		t.Fatal("exact finalization generation was rejected")
	}
	for _, mutate := range []func(*finalizationGeneration){
		func(g *finalizationGeneration) { g.candidateSHA = strings.Repeat("a", 40) },
		func(g *finalizationGeneration) { g.baseSHA = strings.Repeat("b", 40) },
		func(g *finalizationGeneration) { g.evidenceKey = "changed" },
		func(g *finalizationGeneration) { g.evidenceDigest = "sha256:changed" },
	} {
		changed := generation
		mutate(&changed)
		if r.finalizationEvidenceCurrent(t.Context(), sl, changed) {
			t.Fatalf("changed generation was accepted: %+v", changed)
		}
	}
}

func TestConfiguredHighRiskPathTriggersSecurityReview(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.cfg = &project.Config{Review: &project.ReviewConfig{
		HighRiskPaths: []string{"internal/payments/"},
	}}
	writeFile(t, wt+"/internal/payments/settle.go", "package payments\n", 0o644)
	runGit(t, wt, "add", "internal/payments/settle.go")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(payments): settle")
	candidate := strings.TrimSpace(runGit(t, wt, "rev-parse", sl.Branch))
	base := strings.TrimSpace(runGit(t, r.rec.Root, "rev-parse", r.rec.DefaultBranch))
	buildIdentity, err := exactBuildIdentity()
	if err != nil {
		t.Fatal(err)
	}
	installTestGateEvidence(t, r, sl, merge.GateEvidence{
		Schema: merge.GateEvidenceSchema, CandidateSHA: candidate, BaseSHA: base,
		DiffDigest:       "sha256:" + strings.Repeat("1", 64),
		GateConfigDigest: "sha256:" + strings.Repeat("2", 64),
		CommandDigest:    "sha256:" + strings.Repeat("3", 64),
		EngineVersion:    EngineVersion, BuildIdentity: buildIdentity,
		CompletedAt: time.Now().UTC(),
	})

	required, reason := r.securityReviewRequired(t.Context(), sl, review.Verdict{})
	if !required || !strings.Contains(reason, "internal/payments/settle.go") {
		t.Fatalf("security review = %v, %q", required, reason)
	}
}

func TestHighRiskPrefixMatchingUsesPathBoundaries(t *testing.T) {
	if !riskPathMatches("internal/auth/token.go", "internal/auth") {
		t.Fatal("exact directory prefix did not match")
	}
	if riskPathMatches("internal/author/profile.go", "internal/auth") {
		t.Fatal("textual prefix crossed a path boundary")
	}
}
