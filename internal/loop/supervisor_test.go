// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeObserver struct {
	mu           sync.Mutex
	observations []Observation
	errs         []error
	calls        int
	cancel       context.CancelFunc
	cancelAt     int
	onWait       func(time.Duration)
}

type blockingObserver struct {
	started chan struct{}
}

func (b *blockingObserver) Observe(ctx context.Context, _ Scope) (Observation, error) {
	if b.started != nil {
		select {
		case b.started <- struct{}{}:
		default:
		}
	}
	<-ctx.Done()
	return Observation{}, ctx.Err()
}

type uncooperativeObserver struct {
	started chan struct{}
	release chan struct{}
}

func (u *uncooperativeObserver) Observe(context.Context, Scope) (Observation, error) {
	if u.started != nil {
		close(u.started)
	}
	<-u.release
	return Observation{}, nil
}

func (f *fakeObserver) Observe(_ context.Context, _ Scope) (Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.cancel != nil && f.cancelAt > 0 && f.calls >= f.cancelAt {
		defer f.cancel()
	}
	if len(f.observations) == 0 {
		if len(f.errs) == 0 {
			return Observation{}, nil
		}
		i := f.calls - 1
		if i >= len(f.errs) {
			i = len(f.errs) - 1
		}
		return Observation{}, f.errs[i]
	}
	i := f.calls - 1
	if i >= len(f.observations) {
		i = len(f.observations) - 1
	}
	var err error
	if len(f.errs) > 0 {
		j := i
		if j >= len(f.errs) {
			j = len(f.errs) - 1
		}
		err = f.errs[j]
	}
	return f.observations[i], err
}

func (f *fakeObserver) Wait(ctx context.Context, _ Scope, _ Observation, delay time.Duration) error {
	if f.onWait != nil {
		f.onWait(delay)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

type fakeEngine struct {
	mu       sync.Mutex
	requests []RunRequest
	results  []RunResult
	errs     []error
	cancel   context.CancelFunc
	cancelAt int
	block    bool
	runID    string
}

func (f *fakeEngine) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	i := len(f.requests) - 1
	if f.cancel != nil && f.cancelAt > 0 && len(f.requests) >= f.cancelAt {
		defer f.cancel()
	}
	var result RunResult
	if len(f.results) > 0 {
		j := i
		if j >= len(f.results) {
			j = len(f.results) - 1
		}
		result = f.results[j]
	}
	var err error
	if len(f.errs) > 0 {
		j := i
		if j >= len(f.errs) {
			j = len(f.errs) - 1
		}
		err = f.errs[j]
	}
	block := f.block
	runID := f.runID
	f.mu.Unlock()
	if runID != "" && req.OnRunStart != nil {
		if err := req.OnRunStart(runID); err != nil {
			return result, err
		}
	}
	if block {
		<-ctx.Done()
		return result, ctx.Err()
	}
	return result, err
}

func TestActiveRunIDIsDurableWhileEngineIsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}}
	engine := &fakeEngine{block: true, runID: "run-live"}
	supervisor := testSupervisor(t, observer, engine)
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err := supervisor.Store.LoadState()
		if err == nil && state.Mode == ModeRunning && state.CurrentRunID == "run-live" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("active run id did not become visible: state=%+v err=%v", state, err)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state, err := supervisor.Store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.LastRunID != "run-live" || state.CurrentRunID != "" {
		t.Fatalf("terminal supervisor state = %+v", state)
	}
}

type fakeDrainer struct {
	mu      sync.Mutex
	reasons []string
}

type fakeMaintainer struct {
	mu         sync.Mutex
	boundaries []Boundary
	err        error
}

func (f *fakeMaintainer) Maintain(_ context.Context, boundary Boundary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boundaries = append(f.boundaries, boundary)
	return f.err
}

func (f *fakeDrainer) Drain(_ context.Context, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reasons = append(f.reasons, reason)
	return nil
}

type fakeTripwire struct{ events chan Tripwire }

func (f fakeTripwire) Events(context.Context) <-chan Tripwire { return f.events }

type publishingEngine struct{ events chan Tripwire }

func (p *publishingEngine) Events(context.Context) <-chan Tripwire { return p.events }

func (p *publishingEngine) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := req.OnRunStart("run-fast-tripwire"); err != nil {
		return RunResult{}, err
	}
	p.events <- Tripwire{Kind: "engine-invariant", RunID: "run-fast-tripwire"}
	<-ctx.Done()
	return RunResult{RunID: "run-fast-tripwire"}, ctx.Err()
}

type fakeCanaryPublisher struct {
	publication CanaryPublication
	err         error
	requests    []CanaryPublicationRequest
}

func (f *fakeCanaryPublisher) Publish(
	_ context.Context,
	request CanaryPublicationRequest,
) (CanaryPublication, error) {
	f.requests = append(f.requests, request)
	if f.publication.Decision == "" {
		f.publication = CanaryPublication{
			Path: request.Canary.ReportPath, Digest: "sha256:" + strings.Repeat("a", 64),
			Decision: "passed", GeneratedAt: "2026-07-25T12:00:00Z",
		}
	}
	return f.publication, f.err
}

func testSupervisor(t *testing.T, observer Observer, engine Engine) *Supervisor {
	t.Helper()
	store := NewStore(t.TempDir())
	return &Supervisor{
		Config: Config{
			ProjectID:    "demo",
			IdleMin:      time.Nanosecond,
			IdleMax:      time.Nanosecond,
			CrashMin:     time.Nanosecond,
			CrashMax:     time.Nanosecond,
			FailureLimit: 3,
		},
		Store:    store,
		Observer: observer,
		Engine:   engine,
		sleep: func(ctx context.Context, _ time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		},
	}
}

func testCanarySpec(t *testing.T, supervisor *Supervisor, cohort []string, target int) *CanarySpec {
	t.Helper()
	supervisor.Publisher = &fakeCanaryPublisher{}
	repo := filepath.Dir(filepath.Dir(supervisor.Store.Root))
	return &CanarySpec{
		Cohort: cohort, TargetWidth: target, InactivityLimit: DefaultCanaryInactivityLimit,
		InstalledCommit: strings.Repeat("a", 40),
		BinaryVersion:   "0.10.0",
		BuildIdentity:   "test-build-identity",
		ContractDigest:  "sha256:" + strings.Repeat("b", 64),
		ReportPath: filepath.Join(
			repo, ".plan-logs", "koryph", "canary", "autonomous-loop-reliability.json",
		),
	}
}

func TestIdleCreatesNoRunDirectoryAndNeverCallsEngine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := &fakeObserver{cancel: cancel, cancelAt: 2}
	engine := &fakeEngine{}
	supervisor := testSupervisor(t, observer, engine)

	if err := supervisor.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(engine.requests) != 0 {
		t.Fatalf("engine calls = %d, want 0", len(engine.requests))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(supervisor.Store.Root)), ".plan-logs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("idle supervisor created engine ledger root: %v", err)
	}
	state, err := supervisor.Store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != ModeStopped {
		t.Fatalf("mode = %q, want stopped", state.Mode)
	}
}

func TestMaintenanceRunsAtIdleAndTerminalBoundaries(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		observer := &fakeObserver{cancel: cancel, cancelAt: 2}
		maintenance := &fakeMaintainer{}
		supervisor := testSupervisor(t, observer, &fakeEngine{})
		supervisor.Maintainer = maintenance
		if err := supervisor.Run(ctx); err != nil {
			t.Fatal(err)
		}
		maintenance.mu.Lock()
		defer maintenance.mu.Unlock()
		if len(maintenance.boundaries) == 0 || maintenance.boundaries[0] != BoundaryIdle {
			t.Fatalf("idle maintenance = %v", maintenance.boundaries)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		observer := &fakeObserver{
			observations: []Observation{{ReadyIDs: []string{"b1"}}},
			cancel:       cancel,
			cancelAt:     2,
		}
		engine := &fakeEngine{
			results: []RunResult{{RunID: "run-1", Code: 0, Reason: "complete"}},
		}
		maintenance := &fakeMaintainer{}
		supervisor := testSupervisor(t, observer, engine)
		supervisor.Maintainer = maintenance
		if err := supervisor.Run(ctx); err != nil {
			t.Fatal(err)
		}
		maintenance.mu.Lock()
		defer maintenance.mu.Unlock()
		if len(maintenance.boundaries) != 1 || maintenance.boundaries[0] != BoundaryTerminal {
			t.Fatalf("terminal maintenance = %v", maintenance.boundaries)
		}
	})
}

func TestRecoveryRequestPinsObservedRunWithoutFreshSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := &fakeObserver{observations: []Observation{{
		Recovery: true, RecoveryRunID: "run-1",
	}}, cancel: cancel, cancelAt: 2}
	engine := &fakeEngine{results: []RunResult{{RunID: "run-1", Code: 0, Reason: "resumed"}}}
	supervisor := testSupervisor(t, observer, engine)

	if err := supervisor.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(engine.requests) != 1 {
		t.Fatalf("engine requests = %d, want 1", len(engine.requests))
	}
	req := engine.requests[0]
	if !req.Resume || req.RecoveryRunID != "run-1" {
		t.Fatalf("recovery request = %+v", req)
	}
	if req.Only != "" {
		t.Fatalf("completed recovery selected fresh coding target %q", req.Only)
	}
}

func TestStaleClaimReconciliationIsWorkButNotSessionResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := &fakeObserver{
		observations: []Observation{{Reconcile: true}},
		cancel:       cancel,
		cancelAt:     2,
	}
	engine := &fakeEngine{results: []RunResult{{Code: 0, Reason: "reconciled"}}}
	supervisor := testSupervisor(t, observer, engine)
	if err := supervisor.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(engine.requests) != 1 || engine.requests[0].Resume {
		t.Fatalf("requests = %+v, want one fresh reconciliation run", engine.requests)
	}
}

func TestRepeatedIdenticalEngineFailureOpensCircuit(t *testing.T) {
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}}
	engine := &fakeEngine{
		results: []RunResult{{Code: 1, Reason: "same failure"}},
		errs:    []error{errors.New("boom")},
	}
	supervisor := testSupervisor(t, observer, engine)

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	if len(engine.requests) != 3 {
		t.Fatalf("engine requests = %d, want bounded 3", len(engine.requests))
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.Mode != ModeCircuitOpen || state.IdenticalFailures != 3 {
		t.Fatalf("state = %+v", state)
	}
}

func TestRepeatedSuccessfulNonProgressOpensCircuit(t *testing.T) {
	observer := &fakeObserver{observations: []Observation{{
		ReadyIDs: []string{"b1"}, WakeToken: "same-frontier",
	}}}
	engine := &fakeEngine{results: []RunResult{{
		Code: 0, Reason: "nothing selected",
	}}}
	supervisor := testSupervisor(t, observer, engine)

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	if len(engine.requests) != 3 {
		t.Fatalf("engine requests = %d, want bounded 3", len(engine.requests))
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.IdenticalFailures != 3 ||
		!strings.Contains(state.CircuitReason, "repeated identical engine failure") {
		t.Fatalf("state = %+v", state)
	}
}

func TestDistinctEngineCrashesStillHaveBoundedRestartBudget(t *testing.T) {
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}}
	errs := []error{
		errors.New("failure-1"),
		errors.New("failure-2"),
		errors.New("failure-3"),
		errors.New("failure-4"),
		errors.New("failure-5"),
		errors.New("failure-6"),
	}
	engine := &fakeEngine{results: []RunResult{{Code: 1}}, errs: errs}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Config.CrashLimit = 6
	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	if len(engine.requests) != 6 {
		t.Fatalf("engine requests = %d, want bounded 6", len(engine.requests))
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.ConsecutiveFailures != 6 || state.IdenticalFailures != 1 {
		t.Fatalf("failure state = %+v", state)
	}
}

func TestCanaryStartsAtTwoAndWidensOnlyAfterFiveGoodTerminals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := &fakeObserver{
		observations: []Observation{{ReadyIDs: []string{"b1", "b2"}}},
		cancel:       cancel,
		cancelAt:     7,
	}
	results := make([]RunResult, 6)
	for i := range results {
		results[i] = RunResult{
			Code: 0, Dispatched: 1, PressureNormal: true,
			Terminal: []TerminalOutcome{{
				BeadID: fmt.Sprintf("b%d", i+1), Status: "merged", Good: true,
			}},
		}
	}
	engine := &fakeEngine{results: results, cancel: cancel, cancelAt: 6}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Config.Canary = testCanarySpec(
		t, supervisor, []string{"b8", "b7", "b6", "b5", "b4", "b3", "b2", "b1"}, 4,
	)

	if err := supervisor.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(engine.requests) != 6 {
		t.Fatalf("engine requests = %d, want 6", len(engine.requests))
	}
	for i := 0; i < 5; i++ {
		if engine.requests[i].Max != 2 {
			t.Fatalf("request %d width = %d, want 2 before five outcomes", i, engine.requests[i].Max)
		}
		if len(engine.requests[i].AllowedIDs) != 8 {
			t.Fatalf("request %d allowed ids = %v", i, engine.requests[i].AllowedIDs)
		}
		if !engine.requests[i].AuthoritativeWidth ||
			!equalStrings(normalizedHardStops(engine.requests[i].HardStops), normalizedHardStops(nil)) {
			t.Fatalf("request %d canary policy = %+v", i, engine.requests[i])
		}
	}
	if engine.requests[5].Max != 3 {
		t.Fatalf("sixth request width = %d, want 3", engine.requests[5].Max)
	}
}

func TestPressureBreakResetsCanaryStreak(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}, cancel: cancel, cancelAt: 7}
	results := make([]RunResult, 6)
	for i := range results {
		results[i] = RunResult{
			Code: 0, PressureNormal: i != 3,
			Terminal: []TerminalOutcome{{
				BeadID: fmt.Sprintf("b%d", i+1), Status: "merged", Good: true,
			}},
		}
	}
	engine := &fakeEngine{results: results, cancel: cancel, cancelAt: 6}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Config.Canary = testCanarySpec(
		t, supervisor, []string{"b1", "b2", "b3", "b4", "b5", "b6", "b7"}, 3,
	)
	if err := supervisor.Run(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := supervisor.Store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Canary.CurrentWidth != 2 || state.Canary.ConsecutiveGood != 2 {
		t.Fatalf("canary = %+v, want width 2 and streak 2", state.Canary)
	}
}

func TestCanaryRejectsTamperedStoredCohortDigest(t *testing.T) {
	supervisor := testSupervisor(t, &fakeObserver{}, &fakeEngine{})
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	state := State{
		ProjectID: "demo",
		Canary: &CanaryState{
			CohortDigest:    cohortDigest([]string{"b1", "b2"}),
			Cohort:          []string{"b1", "attacker"},
			StartedAt:       "2026-07-25T12:00:00Z",
			TargetWidth:     2,
			CurrentWidth:    2,
			HardStops:       normalizedHardStops(nil),
			InstalledCommit: supervisor.Config.Canary.InstalledCommit,
			BinaryVersion:   supervisor.Config.Canary.BinaryVersion,
			BuildIdentity:   supervisor.Config.Canary.BuildIdentity,
			ContractDigest:  supervisor.Config.Canary.ContractDigest,
			ReportPath:      supervisor.Config.Canary.ReportPath,
		},
	}
	if err := supervisor.configureCanary(&state); !errors.Is(err, ErrCanaryCohortDrift) {
		t.Fatalf("configureCanary error = %v, want drift", err)
	}
}

func TestNewCanaryUsesAdmissionTimeNotPriorSupervisorStart(t *testing.T) {
	supervisor := testSupervisor(t, &fakeObserver{}, &fakeEngine{})
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	admitted := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return admitted }
	state := State{
		ProjectID: "demo",
		Mode:      ModeStopped,
		StartedAt: "2026-07-01T00:00:00Z",
	}
	if err := supervisor.configureCanary(&state); err != nil {
		t.Fatal(err)
	}
	if state.Canary.StartedAt != admitted.Format(time.RFC3339Nano) ||
		state.Canary.StartedAt == state.StartedAt {
		t.Fatalf("canary start = %q, prior supervisor start = %q",
			state.Canary.StartedAt, state.StartedAt)
	}
}

func TestTerminalCanaryPublishesOnceStopsAndRestartDoesNotRepublish(t *testing.T) {
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1", "b2"}}}}
	engine := &fakeEngine{
		runID: "run-1",
		results: []RunResult{{
			RunID: "run-1", Code: 0, Dispatched: 2, PressureNormal: true,
			Pressure: PressureSample{
				At: "2026-07-25T12:00:00Z", Level: "normal", AvailableMB: 4096,
			},
			Terminal: []TerminalOutcome{
				{BeadID: "b1", Status: "merged", Good: true},
				{BeadID: "b2", Status: "merged", Good: true},
			},
		}},
	}
	supervisor := testSupervisor(t, observer, engine)
	spec := testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	supervisor.Config.Canary = spec
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)

	if err := supervisor.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := supervisor.Store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != ModeStopped || state.Canary.Decision != "passed" ||
		state.Canary.ReportDigest == "" || state.Canary.ReportGeneratedAt == "" ||
		len(state.Canary.RunIDs) != 1 || len(state.Canary.Terminal) != 2 ||
		len(state.Canary.Pressure) != 1 || state.Canary.Pressure[0].RunID != "run-1" {
		t.Fatalf("terminal canary state = %+v", state)
	}
	if len(publisher.requests) != 1 {
		t.Fatalf("publication calls = %d, want 1", len(publisher.requests))
	}
	if err := supervisor.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.requests) != 1 {
		t.Fatalf("restart republished canary: calls=%d", len(publisher.requests))
	}
}

func TestCanaryHardStopPublishesFailedEvidenceBeforeCircuit(t *testing.T) {
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1", "b2"}}}}
	engine := &fakeEngine{
		runID: "run-hard-stop",
		results: []RunResult{{
			RunID: "run-hard-stop", Code: 2, Dispatched: 1,
			HardStop: "duplicate-broad-command",
			Pressure: PressureSample{
				At: "2026-07-25T12:00:00Z", Level: "normal", AvailableMB: 4096,
			},
			Terminal: []TerminalOutcome{{
				BeadID: "b1", Status: "failed",
			}},
		}},
	}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path:     supervisor.Config.Canary.ReportPath,
		Digest:   "sha256:" + strings.Repeat("c", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:00:01Z",
	}

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(publisher.requests) != 1 || state.Canary.Decision != "failed" ||
		state.Canary.ReportDigest == "" || state.Canary.ReportGeneratedAt == "" ||
		state.Canary.HardStop != "duplicate-broad-command" ||
		len(state.Canary.Terminal) != 1 {
		t.Fatalf("hard-stop state = %+v; requests=%d", state.Canary, len(publisher.requests))
	}
}

func TestCanaryPendingInjectionIsOperatorInterventionHardStop(t *testing.T) {
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1", "b2"}}}}
	engine := &fakeEngine{}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path:     supervisor.Config.Canary.ReportPath,
		Digest:   "sha256:" + strings.Repeat("d", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:00:01Z",
	}
	if err := supervisor.Store.RequestInject("b1"); err != nil {
		t.Fatal(err)
	}

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(engine.requests) != 0 || len(publisher.requests) != 1 ||
		state.Canary.HardStop != "operator-intervention" ||
		state.Canary.Decision != "failed" {
		t.Fatalf("state=%+v engine=%d publisher=%d",
			state.Canary, len(engine.requests), len(publisher.requests))
	}
}

func TestCanaryStopAndDrainAreOperatorInterventionHardStops(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func(*Store) error
	}{
		{name: "stop", request: func(store *Store) error { return store.RequestStop() }},
		{name: "drain", request: func(store *Store) error { return store.RequestDrain() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supervisor := testSupervisor(t, &fakeObserver{}, &fakeEngine{})
			supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
			publisher := supervisor.Publisher.(*fakeCanaryPublisher)
			publisher.publication = CanaryPublication{
				Path:   supervisor.Config.Canary.ReportPath,
				Digest: "sha256:" + strings.Repeat("d", 64), Decision: "failed",
				GeneratedAt: "2026-07-25T12:00:01Z",
			}
			if err := tc.request(supervisor.Store); err != nil {
				t.Fatal(err)
			}
			err := supervisor.Run(context.Background())
			if !errors.Is(err, ErrCircuitOpen) {
				t.Fatalf("Run error = %v, want circuit open", err)
			}
			state, loadErr := supervisor.Store.LoadState()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if state.Canary.HardStop != "operator-intervention" ||
				state.Canary.Decision != "failed" || len(publisher.requests) != 1 {
				t.Fatalf("state=%+v publisher=%d", state.Canary, len(publisher.requests))
			}
		})
	}
}

func TestCanaryPublicationCrashWindowReconcilesOnRestart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hardStop string
		decision string
		wantMode string
		wantErr  bool
	}{
		{name: "terminal", decision: "passed", wantMode: ModeStopped},
		{name: "hard-stop", hardStop: "engine-invariant", decision: "failed", wantMode: ModeCircuitOpen, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supervisor := testSupervisor(t, &fakeObserver{}, &fakeEngine{})
			supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
			publisher := supervisor.Publisher.(*fakeCanaryPublisher)
			publisher.publication = CanaryPublication{
				Path:   supervisor.Config.Canary.ReportPath,
				Digest: "sha256:" + strings.Repeat("e", 64), Decision: tc.decision,
				GeneratedAt: "2026-07-25T12:00:01Z",
			}
			state := State{ProjectID: "demo", Mode: ModeRunning}
			if err := supervisor.configureCanary(&state); err != nil {
				t.Fatal(err)
			}
			state.Canary.EndedAt = "2026-07-25T12:00:00Z"
			state.Canary.HardStop = tc.hardStop
			if tc.hardStop != "" {
				state.Mode = ModeCircuitOpen
				state.CircuitReason = "canary hard stop: " + tc.hardStop
			}
			// This is the durable crash-window checkpoint: publication has
			// already created the immutable report but its returned identity
			// has not yet been saved into supervisor state.
			if err := supervisor.Store.SaveState(state); err != nil {
				t.Fatal(err)
			}

			err := supervisor.Run(context.Background())
			if tc.wantErr != errors.Is(err, ErrCircuitOpen) {
				t.Fatalf("Run error = %v, want circuit=%t", err, tc.wantErr)
			}
			reconciled, loadErr := supervisor.Store.LoadState()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if len(publisher.requests) != 1 ||
				reconciled.Mode != tc.wantMode ||
				reconciled.Canary.Decision != tc.decision ||
				reconciled.Canary.ReportDigest == "" ||
				reconciled.Canary.ReportGeneratedAt == "" {
				t.Fatalf("reconciled=%+v requests=%d", reconciled, len(publisher.requests))
			}
		})
	}
}

func TestCanaryRejectsInstalledIdentityDriftOnRestart(t *testing.T) {
	supervisor := testSupervisor(t, &fakeObserver{}, &fakeEngine{})
	spec := testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	supervisor.Config.Canary = spec
	state := State{ProjectID: "demo"}
	if err := supervisor.configureCanary(&state); err != nil {
		t.Fatal(err)
	}
	supervisor.Config.Canary.InstalledCommit = strings.Repeat("c", 40)
	if err := supervisor.configureCanary(&state); !errors.Is(err, ErrCanaryIdentityDrift) {
		t.Fatalf("configureCanary error = %v, want identity drift", err)
	}
}

func TestCanaryInactivityIdentityAndProgressSurviveRestart(t *testing.T) {
	supervisor := testSupervisor(t, &fakeObserver{}, &fakeEngine{})
	spec := testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	spec.InactivityLimit = 10 * time.Minute
	supervisor.Config.Canary = spec
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }
	state := State{ProjectID: "demo"}
	if err := supervisor.configureCanary(&state); err != nil {
		t.Fatal(err)
	}
	admittedAt := now.Format(time.RFC3339Nano)
	if state.Canary.StartedAt != admittedAt || state.Canary.ProgressAt != admittedAt ||
		state.Canary.InactivityLimitMS != (10*time.Minute).Milliseconds() {
		t.Fatalf("admitted canary = %+v", state.Canary)
	}

	now = now.Add(5 * time.Minute)
	if err := supervisor.configureCanary(&state); err != nil {
		t.Fatal(err)
	}
	if state.Canary.ProgressAt != admittedAt {
		t.Fatalf("restart refreshed progress to %q", state.Canary.ProgressAt)
	}
	spec.InactivityLimit = 20 * time.Minute
	if err := supervisor.configureCanary(&state); !errors.Is(err, ErrCanaryIdentityDrift) {
		t.Fatalf("limit drift error = %v, want identity drift", err)
	}
	spec.InactivityLimit = 10 * time.Minute

	supervisor.advanceCanary(&state, RunResult{
		PressureNormal: true,
		Terminal:       []TerminalOutcome{{BeadID: "b1", Status: "merged", Good: true}},
	})
	progressAt := now.Format(time.RFC3339Nano)
	if state.Canary.ProgressAt != progressAt {
		t.Fatalf("new terminal progress = %q, want %q", state.Canary.ProgressAt, progressAt)
	}
	now = now.Add(time.Minute)
	supervisor.advanceCanary(&state, RunResult{
		PressureNormal: true,
		Terminal:       []TerminalOutcome{{BeadID: "b1", Status: "merged", Good: true}},
	})
	if state.Canary.ProgressAt != progressAt {
		t.Fatalf("duplicate terminal refreshed progress to %q", state.Canary.ProgressAt)
	}
}

func TestIdleCanaryInactivityPublishesFailedPartialReport(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	observer := &fakeObserver{}
	observer.onWait = func(delay time.Duration) { now = now.Add(delay) }
	engine := &fakeEngine{}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.now = func() time.Time { return now }
	supervisor.Config.IdleMin = time.Minute
	supervisor.Config.IdleMax = time.Minute
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	supervisor.Config.Canary.InactivityLimit = 2 * time.Minute
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path: supervisor.Config.Canary.ReportPath, Digest: "sha256:" + strings.Repeat("f", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:02:00Z",
	}

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(engine.requests) != 0 || len(publisher.requests) != 1 ||
		state.Canary.HardStop != "canary-inactivity-timeout" ||
		state.Canary.Decision != "failed" ||
		state.Canary.EndedAt != now.Format(time.RFC3339Nano) ||
		len(publisher.requests[0].Canary.Terminal) != 0 {
		t.Fatalf("idle expiry state=%+v engine=%d requests=%+v",
			state.Canary, len(engine.requests), publisher.requests)
	}
}

func TestCanaryIdleWaitIsCappedAtInactivityDeadline(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	var waits []time.Duration
	observer := &fakeObserver{}
	observer.onWait = func(delay time.Duration) {
		waits = append(waits, delay)
		now = now.Add(delay)
	}
	supervisor := testSupervisor(t, observer, &fakeEngine{})
	supervisor.now = func() time.Time { return now }
	supervisor.Config.IdleMin = 10 * time.Minute
	supervisor.Config.IdleMax = 10 * time.Minute
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	supervisor.Config.Canary.InactivityLimit = 2 * time.Minute
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path: supervisor.Config.Canary.ReportPath, Digest: "sha256:" + strings.Repeat("4", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:02:00Z",
	}

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(waits) != 1 || waits[0] != 2*time.Minute ||
		state.Canary.HardStop != "canary-inactivity-timeout" {
		t.Fatalf("waits=%v state=%+v", waits, state.Canary)
	}
}

func TestCanaryCrashWaitIsCappedAtInactivityDeadline(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	var waits []time.Duration
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}}
	observer.onWait = func(delay time.Duration) {
		waits = append(waits, delay)
		now = now.Add(delay)
	}
	engine := &fakeEngine{
		results: []RunResult{{Code: 1, Reason: "retryable"}},
		errs:    []error{errors.New("worker crashed")},
	}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.now = func() time.Time { return now }
	supervisor.Config.CrashMin = 10 * time.Minute
	supervisor.Config.CrashMax = 10 * time.Minute
	supervisor.Config.FailureLimit = 3
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	supervisor.Config.Canary.InactivityLimit = 2 * time.Minute
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path: supervisor.Config.Canary.ReportPath, Digest: "sha256:" + strings.Repeat("5", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:02:00Z",
	}

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(waits) != 1 || waits[0] != 2*time.Minute || len(engine.requests) != 1 ||
		state.Canary.HardStop != "canary-inactivity-timeout" {
		t.Fatalf("waits=%v engine=%d state=%+v", waits, len(engine.requests), state.Canary)
	}
}

func TestActiveCanaryInactivityCancelsEngineAndPublishesPartialReport(t *testing.T) {
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}}
	engine := &fakeEngine{block: true, runID: "run-stuck"}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	timeout := make(chan time.Time, 1)
	timeout <- time.Now()
	supervisor.after = func(time.Duration) <-chan time.Time { return timeout }
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path: supervisor.Config.Canary.ReportPath, Digest: "sha256:" + strings.Repeat("0", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:02:00Z",
	}

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(engine.requests) != 1 || len(publisher.requests) != 1 ||
		state.Canary.HardStop != "canary-inactivity-timeout" ||
		state.Canary.Decision != "failed" ||
		state.LastRunID != "run-stuck" {
		t.Fatalf("active expiry state=%+v engine=%d publisher=%d",
			state, len(engine.requests), len(publisher.requests))
	}
}

func TestCanaryInactivityBoundsBlockingObservation(t *testing.T) {
	observer := &uncooperativeObserver{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer close(observer.release)
	engine := &fakeEngine{}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	supervisor.Config.Canary.InactivityLimit = 20 * time.Millisecond
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path: supervisor.Config.Canary.ReportPath, Digest: "sha256:" + strings.Repeat("7", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:02:00Z",
	}

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(engine.requests) != 0 || len(publisher.requests) != 1 ||
		state.Canary.HardStop != "canary-inactivity-timeout" ||
		state.Canary.Decision != "failed" {
		t.Fatalf("blocked observation state=%+v engine=%d publisher=%d",
			state.Canary, len(engine.requests), len(publisher.requests))
	}
}

func TestParentCancellationDuringCanaryObservationDoesNotBecomeInactivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := &blockingObserver{started: make(chan struct{}, 1)}
	supervisor := testSupervisor(t, observer, &fakeEngine{})
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	<-observer.started
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state, err := supervisor.Store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	if state.Mode != ModeStopped || state.Canary.HardStop != "" ||
		len(publisher.requests) != 0 {
		t.Fatalf("parent cancellation was misclassified: state=%+v publisher=%d",
			state, len(publisher.requests))
	}
}

func TestEngineReplyAtCanaryDeadlineRecordsEvidenceWithoutRefreshingProgress(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}}
	engine := &fakeEngine{results: []RunResult{{
		RunID: "run-deadline", Code: 0, Dispatched: 1, PressureNormal: true,
		Terminal: []TerminalOutcome{{BeadID: "b1", Status: "merged", Good: true}},
	}}}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.now = func() time.Time { return now }
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2", "b3"}, 3)
	supervisor.Config.Canary.InactivityLimit = 2 * time.Minute
	supervisor.after = func(delay time.Duration) <-chan time.Time {
		now = now.Add(delay)
		ready := make(chan time.Time, 1)
		ready <- now
		return ready
	}
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path: supervisor.Config.Canary.ReportPath, Digest: "sha256:" + strings.Repeat("8", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:02:00Z",
	}

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	admitted := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if state.Canary.HardStop != "canary-inactivity-timeout" ||
		state.Canary.ProgressAt != admitted ||
		state.Canary.Terminal["b1"].BeadID != "b1" ||
		state.Canary.ConsecutiveGood != 0 || state.Canary.CurrentWidth != 2 {
		t.Fatalf("deadline reply refreshed progress or lost evidence: %+v", state.Canary)
	}
}

func TestRestartCapsCanaryWaitToPersistedRemainingInterval(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	var waits []time.Duration
	observer := &fakeObserver{}
	observer.onWait = func(delay time.Duration) {
		waits = append(waits, delay)
		now = now.Add(delay)
	}
	supervisor := testSupervisor(t, observer, &fakeEngine{})
	supervisor.now = func() time.Time { return now }
	supervisor.Config.IdleMin = 10 * time.Minute
	supervisor.Config.IdleMax = 10 * time.Minute
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	supervisor.Config.Canary.InactivityLimit = 2 * time.Minute
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path: supervisor.Config.Canary.ReportPath, Digest: "sha256:" + strings.Repeat("6", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:02:00Z",
	}
	state := State{ProjectID: "demo"}
	if err := supervisor.configureCanary(&state); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Store.SaveState(state); err != nil {
		t.Fatal(err)
	}

	now = now.Add(90 * time.Second)
	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	if len(waits) != 1 || waits[0] != 30*time.Second ||
		publisher.requests[0].StartedAt != state.Canary.StartedAt {
		t.Fatalf("restart waits=%v request=%+v state=%+v", waits, publisher.requests, state.Canary)
	}
}

func TestRestartCannotResetExpiredCanaryInactivity(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	observer := &fakeObserver{}
	engine := &fakeEngine{}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.now = func() time.Time { return now }
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	supervisor.Config.Canary.InactivityLimit = 2 * time.Minute
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	publisher.publication = CanaryPublication{
		Path: supervisor.Config.Canary.ReportPath, Digest: "sha256:" + strings.Repeat("1", 64),
		Decision: "failed", GeneratedAt: "2026-07-25T12:02:00Z",
	}
	state := State{ProjectID: "demo"}
	if err := supervisor.configureCanary(&state); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Store.SaveState(state); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	if observer.calls != 0 || len(engine.requests) != 0 || len(publisher.requests) != 1 {
		t.Fatalf("restart resumed expired canary: observe=%d engine=%d publish=%d",
			observer.calls, len(engine.requests), len(publisher.requests))
	}
	if publisher.requests[0].StartedAt != state.Canary.StartedAt {
		t.Fatalf("restart changed admission time: request=%+v state=%+v",
			publisher.requests[0], state.Canary)
	}
}

func TestNonCanaryIdleWaitIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var waits []time.Duration
	observer := &fakeObserver{}
	observer.onWait = func(delay time.Duration) {
		waits = append(waits, delay)
		cancel()
	}
	supervisor := testSupervisor(t, observer, &fakeEngine{})
	supervisor.Config.IdleMin = 7 * time.Minute
	supervisor.Config.IdleMax = 7 * time.Minute

	if err := supervisor.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != 7*time.Minute {
		t.Fatalf("non-canary waits = %v, want unchanged 7m", waits)
	}
}

func TestCanaryFailureBudgetsHardStopAndPublishConcreteEvidence(t *testing.T) {
	tests := []struct {
		name       string
		observer   *fakeObserver
		engine     *fakeEngine
		wantDetail string
	}{
		{
			name:       "observation",
			observer:   &fakeObserver{errs: []error{errors.New("watch failed")}},
			engine:     &fakeEngine{},
			wantDetail: "observation failed: watch failed",
		},
		{
			name:       "engine",
			observer:   &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}},
			engine:     &fakeEngine{results: []RunResult{{Code: 1}}, errs: []error{errors.New("worker boom")}},
			wantDetail: "engine failed: worker boom",
		},
		{
			name:       "non-progress",
			observer:   &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}, WakeToken: "same"}}},
			engine:     &fakeEngine{results: []RunResult{{Code: 0, Reason: "selection stalled"}}},
			wantDetail: "engine made no progress while ready or recovery work remained: selection stalled",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			supervisor := testSupervisor(t, tc.observer, tc.engine)
			supervisor.Config.FailureLimit = 2
			supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
			publisher := supervisor.Publisher.(*fakeCanaryPublisher)
			publisher.publication = CanaryPublication{
				Path:   supervisor.Config.Canary.ReportPath,
				Digest: "sha256:" + strings.Repeat("2", 64), Decision: "failed",
				GeneratedAt: "2026-07-25T12:00:01Z",
			}

			err := supervisor.Run(context.Background())
			if !errors.Is(err, ErrCircuitOpen) {
				t.Fatalf("Run error = %v, want circuit open", err)
			}
			state, loadErr := supervisor.Store.LoadState()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if len(publisher.requests) != 1 || state.Canary.Decision != "failed" ||
				!strings.Contains(state.Canary.HardStop, "engine-invariant:") ||
				!strings.Contains(state.Canary.HardStop, tc.wantDetail) {
				t.Fatalf("state=%+v publication=%+v", state.Canary, publisher.requests)
			}
		})
	}
}

func TestOpenHardStopReportsPreAndPostPublicationSaveFailures(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failCall     int
		wantDetail   string
		wantDecision string
	}{
		{
			name: "pre-publication", failCall: 1,
			wantDetail: "pre-publication state save failed", wantDecision: "failed",
		},
		{
			name: "post-publication", failCall: 2,
			wantDetail: "post-publication state save failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supervisor := testSupervisor(t, &fakeObserver{}, &fakeEngine{})
			supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
			publisher := supervisor.Publisher.(*fakeCanaryPublisher)
			publisher.publication = CanaryPublication{
				Path:   supervisor.Config.Canary.ReportPath,
				Digest: "sha256:" + strings.Repeat("3", 64), Decision: "failed",
				GeneratedAt: "2026-07-25T12:00:01Z",
			}
			state := State{ProjectID: "demo"}
			if err := supervisor.configureCanary(&state); err != nil {
				t.Fatal(err)
			}
			saveCalls := 0
			supervisor.persist = func(checkpoint State) error {
				saveCalls++
				if saveCalls == tc.failCall {
					return errors.New("injected save crash")
				}
				return supervisor.Store.SaveState(checkpoint)
			}

			err := supervisor.openHardStop(context.Background(), &state, "engine-invariant: test")
			if !errors.Is(err, ErrCircuitOpen) || !strings.Contains(err.Error(), tc.wantDetail) {
				t.Fatalf("openHardStop error = %v", err)
			}
			if len(publisher.requests) != 1 {
				t.Fatalf("publication calls = %d, want 1", len(publisher.requests))
			}
			checkpoint, loadErr := supervisor.Store.LoadState()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if tc.wantDecision != "" && checkpoint.Canary.Decision != tc.wantDecision {
				t.Fatalf("durable decision = %q, want %q", checkpoint.Canary.Decision, tc.wantDecision)
			}
			if tc.failCall == 2 {
				if checkpoint.Canary.EndedAt == "" || checkpoint.Canary.Decision != "" {
					t.Fatalf("pre-publication checkpoint = %+v", checkpoint.Canary)
				}
				supervisor.persist = nil
				reconcileErr := supervisor.Run(context.Background())
				if !errors.Is(reconcileErr, ErrCircuitOpen) {
					t.Fatalf("reconciliation error = %v, want circuit open", reconcileErr)
				}
				reconciled, err := supervisor.Store.LoadState()
				if err != nil {
					t.Fatal(err)
				}
				if len(publisher.requests) != 2 || reconciled.Canary.Decision != "failed" {
					t.Fatalf("reconciled=%+v publication calls=%d",
						reconciled.Canary, len(publisher.requests))
				}
			}
		})
	}
}

func TestRestartRejectsUncheckpointedImmutableCanaryReport(t *testing.T) {
	observer := &fakeObserver{}
	engine := &fakeEngine{}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Config.Canary = testCanarySpec(t, supervisor, []string{"b1", "b2"}, 2)
	publisher := supervisor.Publisher.(*fakeCanaryPublisher)
	state := State{ProjectID: "demo"}
	if err := supervisor.configureCanary(&state); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Store.SaveState(state); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(state.Canary.ReportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"immutable":"existing-report"}`)
	if err := os.WriteFile(state.Canary.ReportPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) ||
		!strings.Contains(err.Error(), "immutable canary report exists") {
		t.Fatalf("Run error = %v, want immutable report circuit", err)
	}
	after, readErr := os.ReadFile(state.Canary.ReportPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(original) || len(publisher.requests) != 0 ||
		len(engine.requests) != 0 || observer.calls != 0 {
		t.Fatalf("restart touched immutable report or resumed: bytes=%q publisher=%d engine=%d observe=%d",
			after, len(publisher.requests), len(engine.requests), observer.calls)
	}
}

func TestLiveTripwireDrainsAndCancelsEngine(t *testing.T) {
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}}
	engine := &fakeEngine{block: true}
	drainer := &fakeDrainer{}
	events := make(chan Tripwire, 1)
	events <- Tripwire{Kind: "duplicate-broad-command", Detail: "phase b1"}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Tripwire = fakeTripwire{events: events}
	supervisor.Drainer = drainer

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	if len(drainer.reasons) == 0 || drainer.reasons[0] != "duplicate-broad-command: phase b1" {
		t.Fatalf("drain reasons = %v", drainer.reasons)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.Mode != ModeCircuitOpen {
		t.Fatalf("mode = %q", state.Mode)
	}
}

func TestImmediateTripwireStillPublishesRunID(t *testing.T) {
	observer := &fakeObserver{observations: []Observation{{ReadyIDs: []string{"b1"}}}}
	engine := &publishingEngine{events: make(chan Tripwire, 1)}
	supervisor := testSupervisor(t, observer, engine)
	supervisor.Tripwire = engine

	err := supervisor.Run(context.Background())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Run error = %v, want circuit open", err)
	}
	state, loadErr := supervisor.Store.LoadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.LastRunID != "run-fast-tripwire" {
		t.Fatalf("hard-stop state lost run identity: %+v", state)
	}
}

func TestAutonomousRejectsAllowUnvalidated(t *testing.T) {
	supervisor := testSupervisor(t, &fakeObserver{}, &fakeEngine{})
	supervisor.Config.AllowUnvalidated = true
	if err := supervisor.Run(context.Background()); !errors.Is(err, ErrUnvalidated) {
		t.Fatalf("Run error = %v, want ErrUnvalidated", err)
	}
}

func TestStoreControlAndStructuredNotifier(t *testing.T) {
	repo := t.TempDir()
	store := NewStore(repo)
	if err := store.RequestInject("b1"); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestInject("b1"); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestDrain(); err != nil {
		t.Fatal(err)
	}
	control, err := store.LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if !control.Drain || len(control.Inject) != 1 || control.Inject[0] != "b1" {
		t.Fatalf("control = %+v", control)
	}

	notifier := JSONLNotifier{Store: store}
	if err := notifier.Emit(context.Background(), Alert{ProjectID: "demo", Level: "error", Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.AlertPath())
	if err != nil {
		t.Fatal(err)
	}
	var alert Alert
	if err := json.Unmarshal(data[:len(data)-1], &alert); err != nil {
		t.Fatal(err)
	}
	if alert.SchemaVersion != SchemaVersion || alert.Kind != "test" {
		t.Fatalf("alert = %+v", alert)
	}
	if entries, err := os.ReadDir(repo); err != nil || len(entries) != 1 || entries[0].Name() != ".koryph" {
		t.Fatalf("notifier/control touched paths outside .koryph: entries=%v err=%v", entries, err)
	}
}

func TestStoreRejectsNewerSchema(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := os.MkdirAll(store.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.StatePath(), []byte(`{"schema_version":99,"project_id":"demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadState(); err == nil {
		t.Fatal("LoadState accepted a newer schema")
	}
}

func TestStoreRejectsNoncanonicalStateAndControlAliases(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := os.MkdirAll(store.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		store.StatePath(),
		[]byte(`{"schema_version":2,"Schema_Version":2,"project_id":"demo"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadState(); err == nil ||
		!strings.Contains(err.Error(), "case-fold-colliding JSON object keys") {
		t.Fatalf("state alias error = %v", err)
	}
	if err := os.WriteFile(
		store.ControlPath(),
		[]byte(`{"schema_version":2,"stop":false,"Stop":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadControl(); err == nil ||
		!strings.Contains(err.Error(), "case-fold-colliding JSON object keys") {
		t.Fatalf("control alias error = %v", err)
	}
}
