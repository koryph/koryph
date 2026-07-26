// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package loop owns the long-lived, model-free autonomous supervisor around
// the transactional engine. It observes work before invoking the engine, so an
// idle project does not create empty run ledgers or launch a model process.
package loop

import (
	"context"
	"errors"
	"io"
	"time"
)

const (
	// SchemaVersion is the on-disk supervisor and control schema understood by
	// this binary. Loads reject newer versions rather than dropping fields.
	SchemaVersion = 4

	ModeStarting    = "starting"
	ModeRunning     = "running"
	ModeIdle        = "idle"
	ModeDraining    = "draining"
	ModeStopped     = "stopped"
	ModeCircuitOpen = "circuit-open"

	BoundaryIdle     Boundary = "idle"
	BoundaryTerminal Boundary = "terminal"

	// DefaultCanaryInactivityLimit matches the maximum approved p95
	// dispatch-to-terminal SLO. A cohort that makes no authenticated terminal
	// progress for this long cannot remain an indefinitely pending release
	// gate.
	DefaultCanaryInactivityLimit = 30 * time.Minute
)

var (
	ErrCircuitOpen         = errors.New("loop: supervisor circuit is open")
	ErrAlreadyRunning      = errors.New("loop: supervisor is already running")
	ErrUnvalidated         = errors.New("loop: autonomous mode never permits unvalidated projects")
	ErrCanaryCohortDrift   = errors.New("loop: canary cohort differs from durable supervisor state")
	ErrCanaryIdentityDrift = errors.New("loop: canary binary, contract, or report identity differs from durable supervisor state")
)

// RequiredCanaryHardStops are the safety conditions every autonomous canary
// must treat as an immediate drain-and-circuit event. Callers may add stricter
// keys, but cannot remove these.
var RequiredCanaryHardStops = []string{
	"unjustified-frontier-implementation",
	"engine-invariant",
	"cohort-admission-violation",
	"host-memory-pressure",
	"token-semantics-inconsistent",
	"operator-intervention",
	"canary-inactivity-timeout",
}

// Config is the stable policy of one supervisor process.
type Config struct {
	ProjectID string
	Parent    string
	Max       int

	// AllowUnvalidated exists only to make the autonomous prohibition
	// imperative at the package boundary. The CLI intentionally exposes no
	// corresponding flag.
	AllowUnvalidated bool

	IdleMin      time.Duration
	IdleMax      time.Duration
	ControlPoll  time.Duration
	CrashMin     time.Duration
	CrashMax     time.Duration
	FailureLimit int
	CrashLimit   int
	Canary       *CanarySpec
	Out          io.Writer
}

// CanarySpec fixes the complete admissible cohort before the canary starts.
// TargetWidth must be at least two. Width always starts at exactly two.
type CanarySpec struct {
	Cohort                 []string
	TargetWidth            int
	InactivityLimit        time.Duration
	HardStops              []string
	InstalledCommit        string
	BinaryVersion          string
	BuildIdentity          string
	ContractDigest         string
	RegistryIdentityDigest string
	AutonomyPolicyDigest   string
	ExecutionPolicyDigest  string
	GenerationDigest       string
	ReportPath             string
}

// Scope is the model-free query the observer evaluates. FixedCohort is an
// allowlist, never a preference.
type Scope struct {
	Parent            string
	FixedCohort       []string
	PendingInjections []string
}

// Observation is one read-only view of scheduling and recovery state.
type Observation struct {
	ReadyIDs       []string
	Recovery       bool
	Reconcile      bool
	RecoveryRunID  string
	DrainRequested bool
	WakeToken      string
}

// Observer reads ready/recovery state. Implementations may additionally
// implement WaitObserver to wake before the supervisor's bounded idle timeout.
type Observer interface {
	Observe(context.Context, Scope) (Observation, error)
}

// WaitObserver supplies a filesystem/event-backed idle wake. It must remain
// model-free. A plain Observer is still supported and uses a bounded timer.
type WaitObserver interface {
	Wait(context.Context, Scope, Observation, time.Duration) error
}

// RunRequest is the supervisor-to-engine admission contract.
type RunRequest struct {
	Resume        bool
	RecoveryRunID string
	Only          string
	Max           int
	AllowedIDs    []string
	// AuthoritativeWidth prevents project caps, live resize sidecars, and
	// quota scaling from silently changing a supervisor-approved canary width.
	AuthoritativeWidth bool
	HardStops          []string
	// OnRunStart publishes the fresh or resumed engine run ID while Run is
	// still active. The supervisor supplies this callback; engines must invoke
	// it before launching a new implementation process.
	OnRunStart func(string) error
}

// TerminalOutcome is the minimum terminal evidence the canary controller
// needs. Good is set only after all configured deterministic and semantic
// quality gates have succeeded.
type TerminalOutcome struct {
	BeadID string `json:"bead_id"`
	Status string `json:"status"`
	Good   bool   `json:"good"`
}

// RunResult is the engine result consumed by the supervisor. HardStop is a
// typed safety tripwire, not text scraped from engine output.
type RunResult struct {
	RunID          string
	Code           int
	Reason         string
	Dispatched     int
	Terminal       []TerminalOutcome
	PressureNormal bool
	Pressure       PressureSample
	HardStop       string
	Containment    ContainmentResult
}

// ContainmentResult is the engine-owned barrier proof attached to a
// native-canary cancellation. Required=false preserves ordinary Engine
// implementations and operator-interrupt resumability.
type ContainmentResult struct {
	Required         bool
	Complete         bool
	ActiveWorkers    int
	NonTerminalSlots int
	RemainingLeases  int
	Error            string
}

// PressureSample is the supervisor's durable, provider-neutral host sample at
// an engine boundary. It is folded directly into autonomy evidence.
type PressureSample struct {
	RunID       string `json:"run_id"`
	At          string `json:"at"`
	Level       string `json:"level"`
	Persistent  bool   `json:"persistent,omitempty"`
	AvailableMB int    `json:"available_mb,omitempty"`
}

// Engine is the transactional run owner. The supervisor never interprets its
// logs or repeats an individual implementation action.
type Engine interface {
	Run(context.Context, RunRequest) (RunResult, error)
}

// CanaryPublisher derives evidence from engine-owned ledgers and immutable
// artifacts, then atomically publishes the fixed report. The supervisor calls
// it only after every admitted cohort member has a durable terminal outcome.
type CanaryPublisher interface {
	Publish(context.Context, CanaryPublicationRequest) (CanaryPublication, error)
}

type CanaryPublicationRequest struct {
	ProjectID string
	StartedAt string
	EndedAt   string
	Canary    CanaryState
}

type CanaryPublication struct {
	Path        string
	Digest      string
	Decision    string
	GeneratedAt string
}

// Tripwire is a typed, live hard-stop event. Detail is evidence only; Kind is
// the stable policy key.
type Tripwire struct {
	Kind   string `json:"kind"`
	RunID  string `json:"run_id,omitempty"`
	BeadID string `json:"bead_id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// TripwireSource lets a canary interrupt a still-running engine immediately.
// The channel must close when the supplied context is done.
type TripwireSource interface {
	Events(context.Context) <-chan Tripwire
}

// Drainer requests the engine's existing graceful drain sidecar. It must not
// kill an agent or mutate source, branches, Beads, signing, or policy.
type Drainer interface {
	Drain(context.Context, string) error
}

// Boundary names a model-free supervisor lifecycle checkpoint.
type Boundary string

// Maintainer performs bounded model-free housekeeping such as artifact GC.
// It must not mutate Beads, branches, signing, or scheduling policy. Failures
// are alerted and retried at a later boundary; they do not launch a model.
type Maintainer interface {
	Maintain(context.Context, Boundary) error
}

// DrainAcknowledger removes a consumed engine drain request after the
// supervisor has stopped. Implementations use the ledger's one-shot consume
// operation; the optional seam keeps generic drainers small.
type DrainAcknowledger interface {
	AcknowledgeDrain()
}

// Notifier is deliberately write-only: the supervisor can emit structured
// alerts but cannot ask it to repair anything.
type Notifier interface {
	Emit(context.Context, Alert) error
}

// Alert is a schema-versioned health notification.
type Alert struct {
	SchemaVersion int            `json:"schema_version"`
	At            string         `json:"at"`
	ProjectID     string         `json:"project_id"`
	Level         string         `json:"level"`
	Kind          string         `json:"kind"`
	Message       string         `json:"message,omitempty"`
	Evidence      map[string]any `json:"evidence,omitempty"`
}

// State is the durable supervisor checkpoint. It contains control decisions
// and evidence, never source-changing instructions.
type State struct {
	SchemaVersion int    `json:"schema_version"`
	ProjectID     string `json:"project_id"`
	Mode          string `json:"mode"`
	PID           int    `json:"pid,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
	UpdatedAt     string `json:"updated_at"`
	LastObserved  string `json:"last_observed_at,omitempty"`
	CurrentRunID  string `json:"current_run_id,omitempty"`
	LastRunID     string `json:"last_run_id,omitempty"`
	LastCode      int    `json:"last_code,omitempty"`
	LastReason    string `json:"last_reason,omitempty"`
	IdleBackoffMS int64  `json:"idle_backoff_ms,omitempty"`

	FailureFingerprint  string `json:"failure_fingerprint,omitempty"`
	IdenticalFailures   int    `json:"identical_failures,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	CircuitReason       string `json:"circuit_reason,omitempty"`

	Canary *CanaryState `json:"canary,omitempty"`
}

// CanaryState is the durable width gate and fixed-cohort identity.
type CanaryState struct {
	CohortDigest      string   `json:"cohort_digest"`
	Cohort            []string `json:"cohort"`
	StartedAt         string   `json:"started_at"`
	ProgressAt        string   `json:"progress_at"`
	InactivityLimitMS int64    `json:"inactivity_limit_ms"`
	TargetWidth       int      `json:"target_width"`
	CurrentWidth      int      `json:"current_width"`
	ConsecutiveGood   int      `json:"consecutive_good"`
	HardStops         []string `json:"hard_stops"`
	HardStop          string   `json:"hard_stop,omitempty"`

	InstalledCommit        string `json:"installed_commit"`
	BinaryVersion          string `json:"binary_version"`
	BuildIdentity          string `json:"build_identity"`
	ContractDigest         string `json:"contract_digest"`
	RegistryIdentityDigest string `json:"registry_identity_digest"`
	AutonomyPolicyDigest   string `json:"autonomy_policy_digest"`
	ExecutionPolicyDigest  string `json:"execution_policy_digest"`
	GenerationDigest       string `json:"generation_digest"`
	ReportPath             string `json:"report_path"`

	RunIDs            []string                   `json:"run_ids,omitempty"`
	Terminal          map[string]TerminalOutcome `json:"terminal,omitempty"`
	Pressure          []PressureSample           `json:"pressure,omitempty"`
	EndedAt           string                     `json:"ended_at,omitempty"`
	Decision          string                     `json:"decision,omitempty"`
	ReportDigest      string                     `json:"report_digest,omitempty"`
	ReportGeneratedAt string                     `json:"report_generated_at,omitempty"`
	PublishedAt       string                     `json:"published_at,omitempty"`
}

// Control is a separate atomic sidecar so operator commands never race the
// supervisor's single-writer State checkpoint.
type Control struct {
	SchemaVersion int      `json:"schema_version"`
	UpdatedAt     string   `json:"updated_at"`
	Stop          bool     `json:"stop,omitempty"`
	Drain         bool     `json:"drain,omitempty"`
	Inject        []string `json:"inject,omitempty"`
}
