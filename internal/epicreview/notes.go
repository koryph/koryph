// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package epicreview

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Note codec for the two terminal-state notes appended to an epic bead:
// parked (round cap exceeded) and degraded (validator infra failure). This is
// the single source of truth for both formats — engine.parkEpic and Act write
// via the Format* functions, doctor's health check and any future reader
// parse via the Parse* functions. Keeping writer and reader in one file
// closes the drift surface a hand-maintained regex on the reader side would
// otherwise reopen every time a Sprintf on the writer side changes.
//
// New notes are always emitted in the colon form (validation:parked,
// validation:degraded — matching the LabelParked/LabelDegraded label
// spelling). ParseParkedNote additionally accepts the legacy space-form
// ("validation parked: ...") that earlier engine code emitted, so beads
// carrying old notes still parse correctly.

// reParkedColon matches the canonical parked note:
//
//	"validation:parked (round N): would exceed max_rounds=M. Operator recovery: ..."
var reParkedColon = regexp.MustCompile(`validation:parked \(round (\d+)\): would exceed max_rounds=(\d+)`)

// reParkedLegacy matches the pre-koryph-qta.7 parked note emitted by
// engine.parkEpic:
//
//	"validation parked: round N would exceed max_rounds=M. Operator recovery: ..."
var reParkedLegacy = regexp.MustCompile(`validation parked: round (\d+) would exceed max_rounds=(\d+)`)

// reDegraded matches the degraded note emitted by Act:
//
//	"validation:degraded (round N): <reason>"
var reDegraded = regexp.MustCompile(`validation:degraded \(round (\d+)\):\s*(.+)`)

// FormatParkedNote renders the round-cap terminal note that engine.parkEpic
// appends when a round would exceed max_rounds (design §2). recoveryCmd is
// the exact operator command line appended verbatim after
// "Operator recovery: " (e.g. "koryph epic validate <id> --project <proj>").
func FormatParkedNote(round, maxRounds int, recoveryCmd string) string {
	return fmt.Sprintf(
		"validation:parked (round %d): would exceed max_rounds=%d. Operator recovery: %s",
		round, maxRounds, recoveryCmd)
}

// ParseParkedNote extracts the round and max_rounds from a single note line.
// It accepts both the canonical colon form and the legacy space form
// ("validation parked: round N ...") that older beads carry. ok is false
// when the line matches neither form.
func ParseParkedNote(s string) (round int, maxRounds int, ok bool) {
	m := reParkedColon.FindStringSubmatch(s)
	if m == nil {
		m = reParkedLegacy.FindStringSubmatch(s)
	}
	if m == nil {
		return 0, 0, false
	}
	round, _ = strconv.Atoi(m[1])
	maxRounds, _ = strconv.Atoi(m[2])
	return round, maxRounds, true
}

// FormatDegradedNote renders the note Act appends when a round degrades
// (validator infra failure rather than a completeness verdict).
func FormatDegradedNote(round int, reason string) string {
	return fmt.Sprintf("validation:degraded (round %d): %s", round, reason)
}

// ParseDegradedNote extracts the round and reason from a single note line.
// ok is false when the line does not match the degraded note form.
func ParseDegradedNote(s string) (round int, reason string, ok bool) {
	m := reDegraded.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	round, _ = strconv.Atoi(m[1])
	return round, strings.TrimSpace(m[2]), true
}
