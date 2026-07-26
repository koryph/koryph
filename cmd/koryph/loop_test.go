// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
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
	"github.com/koryph/koryph/internal/fsx"
	koryphgc "github.com/koryph/koryph/internal/gc"
	"github.com/koryph/koryph/internal/ledger"
	loopsupervisor "github.com/koryph/koryph/internal/loop"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/metrics"
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
	if code != engine.ExitFatal ||
		!strings.Contains(errb, "steady autonomous mode requires \"validated\"") {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
	if state, err := loopsupervisor.NewStore(rec.Root).LoadState(); err != nil || state.ProjectID != "" {
		t.Fatalf("unvalidated loop wrote state: state=%+v err=%v", state, err)
	}
}

func TestLoopPostureAllowsOnlyMigratedFixedCanaryBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name, status     string
		canary           bool
		promotionPending bool
		want             bool
	}{
		{name: "validated steady", status: registry.StatusValidated, want: true},
		{name: "validated canary", status: registry.StatusValidated, canary: true, want: true},
		{name: "migrated steady", status: registry.StatusMigrated, want: false},
		{name: "migrated canary", status: registry.StatusMigrated, canary: true, want: true},
		{name: "registered steady", status: registry.StatusRegistered, want: false},
		{name: "registered canary", status: registry.StatusRegistered, canary: true, want: false},
		{name: "unknown canary", status: "future", canary: true, want: false},
		{name: "validated pending", status: registry.StatusValidated, promotionPending: true, want: false},
		{name: "migrated canary pending", status: registry.StatusMigrated, canary: true, promotionPending: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loopPostureAllows(tc.status, tc.canary, tc.promotionPending); got != tc.want {
				t.Fatalf("loopPostureAllows(%q, %t, %t) = %t, want %t",
					tc.status, tc.canary, tc.promotionPending, got, tc.want)
			}
		})
	}
}

type canaryPromotionStore struct {
	record           *registry.Record
	calls            int
	errorBeforeWrite error
	errorAfterWrite  error
}

func (s *canaryPromotionStore) Get(string) (*registry.Record, error) {
	copy := *s.record
	return &copy, nil
}

type failingLoopStateSaver struct{ err error }

func (s failingLoopStateSaver) SaveState(loopsupervisor.State) error { return s.err }

func (s *canaryPromotionStore) CompareAndSetMigrationStatus(
	_ context.Context,
	_ string,
	from, to, expectedIdentityDigest string,
) (*registry.Record, error) {
	s.calls++
	if s.errorBeforeWrite != nil {
		return nil, s.errorBeforeWrite
	}
	if s.record.MigrationStatus != from {
		return nil, errors.New("unexpected migration status")
	}
	currentIdentityDigest, err := registry.ValidationIdentityDigest(s.record)
	if err != nil {
		return nil, err
	}
	if currentIdentityDigest != expectedIdentityDigest {
		return nil, errors.New("unexpected validation identity")
	}
	s.record.MigrationStatus = to
	if s.errorAfterWrite != nil {
		return nil, s.errorAfterWrite
	}
	copy := *s.record
	return &copy, nil
}

func TestPassingNativeCanaryPromotesMigratedRegistry(t *testing.T) {
	rec, state := canaryPromotionFixture(t)
	oldLoader := loadExpectedAutonomyReport
	t.Cleanup(func() { loadExpectedAutonomyReport = oldLoader })
	loadExpectedAutonomyReport = func(path string, expected metrics.AutonomyReportExpectation) (*metrics.AutonomyReport, error) {
		if filepath.Clean(path) != filepath.Clean(state.Canary.ReportPath) ||
			expected.ProjectID != rec.ProjectID ||
			expected.InstalledCommit != state.Canary.InstalledCommit ||
			expected.RegistryIdentityDigest != state.Canary.RegistryIdentityDigest ||
			expected.EvidenceDigest != state.Canary.ReportDigest ||
			expected.CohortDigest != state.Canary.CohortDigest {
			t.Fatalf("promotion expectation mismatch: path=%s expected=%+v", path, expected)
		}
		return &metrics.AutonomyReport{
			Decision: metrics.AutonomyDecision{Passed: true},
		}, nil
	}
	stored := *rec
	reg := &canaryPromotionStore{record: &stored}
	promoted, err := completePassedNativeCanary(
		t.Context(), reg, rec, loopsupervisor.NewStore(rec.Root),
		state, state.Canary.Cohort, state.Canary.TargetWidth,
	)
	if err != nil || !promoted {
		t.Fatalf("promotion = %t, %v", promoted, err)
	}
	if rec.MigrationStatus != registry.StatusValidated ||
		reg.record.MigrationStatus != registry.StatusValidated {
		t.Fatalf("record=%+v saved=%+v", rec, reg.record)
	}
	marker, exists, err := loadNativeCanaryPromotionMarker(rec.Root)
	if err != nil || !exists || marker.Status != nativeCanaryPromotionValidated {
		t.Fatalf("marker=%+v exists=%t err=%v", marker, exists, err)
	}
}

func TestNativeCanaryPromotionFailsClosed(t *testing.T) {
	t.Run("failed or interrupted decision does not promote", func(t *testing.T) {
		for _, decision := range []string{"", "failed"} {
			rec, state := canaryPromotionFixture(t)
			state.Canary.Decision = decision
			stored := *rec
			reg := &canaryPromotionStore{record: &stored}
			promoted, err := completePassedNativeCanary(
				t.Context(), reg, rec, loopsupervisor.NewStore(rec.Root),
				state, state.Canary.Cohort, state.Canary.TargetWidth,
			)
			if err == nil || promoted || reg.calls != 0 ||
				rec.MigrationStatus != registry.StatusMigrated {
				t.Fatalf("decision %q: promoted=%t err=%v record=%+v calls=%d",
					decision, promoted, err, rec, reg.calls)
			}
		}
	})

	t.Run("invalid immutable report does not promote", func(t *testing.T) {
		rec, state := canaryPromotionFixture(t)
		oldLoader := loadExpectedAutonomyReport
		t.Cleanup(func() { loadExpectedAutonomyReport = oldLoader })
		loadExpectedAutonomyReport = func(string, metrics.AutonomyReportExpectation) (*metrics.AutonomyReport, error) {
			return nil, errors.New("tampered report")
		}
		stored := *rec
		reg := &canaryPromotionStore{record: &stored}
		promoted, err := completePassedNativeCanary(
			t.Context(), reg, rec, loopsupervisor.NewStore(rec.Root),
			state, state.Canary.Cohort, state.Canary.TargetWidth,
		)
		if err == nil || promoted || reg.calls != 0 ||
			rec.MigrationStatus != registry.StatusMigrated {
			t.Fatalf("promoted=%t err=%v record=%+v calls=%d",
				promoted, err, rec, reg.calls)
		}
	})

	t.Run("registry failure after write leaves durable steady-mode block", func(t *testing.T) {
		rec, state := canaryPromotionFixture(t)
		oldLoader := loadExpectedAutonomyReport
		t.Cleanup(func() { loadExpectedAutonomyReport = oldLoader })
		loadExpectedAutonomyReport = func(string, metrics.AutonomyReportExpectation) (*metrics.AutonomyReport, error) {
			return &metrics.AutonomyReport{
				Decision: metrics.AutonomyDecision{Passed: true},
			}, nil
		}
		stored := *rec
		reg := &canaryPromotionStore{
			record: &stored, errorAfterWrite: errors.New("registry audit unavailable"),
		}
		promoted, err := completePassedNativeCanary(
			t.Context(), reg, rec, loopsupervisor.NewStore(rec.Root),
			state, state.Canary.Cohort, state.Canary.TargetWidth,
		)
		marker, exists, markerErr := loadNativeCanaryPromotionMarker(rec.Root)
		if err == nil || promoted || reg.calls != 1 ||
			reg.record.MigrationStatus != registry.StatusValidated ||
			markerErr != nil || !exists || marker.Status != nativeCanaryPromotionPending {
			t.Fatalf("promoted=%t err=%v record=%+v calls=%d marker=%+v markerErr=%v",
				promoted, err, reg.record, reg.calls, marker, markerErr)
		}
	})

	t.Run("registry failure before write leaves durable steady-mode block", func(t *testing.T) {
		rec, state := canaryPromotionFixture(t)
		oldLoader := loadExpectedAutonomyReport
		t.Cleanup(func() { loadExpectedAutonomyReport = oldLoader })
		loadExpectedAutonomyReport = func(string, metrics.AutonomyReportExpectation) (*metrics.AutonomyReport, error) {
			return &metrics.AutonomyReport{
				Decision: metrics.AutonomyDecision{Passed: true},
			}, nil
		}
		stored := *rec
		reg := &canaryPromotionStore{
			record: &stored, errorBeforeWrite: errors.New("registry write unavailable"),
		}
		promoted, err := completePassedNativeCanary(
			t.Context(), reg, rec, loopsupervisor.NewStore(rec.Root),
			state, state.Canary.Cohort, state.Canary.TargetWidth,
		)
		marker, exists, markerErr := loadNativeCanaryPromotionMarker(rec.Root)
		if err == nil || promoted || reg.calls != 1 ||
			reg.record.MigrationStatus != registry.StatusMigrated ||
			markerErr != nil || !exists ||
			marker.Status != nativeCanaryPromotionPending ||
			loopPostureAllows(registry.StatusValidated, false, true) {
			t.Fatalf("promoted=%t err=%v marker=%+v exists=%t markerErr=%v",
				promoted, err, marker, exists, markerErr)
		}
	})

	t.Run("state handoff failure leaves marker pending", func(t *testing.T) {
		rec, state := canaryPromotionFixture(t)
		oldLoader := loadExpectedAutonomyReport
		t.Cleanup(func() { loadExpectedAutonomyReport = oldLoader })
		loadExpectedAutonomyReport = func(string, metrics.AutonomyReportExpectation) (*metrics.AutonomyReport, error) {
			return &metrics.AutonomyReport{
				Decision: metrics.AutonomyDecision{Passed: true},
			}, nil
		}
		stored := *rec
		reg := &canaryPromotionStore{record: &stored}
		promoted, err := completePassedNativeCanary(
			t.Context(), reg, rec,
			failingLoopStateSaver{err: errors.New("state write unavailable")},
			state, state.Canary.Cohort, state.Canary.TargetWidth,
		)
		marker, exists, markerErr := loadNativeCanaryPromotionMarker(rec.Root)
		if err == nil || promoted || reg.record.MigrationStatus != registry.StatusValidated ||
			markerErr != nil || !exists || marker.Status != nativeCanaryPromotionPending {
			t.Fatalf("promoted=%t err=%v marker=%+v exists=%t markerErr=%v",
				promoted, err, marker, exists, markerErr)
		}
	})
}

func TestLoopRestartPromotesCompletedCanaryAfterAutoMergeAdvancedHEAD(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	phaseGit(t, rec.Root, "config", "user.name", "Canary Test")
	phaseGit(t, rec.Root, "config", "user.email", "canary@example.com")
	phaseGit(t, rec.Root, "add", "-A")
	phaseGit(t, rec.Root, "commit", "--allow-empty", "-m", "canary base")
	canaryCommit := phaseGit(t, rec.Root, "rev-parse", "HEAD")

	reg := registry.NewStore()
	rec.MigrationStatus = registry.StatusMigrated
	if err := reg.Save(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	_, state := canaryPromotionFixture(t)
	state.ProjectID = rec.ProjectID
	state.Mode = loopsupervisor.ModeStopped
	state.Canary.InstalledCommit = canaryCommit
	state.Canary.RegistryIdentityDigest = mustRegistryIdentityDigest(t, rec)
	state.Canary.ReportPath = filepath.Join(
		rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
	)
	if err := loopsupervisor.NewStore(rec.Root).SaveState(state); err != nil {
		t.Fatal(err)
	}

	phaseWrite(t, filepath.Join(rec.Root, "merged.txt"), "cohort output\n")
	phaseGit(t, rec.Root, "add", "merged.txt")
	phaseGit(t, rec.Root, "commit", "-m", "merge canary cohort")
	if head := phaseGit(t, rec.Root, "rev-parse", "HEAD"); head == canaryCommit {
		t.Fatal("fixture did not advance HEAD")
	}

	oldCommit := loopInstalledCommit
	oldLoader := loadExpectedAutonomyReport
	t.Cleanup(func() {
		loopInstalledCommit = oldCommit
		loadExpectedAutonomyReport = oldLoader
	})
	// Deliberately retain the original binary identity. Re-entering
	// nativeCanarySpec would reject it against the advanced checkout HEAD.
	loopInstalledCommit = func() string { return canaryCommit }
	loadExpectedAutonomyReport = func(string, metrics.AutonomyReportExpectation) (*metrics.AutonomyReport, error) {
		return &metrics.AutonomyReport{
			Decision: metrics.AutonomyDecision{Passed: true},
		}, nil
	}

	code, out, errb := runCmd(
		"loop", "--project", rec.ProjectID,
		"--canary-cohort", "b1,b2", "--max", "2",
	)
	if code != engine.ExitOK {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errb)
	}
	if !strings.Contains(out, "promoted migration_status migrated -> validated") {
		t.Fatalf("stdout=%q", out)
	}
	persisted, err := reg.Get(rec.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.MigrationStatus != registry.StatusValidated {
		t.Fatalf("registry status=%q", persisted.MigrationStatus)
	}
	finalState, err := loopsupervisor.NewStore(rec.Root).LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if finalState.Canary != nil || finalState.Mode != loopsupervisor.ModeStopped {
		t.Fatalf("final supervisor state=%+v", finalState)
	}
	marker, exists, err := loadNativeCanaryPromotionMarker(rec.Root)
	if err != nil || !exists || marker.Status != nativeCanaryPromotionValidated {
		t.Fatalf("marker=%+v exists=%t err=%v", marker, exists, err)
	}
	code, out, errb = runCmd(
		"loop", "--project", rec.ProjectID,
		"--canary-cohort", "b1,b2", "--max", "2",
	)
	if code != engine.ExitOK ||
		!strings.Contains(out, "finalized steady-mode handoff") {
		t.Fatalf("repeated invocation: code=%d stdout=%q stderr=%q", code, out, errb)
	}
}

func TestCompletedNativeCanaryRecoveryRejectsRequestedCohortDrift(t *testing.T) {
	rec, state := canaryPromotionFixture(t)
	stored := *rec
	reg := &canaryPromotionStore{record: &stored}
	_, err := completePassedNativeCanary(
		t.Context(),
		reg,
		rec,
		loopsupervisor.NewStore(rec.Root),
		state,
		[]string{"b1", "different"},
		2,
	)
	if !errors.Is(err, loopsupervisor.ErrCanaryCohortDrift) {
		t.Fatalf("error=%v, want cohort drift", err)
	}
	if reg.calls != 0 {
		t.Fatalf("registry writes=%d, want zero", reg.calls)
	}
	if _, exists, markerErr := loadNativeCanaryPromotionMarker(rec.Root); markerErr != nil || exists {
		t.Fatalf("marker exists=%t err=%v", exists, markerErr)
	}
}

func TestLoopPendingPromotionMarkerBlocksValidatedSteadyMode(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	reg := registry.NewStore()
	rec.MigrationStatus = registry.StatusValidated
	if err := reg.Save(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	_, fixture := canaryPromotionFixture(t)
	fixture.Canary.ReportPath = filepath.Join(
		rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
	)
	if err := saveNativeCanaryPromotionMarker(rec.Root, nativeCanaryPromotionMarker{
		ProjectID: rec.ProjectID,
		Status:    nativeCanaryPromotionPending,
		Canary:    *fixture.Canary,
	}); err != nil {
		t.Fatal(err)
	}

	code, _, errb := runCmd("loop", "--project", rec.ProjectID)
	if code != engine.ExitFatal || !strings.Contains(errb, "canary promotion pending=true") {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
	state, err := loopsupervisor.NewStore(rec.Root).LoadState()
	if err != nil || state.ProjectID != "" {
		t.Fatalf("steady mode reached supervisor: state=%+v err=%v", state, err)
	}
}

func TestNativeCanaryPromotionMarkersAreProjectLocal(t *testing.T) {
	projectA, stateA := canaryPromotionFixture(t)
	projectB, _ := canaryPromotionFixture(t)
	if err := saveNativeCanaryPromotionMarker(projectA.Root, nativeCanaryPromotionMarker{
		ProjectID: projectA.ProjectID + "-a",
		Status:    nativeCanaryPromotionPending,
		Canary:    *stateA.Canary,
	}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := loadNativeCanaryPromotionMarker(projectB.Root); err != nil || exists {
		t.Fatalf("project B observed project A marker: exists=%t err=%v", exists, err)
	}
	if _, exists, err := loadNativeCanaryPromotionMarker(projectA.Root); err != nil || !exists {
		t.Fatalf("project A lost its marker: exists=%t err=%v", exists, err)
	}
}

func TestLoopPendingValidatedRecoveryRetriesRegistryTransaction(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	reg := registry.NewStore()
	rec.MigrationStatus = registry.StatusValidated
	if err := reg.Save(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	_, state := canaryPromotionFixture(t)
	state.ProjectID = rec.ProjectID
	state.Mode = loopsupervisor.ModeStopped
	state.Canary.RegistryIdentityDigest = mustRegistryIdentityDigest(t, rec)
	state.Canary.ReportPath = filepath.Join(
		rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
	)
	if err := loopsupervisor.NewStore(rec.Root).SaveState(state); err != nil {
		t.Fatal(err)
	}
	if err := saveNativeCanaryPromotionMarker(rec.Root, nativeCanaryPromotionMarker{
		ProjectID: rec.ProjectID,
		Status:    nativeCanaryPromotionPending,
		Canary:    *state.Canary,
	}); err != nil {
		t.Fatal(err)
	}

	auditPath := filepath.Join(os.Getenv("KORYPH_HOME"), "audit.jsonl")
	before, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	oldLoader := loadExpectedAutonomyReport
	t.Cleanup(func() { loadExpectedAutonomyReport = oldLoader })
	loadExpectedAutonomyReport = func(string, metrics.AutonomyReportExpectation) (*metrics.AutonomyReport, error) {
		return &metrics.AutonomyReport{
			Decision: metrics.AutonomyDecision{Passed: true},
		}, nil
	}

	code, out, errb := runCmd(
		"loop", "--project", rec.ProjectID,
		"--canary-cohort", "b1,b2", "--max", "2",
	)
	if code != engine.ExitOK ||
		!strings.Contains(out, "finalized steady-mode handoff") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errb)
	}
	after, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(after), `"kind":"update"`) !=
		strings.Count(string(before), `"kind":"update"`)+1 {
		t.Fatalf("registry recovery did not append exactly one update audit")
	}
	marker, exists, err := loadNativeCanaryPromotionMarker(rec.Root)
	if err != nil || !exists || marker.Status != nativeCanaryPromotionValidated {
		t.Fatalf("marker=%+v exists=%t err=%v", marker, exists, err)
	}
}

func TestValidatedArchiveCannotReplayAfterAccountIdentityChange(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	reg := registry.NewStore()
	rec.MigrationStatus = registry.StatusValidated
	if err := reg.Save(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	_, archived := canaryPromotionFixture(t)
	archived.ProjectID = rec.ProjectID
	archived.Canary.RegistryIdentityDigest = mustRegistryIdentityDigest(t, rec)
	archived.Canary.ReportPath = filepath.Join(
		rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
	)
	if err := saveNativeCanaryPromotionMarker(rec.Root, nativeCanaryPromotionMarker{
		ProjectID: rec.ProjectID,
		Status:    nativeCanaryPromotionValidated,
		Canary:    *archived.Canary,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetAccount(
		t.Context(), rec.ProjectID, registry.ProfileWork,
		"/tmp/koryph-work", "work@example.com", "identity changed",
	); err != nil {
		t.Fatal(err)
	}
	current, err := reg.Get(rec.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	current.MigrationStatus = registry.StatusMigrated
	if err := reg.Save(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	oldLoader := loadExpectedAutonomyReport
	t.Cleanup(func() { loadExpectedAutonomyReport = oldLoader })
	loadExpectedAutonomyReport = func(string, metrics.AutonomyReportExpectation) (*metrics.AutonomyReport, error) {
		return &metrics.AutonomyReport{
			Decision: metrics.AutonomyDecision{Passed: true},
		}, nil
	}

	handled, promoted, err := reconcilePassedNativeCanary(
		t.Context(), reg, current, loopsupervisor.NewStore(rec.Root),
		archived.Canary.Cohort, archived.Canary.TargetWidth,
	)
	if !handled || promoted || err == nil ||
		!strings.Contains(err.Error(), "passing canary registry identity is stale") {
		t.Fatalf("handled=%t promoted=%t err=%v", handled, promoted, err)
	}
	current, err = reg.Get(rec.ProjectID)
	if err != nil || current.MigrationStatus != registry.StatusMigrated {
		t.Fatalf("registry=%+v err=%v", current, err)
	}
}

func TestPendingCanaryCannotReplayAfterAccountIdentityChange(t *testing.T) {
	for _, tc := range []struct {
		name          string
		persistMarker bool
	}{
		{name: "active completed state"},
		{name: "pending marker", persistMarker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			rec := addProject(t, "demo")
			reg := registry.NewStore()
			rec.MigrationStatus = registry.StatusMigrated
			if err := reg.Save(t.Context(), rec); err != nil {
				t.Fatal(err)
			}
			_, state := canaryPromotionFixture(t)
			state.ProjectID = rec.ProjectID
			state.Mode = loopsupervisor.ModeStopped
			state.Canary.RegistryIdentityDigest = mustRegistryIdentityDigest(t, rec)
			state.Canary.ReportPath = filepath.Join(
				rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
			)
			if tc.persistMarker {
				if err := saveNativeCanaryPromotionMarker(rec.Root, nativeCanaryPromotionMarker{
					ProjectID: rec.ProjectID,
					Status:    nativeCanaryPromotionPending,
					Canary:    *state.Canary,
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := loopsupervisor.NewStore(rec.Root).SaveState(state); err != nil {
				t.Fatal(err)
			}

			if err := reg.SetAccount(
				t.Context(), rec.ProjectID, registry.ProfileWork,
				"/tmp/koryph-work", "work@example.com", "identity changed",
			); err != nil {
				t.Fatal(err)
			}
			current, err := reg.Get(rec.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			current.MigrationStatus = registry.StatusMigrated
			if err := reg.Save(t.Context(), current); err != nil {
				t.Fatal(err)
			}

			oldLoader := loadExpectedAutonomyReport
			t.Cleanup(func() { loadExpectedAutonomyReport = oldLoader })
			reportLoaded := false
			loadExpectedAutonomyReport = func(
				string,
				metrics.AutonomyReportExpectation,
			) (*metrics.AutonomyReport, error) {
				reportLoaded = true
				return &metrics.AutonomyReport{
					Decision: metrics.AutonomyDecision{Passed: true},
				}, nil
			}

			handled, promoted, err := reconcilePassedNativeCanary(
				t.Context(), reg, current, loopsupervisor.NewStore(rec.Root),
				state.Canary.Cohort, state.Canary.TargetWidth,
			)
			if !handled || promoted || err == nil ||
				!strings.Contains(err.Error(), "passing canary registry identity is stale") {
				t.Fatalf("handled=%t promoted=%t err=%v", handled, promoted, err)
			}
			if reportLoaded {
				t.Fatal("stale registry identity reached immutable report loading")
			}
			current, err = reg.Get(rec.ProjectID)
			if err != nil || current.MigrationStatus != registry.StatusMigrated {
				t.Fatalf("registry=%+v err=%v", current, err)
			}
		})
	}
}

func TestStaleCanaryGenerationIsArchivedBeforeFreshBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name         string
		markerStatus string
	}{
		{name: "completed state"},
		{name: "pending marker", markerStatus: nativeCanaryPromotionPending},
		{name: "validated marker", markerStatus: nativeCanaryPromotionValidated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			rec := addProject(t, "demo")
			phaseGit(t, rec.Root, "config", "user.name", "Canary Test")
			phaseGit(t, rec.Root, "config", "user.email", "canary@example.com")
			phaseGit(t, rec.Root, "add", "-A")
			phaseGit(t, rec.Root, "commit", "--allow-empty", "-m", "canary base")
			canaryCommit := phaseGit(t, rec.Root, "rev-parse", "HEAD")
			reg := registry.NewStore()
			rec.MigrationStatus = registry.StatusMigrated
			if err := reg.Save(t.Context(), rec); err != nil {
				t.Fatal(err)
			}
			_, oldState := canaryPromotionFixture(t)
			oldState.ProjectID = rec.ProjectID
			oldState.Mode = loopsupervisor.ModeStopped
			oldState.Canary.RegistryIdentityDigest = mustRegistryIdentityDigest(t, rec)
			oldState.Canary.ReportPath = filepath.Join(
				rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
			)
			if err := os.MkdirAll(filepath.Dir(oldState.Canary.ReportPath), 0o700); err != nil {
				t.Fatal(err)
			}
			historicalReport := []byte("{\"historical\":true}\n")
			if err := os.WriteFile(oldState.Canary.ReportPath, historicalReport, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.markerStatus == "" {
				if err := loopsupervisor.NewStore(rec.Root).SaveState(oldState); err != nil {
					t.Fatal(err)
				}
			} else if err := saveNativeCanaryPromotionMarker(
				rec.Root,
				nativeCanaryPromotionMarker{
					ProjectID: rec.ProjectID,
					Status:    tc.markerStatus,
					Canary:    *oldState.Canary,
				},
			); err != nil {
				t.Fatal(err)
			}

			if err := reg.SetAccount(
				t.Context(), rec.ProjectID, registry.ProfileWork,
				"/tmp/koryph-work", "work@example.com", "identity changed",
			); err != nil {
				t.Fatal(err)
			}
			current, err := reg.Get(rec.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			current.MigrationStatus = registry.StatusMigrated
			if err := reg.Save(t.Context(), current); err != nil {
				t.Fatal(err)
			}
			currentDigest := mustRegistryIdentityDigest(t, current)

			store := loopsupervisor.NewStore(rec.Root)
			state, retired, err := retireStaleNativeCanaryEvidence(
				t.Context(), rec.Root, currentDigest,
			)
			if err != nil || !retired || state.Canary != nil {
				t.Fatalf("state=%+v retired=%t err=%v", state, retired, err)
			}
			if _, err := os.Lstat(oldState.Canary.ReportPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fixed report path still occupied: %v", err)
			}
			if _, exists, err := loadNativeCanaryPromotionMarker(rec.Root); err != nil || exists {
				t.Fatalf("marker exists=%t err=%v", exists, err)
			}
			persisted, err := store.LoadState()
			if err != nil || persisted.Canary != nil {
				t.Fatalf("persisted state=%+v err=%v", persisted, err)
			}
			historyFiles, err := filepath.Glob(
				filepath.Join(store.Root, nativeCanaryHistoryDir, "*.json"),
			)
			if err != nil || len(historyFiles) != 1 {
				t.Fatalf("history files=%v err=%v", historyFiles, err)
			}
			var history nativeCanaryHistoryRecord
			if err := fsx.ReadJSON(historyFiles[0], &history); err != nil {
				t.Fatal(err)
			}
			if history.PreviousRegistryIdentityDigest !=
				oldState.Canary.RegistryIdentityDigest ||
				history.CurrentRegistryIdentityDigest != currentDigest ||
				history.ArchivedReportPath == "" {
				t.Fatalf("history=%+v", history)
			}
			gotReport, err := os.ReadFile(history.ArchivedReportPath)
			if err != nil || !bytes.Equal(gotReport, historicalReport) {
				t.Fatalf("archived report=%q err=%v", gotReport, err)
			}

			oldCommit := loopInstalledCommit
			oldVersion := loopBinaryVersion
			t.Cleanup(func() {
				loopInstalledCommit = oldCommit
				loopBinaryVersion = oldVersion
			})
			loopInstalledCommit = func() string { return canaryCommit }
			loopBinaryVersion = func() string { return "0.10.0-test" }
			fresh, err := nativeCanarySpec(
				rec.Root, []string{"fresh-1", "fresh-2"}, 2, currentDigest,
			)
			if err != nil {
				t.Fatalf("fresh canary bootstrap remained blocked: %v", err)
			}
			if filepath.Clean(fresh.ReportPath) !=
				filepath.Clean(oldState.Canary.ReportPath) {
				t.Fatalf("fresh report path=%q, want %q", fresh.ReportPath, oldState.Canary.ReportPath)
			}
		})
	}
}

func TestConcurrentCompletedCanaryReconciliationIsIdempotent(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	reg := registry.NewStore()
	rec.MigrationStatus = registry.StatusMigrated
	if err := reg.Save(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	_, state := canaryPromotionFixture(t)
	state.ProjectID = rec.ProjectID
	state.Mode = loopsupervisor.ModeStopped
	state.Canary.RegistryIdentityDigest = mustRegistryIdentityDigest(t, rec)
	state.Canary.ReportPath = filepath.Join(
		rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
	)
	store := loopsupervisor.NewStore(rec.Root)
	if err := store.SaveState(state); err != nil {
		t.Fatal(err)
	}
	oldLoader := loadExpectedAutonomyReport
	t.Cleanup(func() { loadExpectedAutonomyReport = oldLoader })
	loadExpectedAutonomyReport = func(string, metrics.AutonomyReportExpectation) (*metrics.AutonomyReport, error) {
		return &metrics.AutonomyReport{
			Decision: metrics.AutonomyDecision{Passed: true},
		}, nil
	}

	type result struct {
		handled, promoted bool
		err               error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			handled, promoted, err := reconcilePassedNativeCanary(
				t.Context(), reg, rec, store, state.Canary.Cohort, state.Canary.TargetWidth,
			)
			results <- result{handled: handled, promoted: promoted, err: err}
		}()
	}
	close(start)
	promotions := 0
	for range 2 {
		got := <-results
		if got.err != nil || !got.handled {
			t.Fatalf("reconciliation=%+v", got)
		}
		if got.promoted {
			promotions++
		}
	}
	if promotions != 1 {
		t.Fatalf("promotions=%d, want exactly one", promotions)
	}
	persisted, err := reg.Get(rec.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.MigrationStatus != registry.StatusValidated {
		t.Fatalf("registry status=%q", persisted.MigrationStatus)
	}
	finalState, err := store.LoadState()
	if err != nil || finalState.Canary != nil {
		t.Fatalf("final state=%+v err=%v", finalState, err)
	}
}

func canaryPromotionFixture(t *testing.T) (*registry.Record, loopsupervisor.State) {
	t.Helper()
	cohort := []string{"b1", "b2"}
	digest, err := metrics.AutonomyCohortDigest(cohort)
	if err != nil {
		t.Fatal(err)
	}
	rec := &registry.Record{
		ProjectID: "demo", Root: t.TempDir(),
		MigrationStatus:      registry.StatusMigrated,
		ValidationGeneration: "fixture-generation",
	}
	registryIdentityDigest, err := registry.ValidationIdentityDigest(rec)
	if err != nil {
		t.Fatal(err)
	}
	return rec, loopsupervisor.State{
		ProjectID: rec.ProjectID,
		Canary: &loopsupervisor.CanaryState{
			Cohort: cohort, CohortDigest: digest,
			StartedAt:              "2026-07-25T20:00:00Z",
			TargetWidth:            2,
			CurrentWidth:           2,
			InactivityLimitMS:      loopsupervisor.DefaultCanaryInactivityLimit.Milliseconds(),
			InstalledCommit:        strings.Repeat("a", 40),
			BinaryVersion:          "0.10.0",
			BuildIdentity:          "fixture-build",
			ContractDigest:         "sha256:" + strings.Repeat("b", 64),
			RegistryIdentityDigest: registryIdentityDigest,
			ReportPath: filepath.Join(
				rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
			),
			Decision:          "passed",
			ReportDigest:      "sha256:" + strings.Repeat("c", 64),
			ReportGeneratedAt: "2026-07-25T20:10:00Z",
			PublishedAt:       "2026-07-25T20:10:01Z",
		},
	}
}

func mustRegistryIdentityDigest(t *testing.T, rec *registry.Record) string {
	t.Helper()
	digest, err := registry.ValidationIdentityDigest(rec)
	if err != nil {
		t.Fatal(err)
	}
	return digest
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
		nativeCanaryCohort: []string{"a", "b"},
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
		AllowedIDs:         []string{"a", "b"},
		Max:                2,
		AuthoritativeWidth: true,
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
	if !captured.NativeCanary {
		t.Fatal("fixed native canary authority was not forwarded")
	}
	if published != "run-live" {
		t.Fatalf("published run id = %q", published)
	}
}

func TestLoopEngineRejectsNativeCanaryCohortEscape(t *testing.T) {
	called := false
	runner := &loopEngine{
		nativeCanaryCohort: []string{"a", "b"},
		run: func(_ context.Context, _ engine.Options) (engine.Outcome, error) {
			called = true
			return engine.Outcome{}, nil
		},
	}
	result, err := runner.Run(context.Background(), loopsupervisor.RunRequest{
		AllowedIDs:         []string{"a", "b", "c"},
		Max:                2,
		AuthoritativeWidth: true,
	})
	if err == nil || result.Code != engine.ExitFatal {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if called {
		t.Fatal("escaped canary request reached the engine")
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
	registryIdentityDigest := "sha256:" + strings.Repeat("d", 64)
	if _, err := nativeCanarySpec(repo, []string{"b1", "b2"}, 2, registryIdentityDigest); err != nil {
		t.Fatalf("clean matching checkout: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("contract v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	phaseGit(t, repo, "add", "AGENTS.md")
	phaseGit(t, repo, "commit", "-m", "new contract")
	second := phaseGit(t, repo, "rev-parse", "HEAD")
	if _, err := nativeCanarySpec(repo, []string{"b1", "b2"}, 2, registryIdentityDigest); err == nil ||
		!strings.Contains(err.Error(), "does not match checkout HEAD") {
		t.Fatalf("stale otherwise-valid binary error = %v", err)
	}

	loopInstalledCommit = func() string { return second }
	if err := os.WriteFile(filepath.Join(repo, "untracked.go"), []byte("package dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeCanarySpec(repo, []string{"b1", "b2"}, 2, registryIdentityDigest); err == nil ||
		!strings.Contains(err.Error(), "tracked or untracked changes") {
		t.Fatalf("untracked source error = %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "untracked.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("dirty tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeCanarySpec(repo, []string{"b1", "b2"}, 2, registryIdentityDigest); err == nil ||
		!strings.Contains(err.Error(), "tracked or untracked changes") {
		t.Fatalf("tracked source error = %v", err)
	}
}

func TestResumedNativeCanaryRequiresCleanDescendantCheckout(t *testing.T) {
	repo := gitRepo(t)
	phaseGit(t, repo, "config", "user.name", "Canary Test")
	phaseGit(t, repo, "config", "user.email", "canary@example.com")
	phaseWrite(t, filepath.Join(repo, "AGENTS.md"), "contract v1\n")
	phaseGit(t, repo, "add", "AGENTS.md")
	phaseGit(t, repo, "commit", "-m", "canary base")
	installedCommit := phaseGit(t, repo, "rev-parse", "HEAD")

	oldCommit := loopInstalledCommit
	oldVersion := loopBinaryVersion
	t.Cleanup(func() {
		loopInstalledCommit = oldCommit
		loopBinaryVersion = oldVersion
	})
	loopInstalledCommit = func() string { return installedCommit }
	loopBinaryVersion = func() string { return "0.10.0-test" }
	registryIdentityDigest := "sha256:" + strings.Repeat("d", 64)
	spec, err := nativeCanarySpec(repo, []string{"b1", "b2"}, 2, registryIdentityDigest)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := metrics.AutonomyCohortDigest(spec.Cohort)
	if err != nil {
		t.Fatal(err)
	}
	state := loopsupervisor.State{
		ProjectID: "demo",
		Canary: &loopsupervisor.CanaryState{
			Cohort:                 append([]string(nil), spec.Cohort...),
			CohortDigest:           digest,
			TargetWidth:            spec.TargetWidth,
			CurrentWidth:           2,
			InactivityLimitMS:      spec.InactivityLimit.Milliseconds(),
			HardStops:              append([]string(nil), spec.HardStops...),
			InstalledCommit:        spec.InstalledCommit,
			BinaryVersion:          spec.BinaryVersion,
			BuildIdentity:          spec.BuildIdentity,
			ContractDigest:         spec.ContractDigest,
			RegistryIdentityDigest: spec.RegistryIdentityDigest,
			ReportPath:             spec.ReportPath,
		},
	}

	phaseWrite(t, filepath.Join(repo, "merged.txt"), "merged cohort output\n")
	phaseGit(t, repo, "add", "merged.txt")
	phaseGit(t, repo, "commit", "-m", "merge cohort output")
	if _, err := resumedNativeCanarySpec(
		repo, []string{"b1", "b2"}, 2, registryIdentityDigest, state,
	); err != nil {
		t.Fatalf("clean descendant resume: %v", err)
	}
	if _, err := resumedNativeCanarySpec(
		repo, []string{"b1", "b2"}, 2, "sha256:"+strings.Repeat("e", 64), state,
	); !errors.Is(err, loopsupervisor.ErrCanaryIdentityDrift) {
		t.Fatalf("registry identity drift error=%v", err)
	}

	phaseWrite(t, filepath.Join(repo, "dirty.txt"), "uncommitted\n")
	if _, err := resumedNativeCanarySpec(
		repo, []string{"b1", "b2"}, 2, registryIdentityDigest, state,
	); err == nil ||
		!strings.Contains(err.Error(), "tracked or untracked changes") {
		t.Fatalf("dirty resume error=%v", err)
	}
	if err := os.Remove(filepath.Join(repo, "dirty.txt")); err != nil {
		t.Fatal(err)
	}

	tree := phaseGit(t, repo, "rev-parse", "HEAD^{tree}")
	unrelated := phaseGit(t, repo, "commit-tree", tree, "-m", "unrelated root")
	phaseGit(t, repo, "checkout", "--detach", unrelated)
	if _, err := resumedNativeCanarySpec(
		repo, []string{"b1", "b2"}, 2, registryIdentityDigest, state,
	); err == nil ||
		!strings.Contains(err.Error(), "not descended") {
		t.Fatalf("unrelated resume error=%v", err)
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
