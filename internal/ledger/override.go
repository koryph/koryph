// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"path/filepath"

	"github.com/koryph/koryph/internal/fsx"
)

// overrideFile is the operator-override sidecar that lives beside a run's
// ledger.json. It is the supported channel for signalling a manual state change
// to a running engine: hand-editing ledger.json does not work because the engine
// holds the run in memory and its single-writer rewrite clobbers the edit (D5).
// The operator (or `koryph merge` on a manual land) appends here; the engine —
// still the ONLY writer of ledger.json — reads it each cycle and folds the
// overrides into its in-memory run, so the next rewrite adopts them.
const overrideFile = "overrides.json"

// SlotOverride is one operator directive: adopt Status (a terminal state such as
// "merged") for the slot of BeadID, recording Note as the reason. At is the
// RFC3339 time it was recorded.
type SlotOverride struct {
	BeadID string `json:"bead_id"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
	At     string `json:"at,omitempty"`
}

// OverrideFile is the on-disk shape of the sidecar.
type OverrideFile struct {
	Overrides []SlotOverride `json:"overrides"`
}

// overridePath is the sidecar path for a run.
func (s *Store) overridePath(runID string) string {
	return filepath.Join(s.KoryphRoot, runID, overrideFile)
}

// LoadOverrides reads a run's override sidecar. A missing sidecar is the common
// case and returns an empty set with no error.
func (s *Store) LoadOverrides(runID string) (OverrideFile, error) {
	var of OverrideFile
	path := s.overridePath(runID)
	if !fsx.Exists(path) {
		return of, nil
	}
	if err := fsx.ReadJSON(path, &of); err != nil {
		return OverrideFile{}, err
	}
	return of, nil
}

// RecordOverride appends an override to a run's sidecar (read-modify-write,
// atomic). It is used by callers OTHER than the engine — `koryph merge` on a
// manual land — so the engine adopts the change rather than reverting it. A
// duplicate (same bead + status) collapses so the sidecar cannot grow without
// bound when the same manual action is repeated.
func (s *Store) RecordOverride(runID string, ov SlotOverride) error {
	if ov.At == "" {
		ov.At = nowRFC3339()
	}
	of, err := s.LoadOverrides(runID)
	if err != nil {
		return err
	}
	for i, existing := range of.Overrides {
		if existing.BeadID == ov.BeadID {
			of.Overrides[i] = ov // newest directive per bead wins
			return fsx.WriteJSONAtomic(s.overridePath(runID), of)
		}
	}
	of.Overrides = append(of.Overrides, ov)
	return fsx.WriteJSONAtomic(s.overridePath(runID), of)
}
