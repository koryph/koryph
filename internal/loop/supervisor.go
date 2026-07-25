// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Supervisor is the binary-native autonomous control plane.
type Supervisor struct {
	Config     Config
	Store      *Store
	Observer   Observer
	Engine     Engine
	Tripwire   TripwireSource
	Drainer    Drainer
	Maintainer Maintainer
	Notifier   Notifier
	Publisher  CanaryPublisher

	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	after   func(time.Duration) <-chan time.Time
	persist func(State) error
}

type engineReply struct {
	result RunResult
	err    error
}

func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	lock, err := s.Store.Acquire()
	if err != nil {
		return err
	}
	defer lock.Unlock() //nolint:errcheck

	state, err := s.loadState()
	if err != nil {
		return err
	}
	if err := s.configureCanary(&state); err != nil {
		return err
	}
	if err := s.rejectUncheckpointedCanaryReport(&state); err != nil {
		return err
	}
	if state.Canary != nil && state.Canary.EndedAt != "" && state.Canary.Decision == "" {
		return s.reconcileCanaryPublication(ctx, &state)
	}
	if state.Mode == ModeCircuitOpen {
		return fmt.Errorf("%w: %s", ErrCircuitOpen, state.CircuitReason)
	}
	if state.Canary != nil && state.Canary.Decision != "" {
		// A bounded canary is a one-shot release gate. Status/doctor may inspect
		// its immutable result, but restarting the supervisor cannot extend or
		// overwrite the admitted cohort.
		return nil
	}

	now := s.clock()
	state.Mode = ModeStarting
	state.PID = os.Getpid()
	if state.StartedAt == "" {
		state.StartedAt = now.Format(time.RFC3339Nano)
	}
	if err := s.persistOrHardStop(ctx, &state, "starting checkpoint"); err != nil {
		return err
	}

	idle := s.Config.IdleMin
	for {
		if err := ctx.Err(); err != nil {
			return s.stop(&state, "context-cancelled")
		}
		if s.canaryInactivityExpired(state) {
			return s.openHardStop(ctx, &state, "canary-inactivity-timeout")
		}
		control, err := s.Store.LoadControl()
		if err != nil {
			return s.failSupervisor(&state, "control-read", err)
		}
		if state.Canary != nil && (control.Stop || control.Drain || len(control.Inject) > 0) {
			return s.openHardStop(ctx, &state, "operator-intervention")
		}
		if control.Stop || control.Drain {
			reason := "operator-stop"
			if control.Drain && !control.Stop {
				reason = "operator-drain"
			}
			return s.drainAndStop(ctx, &state, reason)
		}
		scope := Scope{
			Parent:            s.Config.Parent,
			PendingInjections: append([]string(nil), control.Inject...),
		}
		if state.Canary != nil {
			scope.FixedCohort = append([]string(nil), state.Canary.Cohort...)
		}
		observation, err, observationDeadlineExpired := s.observe(ctx, state, scope)
		state.LastObserved = s.clock().Format(time.RFC3339Nano)
		if ctx.Err() != nil {
			return s.stop(&state, "context-cancelled")
		}
		if observationDeadlineExpired || s.canaryInactivityExpired(state) {
			return s.openHardStop(ctx, &state, "canary-inactivity-timeout")
		}
		if err != nil {
			fingerprint := "observe:" + err.Error()
			if failureErr := s.recordAndCheckpointFailure(
				ctx, &state, fingerprint, "observation failed: "+err.Error(),
			); failureErr != nil {
				return failureErr
			}
			s.alert(ctx, "error", "observation-failed", err.Error(), nil)
			delay, expired := s.boundedCanaryWaitDelay(
				state, s.crashDelay(state.IdenticalFailures),
			)
			if expired {
				return s.openHardStop(ctx, &state, "canary-inactivity-timeout")
			}
			if err := s.wait(ctx, scope, observation, delay); err != nil {
				return s.stop(&state, "context-cancelled")
			}
			continue
		}
		if observation.DrainRequested {
			return s.drainAndStop(ctx, &state, "operator-drain")
		}

		only := firstReadyInjection(control.Inject, observation.ReadyIDs)
		if !observation.Recovery && !observation.Reconcile && len(observation.ReadyIDs) == 0 {
			enteringIdle := state.Mode != ModeIdle
			state.Mode = ModeIdle
			state.CurrentRunID = ""
			state.IdleBackoffMS = idle.Milliseconds()
			if err := s.persistOrHardStop(ctx, &state, "idle checkpoint"); err != nil {
				return err
			}
			if enteringIdle {
				s.maintain(ctx, BoundaryIdle)
			}
			delay, expired := s.boundedCanaryWaitDelay(state, idle)
			if expired {
				return s.openHardStop(ctx, &state, "canary-inactivity-timeout")
			}
			if err := s.wait(ctx, scope, observation, delay); err != nil {
				return s.stop(&state, "context-cancelled")
			}
			idle = grow(idle, s.Config.IdleMax)
			continue
		}

		idle = s.Config.IdleMin
		state.Mode = ModeRunning
		state.CurrentRunID = observation.RecoveryRunID
		state.IdleBackoffMS = 0
		if err := s.persistOrHardStop(ctx, &state, "running checkpoint"); err != nil {
			return err
		}
		req := RunRequest{
			Resume:        observation.Recovery,
			RecoveryRunID: observation.RecoveryRunID,
			Only:          only,
			Max:           s.width(state),
		}
		if state.Canary != nil {
			req.AllowedIDs = append([]string(nil), state.Canary.Cohort...)
			req.AuthoritativeWidth = true
			req.HardStops = append([]string(nil), state.Canary.HardStops...)
		}
		inactivityRemaining, inactivityBounded := s.canaryInactivityRemaining(state)
		if inactivityBounded && inactivityRemaining <= 0 {
			return s.openHardStop(ctx, &state, "canary-inactivity-timeout")
		}
		var runStartCheckpointErr error
		result, hardStop, runErr := s.runEngine(
			ctx, req, inactivityRemaining, func(runID string) error {
				if runID == "" {
					return errors.New("loop: engine published an empty run id")
				}
				state.CurrentRunID = runID
				s.recordCanaryRun(&state, runID)
				runStartCheckpointErr = s.persistState(state)
				return runStartCheckpointErr
			})
		activeRunID := state.CurrentRunID
		state.CurrentRunID = ""
		// A cancelled engine may return before constructing its terminal
		// Outcome, but the start publication is still authoritative.
		if result.RunID == "" {
			result.RunID = activeRunID
		}
		state.LastRunID = result.RunID
		state.LastCode = result.Code
		state.LastReason = result.Reason
		s.recordCanaryRun(&state, result.RunID)
		s.recordCanaryPressure(&state, result.RunID, result.Pressure)

		if hardStop == "canary-inactivity-timeout" ||
			(state.Canary != nil && s.canaryInactivityExpired(state)) {
			result.HardStop = "canary-inactivity-timeout"
			s.recordCanaryTerminals(&state, result, false)
			return s.openHardStop(ctx, &state, "canary-inactivity-timeout")
		}
		if hardStop != "" {
			result.HardStop = hardStop
			s.advanceCanary(&state, result)
			return s.openHardStop(ctx, &state, hardStop)
		}
		if result.HardStop != "" {
			s.advanceCanary(&state, result)
			return s.openHardStop(ctx, &state, result.HardStop)
		}
		if runStartCheckpointErr != nil {
			s.advanceCanary(&state, result)
			return s.openHardStop(ctx, &state,
				"engine-invariant: run-start checkpoint failed: "+runStartCheckpointErr.Error())
		}
		s.maintain(ctx, BoundaryTerminal)
		if runErr != nil || result.Code == 1 || result.Code == 2 {
			fingerprint := failureFingerprint(result, runErr)
			if failureErr := s.recordAndCheckpointFailure(
				ctx, &state, fingerprint, "engine failed: "+errorMessage(result, runErr),
			); failureErr != nil {
				return failureErr
			}
			s.alert(ctx, "error", "engine-failed", errorMessage(result, runErr), map[string]any{
				"code": result.Code, "run_id": result.RunID,
				"identical_failures": state.IdenticalFailures,
			})
			delay, expired := s.boundedCanaryWaitDelay(
				state, s.crashDelay(state.IdenticalFailures),
			)
			if expired {
				return s.openHardStop(ctx, &state, "canary-inactivity-timeout")
			}
			if err := s.wait(ctx, scope, observation, delay); err != nil {
				return s.stop(&state, "context-cancelled")
			}
			continue
		}
		if result.Dispatched == 0 && len(result.Terminal) == 0 &&
			(observation.Recovery || observation.Reconcile || len(observation.ReadyIDs) > 0) {
			fingerprint := nonProgressFingerprint(observation, result)
			detail := "engine made no progress while ready or recovery work remained"
			if result.Reason != "" {
				detail += ": " + result.Reason
			}
			if failureErr := s.recordAndCheckpointFailure(
				ctx, &state, fingerprint, detail,
			); failureErr != nil {
				return failureErr
			}
			s.alert(ctx, "error", "engine-non-progress",
				"engine returned success without dispatch or terminal evidence while work remained",
				map[string]any{"run_id": result.RunID, "reason": result.Reason})
			delay, expired := s.boundedCanaryWaitDelay(
				state, s.crashDelay(state.IdenticalFailures),
			)
			if expired {
				return s.openHardStop(ctx, &state, "canary-inactivity-timeout")
			}
			if err := s.wait(ctx, scope, observation, delay); err != nil {
				return s.stop(&state, "context-cancelled")
			}
			continue
		}

		state.FailureFingerprint = ""
		state.IdenticalFailures = 0
		state.ConsecutiveFailures = 0
		if only != "" && result.Dispatched > 0 {
			_ = s.Store.AcknowledgeInjection(only)
		}
		complete := s.advanceCanary(&state, result)
		if err := s.persistOrHardStop(ctx, &state, "terminal checkpoint"); err != nil {
			return err
		}
		if state.Canary != nil && state.Canary.HardStop != "" {
			return s.openHardStop(ctx, &state, state.Canary.HardStop)
		}
		if complete {
			return s.publishCanary(ctx, &state)
		}
		if result.Reason == "operator-drain" {
			_ = s.Store.ClearControl()
			s.acknowledgeDrain()
			return s.stop(&state, "operator-drain")
		}
	}
}

func (s *Supervisor) validate() error {
	if s.Config.AllowUnvalidated {
		return ErrUnvalidated
	}
	if s.Config.ProjectID == "" {
		return errors.New("loop: project id is required")
	}
	if s.Store == nil || s.Observer == nil || s.Engine == nil {
		return errors.New("loop: store, observer, and engine are required")
	}
	if s.Config.IdleMin <= 0 {
		s.Config.IdleMin = time.Second
	}
	if s.Config.IdleMax < s.Config.IdleMin {
		s.Config.IdleMax = time.Minute
	}
	if s.Config.ControlPoll <= 0 {
		s.Config.ControlPoll = time.Second
	}
	if s.Config.CrashMin <= 0 {
		s.Config.CrashMin = 2 * time.Second
	}
	if s.Config.CrashMax < s.Config.CrashMin {
		s.Config.CrashMax = 30 * time.Second
	}
	if s.Config.FailureLimit <= 0 {
		s.Config.FailureLimit = 3
	}
	if s.Config.CrashLimit < s.Config.FailureLimit {
		s.Config.CrashLimit = 2 * s.Config.FailureLimit
	}
	if s.Notifier == nil {
		s.Notifier = JSONLNotifier{Store: s.Store}
	}
	return nil
}

func (s *Supervisor) loadState() (State, error) {
	state, err := s.Store.LoadState()
	if err != nil {
		return State{}, err
	}
	if state.ProjectID != "" && state.ProjectID != s.Config.ProjectID {
		return State{}, fmt.Errorf("loop: state belongs to project %q, not %q", state.ProjectID, s.Config.ProjectID)
	}
	state.ProjectID = s.Config.ProjectID
	state.SchemaVersion = SchemaVersion
	return state, nil
}

func (s *Supervisor) configureCanary(state *State) error {
	if s.Config.Canary == nil {
		if state.Canary != nil {
			return ErrCanaryCohortDrift
		}
		return nil
	}
	cohort := normalizedCohort(s.Config.Canary.Cohort)
	if len(cohort) < 2 {
		return errors.New("loop: canary cohort must contain at least two beads")
	}
	target := s.Config.Canary.TargetWidth
	if target < 2 {
		return errors.New("loop: canary target width must be at least 2")
	}
	if target > len(cohort) {
		return errors.New("loop: canary target width exceeds fixed cohort size")
	}
	digest := cohortDigest(cohort)
	hardStops := normalizedHardStops(s.Config.Canary.HardStops)
	inactivityLimit := s.Config.Canary.InactivityLimit
	if inactivityLimit <= 0 {
		inactivityLimit = DefaultCanaryInactivityLimit
	}
	inactivityLimitMS := inactivityLimit.Milliseconds()
	if inactivityLimitMS <= 0 {
		return errors.New("loop: canary inactivity limit must be at least one millisecond")
	}
	commit := strings.TrimSpace(s.Config.Canary.InstalledCommit)
	binaryVersion := strings.TrimSpace(s.Config.Canary.BinaryVersion)
	buildIdentity := strings.TrimSpace(s.Config.Canary.BuildIdentity)
	contractDigest := strings.TrimSpace(s.Config.Canary.ContractDigest)
	reportPath := filepath.Clean(strings.TrimSpace(s.Config.Canary.ReportPath))
	if !validCommit(commit) || binaryVersion == "" || buildIdentity == "" ||
		!validDigest(contractDigest) || !filepath.IsAbs(reportPath) ||
		s.Publisher == nil {
		return errors.New("loop: canary requires a clean installed commit, binary version, authenticated contract digest, absolute report path, and evidence publisher")
	}
	if state.Canary != nil {
		stored := normalizedCohort(state.Canary.Cohort)
		if len(stored) != len(state.Canary.Cohort) ||
			cohortDigest(stored) != state.Canary.CohortDigest ||
			state.Canary.CohortDigest != digest ||
			state.Canary.TargetWidth != target ||
			state.Canary.CurrentWidth < 2 ||
			state.Canary.CurrentWidth > target ||
			!equalStrings(normalizedHardStops(state.Canary.HardStops), hardStops) {
			return ErrCanaryCohortDrift
		}
		startedAt, startedErr := time.Parse(time.RFC3339Nano, state.Canary.StartedAt)
		progressAt, progressErr := time.Parse(time.RFC3339Nano, state.Canary.ProgressAt)
		if startedErr != nil || progressErr != nil || progressAt.Before(startedAt) ||
			state.Canary.InactivityLimitMS != inactivityLimitMS {
			return ErrCanaryIdentityDrift
		}
		if state.Canary.InstalledCommit != commit ||
			state.Canary.BinaryVersion != binaryVersion ||
			state.Canary.BuildIdentity != buildIdentity ||
			state.Canary.ContractDigest != contractDigest ||
			filepath.Clean(state.Canary.ReportPath) != reportPath {
			return ErrCanaryIdentityDrift
		}
		if state.Canary.Terminal == nil {
			state.Canary.Terminal = make(map[string]TerminalOutcome)
		}
		return nil
	}
	if _, err := os.Lstat(reportPath); err == nil {
		return fmt.Errorf("%w: report already exists at %s", ErrCanaryIdentityDrift, reportPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	admittedAt := s.clock().Format(time.RFC3339Nano)
	state.Canary = &CanaryState{
		CohortDigest:      digest,
		Cohort:            cohort,
		StartedAt:         admittedAt,
		ProgressAt:        admittedAt,
		InactivityLimitMS: inactivityLimitMS,
		TargetWidth:       target,
		CurrentWidth:      2,
		HardStops:         hardStops,
		InstalledCommit:   commit,
		BinaryVersion:     binaryVersion,
		BuildIdentity:     buildIdentity,
		ContractDigest:    contractDigest,
		ReportPath:        reportPath,
		Terminal:          make(map[string]TerminalOutcome),
	}
	return nil
}

func (s *Supervisor) width(state State) int {
	if state.Canary != nil {
		return state.Canary.CurrentWidth
	}
	return s.Config.Max
}

func (s *Supervisor) runEngine(
	ctx context.Context,
	req RunRequest,
	inactivityRemaining time.Duration,
	onRunStart func(string) error,
) (RunResult, string, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reply := make(chan engineReply, 1)
	req.OnRunStart = func(runID string) error {
		if onRunStart == nil {
			return nil
		}
		return onRunStart(runID)
	}
	// Subscribe before starting the engine. A hard stop emitted during setup
	// must not race past a subscriber that is registered only after launch.
	var events <-chan Tripwire
	if s.Tripwire != nil {
		events = s.Tripwire.Events(runCtx)
	}
	var inactivity <-chan time.Time
	var inactivityTimer *time.Timer
	if inactivityRemaining > 0 {
		if s.after != nil {
			inactivity = s.after(inactivityRemaining)
		} else {
			inactivityTimer = time.NewTimer(inactivityRemaining)
			inactivity = inactivityTimer.C
			defer inactivityTimer.Stop()
		}
	}
	go func() {
		result, err := s.Engine.Run(runCtx, req)
		reply <- engineReply{result: result, err: err}
	}()
	for {
		select {
		case r := <-reply:
			select {
			case event, ok := <-events:
				if ok {
					reason := event.Kind
					if event.Detail != "" {
						reason += ": " + event.Detail
					}
					if s.Drainer != nil {
						_ = s.Drainer.Drain(context.Background(), reason)
					}
					return r.result, reason, r.err
				}
			default:
			}
			return r.result, "", r.err
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			reason := event.Kind
			if event.Detail != "" {
				reason += ": " + event.Detail
			}
			if s.Drainer != nil {
				_ = s.Drainer.Drain(context.Background(), reason)
			}
			cancel()
			r := <-reply
			return r.result, reason, r.err
		case <-inactivity:
			const reason = "canary-inactivity-timeout"
			if s.Drainer != nil {
				_ = s.Drainer.Drain(context.Background(), reason)
			}
			cancel()
			r := <-reply
			return r.result, reason, r.err
		}
	}
}

func (s *Supervisor) observe(
	ctx context.Context,
	state State,
	scope Scope,
) (Observation, error, bool) {
	remaining, bounded := s.canaryInactivityRemaining(state)
	if !bounded {
		observation, err := s.Observer.Observe(ctx, scope)
		return observation, err, false
	}
	if remaining <= 0 {
		return Observation{}, context.DeadlineExceeded, true
	}
	observeCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	type reply struct {
		observation Observation
		err         error
	}
	replies := make(chan reply, 1)
	go func() {
		observation, err := s.Observer.Observe(observeCtx, scope)
		replies <- reply{observation: observation, err: err}
	}()
	select {
	case result := <-replies:
		deadlineExpired := ctx.Err() == nil &&
			errors.Is(observeCtx.Err(), context.DeadlineExceeded)
		return result.observation, result.err, deadlineExpired
	case <-observeCtx.Done():
		return Observation{}, observeCtx.Err(),
			ctx.Err() == nil && errors.Is(observeCtx.Err(), context.DeadlineExceeded)
	}
}

func (s *Supervisor) advanceCanary(state *State, result RunResult) bool {
	return s.recordCanaryTerminals(state, result, true)
}

func (s *Supervisor) recordCanaryTerminals(
	state *State,
	result RunResult,
	refreshProgress bool,
) bool {
	if state.Canary == nil {
		return false
	}
	progressed := false
	for _, terminal := range result.Terminal {
		if !containsString(state.Canary.Cohort, terminal.BeadID) {
			state.Canary.HardStop = "cohort-admission-violation"
			continue
		}
		if _, recorded := state.Canary.Terminal[terminal.BeadID]; recorded {
			continue
		}
		state.Canary.Terminal[terminal.BeadID] = terminal
		progressed = true
		if !refreshProgress {
			continue
		}
		if terminal.Good && result.PressureNormal && result.HardStop == "" {
			state.Canary.ConsecutiveGood++
			if state.Canary.ConsecutiveGood >= 5 && state.Canary.CurrentWidth < state.Canary.TargetWidth {
				state.Canary.CurrentWidth++
				state.Canary.ConsecutiveGood = 0
				s.alert(context.Background(), "info", "canary-width-increased", "five consecutive good terminal outcomes", map[string]any{
					"width": state.Canary.CurrentWidth,
				})
			}
		} else {
			state.Canary.ConsecutiveGood = 0
		}
	}
	if progressed && refreshProgress {
		state.Canary.ProgressAt = s.clock().Format(time.RFC3339Nano)
	}
	return len(state.Canary.Terminal) == len(state.Canary.Cohort)
}

func (s *Supervisor) recordCanaryRun(state *State, runID string) {
	if state == nil || state.Canary == nil || strings.TrimSpace(runID) == "" ||
		containsString(state.Canary.RunIDs, runID) {
		return
	}
	state.Canary.RunIDs = append(state.Canary.RunIDs, runID)
}

func (s *Supervisor) recordCanaryPressure(state *State, runID string, sample PressureSample) {
	if state == nil || state.Canary == nil || strings.TrimSpace(runID) == "" ||
		sample.At == "" || sample.Level == "" {
		return
	}
	sample.RunID = runID
	// One engine boundary produces one sample. A replay of the same completed
	// run during recovery must not duplicate it.
	for _, existing := range state.Canary.Pressure {
		if existing.RunID == runID {
			return
		}
	}
	state.Canary.Pressure = append(state.Canary.Pressure, sample)
}

func (s *Supervisor) publishCanary(ctx context.Context, state *State) error {
	if state == nil || state.Canary == nil {
		return errors.New("loop: cannot publish absent canary state")
	}
	ended := s.clock().Format(time.RFC3339Nano)
	state.Canary.EndedAt = ended
	if err := s.persistState(*state); err != nil {
		return s.openHardStop(ctx, state,
			"engine-invariant: canary terminal checkpoint failed: "+err.Error())
	}
	publication, err := s.Publisher.Publish(ctx, CanaryPublicationRequest{
		ProjectID: state.ProjectID,
		StartedAt: state.Canary.StartedAt,
		EndedAt:   ended,
		Canary:    *state.Canary,
	})
	if err != nil {
		return s.failSupervisor(state, "canary-publication", err)
	}
	state.Canary.Decision = publication.Decision
	state.Canary.ReportDigest = publication.Digest
	state.Canary.ReportGeneratedAt = publication.GeneratedAt
	state.Canary.ReportPath = publication.Path
	state.Canary.PublishedAt = s.clock().Format(time.RFC3339Nano)
	if publication.Decision != "passed" {
		state.Mode = ModeCircuitOpen
		state.CircuitReason = "autonomy canary report failed"
		if err := s.persistState(*state); err != nil {
			return s.openHardStop(ctx, state,
				"engine-invariant: canary decision checkpoint failed: "+err.Error())
		}
		return fmt.Errorf("%w: %s", ErrCircuitOpen, state.CircuitReason)
	}
	if err := s.stop(state, "canary-complete"); err != nil {
		return s.openHardStop(ctx, state,
			"engine-invariant: canary completion checkpoint failed: "+err.Error())
	}
	return nil
}

func (s *Supervisor) reconcileCanaryPublication(ctx context.Context, state *State) error {
	publication, err := s.Publisher.Publish(ctx, CanaryPublicationRequest{
		ProjectID: state.ProjectID,
		StartedAt: state.Canary.StartedAt,
		EndedAt:   state.Canary.EndedAt,
		Canary:    *state.Canary,
	})
	if err != nil {
		return s.failSupervisor(state, "canary-publication-reconciliation", err)
	}
	state.Canary.Decision = publication.Decision
	state.Canary.ReportDigest = publication.Digest
	state.Canary.ReportGeneratedAt = publication.GeneratedAt
	state.Canary.ReportPath = publication.Path
	state.Canary.PublishedAt = s.clock().Format(time.RFC3339Nano)
	if state.Canary.HardStop != "" {
		state.Mode = ModeCircuitOpen
		if state.CircuitReason == "" {
			state.CircuitReason = "canary hard stop: " + state.Canary.HardStop
		}
		if publication.Decision == "passed" {
			state.CircuitReason += "; publisher returned an invalid passing hard-stop report"
		}
		if err := s.persistState(*state); err != nil {
			return s.openHardStop(ctx, state,
				"engine-invariant: hard-stop reconciliation checkpoint failed: "+err.Error())
		}
		return fmt.Errorf("%w: %s", ErrCircuitOpen, state.CircuitReason)
	}
	if publication.Decision != "passed" {
		state.Mode = ModeCircuitOpen
		state.CircuitReason = "autonomy canary report failed"
		if err := s.persistState(*state); err != nil {
			return s.openHardStop(ctx, state,
				"engine-invariant: failed reconciliation checkpoint failed: "+err.Error())
		}
		return fmt.Errorf("%w: %s", ErrCircuitOpen, state.CircuitReason)
	}
	if err := s.stop(state, "canary-complete"); err != nil {
		return s.openHardStop(ctx, state,
			"engine-invariant: reconciliation completion checkpoint failed: "+err.Error())
	}
	return nil
}

func (s *Supervisor) openHardStop(ctx context.Context, state *State, reason string) error {
	if state == nil {
		return fmt.Errorf("%w: canary hard stop without supervisor state: %s", ErrCircuitOpen, reason)
	}
	if s.Drainer != nil {
		_ = s.Drainer.Drain(ctx, reason)
	}
	state.Mode = ModeCircuitOpen
	state.CircuitReason = "canary hard stop: " + reason
	if state.Canary != nil {
		state.Canary.HardStop = reason
		if state.Canary.Decision == "" {
			if state.Canary.EndedAt == "" {
				state.Canary.EndedAt = s.clock().Format(time.RFC3339Nano)
			}
			if err := s.persistState(*state); err != nil {
				appendCircuitDetail(state, "pre-publication state save failed: "+err.Error())
			}
			if s.Publisher == nil {
				appendCircuitDetail(state, "failed report publication: evidence publisher is unavailable")
			} else {
				publication, err := s.Publisher.Publish(ctx, CanaryPublicationRequest{
					ProjectID: state.ProjectID,
					StartedAt: state.Canary.StartedAt,
					EndedAt:   state.Canary.EndedAt,
					Canary:    *state.Canary,
				})
				if err != nil {
					appendCircuitDetail(state, "failed report publication: "+err.Error())
				} else {
					state.Canary.Decision = publication.Decision
					state.Canary.ReportDigest = publication.Digest
					state.Canary.ReportGeneratedAt = publication.GeneratedAt
					state.Canary.ReportPath = publication.Path
					state.Canary.PublishedAt = s.clock().Format(time.RFC3339Nano)
					if publication.Decision == "passed" {
						appendCircuitDetail(state, "publisher returned an invalid passing hard-stop report")
					}
				}
			}
		}
	}
	if err := s.persistState(*state); err != nil {
		appendCircuitDetail(state, "post-publication state save failed: "+err.Error())
	}
	s.alert(ctx, "error", "canary-hard-stop", state.CircuitReason, nil)
	return fmt.Errorf("%w: %s", ErrCircuitOpen, state.CircuitReason)
}

func (s *Supervisor) recordFailure(state *State, fingerprint, detail string) bool {
	state.ConsecutiveFailures++
	if state.FailureFingerprint == fingerprint {
		state.IdenticalFailures++
	} else {
		state.FailureFingerprint = fingerprint
		state.IdenticalFailures = 1
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		detail = fingerprint
	}
	switch {
	case state.IdenticalFailures >= s.Config.FailureLimit:
		state.Mode = ModeCircuitOpen
		state.CircuitReason = fmt.Sprintf(
			"repeated identical engine failure (%d): %s [fingerprint %s]",
			state.IdenticalFailures, detail, fingerprint,
		)
	case state.ConsecutiveFailures >= s.Config.CrashLimit:
		state.Mode = ModeCircuitOpen
		state.CircuitReason = fmt.Sprintf(
			"bounded crash restart exhausted after %d consecutive failures: %s [fingerprint %s]",
			state.ConsecutiveFailures, detail, fingerprint,
		)
	}
	return state.Mode == ModeCircuitOpen
}

func (s *Supervisor) recordAndCheckpointFailure(
	ctx context.Context,
	state *State,
	fingerprint string,
	detail string,
) error {
	exhausted := s.recordFailure(state, fingerprint, detail)
	if exhausted && state.Canary != nil {
		return s.openHardStop(ctx, state, "engine-invariant: "+state.CircuitReason)
	}
	if err := s.persistState(*state); err != nil {
		if state.Canary != nil {
			return s.openHardStop(ctx, state,
				"engine-invariant: failure checkpoint failed: "+err.Error())
		}
		return err
	}
	if exhausted {
		s.alert(ctx, "error", "supervisor-circuit-open", state.CircuitReason, nil)
		return fmt.Errorf("%w: %s", ErrCircuitOpen, state.CircuitReason)
	}
	return nil
}

func (s *Supervisor) drainAndStop(ctx context.Context, state *State, reason string) error {
	state.Mode = ModeDraining
	if err := s.persistState(*state); err != nil {
		return err
	}
	if s.Drainer != nil {
		if err := s.Drainer.Drain(ctx, reason); err != nil {
			return err
		}
	}
	_ = s.Store.ClearControl()
	s.acknowledgeDrain()
	return s.stop(state, reason)
}

func (s *Supervisor) acknowledgeDrain() {
	if acknowledger, ok := s.Drainer.(DrainAcknowledger); ok {
		acknowledger.AcknowledgeDrain()
	}
}

func (s *Supervisor) failSupervisor(state *State, kind string, err error) error {
	state.Mode = ModeCircuitOpen
	state.CircuitReason = kind + ": " + err.Error()
	if saveErr := s.persistState(*state); saveErr != nil {
		appendCircuitDetail(state, "state save failed: "+saveErr.Error())
	}
	s.alert(context.Background(), "error", kind, err.Error(), nil)
	return fmt.Errorf("%w: %s", ErrCircuitOpen, state.CircuitReason)
}

func (s *Supervisor) stop(state *State, reason string) error {
	state.Mode = ModeStopped
	state.PID = 0
	state.CurrentRunID = ""
	state.LastReason = reason
	return s.persistState(*state)
}

func (s *Supervisor) persistState(state State) error {
	if s.persist != nil {
		return s.persist(state)
	}
	return s.Store.SaveState(state)
}

func (s *Supervisor) persistOrHardStop(ctx context.Context, state *State, stage string) error {
	if err := s.persistState(*state); err != nil {
		if state.Canary != nil {
			return s.openHardStop(ctx, state,
				"engine-invariant: "+stage+" failed: "+err.Error())
		}
		return err
	}
	return nil
}

func (s *Supervisor) canaryInactivityExpired(state State) bool {
	remaining, bounded := s.canaryInactivityRemaining(state)
	return bounded && remaining <= 0
}

func (s *Supervisor) canaryInactivityRemaining(state State) (time.Duration, bool) {
	if state.Canary == nil || state.Canary.Decision != "" || state.Canary.EndedAt != "" {
		return 0, false
	}
	progressAt, err := time.Parse(time.RFC3339Nano, state.Canary.ProgressAt)
	if err != nil || state.Canary.InactivityLimitMS <= 0 {
		return 0, true
	}
	expiresAt := progressAt.Add(time.Duration(state.Canary.InactivityLimitMS) * time.Millisecond)
	return expiresAt.Sub(s.clock()), true
}

func (s *Supervisor) boundedCanaryWaitDelay(
	state State,
	requested time.Duration,
) (time.Duration, bool) {
	remaining, bounded := s.canaryInactivityRemaining(state)
	if !bounded {
		return requested, false
	}
	if remaining <= 0 {
		return 0, true
	}
	if requested > remaining {
		return remaining, false
	}
	return requested, false
}

func (s *Supervisor) rejectUncheckpointedCanaryReport(state *State) error {
	if state == nil || state.Canary == nil || state.Canary.Decision != "" ||
		state.Canary.EndedAt != "" {
		return nil
	}
	_, err := os.Lstat(state.Canary.ReportPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	reason := "engine-invariant: immutable canary report exists without a durable publication checkpoint"
	if err != nil {
		reason = "engine-invariant: cannot prove the canary report path is absent: " + err.Error()
	}
	state.Mode = ModeCircuitOpen
	state.Canary.HardStop = reason
	state.CircuitReason = "canary hard stop: " + reason
	if saveErr := s.persistState(*state); saveErr != nil {
		appendCircuitDetail(state, "state save failed: "+saveErr.Error())
	}
	s.alert(context.Background(), "error", "canary-report-checkpoint-mismatch", state.CircuitReason, nil)
	return fmt.Errorf("%w: %s", ErrCircuitOpen, state.CircuitReason)
}

func appendCircuitDetail(state *State, detail string) {
	if state == nil || strings.TrimSpace(detail) == "" {
		return
	}
	if state.CircuitReason == "" {
		state.CircuitReason = detail
		return
	}
	state.CircuitReason += "; " + detail
}

func (s *Supervisor) wait(ctx context.Context, scope Scope, observation Observation, delay time.Duration) error {
	if waiter, ok := s.Observer.(WaitObserver); ok {
		return waiter.Wait(ctx, scope, observation, delay)
	}
	if s.sleep != nil {
		return s.sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Supervisor) crashDelay(failures int) time.Duration {
	delay := s.Config.CrashMin
	for i := 1; i < failures && delay < s.Config.CrashMax; i++ {
		delay *= 2
	}
	if delay > s.Config.CrashMax {
		delay = s.Config.CrashMax
	}
	return delay
}

func (s *Supervisor) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Supervisor) alert(ctx context.Context, level, kind, message string, evidence map[string]any) {
	if s.Notifier == nil {
		return
	}
	_ = s.Notifier.Emit(ctx, Alert{
		SchemaVersion: SchemaVersion,
		At:            s.clock().Format(time.RFC3339Nano),
		ProjectID:     s.Config.ProjectID,
		Level:         level,
		Kind:          kind,
		Message:       message,
		Evidence:      evidence,
	})
}

func (s *Supervisor) maintain(ctx context.Context, boundary Boundary) {
	if s.Maintainer == nil {
		return
	}
	if err := s.Maintainer.Maintain(ctx, boundary); err != nil {
		s.alert(ctx, "warn", "maintenance-failed", err.Error(), map[string]any{
			"boundary": string(boundary),
		})
	}
}

func normalizedCohort(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func cohortDigest(ids []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(sum[:])
}

func normalizedHardStops(extra []string) []string {
	return normalizedCohort(append(append([]string(nil), RequiredCanaryHardStops...), extra...))
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(values []string, value string) bool {
	for _, current := range values {
		if current == value {
			return true
		}
	}
	return false
}

func validDigest(value string) bool {
	raw := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == sha256.Size && raw == strings.ToLower(raw)
}

func validCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == len(value) && value == strings.ToLower(value)
}

func failureFingerprint(result RunResult, err error) string {
	message := errorMessage(result, err)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", result.Code, result.Reason, message)))
	return hex.EncodeToString(sum[:])
}

func nonProgressFingerprint(observation Observation, result RunResult) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"non-progress", observation.WakeToken, observation.RecoveryRunID,
		strings.Join(observation.ReadyIDs, "\x00"), result.Reason,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func errorMessage(result RunResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if result.Reason != "" {
		return result.Reason
	}
	return fmt.Sprintf("engine exit %d", result.Code)
}

func firstReadyInjection(injections, ready []string) string {
	readySet := make(map[string]bool, len(ready))
	for _, id := range ready {
		readySet[id] = true
	}
	for _, id := range injections {
		if readySet[id] {
			return id
		}
	}
	return ""
}

func grow(current, max time.Duration) time.Duration {
	if current >= max {
		return max
	}
	current *= 2
	if current > max {
		return max
	}
	return current
}
