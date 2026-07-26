// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

const (
	// CanaryGenerationSchema identifies the canonical identity envelope whose
	// digest separates immutable fixed-path canary publications.
	CanaryGenerationSchema = "koryph.canary-generation/v1"

	// CanaryGenerationArchiveVersion is the durable retirement manifest
	// schema. Archive artifacts are create-once and authenticated by this
	// manifest before the fixed report path is released.
	CanaryGenerationArchiveVersion = 2
)

// CanaryGenerationArchive is the complete create-once manifest for one
// retired canary generation.
type CanaryGenerationArchive struct {
	SchemaVersion int    `json:"schema_version"`
	RetiredAt     string `json:"retired_at"`
	Reason        string `json:"reason"`
	ProjectID     string `json:"project_id"`

	PreviousGenerationDigest       string `json:"previous_generation_digest"`
	CurrentGenerationDigest        string `json:"current_generation_digest"`
	PreviousRegistryIdentityDigest string `json:"previous_registry_identity_digest"`
	CurrentRegistryIdentityDigest  string `json:"current_registry_identity_digest"`

	Canary CanaryState `json:"canary"`

	OriginalStatePath   string `json:"original_state_path,omitempty"`
	ArchivedStatePath   string `json:"archived_state_path,omitempty"`
	ArchivedStateDigest string `json:"archived_state_digest,omitempty"`

	OriginalMarkerPath   string `json:"original_marker_path,omitempty"`
	ArchivedMarkerPath   string `json:"archived_marker_path,omitempty"`
	ArchivedMarkerDigest string `json:"archived_marker_digest,omitempty"`
	MarkerStatus         string `json:"marker_status,omitempty"`

	OriginalReportPath   string `json:"original_report_path"`
	ArchivedReportPath   string `json:"archived_report_path"`
	ArchivedReportDigest string `json:"archived_report_digest"`
}

// SupervisorPolicy is the effective, normalized model-free retry and
// observation policy used by one loop process.
type SupervisorPolicy struct {
	IdleMinMS     int64 `json:"idle_min_ms"`
	IdleMaxMS     int64 `json:"idle_max_ms"`
	ControlPollMS int64 `json:"control_poll_ms"`
	CrashMinMS    int64 `json:"crash_min_ms"`
	CrashMaxMS    int64 `json:"crash_max_ms"`
	FailureLimit  int   `json:"failure_limit"`
	CrashLimit    int   `json:"crash_limit"`
}

// EffectiveSupervisorPolicy applies the same defaults as Supervisor.Run.
func EffectiveSupervisorPolicy(config Config) SupervisorPolicy {
	if config.IdleMin <= 0 {
		config.IdleMin = time.Second
	}
	if config.IdleMax < config.IdleMin {
		config.IdleMax = time.Minute
	}
	if config.ControlPoll <= 0 {
		config.ControlPoll = time.Second
	}
	if config.CrashMin <= 0 {
		config.CrashMin = 2 * time.Second
	}
	if config.CrashMax < config.CrashMin {
		config.CrashMax = 30 * time.Second
	}
	if config.FailureLimit <= 0 {
		config.FailureLimit = 3
	}
	if config.CrashLimit < config.FailureLimit {
		config.CrashLimit = 2 * config.FailureLimit
	}
	return SupervisorPolicy{
		IdleMinMS: config.IdleMin.Milliseconds(), IdleMaxMS: config.IdleMax.Milliseconds(),
		ControlPollMS: config.ControlPoll.Milliseconds(),
		CrashMinMS:    config.CrashMin.Milliseconds(), CrashMaxMS: config.CrashMax.Milliseconds(),
		FailureLimit: config.FailureLimit, CrashLimit: config.CrashLimit,
	}
}

// CanonicalCanaryHardStops returns the required hard-stop set plus any
// stricter caller entries in deterministic order.
func CanonicalCanaryHardStops(extra []string) []string {
	return normalizedHardStops(extra)
}

type canaryGenerationEnvelope struct {
	SchemaVersion          string   `json:"schema_version"`
	ProjectID              string   `json:"project_id"`
	InstalledCommit        string   `json:"installed_commit"`
	BinaryVersion          string   `json:"binary_version"`
	BuildIdentity          string   `json:"build_identity"`
	ContractDigest         string   `json:"contract_digest"`
	RegistryIdentityDigest string   `json:"registry_identity_digest"`
	AutonomyPolicyDigest   string   `json:"autonomy_policy_digest"`
	ExecutionPolicyDigest  string   `json:"execution_policy_digest"`
	Cohort                 []string `json:"cohort"`
	CohortDigest           string   `json:"cohort_digest"`
	TargetWidth            int      `json:"target_width"`
	InactivityLimitMS      int64    `json:"inactivity_limit_ms"`
	HardStops              []string `json:"hard_stops"`
	ReportPath             string   `json:"report_path"`
}

// CanaryGenerationDigest authenticates all stable binary, contract,
// registry, cohort, report-path, and supervisor-policy inputs that can affect
// a native canary decision.
func CanaryGenerationDigest(projectID string, spec CanarySpec) (string, error) {
	projectID = strings.TrimSpace(projectID)
	cohort := normalizedCohort(spec.Cohort)
	inactivity := spec.InactivityLimit
	if inactivity <= 0 {
		inactivity = DefaultCanaryInactivityLimit
	}
	if projectID == "" || len(cohort) < 2 || len(cohort) != len(spec.Cohort) ||
		spec.TargetWidth < 2 || spec.TargetWidth > len(cohort) ||
		inactivity.Milliseconds() <= 0 ||
		!validCommit(strings.TrimSpace(spec.InstalledCommit)) ||
		strings.TrimSpace(spec.BinaryVersion) == "" ||
		strings.TrimSpace(spec.BuildIdentity) == "" ||
		!validDigest(strings.TrimSpace(spec.ContractDigest)) ||
		!validDigest(strings.TrimSpace(spec.RegistryIdentityDigest)) ||
		!validDigest(strings.TrimSpace(spec.AutonomyPolicyDigest)) ||
		!validDigest(strings.TrimSpace(spec.ExecutionPolicyDigest)) ||
		!filepath.IsAbs(filepath.Clean(strings.TrimSpace(spec.ReportPath))) {
		return "", errors.New("loop: canary generation identity is incomplete")
	}
	envelope := canaryGenerationEnvelope{
		SchemaVersion:          CanaryGenerationSchema,
		ProjectID:              projectID,
		InstalledCommit:        strings.TrimSpace(spec.InstalledCommit),
		BinaryVersion:          strings.TrimSpace(spec.BinaryVersion),
		BuildIdentity:          strings.TrimSpace(spec.BuildIdentity),
		ContractDigest:         strings.TrimSpace(spec.ContractDigest),
		RegistryIdentityDigest: strings.TrimSpace(spec.RegistryIdentityDigest),
		AutonomyPolicyDigest:   strings.TrimSpace(spec.AutonomyPolicyDigest),
		ExecutionPolicyDigest:  strings.TrimSpace(spec.ExecutionPolicyDigest),
		Cohort:                 cohort,
		CohortDigest:           cohortDigest(cohort),
		TargetWidth:            spec.TargetWidth,
		InactivityLimitMS:      inactivity.Milliseconds(),
		HardStops:              normalizedHardStops(spec.HardStops),
		ReportPath:             filepath.Clean(strings.TrimSpace(spec.ReportPath)),
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CanaryStateGenerationDigest recomputes a generation identity from a durable
// checkpoint. It supports schema-v3 checkpoints written before the digest was
// stored explicitly.
func CanaryStateGenerationDigest(projectID string, state *CanaryState) (string, error) {
	if state == nil {
		return "", errors.New("loop: missing canary state")
	}
	return CanaryGenerationDigest(projectID, CanarySpec{
		Cohort:                 append([]string(nil), state.Cohort...),
		TargetWidth:            state.TargetWidth,
		InactivityLimit:        time.Duration(state.InactivityLimitMS) * time.Millisecond,
		HardStops:              append([]string(nil), state.HardStops...),
		InstalledCommit:        state.InstalledCommit,
		BinaryVersion:          state.BinaryVersion,
		BuildIdentity:          state.BuildIdentity,
		ContractDigest:         state.ContractDigest,
		RegistryIdentityDigest: state.RegistryIdentityDigest,
		AutonomyPolicyDigest:   state.AutonomyPolicyDigest,
		ExecutionPolicyDigest:  state.ExecutionPolicyDigest,
		ReportPath:             filepath.Clean(state.ReportPath),
	})
}
