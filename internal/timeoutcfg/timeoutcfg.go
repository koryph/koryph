// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package timeoutcfg is the single home for koryph's unified agent-facing wall
// timeout and its override hierarchy (koryph-w82i).
//
// One default — BuiltinDefaultSec (1200s / 20 min) — governs every agent-facing
// wall timeout (the post-implement reviewer, each pipeline stage, epic
// validation). It is overridable at three levels, in strict precedence:
//
//		bead  >  project  >  system  >  built-in (1200)
//
//	  - bead    — a bare `timeout:<seconds>` label on the bead (BeadTimeout),
//	    mirroring the `model:<tier>` / `runtime:<name>` bare-label grammar in
//	    internal/modelroute. Applies to that bead's agent-facing timeouts.
//	  - project — the existing koryph.project.json fields (review.timeout_seconds,
//	    per-stage timeout_sec, epic_validation.timeout_seconds).
//	  - system  — a machine-wide default in ~/.koryph/config.json
//	    (signing.GlobalConfig.DefaultTimeoutSeconds). Applies to every project on
//	    the machine that does not override it.
//	  - built-in — BuiltinDefaultSec, when nothing above sets a value.
//
// Resolve applies that ladder. There is NO hard ceiling: an explicit override at
// any level may exceed BuiltinDefaultSec (the former 20-minute review cap was
// removed in koryph-w82i); only values <= 0 are treated as "unset". A per-tool
// break-glass runtime override (e.g. review's KORYPH_REVIEW_TIMEOUT_SEC) still
// sits above this ladder in the tool that reads it.
package timeoutcfg

import (
	"strconv"
	"strings"
)

// BuiltinDefaultSec is the built-in agent-facing wall timeout (20 minutes)
// applied when nothing at the bead, project, or system level sets one.
const BuiltinDefaultSec = 1200

// MaxSaneSec is a defensive upper bound (30 days) applied to any resolved
// timeout. It is NOT the policy ceiling the design deliberately removed — no
// real agent timeout comes anywhere near it. Its sole job is to stop an absurd
// value from overflowing `time.Duration(sec) * time.Second` (an int64
// nanosecond count wraps around ~9.2e9 seconds), which would produce a negative
// deadline that kills the agent instantly instead of granting the long run the
// operator asked for (koryph-w82i review finding). A caller that passes a value
// above this is clamped down to it rather than wrapping.
const MaxSaneSec = 30 * 24 * 60 * 60 // 2_592_000

// Clamp bounds sec to the defensive overflow guard (see MaxSaneSec): a value
// above MaxSaneSec is clamped down to it; everything else passes through
// unchanged. It does not floor at 0 — callers decide their own "unset" meaning.
func Clamp(sec int) int {
	if sec > MaxSaneSec {
		return MaxSaneSec
	}
	return sec
}

// beadLabelPrefix is the bead-label prefix for a per-bead timeout override.
const beadLabelPrefix = "timeout:"

// BeadTimeout parses a bead's bare `timeout:<seconds>` label, mirroring the
// bare-label grammar of `model:<tier>` / `runtime:<name>` in internal/modelroute.
// It returns the first well-formed positive value and true; (0, false) when no
// valid label is present. A value that is empty, non-numeric, non-positive, or
// carries a further ':' (a hypothetical stage-scoped form, reserved) is skipped
// rather than treated as a bare seconds value — the same defensive rule
// plainModelLabel applies so a scoped label can never be mistaken for a bare one.
func BeadTimeout(labels []string) (int, bool) {
	for _, l := range labels {
		rest, ok := strings.CutPrefix(l, beadLabelPrefix)
		if !ok || rest == "" || strings.Contains(rest, ":") {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 {
			continue
		}
		return n, true
	}
	return 0, false
}

// Resolve applies the unified precedence bead > project > system > built-in and
// returns the winning timeout in seconds. Each argument <= 0 means "unset at
// that level". The result is always > 0 (BuiltinDefaultSec when every level is
// unset). No policy ceiling is imposed — an explicit override may exceed the
// built-in default — but the winner is passed through Clamp so an absurd value
// can never overflow a time.Duration (see MaxSaneSec).
func Resolve(beadSec, projectSec, systemSec int) int {
	switch {
	case beadSec > 0:
		return Clamp(beadSec)
	case projectSec > 0:
		return Clamp(projectSec)
	case systemSec > 0:
		return Clamp(systemSec)
	default:
		return BuiltinDefaultSec
	}
}
