// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/koryph/koryph/internal/fsx"
)

const capabilityHoldsFile = "capability-holds.json"
const capabilityHoldsLockFile = "capability-holds.lock"

// CapabilityHold is the durable, project-level dispatch hold created by a
// structured capability block. It deliberately contains digests rather than
// raw environment, configuration, probe output, or operator text.
type CapabilityHold struct {
	BeadID        string `json:"bead_id"`
	Capability    string `json:"capability"`
	EvidenceHash  string `json:"evidence_hash"`
	ProbeHash     string `json:"probe_hash,omitempty"`
	ProbePassed   bool   `json:"probe_passed,omitempty"`
	OperatorHash  string `json:"operator_hash,omitempty"`
	RetryCount    int    `json:"retry_count,omitempty"`
	RetryLimit    int    `json:"retry_limit"`
	BlockedAt     string `json:"blocked_at"`
	LastRetryAt   string `json:"last_retry_at,omitempty"`
	LastRetryHash string `json:"last_retry_hash,omitempty"`
}

type capabilityHolds struct {
	SchemaVersion int                       `json:"schema_version"`
	Holds         map[string]CapabilityHold `json:"holds"`
}

func (s *Store) capabilityHoldsPath() string {
	return filepath.Join(s.KoryphRoot, capabilityHoldsFile)
}

func (s *Store) lockCapabilityHolds() (func(), error) {
	if err := os.MkdirAll(s.KoryphRoot, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.KoryphRoot, capabilityHoldsLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func (s *Store) loadCapabilityHolds() (capabilityHolds, error) {
	var state capabilityHolds
	if err := fsx.ReadJSON(s.capabilityHoldsPath(), &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return capabilityHolds{SchemaVersion: 1, Holds: map[string]CapabilityHold{}}, nil
		}
		return capabilityHolds{}, err
	}
	if state.SchemaVersion != 1 {
		return capabilityHolds{}, errors.New("unsupported capability holds schema")
	}
	if state.Holds == nil {
		state.Holds = map[string]CapabilityHold{}
	}
	return state, nil
}

func (s *Store) saveCapabilityHolds(state capabilityHolds) error {
	if err := os.MkdirAll(s.KoryphRoot, 0o755); err != nil {
		return err
	}
	state.SchemaVersion = 1
	if state.Holds == nil {
		state.Holds = map[string]CapabilityHold{}
	}
	return fsx.WriteJSONAtomic(s.capabilityHoldsPath(), state)
}

// LoadCapabilityHold returns the current hold for beadID.
func (s *Store) LoadCapabilityHold(beadID string) (CapabilityHold, bool, error) {
	state, err := s.loadCapabilityHolds()
	if err != nil {
		return CapabilityHold{}, false, err
	}
	hold, ok := state.Holds[beadID]
	return hold, ok, nil
}

// ListCapabilityHolds returns a stable bead-id ordered snapshot.
func (s *Store) ListCapabilityHolds() ([]CapabilityHold, error) {
	state, err := s.loadCapabilityHolds()
	if err != nil {
		return nil, err
	}
	out := make([]CapabilityHold, 0, len(state.Holds))
	for _, hold := range state.Holds {
		out = append(out, hold)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BeadID < out[j].BeadID })
	return out, nil
}

// SetCapabilityHold records a structured block without resetting a retry
// budget already spent for the same bead.
func (s *Store) SetCapabilityHold(hold CapabilityHold) error {
	unlock, err := s.lockCapabilityHolds()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.loadCapabilityHolds()
	if err != nil {
		return err
	}
	if old, ok := state.Holds[hold.BeadID]; ok {
		hold.RetryCount = old.RetryCount
		hold.OperatorHash = old.OperatorHash
		hold.ProbeHash = old.ProbeHash
		hold.ProbePassed = old.ProbePassed
		hold.LastRetryAt = old.LastRetryAt
		hold.LastRetryHash = old.LastRetryHash
	}
	if hold.RetryLimit <= 0 {
		hold.RetryLimit = 1
	}
	if hold.BlockedAt == "" {
		hold.BlockedAt = nowRFC3339()
	}
	state.Holds[hold.BeadID] = hold
	return s.saveCapabilityHolds(state)
}

// ConsumeCapabilityRetry atomically advances the hold's evidence baseline and
// bounded retry counter. It returns false when the evidence is unchanged or
// the capability retry budget is exhausted.
func (s *Store) ConsumeCapabilityRetry(beadID, evidenceHash string) (bool, error) {
	unlock, err := s.lockCapabilityHolds()
	if err != nil {
		return false, err
	}
	defer unlock()
	state, err := s.loadCapabilityHolds()
	if err != nil {
		return false, err
	}
	hold, ok := state.Holds[beadID]
	if !ok || evidenceHash == "" || evidenceHash == hold.EvidenceHash ||
		hold.RetryCount >= hold.RetryLimit {
		return false, nil
	}
	hold.RetryCount++
	hold.EvidenceHash = evidenceHash
	hold.LastRetryHash = evidenceHash
	hold.LastRetryAt = nowRFC3339()
	state.Holds[beadID] = hold
	return true, s.saveCapabilityHolds(state)
}

// RequestCapabilityRetry records an explicit operator request as a digest.
// The caller may pass arbitrary text; no text is persisted.
func (s *Store) RequestCapabilityRetry(beadID, text string) error {
	unlock, err := s.lockCapabilityHolds()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.loadCapabilityHolds()
	if err != nil {
		return err
	}
	hold, ok := state.Holds[beadID]
	if !ok {
		return nil
	}
	hold.OperatorHash = capabilityDigest("operator", beadID, text, nowRFC3339())
	state.Holds[beadID] = hold
	return s.saveCapabilityHolds(state)
}

// RecordCapabilityProbe records only a digest of a named probe result. A
// caller should record a successful result after its deterministic health
// check; changing the digest then becomes retry evidence.
func (s *Store) RecordCapabilityProbe(beadID, name, result string, passed bool) error {
	unlock, err := s.lockCapabilityHolds()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.loadCapabilityHolds()
	if err != nil {
		return err
	}
	hold, ok := state.Holds[beadID]
	if !ok {
		return nil
	}
	verdict := "failed"
	if passed {
		verdict = "passed"
	}
	hold.ProbeHash = capabilityDigest("probe", strings.TrimSpace(name), verdict, result)
	hold.ProbePassed = passed
	state.Holds[beadID] = hold
	return s.saveCapabilityHolds(state)
}

// ClearCapabilityHold removes a hold after successful completion or an
// explicit administrative repair.
func (s *Store) ClearCapabilityHold(beadID string) error {
	unlock, err := s.lockCapabilityHolds()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.loadCapabilityHolds()
	if err != nil {
		return err
	}
	if _, ok := state.Holds[beadID]; !ok {
		return nil
	}
	delete(state.Holds, beadID)
	return s.saveCapabilityHolds(state)
}

func capabilityDigest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
