// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package metrics

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/strictjson"
	"golang.org/x/sys/unix"
)

const (
	AutonomyInputSchema               = "koryph.autonomy-input/v1"
	AutonomyReportSchema              = "koryph.autonomy-report/v1"
	DefaultAutonomyReportRelativePath = ".plan-logs/koryph/canary/autonomous-loop-reliability.json"

	ExclusionExternalCapability = "external-capability"
	OutcomeCapabilityHold       = "capability-hold"
	ModelTierLight              = "light"
	ModelTierStandard           = "standard"
	ModelTierFrontier           = "frontier"
	TokenSemanticsDisjointV1    = "disjoint-v1"
	maxAutonomyJSONBytes        = 64 << 20
	commandSignatureHexLength   = 24
	commandGenerationHexLength  = 32
)

var ErrAutonomyReportExists = errors.New("autonomy report already exists")

// RequiredSafetyInvariants are the independently-evidenced invariants a
// release canary may not omit. Missing evidence fails closed.
var RequiredSafetyInvariants = []string{
	"candidate-contract",
	"protected-path-refusal",
	"signature-and-dco",
	"single-broad-command",
	"capability-no-redispatch",
	"host-pressure-containment",
	"no-regex-mutation",
	"acceptance-criteria-satisfied",
	"gate-candidate-tuple",
	"resume-without-redispatch",
}

// AutonomyThresholds are persisted with the report so doctor evaluates the
// exact policy under which the immutable canary was produced.
type AutonomyThresholds struct {
	MinEligibleBeads            int     `json:"min_eligible_beads"`
	MinAutonomousCompletionRate float64 `json:"min_autonomous_completion_rate"`
	MinFirstReviewPassRate      float64 `json:"min_first_review_pass_rate"`
	MaxRetryDispatchRate        float64 `json:"max_retry_dispatch_rate"`
	MaxFrontierImplementation   float64 `json:"max_frontier_implementation_rate"`
	MaxMedianLatencyMS          int64   `json:"max_median_dispatch_to_terminal_ms"`
	MaxP95LatencyMS             int64   `json:"max_p95_dispatch_to_terminal_ms"`
	MaxArtifactBytes            int64   `json:"max_active_artifact_bytes"`
	HardArtifactBytes           int64   `json:"hard_active_artifact_bytes"`
}

// DefaultAutonomyThresholds are the approved initial self-hosting SLOs.
func DefaultAutonomyThresholds() AutonomyThresholds {
	return AutonomyThresholds{
		MinEligibleBeads:            10,
		MinAutonomousCompletionRate: 0.90,
		MinFirstReviewPassRate:      0.75,
		MaxRetryDispatchRate:        0.20,
		MaxFrontierImplementation:   0.05,
		MaxMedianLatencyMS:          (15 * time.Minute).Milliseconds(),
		MaxP95LatencyMS:             (30 * time.Minute).Milliseconds(),
		MaxArtifactBytes:            2 << 30,
		HardArtifactBytes:           5 << 30,
	}
}

// AutonomyInput is the complete evidence envelope from which a canary report
// is deterministically derived. Producers must record each implementation
// attempt separately; a cumulative attempt count is intentionally insufficient.
type AutonomyInput struct {
	SchemaVersion   string             `json:"schema_version"`
	ProjectID       string             `json:"project_id"`
	InstalledCommit string             `json:"installed_commit"`
	BinaryVersion   string             `json:"binary_version"`
	BuildIdentity   string             `json:"build_identity"`
	ContractDigest  string             `json:"contract_digest"`
	Cohort          []string           `json:"cohort"`
	StartedAt       string             `json:"started_at"`
	EndedAt         string             `json:"ended_at"`
	Thresholds      AutonomyThresholds `json:"thresholds"`
	Evidence        AutonomyEvidence   `json:"evidence"`
}

// AutonomyEvidence is retained verbatim in the immutable report.
type AutonomyEvidence struct {
	Attempts            []AutonomyAttempt   `json:"attempts"`
	Exclusions          []AutonomyExclusion `json:"exclusions,omitempty"`
	Safety              []SafetyEvidence    `json:"safety"`
	Pressure            []PressureEvidence  `json:"pressure,omitempty"`
	Artifacts           ArtifactEvidence    `json:"artifacts"`
	IdleLedgerCreations int                 `json:"idle_ledger_creations"`
}

// AutonomyAttempt is one implementation dispatch, not a final cumulative slot.
type AutonomyAttempt struct {
	BeadID                string               `json:"bead_id"`
	RunID                 string               `json:"run_id"`
	PhaseID               string               `json:"phase_id"`
	Attempt               int                  `json:"attempt"`
	ModelTier             string               `json:"model_tier"`
	ModelActual           string               `json:"model_actual,omitempty"`
	FrontierJustification string               `json:"frontier_justification,omitempty"`
	DispatchedAt          string               `json:"dispatched_at"`
	TerminalAt            string               `json:"terminal_at,omitempty"`
	Outcome               TypedOutcome         `json:"outcome"`
	OperatorIntervention  bool                 `json:"operator_intervention,omitempty"`
	Review                ReviewEvidence       `json:"review"`
	Gate                  StageTiming          `json:"gate"`
	SemanticReview        StageTiming          `json:"semantic_review"`
	Merge                 StageTiming          `json:"merge"`
	PR                    StageTiming          `json:"pr"`
	Retry                 *RetryEvidence       `json:"retry,omitempty"`
	GateEvidence          *GateEvidenceRef     `json:"gate_evidence,omitempty"`
	ReviewEvidenceRefs    []ReviewEvidenceRef  `json:"review_evidence_refs,omitempty"`
	TerminalEvidence      *TerminalEvidenceRef `json:"terminal_evidence,omitempty"`
	ProcessEvents         []ProcessEvidence    `json:"process_events,omitempty"`
	Tokens                TokenEvidence        `json:"tokens"`
}

// GateEvidenceRef persists the authenticated immutable gate identity used by
// this attempt. It is intentionally self-contained so a report cannot reduce
// a successful gate to a boolean or path spelling.
type GateEvidenceRef struct {
	ArtifactDigest   string `json:"artifact_digest"`
	CandidateSHA     string `json:"candidate_sha"`
	BaseSHA          string `json:"base_sha"`
	DiffDigest       string `json:"diff_digest"`
	GateConfigDigest string `json:"gate_config_digest"`
	CommandDigest    string `json:"command_digest"`
	EngineVersion    string `json:"engine_version"`
	BuildIdentity    string `json:"build_identity"`
	CompletedAt      string `json:"completed_at"`
}

// ReviewEvidenceRef identifies one immutable general or security review and
// its cumulative finding history.
type ReviewEvidenceRef struct {
	Kind             string `json:"kind"`
	ArtifactDigest   string `json:"artifact_digest"`
	HistoryDigest    string `json:"history_digest"`
	CandidateSHA     string `json:"candidate_sha"`
	BaseSHA          string `json:"base_sha"`
	CompletedAt      string `json:"completed_at"`
	SecurityRequired bool   `json:"security_required,omitempty"`
}

// TerminalEvidenceRef authenticates the worker result and its confined
// summary/evidence payloads at the exact candidate/base admitted by the gate.
type TerminalEvidenceRef struct {
	ArtifactDigest string `json:"artifact_digest"`
	Generation     string `json:"generation"`
	CandidateSHA   string `json:"candidate_sha"`
	BaseSHA        string `json:"base_sha"`
	SummaryDigest  string `json:"summary_digest"`
	EvidenceDigest string `json:"evidence_digest"`
	CompletedAt    string `json:"completed_at"`
}

// TypedOutcome separates an external hold from an implementation, validation,
// or orchestration failure without interpreting prose.
type TypedOutcome struct {
	Kind       string `json:"kind"`
	Terminal   bool   `json:"terminal"`
	Correct    bool   `json:"correct"`
	BlockKind  string `json:"block_kind,omitempty"`
	Capability string `json:"capability,omitempty"`
}

type ReviewEvidence struct {
	Reached   bool `json:"reached"`
	Attempts  int  `json:"attempts"`
	FirstPass bool `json:"first_pass"`
}

type StageTiming struct {
	Reached   bool  `json:"reached"`
	QueueMS   int64 `json:"queue_ms"`
	ServiceMS int64 `json:"service_ms"`
}

type RetryEvidence struct {
	Kind                string `json:"kind"`
	PriorEvidenceDigest string `json:"prior_evidence_digest"`
	EvidenceDigest      string `json:"evidence_digest"`
}

type ProcessEvidence struct {
	Event      string  `json:"event"`
	Class      string  `json:"class"`
	Signature  string  `json:"signature,omitempty"`
	Generation string  `json:"generation,omitempty"`
	At         string  `json:"at"`
	PeakRSSKB  int64   `json:"peak_rss_kb,omitempty"`
	CPUSeconds float64 `json:"cpu_seconds,omitempty"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	Duplicate  bool    `json:"duplicate,omitempty"`
}

type TokenEvidence struct {
	Semantics          string `json:"semantics"`
	Input              int64  `json:"input"`
	Output             int64  `json:"output"`
	CacheRead          int64  `json:"cache_read"`
	CacheCreation      int64  `json:"cache_creation"`
	Total              int64  `json:"total"`
	ProviderTotalInput int64  `json:"provider_total_input,omitempty"`
}

type AutonomyExclusion struct {
	BeadID      string `json:"bead_id"`
	Kind        string `json:"kind"`
	Predeclared bool   `json:"predeclared"`
	Detail      string `json:"detail"`
}

type SafetyEvidence struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type PressureEvidence struct {
	At          string `json:"at"`
	Level       string `json:"level"`
	Persistent  bool   `json:"persistent,omitempty"`
	AvailableMB int    `json:"available_mb,omitempty"`
}

type ArtifactEvidence struct {
	ActiveBytes          int64 `json:"active_bytes"`
	CompactEvidenceBytes int64 `json:"compact_evidence_bytes"`
	RetainedFailureBytes int64 `json:"retained_failure_bytes"`
	ReclaimableBytes     int64 `json:"reclaimable_bytes"`
}

type Distribution struct {
	Count    int   `json:"count"`
	TotalMS  int64 `json:"total_ms"`
	MedianMS int64 `json:"median_ms"`
	P95MS    int64 `json:"p95_ms"`
}

type StageMetrics struct {
	Queue   Distribution `json:"queue"`
	Service Distribution `json:"service"`
}

type RatioMetric struct {
	Numerator   int     `json:"numerator"`
	Denominator int     `json:"denominator"`
	Rate        float64 `json:"rate"`
}

type AutonomyMetrics struct {
	CohortBeads                   int              `json:"cohort_beads"`
	EligibleBeads                 int              `json:"eligible_beads"`
	ExcludedExternalHolds         int              `json:"excluded_external_capability_holds"`
	AutonomousCompletion          RatioMetric      `json:"autonomous_completion"`
	FirstReviewPass               RatioMetric      `json:"first_review_pass"`
	RetryDispatches               RatioMetric      `json:"retry_dispatches"`
	UnchangedRetries              int              `json:"unchanged_retries"`
	FrontierImplementation        RatioMetric      `json:"frontier_implementation"`
	UnjustifiedFrontierAttempts   int              `json:"unjustified_frontier_attempts"`
	DispatchToTerminal            Distribution     `json:"dispatch_to_terminal"`
	Gate                          StageMetrics     `json:"gate"`
	Review                        StageMetrics     `json:"review"`
	Merge                         StageMetrics     `json:"merge"`
	IdleLedgerCreations           int              `json:"idle_ledger_creations"`
	DuplicateBroadCommands        int              `json:"duplicate_broad_commands"`
	ArtifactBytes                 ArtifactEvidence `json:"artifact_bytes"`
	EligibleArtifactBytes         int64            `json:"eligible_artifact_bytes"`
	PressureEvents                int              `json:"pressure_events"`
	PersistentCriticalPressure    int              `json:"persistent_critical_pressure_events"`
	Tokens                        TokenEvidence    `json:"normalized_tokens"`
	TokenInconsistencies          int              `json:"token_inconsistencies"`
	MisclassifiedCapabilityBlocks int              `json:"misclassified_capability_blocks"`
}

type SLOCheck struct {
	Name        string  `json:"name"`
	Passed      bool    `json:"passed"`
	Actual      float64 `json:"actual"`
	Comparator  string  `json:"comparator"`
	Threshold   float64 `json:"threshold"`
	Numerator   int     `json:"numerator,omitempty"`
	Denominator int     `json:"denominator,omitempty"`
}

type AutonomyDecision struct {
	Passed           bool       `json:"passed"`
	SafetyViolations []string   `json:"safety_violations,omitempty"`
	Checks           []SLOCheck `json:"checks"`
}

// AutonomyReport is immutable release evidence. EvidenceDigest authenticates
// the input, metrics, and decision; it is checked again by doctor.
type AutonomyReport struct {
	SchemaVersion   string             `json:"schema_version"`
	GeneratedAt     string             `json:"generated_at"`
	ProjectID       string             `json:"project_id"`
	InstalledCommit string             `json:"installed_commit"`
	BinaryVersion   string             `json:"binary_version"`
	BuildIdentity   string             `json:"build_identity"`
	ContractDigest  string             `json:"contract_digest"`
	CohortDigest    string             `json:"cohort_digest"`
	Cohort          []string           `json:"cohort"`
	StartedAt       string             `json:"started_at"`
	EndedAt         string             `json:"ended_at"`
	Thresholds      AutonomyThresholds `json:"thresholds"`
	Evidence        AutonomyEvidence   `json:"evidence"`
	Metrics         AutonomyMetrics    `json:"metrics"`
	Decision        AutonomyDecision   `json:"decision"`
	EvidenceDigest  string             `json:"evidence_digest"`
}

// AutonomyReportExpectation anchors an otherwise self-consistent report to
// the live release being checked. FreshAt is a trusted release-owned
// freshness checkpoint; MaxAge bounds how far GeneratedAt may precede it.
type AutonomyReportExpectation struct {
	ProjectID       string
	InstalledCommit string
	BinaryVersion   string
	BuildIdentity   string
	ContractDigest  string
	Cohort          []string
	CohortDigest    string
	Thresholds      AutonomyThresholds
	CanaryStartedAt string
	EvidenceDigest  string
	GeneratedAt     string
	FreshAt         time.Time
	MaxAge          time.Duration
	MaxFutureSkew   time.Duration
}

// BuildAutonomyReport validates raw evidence and computes every SLO.
func BuildAutonomyReport(in AutonomyInput, now time.Time) (*AutonomyReport, error) {
	if in.SchemaVersion != AutonomyInputSchema {
		return nil, fmt.Errorf("autonomy input schema %q is unsupported", in.SchemaVersion)
	}
	if strings.TrimSpace(in.ProjectID) == "" || !validFullCommit(in.InstalledCommit) ||
		strings.TrimSpace(in.BinaryVersion) == "" || strings.TrimSpace(in.BuildIdentity) == "" ||
		!validSHA256(in.ContractDigest) {
		return nil, errors.New("autonomy input identity is incomplete")
	}
	cohort, err := normalizeStrictCohort(in.Cohort)
	if err != nil {
		return nil, err
	}
	started, ended, err := parseIntervalBounds(in.StartedAt, in.EndedAt)
	if err != nil {
		return nil, err
	}
	generated := now.UTC()
	if generated.Before(ended) {
		return nil, errors.New("autonomy report generation precedes canary end")
	}
	if err := validateThresholds(in.Thresholds); err != nil {
		return nil, err
	}
	if err := validateAutonomyArithmetic(in.Evidence); err != nil {
		return nil, err
	}
	reportMetrics, decision := evaluateAutonomy(
		cohort, in.Evidence, in.Thresholds, started, ended,
		in.BinaryVersion, in.BuildIdentity,
	)
	report := &AutonomyReport{
		SchemaVersion: AutonomyReportSchema, GeneratedAt: generated.Format(time.RFC3339Nano),
		ProjectID: in.ProjectID, InstalledCommit: in.InstalledCommit, BinaryVersion: in.BinaryVersion,
		BuildIdentity: in.BuildIdentity, ContractDigest: in.ContractDigest,
		CohortDigest: cohortDigest(cohort), Cohort: cohort,
		StartedAt: in.StartedAt, EndedAt: in.EndedAt, Thresholds: in.Thresholds,
		Evidence: in.Evidence, Metrics: reportMetrics, Decision: decision,
	}
	digest, err := autonomyDigest(report)
	if err != nil {
		return nil, err
	}
	report.EvidenceDigest = digest
	return report, nil
}

// PublishAutonomyReport is the product integration seam used by both the CLI
// and the native loop's canary finalizer.
func PublishAutonomyReport(path string, in AutonomyInput, now time.Time) (*AutonomyReport, error) {
	report, err := BuildAutonomyReport(in, now)
	if err != nil {
		return nil, err
	}
	if err := WriteAutonomyReport(path, report); err != nil {
		return nil, err
	}
	return report, nil
}

func evaluateAutonomy(
	cohort []string,
	evidence AutonomyEvidence,
	thresholds AutonomyThresholds,
	started time.Time,
	ended time.Time,
	binaryVersion string,
	buildIdentity string,
) (AutonomyMetrics, AutonomyDecision) {
	metrics := AutonomyMetrics{
		CohortBeads: len(cohort), IdleLedgerCreations: evidence.IdleLedgerCreations,
		ArtifactBytes: evidence.Artifacts,
	}
	violations := make([]string, 0)
	addViolation := func(message string) {
		for _, current := range violations {
			if current == message {
				return
			}
		}
		violations = append(violations, message)
	}
	cohortSet := make(map[string]bool, len(cohort))
	for _, id := range cohort {
		cohortSet[id] = true
	}
	excluded := make(map[string]bool)
	for _, exclusion := range evidence.Exclusions {
		switch {
		case !cohortSet[exclusion.BeadID]:
			addViolation("exclusion-outside-cohort:" + exclusion.BeadID)
		case exclusion.Kind != ExclusionExternalCapability || !exclusion.Predeclared:
			addViolation("invalid-exclusion:" + exclusion.BeadID)
		case excluded[exclusion.BeadID]:
			addViolation("duplicate-exclusion:" + exclusion.BeadID)
		default:
			excluded[exclusion.BeadID] = true
		}
	}
	metrics.ExcludedExternalHolds = len(excluded)
	metrics.EligibleBeads = len(cohort) - len(excluded)
	metrics.AutonomousCompletion.Denominator = metrics.EligibleBeads

	byBead := make(map[string][]AutonomyAttempt)
	type commandLifecycle struct {
		starts     int
		completes  int
		startAt    time.Time
		completeAt time.Time
	}
	broadLifecycles := make(map[string]commandLifecycle)
	for _, attempt := range evidence.Attempts {
		if !cohortSet[attempt.BeadID] {
			addViolation("attempt-outside-cohort:" + attempt.BeadID)
			continue
		}
		byBead[attempt.BeadID] = append(byBead[attempt.BeadID], attempt)
		dispatched, dispatchErr := time.Parse(time.RFC3339Nano, attempt.DispatchedAt)
		if dispatchErr != nil || !withinInterval(dispatched, started, ended) {
			addViolation(fmt.Sprintf("attempt-dispatch-time:%s:%d", attempt.BeadID, attempt.Attempt))
		}
		if attempt.Outcome.Terminal {
			terminal, terminalErr := time.Parse(time.RFC3339Nano, attempt.TerminalAt)
			if terminalErr != nil || terminal.IsZero() ||
				!withinInterval(terminal, started, ended) ||
				(dispatchErr == nil && terminal.Before(dispatched)) {
				addViolation(fmt.Sprintf("attempt-terminal-time:%s:%d", attempt.BeadID, attempt.Attempt))
			}
		} else if attempt.TerminalAt != "" {
			addViolation(fmt.Sprintf("nonterminal-has-terminal-time:%s:%d", attempt.BeadID, attempt.Attempt))
		}
		if err := validateOutcome(attempt.Outcome); err != nil {
			addViolation(fmt.Sprintf("outcome-invalid:%s:%d:%s", attempt.BeadID, attempt.Attempt, err))
		}
		switch attempt.ModelTier {
		case ModelTierLight, ModelTierStandard, ModelTierFrontier:
		default:
			addViolation(fmt.Sprintf("model-tier-invalid:%s:%d", attempt.BeadID, attempt.Attempt))
		}
		if attempt.Review.Reached {
			if attempt.Review.Attempts <= 0 || (attempt.Review.FirstPass && attempt.Review.Attempts != 1) {
				addViolation(fmt.Sprintf("review-evidence-invalid:%s:%d", attempt.BeadID, attempt.Attempt))
			}
		} else if attempt.Review.Attempts != 0 || attempt.Review.FirstPass {
			addViolation(fmt.Sprintf("review-evidence-invalid:%s:%d", attempt.BeadID, attempt.Attempt))
		}
		if attempt.Gate.QueueMS < 0 || attempt.Gate.ServiceMS < 0 ||
			attempt.SemanticReview.QueueMS < 0 || attempt.SemanticReview.ServiceMS < 0 ||
			attempt.Merge.QueueMS < 0 || attempt.Merge.ServiceMS < 0 ||
			attempt.PR.QueueMS < 0 || attempt.PR.ServiceMS < 0 {
			addViolation(fmt.Sprintf("stage-timing-invalid:%s:%d", attempt.BeadID, attempt.Attempt))
		}
		if (!attempt.Gate.Reached && stageHasEvidence(attempt.Gate)) ||
			(!attempt.SemanticReview.Reached && stageHasEvidence(attempt.SemanticReview)) ||
			(!attempt.Merge.Reached && stageHasEvidence(attempt.Merge)) ||
			(!attempt.PR.Reached && stageHasEvidence(attempt.PR)) ||
			attempt.SemanticReview.Reached != attempt.Review.Reached {
			addViolation(fmt.Sprintf("stage-reach-invalid:%s:%d", attempt.BeadID, attempt.Attempt))
		}
		if attempt.Outcome.Terminal && attempt.Outcome.Correct &&
			!correctTerminalLane(attempt) {
			addViolation(fmt.Sprintf("terminal-lane-invalid:%s:%d", attempt.BeadID, attempt.Attempt))
		}
		if err := validateAttemptEvidenceRefs(
			attempt, started, ended, binaryVersion, buildIdentity,
		); err != nil {
			addViolation(fmt.Sprintf("evidence-reference-invalid:%s:%d:%s",
				attempt.BeadID, attempt.Attempt, err))
		}
		if attempt.ModelTier == ModelTierFrontier {
			if attempt.FrontierJustification != "model-capability" {
				metrics.UnjustifiedFrontierAttempts++
			}
		}
		if attempt.Attempt > 1 {
			if attempt.Retry == nil || strings.TrimSpace(attempt.Retry.Kind) == "" ||
				!validSHA256(attempt.Retry.PriorEvidenceDigest) || !validSHA256(attempt.Retry.EvidenceDigest) {
				addViolation(fmt.Sprintf("retry-evidence-missing:%s:%d", attempt.BeadID, attempt.Attempt))
			} else if validateOutcome(TypedOutcome{Kind: attempt.Retry.Kind}) != nil {
				addViolation(fmt.Sprintf("retry-kind-invalid:%s:%d", attempt.BeadID, attempt.Attempt))
			} else if attempt.Retry.PriorEvidenceDigest == attempt.Retry.EvidenceDigest {
				metrics.UnchangedRetries++
			}
		} else if attempt.Retry != nil {
			addViolation(fmt.Sprintf("unexpected-retry-evidence:%s:%d", attempt.BeadID, attempt.Attempt))
		}
		if !excluded[attempt.BeadID] {
			metrics.FrontierImplementation.Denominator++
			metrics.RetryDispatches.Denominator++
			if attempt.ModelTier == ModelTierFrontier {
				metrics.FrontierImplementation.Numerator++
			}
			if attempt.Attempt > 1 {
				metrics.RetryDispatches.Numerator++
			}
		}
		terminalAt, _ := time.Parse(time.RFC3339Nano, attempt.TerminalAt)
		var priorProcessAt time.Time
		for _, event := range attempt.ProcessEvents {
			if strings.TrimSpace(event.Event) == "" || strings.TrimSpace(event.Class) == "" ||
				event.PeakRSSKB < 0 || event.CPUSeconds < 0 || event.DurationMS < 0 {
				addViolation(fmt.Sprintf("process-evidence-invalid:%s:%d", attempt.BeadID, attempt.Attempt))
			}
			eventAt, eventErr := time.Parse(time.RFC3339Nano, event.At)
			if eventErr != nil || !withinInterval(eventAt, started, ended) ||
				(dispatchErr == nil && eventAt.Before(dispatched)) ||
				(!terminalAt.IsZero() && eventAt.After(terminalAt)) ||
				(!priorProcessAt.IsZero() && eventAt.Before(priorProcessAt)) {
				addViolation(fmt.Sprintf("process-evidence-time:%s:%d", attempt.BeadID, attempt.Attempt))
			} else {
				priorProcessAt = eventAt
			}
			if event.Class == "broad" || event.Class == "full-gate" {
				signature := strings.TrimSpace(event.Signature)
				generation := strings.TrimSpace(event.Generation)
				if !validLowerHex(signature, commandSignatureHexLength) ||
					(event.Event != "denial" &&
						!validLowerHex(generation, commandGenerationHexLength)) ||
					(event.Event == "denial" && generation != "" &&
						!validLowerHex(generation, commandGenerationHexLength)) {
					addViolation(fmt.Sprintf("process-evidence-identity:%s:%d", attempt.BeadID, attempt.Attempt))
					continue
				}
				key := signature + "\x00" + generation
				lifecycle := broadLifecycles[key]
				switch event.Event {
				case "start":
					lifecycle.starts++
					if lifecycle.starts > 1 {
						metrics.DuplicateBroadCommands++
					} else {
						lifecycle.startAt = eventAt
					}
					broadLifecycles[key] = lifecycle
				case "complete":
					lifecycle.completes++
					if lifecycle.completes > 1 {
						metrics.DuplicateBroadCommands++
					} else {
						lifecycle.completeAt = eventAt
					}
					broadLifecycles[key] = lifecycle
				case "reuse", "denial":
					metrics.DuplicateBroadCommands++
				default:
					addViolation(fmt.Sprintf("process-evidence-event:%s:%d", attempt.BeadID, attempt.Attempt))
				}
			}
		}
		if attempt.Tokens.Semantics != TokenSemanticsDisjointV1 ||
			attempt.Tokens.Input < 0 || attempt.Tokens.Output < 0 ||
			attempt.Tokens.CacheRead < 0 || attempt.Tokens.CacheCreation < 0 ||
			attempt.Tokens.ProviderTotalInput < 0 ||
			!validTokenTotal(attempt.Tokens) {
			metrics.TokenInconsistencies++
		} else {
			metrics.Tokens.Input += attempt.Tokens.Input
			metrics.Tokens.Output += attempt.Tokens.Output
			metrics.Tokens.CacheRead += attempt.Tokens.CacheRead
			metrics.Tokens.CacheCreation += attempt.Tokens.CacheCreation
			metrics.Tokens.Total += attempt.Tokens.Total
			metrics.Tokens.ProviderTotalInput += attempt.Tokens.ProviderTotalInput
		}
	}
	metrics.Tokens.Semantics = TokenSemanticsDisjointV1
	metrics.RetryDispatches.Rate = ratio(metrics.RetryDispatches.Numerator, metrics.RetryDispatches.Denominator)
	metrics.FrontierImplementation.Rate = ratio(metrics.FrontierImplementation.Numerator, metrics.FrontierImplementation.Denominator)
	metrics.Gate = stageDistribution(evidence.Attempts, func(a AutonomyAttempt) StageTiming { return a.Gate })
	metrics.Review = stageDistribution(evidence.Attempts, func(a AutonomyAttempt) StageTiming { return a.SemanticReview })
	metrics.Merge = stageDistribution(evidence.Attempts, terminalStageTiming)

	var latencies []int64
	for _, beadID := range cohort {
		attempts := byBead[beadID]
		sort.Slice(attempts, func(i, j int) bool { return attempts[i].Attempt < attempts[j].Attempt })
		if len(attempts) == 0 {
			addViolation("missing-attempts:" + beadID)
			continue
		}
		var priorDispatch time.Time
		for i, attempt := range attempts {
			if attempt.Attempt != i+1 {
				addViolation(fmt.Sprintf("attempt-sequence:%s", beadID))
				break
			}
			if strings.TrimSpace(attempt.RunID) == "" || strings.TrimSpace(attempt.PhaseID) == "" ||
				strings.TrimSpace(attempt.Outcome.Kind) == "" {
				addViolation(fmt.Sprintf("attempt-shape:%s:%d", beadID, attempt.Attempt))
			}
			if i < len(attempts)-1 && attempt.Outcome.Terminal {
				addViolation(fmt.Sprintf("nonfinal-attempt-terminal:%s:%d", beadID, attempt.Attempt))
			}
			dispatched, err := time.Parse(time.RFC3339Nano, attempt.DispatchedAt)
			if err == nil {
				if !priorDispatch.IsZero() && dispatched.Before(priorDispatch) {
					addViolation(fmt.Sprintf("attempt-order:%s:%d", beadID, attempt.Attempt))
				}
				priorDispatch = dispatched
			}
			if i < len(attempts)-1 {
				nextDispatch, nextErr := time.Parse(time.RFC3339Nano, attempts[i+1].DispatchedAt)
				if nextErr == nil {
					for _, event := range attempt.ProcessEvents {
						eventAt, eventErr := time.Parse(time.RFC3339Nano, event.At)
						if eventErr == nil && eventAt.After(nextDispatch) {
							addViolation(fmt.Sprintf("process-evidence-time:%s:%d", beadID, attempt.Attempt))
							break
						}
					}
				}
			}
		}
		last := attempts[len(attempts)-1]
		if !last.Outcome.Terminal {
			addViolation("final-attempt-nonterminal:" + beadID)
		}
		if last.Outcome.Kind == OutcomeCapabilityHold && !excluded[beadID] {
			metrics.MisclassifiedCapabilityBlocks++
		}
		if excluded[beadID] {
			if last.Outcome.Kind != OutcomeCapabilityHold ||
				last.Outcome.BlockKind != ExclusionExternalCapability ||
				!last.Outcome.Terminal || last.Outcome.Correct {
				addViolation("excluded-without-external-hold:" + beadID)
			}
			continue
		}
		intervened := false
		for _, attempt := range attempts {
			intervened = intervened || attempt.OperatorIntervention
		}
		if last.Outcome.Terminal && last.Outcome.Correct && !intervened &&
			validateOutcome(last.Outcome) == nil && correctTerminalLane(last) {
			metrics.AutonomousCompletion.Numerator++
		}
		reviewed, firstPass := false, false
		for _, attempt := range attempts {
			if attempt.Review.Reached {
				reviewed = true
				if attempt.Review.FirstPass {
					firstPass = true
				}
				break
			}
		}
		if reviewed {
			metrics.FirstReviewPass.Denominator++
			if firstPass {
				metrics.FirstReviewPass.Numerator++
			}
		}
		if last.Outcome.Terminal {
			start, startErr := time.Parse(time.RFC3339Nano, attempts[0].DispatchedAt)
			end, endErr := time.Parse(time.RFC3339Nano, last.TerminalAt)
			if startErr != nil || endErr != nil || end.Before(start) {
				addViolation("invalid-latency:" + beadID)
			} else {
				latencies = append(latencies, end.Sub(start).Milliseconds())
			}
		}
	}
	for key, lifecycle := range broadLifecycles {
		if lifecycle.starts != 1 || lifecycle.completes != 1 ||
			!lifecycle.startAt.Before(lifecycle.completeAt) {
			addViolation("process-evidence-lifecycle:" + strings.ReplaceAll(key, "\x00", ":"))
		}
	}
	metrics.AutonomousCompletion.Rate = ratio(metrics.AutonomousCompletion.Numerator, metrics.AutonomousCompletion.Denominator)
	metrics.FirstReviewPass.Rate = ratio(metrics.FirstReviewPass.Numerator, metrics.FirstReviewPass.Denominator)
	metrics.DispatchToTerminal = distribution(latencies)

	safety := make(map[string]SafetyEvidence, len(evidence.Safety))
	for _, invariant := range evidence.Safety {
		if _, exists := safety[invariant.Name]; exists {
			addViolation("duplicate-safety-evidence:" + invariant.Name)
			continue
		}
		safety[invariant.Name] = invariant
		if !invariant.Passed {
			addViolation("safety-invariant-failed:" + invariant.Name)
		}
	}
	for _, required := range RequiredSafetyInvariants {
		if _, ok := safety[required]; !ok {
			addViolation("safety-invariant-missing:" + required)
		}
	}
	var priorPressureAt time.Time
	for _, pressure := range evidence.Pressure {
		metrics.PressureEvents++
		pressureAt, pressureErr := time.Parse(time.RFC3339Nano, pressure.At)
		if pressureErr != nil || !withinInterval(pressureAt, started, ended) ||
			(!priorPressureAt.IsZero() && pressureAt.Before(priorPressureAt)) ||
			(pressure.Level != "normal" && pressure.Level != "warning" && pressure.Level != "critical") ||
			pressure.AvailableMB < 0 {
			addViolation("pressure-evidence-invalid")
		} else {
			priorPressureAt = pressureAt
		}
		if pressure.Level == "critical" && pressure.Persistent {
			metrics.PersistentCriticalPressure++
		}
	}
	if evidence.Artifacts.ActiveBytes < 0 || evidence.Artifacts.CompactEvidenceBytes < 0 ||
		evidence.Artifacts.RetainedFailureBytes < 0 ||
		evidence.Artifacts.ReclaimableBytes < 0 ||
		evidence.Artifacts.CompactEvidenceBytes >
			evidence.Artifacts.ActiveBytes-evidence.Artifacts.RetainedFailureBytes ||
		evidence.Artifacts.ReclaimableBytes >
			evidence.Artifacts.ActiveBytes-evidence.Artifacts.RetainedFailureBytes-
				evidence.Artifacts.CompactEvidenceBytes {
		addViolation("artifact-evidence-invalid")
		metrics.EligibleArtifactBytes = 0
	} else {
		metrics.EligibleArtifactBytes = evidence.Artifacts.ActiveBytes -
			evidence.Artifacts.RetainedFailureBytes - evidence.Artifacts.CompactEvidenceBytes
	}
	if metrics.UnchangedRetries > 0 {
		addViolation("unchanged-retry")
	}
	if metrics.DuplicateBroadCommands > 0 {
		addViolation("duplicate-broad-command")
	}
	if metrics.IdleLedgerCreations > 0 {
		addViolation("idle-ledger-creation")
	}
	if metrics.TokenInconsistencies > 0 {
		addViolation("token-semantics-inconsistent")
	}
	if metrics.UnjustifiedFrontierAttempts > 0 {
		addViolation("unjustified-frontier-implementation")
	}
	if metrics.MisclassifiedCapabilityBlocks > 0 {
		addViolation("misclassified-capability-block")
	}
	if metrics.PersistentCriticalPressure > 0 {
		addViolation("persistent-critical-pressure")
	}
	if metrics.EligibleArtifactBytes > thresholds.HardArtifactBytes {
		addViolation("artifact-hard-limit")
	}
	sort.Strings(violations)

	checks := []SLOCheck{
		checkMin("eligible-beads", float64(metrics.EligibleBeads), float64(thresholds.MinEligibleBeads), metrics.EligibleBeads, thresholds.MinEligibleBeads),
		checkMinRatio("autonomous-completion", metrics.AutonomousCompletion, thresholds.MinAutonomousCompletionRate),
		checkMinRatio("first-review-pass", metrics.FirstReviewPass, thresholds.MinFirstReviewPassRate),
		checkMaxRatio("retry-dispatches", metrics.RetryDispatches, thresholds.MaxRetryDispatchRate),
		checkMaxRatio("frontier-implementation", metrics.FrontierImplementation, thresholds.MaxFrontierImplementation),
		checkMax("median-dispatch-to-terminal-ms", float64(metrics.DispatchToTerminal.MedianMS), float64(thresholds.MaxMedianLatencyMS)),
		checkMax("p95-dispatch-to-terminal-ms", float64(metrics.DispatchToTerminal.P95MS), float64(thresholds.MaxP95LatencyMS)),
		checkMax("active-artifact-bytes", float64(metrics.EligibleArtifactBytes), float64(thresholds.MaxArtifactBytes)),
	}
	if metrics.DispatchToTerminal.Count == 0 {
		checks[5].Passed = false
		checks[6].Passed = false
	}
	passed := len(violations) == 0
	for _, check := range checks {
		passed = passed && check.Passed
	}
	return metrics, AutonomyDecision{Passed: passed, SafetyViolations: violations, Checks: checks}
}

func validateAttemptEvidenceRefs(
	attempt AutonomyAttempt,
	started, ended time.Time,
	binaryVersion, buildIdentity string,
) error {
	dispatched, err := time.Parse(time.RFC3339Nano, attempt.DispatchedAt)
	if err != nil || !withinInterval(dispatched, started, ended) {
		return errors.New("dispatch-time")
	}
	var candidateCompleted time.Time
	terminal := attempt.TerminalEvidence
	if (attempt.Outcome.Correct || attempt.Merge.Reached || attempt.PR.Reached) && terminal == nil {
		return errors.New("missing-terminal")
	}
	if terminal != nil {
		if !validSHA256(terminal.ArtifactDigest) ||
			!validLowerHex(terminal.Generation, sha256.Size*2) ||
			!validFullCommit(terminal.CandidateSHA) || !validFullCommit(terminal.BaseSHA) ||
			!validSHA256(terminal.SummaryDigest) || !validSHA256(terminal.EvidenceDigest) {
			return errors.New("malformed-terminal")
		}
		candidateCompleted, err = time.Parse(time.RFC3339Nano, terminal.CompletedAt)
		if err != nil || !withinInterval(candidateCompleted, started, ended) ||
			candidateCompleted.Before(dispatched) {
			return errors.New("terminal-evidence-time")
		}
	}

	gate := attempt.GateEvidence
	if (attempt.Gate.Reached || attempt.Outcome.Correct) && gate == nil {
		return errors.New("missing-gate")
	}
	var gateCompleted time.Time
	if gate != nil {
		if !validSHA256(gate.ArtifactDigest) || !validFullCommit(gate.CandidateSHA) ||
			!validFullCommit(gate.BaseSHA) || !validSHA256(gate.DiffDigest) ||
			!validSHA256(gate.GateConfigDigest) || !validSHA256(gate.CommandDigest) ||
			strings.TrimSpace(gate.EngineVersion) == "" ||
			strings.TrimSpace(gate.BuildIdentity) == "" {
			return errors.New("malformed-gate")
		}
		if gate.EngineVersion != binaryVersion || gate.BuildIdentity != buildIdentity {
			return errors.New("gate-build-identity")
		}
		if !attempt.Gate.Reached {
			return errors.New("gate-stage-unreached")
		}
		gateCompleted, err = time.Parse(time.RFC3339Nano, gate.CompletedAt)
		if err != nil || !withinInterval(gateCompleted, started, ended) ||
			gateCompleted.Before(dispatched) ||
			(!candidateCompleted.IsZero() && gateCompleted.Before(candidateCompleted)) {
			return errors.New("gate-time")
		}
	}

	reviews := make(map[string]ReviewEvidenceRef, len(attempt.ReviewEvidenceRefs))
	reviewCompleted := make(map[string]time.Time, len(attempt.ReviewEvidenceRefs))
	for _, ref := range attempt.ReviewEvidenceRefs {
		if ref.Kind != "general" && ref.Kind != "security" {
			return errors.New("review-kind")
		}
		if _, exists := reviews[ref.Kind]; exists {
			return errors.New("duplicate-review-kind")
		}
		if !validSHA256(ref.ArtifactDigest) || !validSHA256(ref.HistoryDigest) ||
			!validFullCommit(ref.CandidateSHA) || !validFullCommit(ref.BaseSHA) {
			return errors.New("malformed-review")
		}
		completed, err := time.Parse(time.RFC3339Nano, ref.CompletedAt)
		if err != nil || !withinInterval(completed, started, ended) {
			return errors.New("review-time")
		}
		if gate == nil || ref.CandidateSHA != gate.CandidateSHA || ref.BaseSHA != gate.BaseSHA {
			return errors.New("review-gate-tuple")
		}
		reviews[ref.Kind] = ref
		reviewCompleted[ref.Kind] = completed
	}
	if (attempt.Review.Reached || attempt.Outcome.Correct) && reviews["general"].Kind == "" {
		return errors.New("missing-general-review")
	}
	if reviews["general"].SecurityRequired && reviews["security"].Kind == "" {
		return errors.New("missing-security-review")
	}
	if len(reviews) > 0 && !attempt.SemanticReview.Reached {
		return errors.New("review-stage-unreached")
	}
	generalCompleted := reviewCompleted["general"]
	if !generalCompleted.IsZero() {
		if gateCompleted.IsZero() || generalCompleted.Before(gateCompleted) {
			return errors.New("general-review-order")
		}
	}
	securityCompleted := reviewCompleted["security"]
	if !securityCompleted.IsZero() {
		prior := gateCompleted
		if !generalCompleted.IsZero() {
			prior = generalCompleted
		}
		if prior.IsZero() || securityCompleted.Before(prior) {
			return errors.New("security-review-order")
		}
	}

	if terminal != nil {
		if gate == nil || terminal.CandidateSHA != gate.CandidateSHA ||
			terminal.BaseSHA != gate.BaseSHA {
			return errors.New("terminal-gate-tuple")
		}
	}
	if attempt.Outcome.Terminal {
		terminalAt, err := time.Parse(time.RFC3339Nano, attempt.TerminalAt)
		if err != nil || !withinInterval(terminalAt, started, ended) {
			return errors.New("terminal-time")
		}
		prior := gateCompleted
		if !generalCompleted.IsZero() {
			prior = generalCompleted
		}
		if !securityCompleted.IsZero() {
			prior = securityCompleted
		}
		if prior.IsZero() || terminalAt.Before(prior) ||
			(!candidateCompleted.IsZero() && terminalAt.Before(candidateCompleted)) {
			return errors.New("terminal-order")
		}
	}
	return nil
}

func stageDistribution(
	attempts []AutonomyAttempt,
	selectTiming func(AutonomyAttempt) StageTiming,
) StageMetrics {
	queue := make([]int64, 0, len(attempts))
	service := make([]int64, 0, len(attempts))
	for _, attempt := range attempts {
		timing := selectTiming(attempt)
		if timing.QueueMS < 0 || timing.ServiceMS < 0 || !timing.Reached {
			continue
		}
		queue = append(queue, timing.QueueMS)
		service = append(service, timing.ServiceMS)
	}
	return StageMetrics{Queue: distribution(queue), Service: distribution(service)}
}

func correctTerminalLane(attempt AutonomyAttempt) bool {
	switch attempt.Outcome.Kind {
	case "merged", "done":
		return attempt.Merge.Reached
	case "pr-opened":
		return attempt.PR.Reached
	default:
		return false
	}
}

func terminalStageTiming(attempt AutonomyAttempt) StageTiming {
	if attempt.Outcome.Kind == "pr-opened" {
		return attempt.PR
	}
	return attempt.Merge
}

func stageHasEvidence(timing StageTiming) bool {
	return timing.QueueMS != 0 || timing.ServiceMS != 0
}

func distribution(values []int64) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total int64
	for _, value := range sorted {
		sum, ok := checkedAddInt64(total, value)
		if !ok {
			// Build and validation reject this structurally. Saturation here is
			// defense in depth for package-local callers and never permits a
			// wrapped negative total.
			total = math.MaxInt64
			break
		}
		total = sum
	}
	var median int64
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		low, high := sorted[mid-1], sorted[mid]
		median = low + (high-low)/2
		if (high-low)%2 != 0 {
			median++
		}
	} else {
		median = sorted[mid]
	}
	p95 := sorted[int(math.Ceil(0.95*float64(len(sorted))))-1]
	return Distribution{Count: len(sorted), TotalMS: total, MedianMS: median, P95MS: p95}
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func checkMinRatio(name string, metric RatioMetric, threshold float64) SLOCheck {
	check := checkMin(name, metric.Rate, threshold, metric.Numerator, metric.Denominator)
	if metric.Denominator == 0 {
		check.Passed = false
	}
	return check
}

func checkMaxRatio(name string, metric RatioMetric, threshold float64) SLOCheck {
	check := checkMax(name, metric.Rate, threshold)
	check.Numerator, check.Denominator = metric.Numerator, metric.Denominator
	if metric.Denominator == 0 {
		check.Passed = false
	}
	return check
}

func checkMin(name string, actual, threshold float64, numerator, denominator int) SLOCheck {
	return SLOCheck{Name: name, Passed: actual >= threshold, Actual: actual, Comparator: ">=", Threshold: threshold,
		Numerator: numerator, Denominator: denominator}
}

func checkMax(name string, actual, threshold float64) SLOCheck {
	return SLOCheck{Name: name, Passed: actual <= threshold, Actual: actual, Comparator: "<=", Threshold: threshold}
}

func validateThresholds(t AutonomyThresholds) error {
	if math.IsNaN(t.MinAutonomousCompletionRate) || math.IsInf(t.MinAutonomousCompletionRate, 0) ||
		math.IsNaN(t.MinFirstReviewPassRate) || math.IsInf(t.MinFirstReviewPassRate, 0) ||
		math.IsNaN(t.MaxRetryDispatchRate) || math.IsInf(t.MaxRetryDispatchRate, 0) ||
		math.IsNaN(t.MaxFrontierImplementation) || math.IsInf(t.MaxFrontierImplementation, 0) ||
		t.MinEligibleBeads <= 0 || t.MinAutonomousCompletionRate <= 0 || t.MinAutonomousCompletionRate > 1 ||
		t.MinFirstReviewPassRate <= 0 || t.MinFirstReviewPassRate > 1 ||
		t.MaxRetryDispatchRate < 0 || t.MaxRetryDispatchRate > 1 ||
		t.MaxFrontierImplementation < 0 || t.MaxFrontierImplementation > 1 ||
		t.MaxMedianLatencyMS <= 0 || t.MaxP95LatencyMS < t.MaxMedianLatencyMS ||
		t.MaxArtifactBytes <= 0 || t.HardArtifactBytes < t.MaxArtifactBytes {
		return errors.New("autonomy thresholds are invalid")
	}
	return nil
}

func validateAutonomyArithmetic(evidence AutonomyEvidence) error {
	var aggregate TokenEvidence
	stageTotals := make([]int64, 6)
	for i, attempt := range evidence.Attempts {
		if attempt.Tokens.ProviderTotalInput < 0 {
			return fmt.Errorf("attempt %d provider total input is negative", i+1)
		}
		if attempt.Tokens.Input >= 0 && attempt.Tokens.Output >= 0 &&
			attempt.Tokens.CacheRead >= 0 && attempt.Tokens.CacheCreation >= 0 {
			if _, ok := checkedTokenSum(attempt.Tokens); !ok {
				return fmt.Errorf("attempt %d token total overflows", i+1)
			}
		}
		tokenValues := []int64{
			attempt.Tokens.Input,
			attempt.Tokens.Output,
			attempt.Tokens.CacheRead,
			attempt.Tokens.CacheCreation,
			attempt.Tokens.Total,
			attempt.Tokens.ProviderTotalInput,
		}
		aggregateValues := []*int64{
			&aggregate.Input,
			&aggregate.Output,
			&aggregate.CacheRead,
			&aggregate.CacheCreation,
			&aggregate.Total,
			&aggregate.ProviderTotalInput,
		}
		for j, value := range tokenValues {
			if value < 0 {
				continue
			}
			sum, ok := checkedAddInt64(*aggregateValues[j], value)
			if !ok {
				return fmt.Errorf("aggregate token field %d overflows", j+1)
			}
			*aggregateValues[j] = sum
		}
		stageValues := []int64{
			attempt.Gate.QueueMS,
			attempt.Gate.ServiceMS,
			attempt.SemanticReview.QueueMS,
			attempt.SemanticReview.ServiceMS,
			attempt.Merge.QueueMS,
			attempt.Merge.ServiceMS,
		}
		for j, value := range stageValues {
			if value < 0 {
				continue
			}
			sum, ok := checkedAddInt64(stageTotals[j], value)
			if !ok {
				return fmt.Errorf("aggregate stage timing field %d overflows", j+1)
			}
			stageTotals[j] = sum
		}
	}
	byBead := make(map[string][]AutonomyAttempt)
	for _, attempt := range evidence.Attempts {
		byBead[attempt.BeadID] = append(byBead[attempt.BeadID], attempt)
	}
	var latencyTotal int64
	for _, attempts := range byBead {
		sort.Slice(attempts, func(i, j int) bool { return attempts[i].Attempt < attempts[j].Attempt })
		if len(attempts) == 0 || !attempts[len(attempts)-1].Outcome.Terminal {
			continue
		}
		start, startErr := time.Parse(time.RFC3339Nano, attempts[0].DispatchedAt)
		end, endErr := time.Parse(time.RFC3339Nano, attempts[len(attempts)-1].TerminalAt)
		if startErr != nil || endErr != nil || end.Before(start) {
			continue
		}
		var ok bool
		latencyTotal, ok = checkedAddInt64(latencyTotal, end.Sub(start).Milliseconds())
		if !ok {
			return errors.New("aggregate dispatch-to-terminal latency overflows")
		}
	}
	return nil
}

func validTokenTotal(tokens TokenEvidence) bool {
	total, ok := checkedTokenSum(tokens)
	return ok && tokens.Total == total
}

func checkedTokenSum(tokens TokenEvidence) (int64, bool) {
	total := int64(0)
	for _, value := range []int64{
		tokens.Input,
		tokens.Output,
		tokens.CacheRead,
		tokens.CacheCreation,
	} {
		var ok bool
		total, ok = checkedAddInt64(total, value)
		if !ok {
			return 0, false
		}
	}
	return total, true
}

func checkedAddInt64(left, right int64) (int64, bool) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, false
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, false
	}
	return left + right, true
}

func validateOutcome(outcome TypedOutcome) error {
	switch outcome.Kind {
	case "candidate-ready", "completion-contract-missing", "code-defect",
		"semantic-defect", "persistent-semantic-defect", "security-defect",
		"runtime-transient", "budget-exhausted", "turn-exhausted",
		"merge-base-moved", "commit-style-mechanical", "operator-stop-drain",
		"engine-invariant", "model-capability":
		if outcome.Terminal || outcome.Correct || outcome.BlockKind != "" || outcome.Capability != "" {
			return errors.New("nonterminal-truth")
		}
	case "merged", "done", "pr-opened":
		if !outcome.Terminal || !outcome.Correct || outcome.BlockKind != "" || outcome.Capability != "" {
			return errors.New("successful-terminal-truth")
		}
	case "blocked", "conflict", "failed":
		if !outcome.Terminal || outcome.Correct || outcome.BlockKind != "" || outcome.Capability != "" {
			return errors.New("failed-terminal-truth")
		}
	case OutcomeCapabilityHold:
		if !outcome.Terminal || outcome.Correct ||
			outcome.BlockKind != ExclusionExternalCapability ||
			strings.TrimSpace(outcome.Capability) == "" {
			return errors.New("capability-hold-truth")
		}
	case "capability-unavailable":
		return errors.New("capability-hold-must-be-canonical-terminal")
	default:
		return errors.New("unknown-kind")
	}
	return nil
}

func normalizeStrictCohort(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, errors.New("autonomy cohort is empty")
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return nil, errors.New("autonomy cohort contains an empty or duplicate id")
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func cohortDigest(ids []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(sum[:])
}

func validSHA256(value string) bool {
	raw := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(raw) == raw
}

func validLowerHex(value string, length int) bool {
	return len(value) == length && strings.ToLower(value) == value &&
		func() bool {
			_, err := hex.DecodeString(value)
			return err == nil
		}()
}

func validFullCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == len(value) && strings.ToLower(value) == value
}

func parseIntervalBounds(startRaw, endRaw string) (time.Time, time.Time, error) {
	start, err := time.Parse(time.RFC3339Nano, startRaw)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid canary start: %w", err)
	}
	end, err := time.Parse(time.RFC3339Nano, endRaw)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid canary end: %w", err)
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, errors.New("canary end precedes start")
	}
	return start, end, nil
}

func withinInterval(value, start, end time.Time) bool {
	return !value.Before(start) && !value.After(end)
}

func autonomyDigest(report *AutonomyReport) (string, error) {
	copy := *report
	copy.EvidenceDigest = ""
	raw, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ValidateAutonomyReport authenticates identity/evidence and independently
// recomputes the decision. It never trusts the persisted Passed bit.
func ValidateAutonomyReport(report *AutonomyReport) error {
	if report == nil || report.SchemaVersion != AutonomyReportSchema {
		return errors.New("unsupported autonomy report schema")
	}
	if strings.TrimSpace(report.ProjectID) == "" || !validFullCommit(report.InstalledCommit) ||
		strings.TrimSpace(report.BinaryVersion) == "" || strings.TrimSpace(report.BuildIdentity) == "" {
		return errors.New("autonomy report identity is incomplete")
	}
	cohort, err := normalizeStrictCohort(report.Cohort)
	if err != nil {
		return err
	}
	if cohortDigest(cohort) != report.CohortDigest {
		return errors.New("autonomy report cohort digest mismatch")
	}
	if !validSHA256(report.ContractDigest) || !validSHA256(report.EvidenceDigest) {
		return errors.New("autonomy report digest is invalid")
	}
	started, ended, err := parseIntervalBounds(report.StartedAt, report.EndedAt)
	if err != nil {
		return err
	}
	generated, err := time.Parse(time.RFC3339Nano, report.GeneratedAt)
	if err != nil || generated.Before(ended) {
		return errors.New("autonomy report generation time is invalid")
	}
	if err := validateThresholds(report.Thresholds); err != nil {
		return err
	}
	if err := validateAutonomyArithmetic(report.Evidence); err != nil {
		return err
	}
	wantMetrics, wantDecision := evaluateAutonomy(
		cohort, report.Evidence, report.Thresholds, started, ended,
		report.BinaryVersion, report.BuildIdentity,
	)
	if !jsonEqual(report.Metrics, wantMetrics) || !jsonEqual(report.Decision, wantDecision) {
		return errors.New("autonomy report derived metrics or decision mismatch")
	}
	wantDigest, err := autonomyDigest(report)
	if err != nil {
		return err
	}
	if report.EvidenceDigest != wantDigest {
		return errors.New("autonomy report evidence digest mismatch")
	}
	return nil
}

// ValidateAutonomyReportExpected first validates the report generically, then
// requires exact live release identity and freshness. A self-consistent report
// from another release is never sufficient.
func ValidateAutonomyReportExpected(report *AutonomyReport, expected AutonomyReportExpectation) error {
	if err := validateAutonomyExpectation(expected); err != nil {
		return err
	}
	if err := ValidateAutonomyReport(report); err != nil {
		return err
	}
	expectedCohort, _ := normalizeStrictCohort(expected.Cohort)
	if report.ProjectID != expected.ProjectID ||
		report.InstalledCommit != expected.InstalledCommit ||
		report.BinaryVersion != expected.BinaryVersion ||
		report.BuildIdentity != expected.BuildIdentity ||
		report.ContractDigest != expected.ContractDigest ||
		report.CohortDigest != expected.CohortDigest ||
		!jsonEqual(report.Cohort, expectedCohort) ||
		report.Thresholds != expected.Thresholds ||
		report.StartedAt != expected.CanaryStartedAt ||
		report.EvidenceDigest != expected.EvidenceDigest ||
		report.GeneratedAt != expected.GeneratedAt {
		return errors.New("autonomy report does not match expected release identity")
	}
	generated, _ := time.Parse(time.RFC3339Nano, report.GeneratedAt)
	freshAt := expected.FreshAt.UTC()
	if generated.Before(freshAt.Add(-expected.MaxAge)) {
		return errors.New("autonomy report is stale")
	}
	if generated.After(freshAt.Add(expected.MaxFutureSkew)) {
		return errors.New("autonomy report generation time is in the future")
	}
	return nil
}

func validateAutonomyExpectation(expected AutonomyReportExpectation) error {
	cohort, err := normalizeStrictCohort(expected.Cohort)
	if err != nil {
		return fmt.Errorf("autonomy report expectation is incomplete: %w", err)
	}
	if strings.TrimSpace(expected.ProjectID) == "" ||
		!validFullCommit(expected.InstalledCommit) ||
		strings.TrimSpace(expected.BinaryVersion) == "" ||
		strings.TrimSpace(expected.BuildIdentity) == "" ||
		!validSHA256(expected.ContractDigest) ||
		!validSHA256(expected.CohortDigest) ||
		expected.CohortDigest != cohortDigest(cohort) ||
		!validSHA256(expected.EvidenceDigest) ||
		expected.GeneratedAt == "" ||
		expected.FreshAt.IsZero() ||
		expected.MaxAge <= 0 ||
		expected.MaxFutureSkew < 0 {
		return errors.New("autonomy report expectation is incomplete")
	}
	if _, err := time.Parse(time.RFC3339Nano, expected.CanaryStartedAt); err != nil {
		return errors.New("autonomy report expected canary start is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, expected.GeneratedAt); err != nil {
		return errors.New("autonomy report expected generation time is invalid")
	}
	if err := validateThresholds(expected.Thresholds); err != nil {
		return fmt.Errorf("autonomy report expected thresholds: %w", err)
	}
	return nil
}

// AutonomyCohortDigest returns the canonical digest callers persist beside a
// release expectation.
func AutonomyCohortDigest(ids []string) (string, error) {
	cohort, err := normalizeStrictCohort(ids)
	if err != nil {
		return "", err
	}
	return cohortDigest(cohort), nil
}

func jsonEqual(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

// DecodeAutonomyInput performs strict decoding so misspelled evidence fields
// cannot silently disappear from a release decision.
func DecodeAutonomyInput(r io.Reader) (AutonomyInput, error) {
	var input AutonomyInput
	if err := decodeStrictAutonomyJSON(r, &input); err != nil {
		return input, err
	}
	return input, nil
}

// LoadAutonomyReport strictly loads and validates an immutable report.
func LoadAutonomyReport(path string) (*AutonomyReport, error) {
	file, err := openAutonomyRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var report AutonomyReport
	if err := decodeStrictAutonomyJSON(file, &report); err != nil {
		return nil, err
	}
	if err := ValidateAutonomyReport(&report); err != nil {
		return nil, err
	}
	return &report, nil
}

// LoadAutonomyReportExpected is the release-gate loader. Use
// LoadAutonomyReport only for internal generic inspection where no live
// release identity exists.
func LoadAutonomyReportExpected(
	path string,
	expected AutonomyReportExpectation,
) (*AutonomyReport, error) {
	report, err := LoadAutonomyReport(path)
	if err != nil {
		return nil, err
	}
	if err := ValidateAutonomyReportExpected(report, expected); err != nil {
		return nil, err
	}
	return report, nil
}

func decodeStrictAutonomyJSON(r io.Reader, target any) error {
	raw, err := io.ReadAll(io.LimitReader(r, maxAutonomyJSONBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxAutonomyJSONBytes {
		return fmt.Errorf("autonomy JSON exceeds %d bytes", maxAutonomyJSONBytes)
	}
	return strictjson.Decode(raw, target)
}

// WriteAutonomyReport publishes a create-once file. A temporary file is
// synced and hard-linked into place, so readers see either no report or the
// complete immutable bytes; an existing target is never replaced.
func WriteAutonomyReport(path string, report *AutonomyReport) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("autonomy report path is empty")
	}
	if err := ValidateAutonomyReport(report); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	parent, base, err := openAutonomyParent(path, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := autonomyTargetAvailable(int(parent.Fd()), base); err != nil {
		return err
	}
	tempName, err := autonomyTempName()
	if err != nil {
		return err
	}
	tempFD, err := unix.Openat(
		int(parent.Fd()),
		tempName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return err
	}
	temp := os.NewFile(uintptr(tempFD), tempName)
	defer func() {
		_ = unix.Unlinkat(int(parent.Fd()), tempName, 0)
	}()
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := unix.Linkat(int(parent.Fd()), tempName, int(parent.Fd()), base, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return autonomyExistingTargetError(int(parent.Fd()), base)
		}
		return err
	}
	if err := unix.Unlinkat(int(parent.Fd()), tempName, 0); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	return nil
}

func openAutonomyRegular(path string) (*os.File, error) {
	parent, base, err := openAutonomyParent(path, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(
		int(parent.Fd()),
		base,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open autonomy report without symlinks: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, errors.New("autonomy report is not a regular file")
	}
	return os.NewFile(uintptr(fd), filepath.Clean(path)), nil
}

func openAutonomyParent(path string, create bool) (*os.File, string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, "", errors.New("autonomy report path is empty")
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, "", err
	}
	relative := strings.TrimPrefix(absolute, string(filepath.Separator))
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) == 0 || parts[len(parts)-1] == "" || parts[len(parts)-1] == "." {
		return nil, "", errors.New("autonomy report path has no file name")
	}
	base := parts[len(parts)-1]
	fd, err := unix.Open(
		string(filepath.Separator),
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, "", err
	}
	for _, component := range parts[:len(parts)-1] {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(fd)
			return nil, "", errors.New("autonomy report path contains an invalid component")
		}
		next, openErr := unix.Openat(
			fd,
			component,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW,
			0,
		)
		if errors.Is(openErr, unix.ENOENT) && create {
			if mkdirErr := unix.Mkdirat(fd, component, 0o755); mkdirErr != nil &&
				!errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return nil, "", mkdirErr
			}
			next, openErr = unix.Openat(
				fd,
				component,
				unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW,
				0,
			)
		}
		if openErr != nil {
			_ = unix.Close(fd)
			return nil, "", fmt.Errorf("open autonomy report parent %q without symlinks: %w", component, openErr)
		}
		_ = unix.Close(fd)
		fd = next
	}
	return os.NewFile(uintptr(fd), filepath.Dir(absolute)), base, nil
}

func autonomyTargetAvailable(parentFD int, base string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(parentFD, base, &stat, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil:
		return autonomyModeTargetError(uint32(stat.Mode))
	case errors.Is(err, unix.ENOENT):
		return nil
	default:
		return err
	}
}

func autonomyExistingTargetError(parentFD int, base string) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, base, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ErrAutonomyReportExists
		}
		return err
	}
	return autonomyModeTargetError(uint32(stat.Mode))
}

func autonomyModeTargetError(mode uint32) error {
	if mode&unix.S_IFMT == unix.S_IFREG {
		return ErrAutonomyReportExists
	}
	if mode&unix.S_IFMT == unix.S_IFLNK {
		return errors.New("autonomy report target is a symlink")
	}
	return errors.New("autonomy report target is not a regular file")
}

func autonomyTempName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return ".autonomy-report.tmp-" + hex.EncodeToString(random[:]), nil
}
