// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/koryph/koryph/internal/fsx"
)

// Operator control files (koryph-57v.1, docs/user-guide/running-waves.md
// "Drain and resize"): sit NEXT TO koryph.lock, directly under KoryphRoot —
// the same natural home as the run ledger and lock, not the repo's tracked
// tree (.plan-logs is gitignored) and not central ~/.koryph (a project
// already has a per-project state dir here; no need to key by project id a
// second time). Both are read by runner.governorGate so waveLoop and
// rollingLoop honor them identically at every scheduling boundary.
const (
	drainFile  = "drain.request"
	resizeFile = "resize.json"
)

// DrainSentinel is the one-shot operator drain request written by
// `koryph drain`. Presence alone is the signal — the timestamp is
// informational only (progress lines / debugging), never consulted for
// control flow.
type DrainSentinel struct {
	RequestedAt string `json:"requested_at"`
}

func (s *Store) drainPath() string  { return filepath.Join(s.KoryphRoot, drainFile) }
func (s *Store) resizePath() string { return filepath.Join(s.KoryphRoot, resizeFile) }

// RequestDrain writes (or refreshes) the one-shot drain sentinel. Idempotent:
// requesting a drain that is already pending just refreshes the timestamp,
// it never errors or duplicates state.
func (s *Store) RequestDrain() error {
	if err := os.MkdirAll(s.KoryphRoot, 0o755); err != nil {
		return err
	}
	return fsx.WriteJSONAtomic(s.drainPath(), DrainSentinel{RequestedAt: nowRFC3339()})
}

// DrainRequested reports whether an operator drain sentinel is currently
// present. Re-checked at every wave/refill boundary (governorGate) — never
// cached, so `koryph drain` takes effect on the very next boundary of an
// already-running loop with no restart.
func (s *Store) DrainRequested() bool {
	return fsx.Exists(s.drainPath())
}

// ConsumeDrain removes the drain sentinel (best-effort: a missing file is not
// an error) and reports whether one was actually present. Called from two
// places: the engine's normal operator-drain finalize path (so the next run
// starts clean) and unconditionally at every run's start (koryph-57v.1) — a
// sentinel stranded by a run that never got back around to a boundary (e.g.
// killed out-of-band) must not instantly drain-and-exit a fresh, intentional
// run before it dispatches anything.
func (s *Store) ConsumeDrain() bool {
	if !s.DrainRequested() {
		return false
	}
	_ = os.Remove(s.drainPath())
	return true
}

// ResizeOverride is the live wave-width override written by `koryph resize`.
// Unlike the drain sentinel it is NOT one-shot: it stays in effect (re-read
// at every boundary) until an operator clears it with `koryph resize
// --clear`, surviving across runs exactly like a project config change would.
type ResizeOverride struct {
	Max   int    `json:"max"`
	Force bool   `json:"force,omitempty"` // written verbatim for status/audit visibility; clamping itself happens at write time (cmd/koryph)
	SetAt string `json:"set_at"`
}

// SetResize writes (or replaces) the width override atomically.
func (s *Store) SetResize(o ResizeOverride) error {
	if o.SetAt == "" {
		o.SetAt = nowRFC3339()
	}
	if err := os.MkdirAll(s.KoryphRoot, 0o755); err != nil {
		return err
	}
	return fsx.WriteJSONAtomic(s.resizePath(), o)
}

// LoadResize reads the current width override. ok is false when no override
// is set, or the file is missing/corrupt/non-positive — this fails OPEN to
// "no override" (defer to project config) rather than wedging dispatch width
// on a bad file; a resize override is a convenience lever, not a safety path.
func (s *Store) LoadResize() (ResizeOverride, bool) {
	var o ResizeOverride
	if err := fsx.ReadJSON(s.resizePath(), &o); err != nil {
		return ResizeOverride{}, false
	}
	if o.Max <= 0 {
		return ResizeOverride{}, false
	}
	return o, true
}

// ClearResize removes the width override (best-effort; a missing file is not
// an error), reverting the next boundary's width resolution to project
// config.
func (s *Store) ClearResize() error {
	err := os.Remove(s.resizePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
