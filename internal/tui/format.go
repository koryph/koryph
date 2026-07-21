// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"strings"
	"time"
)

// formatTimestamp renders an absolute instant with BOTH date and time.
//
// A koryph run can span several days (a wave loop is often left running
// overnight), so a bare "15:04:05" is ambiguous — 09:00 today and 09:00
// yesterday are indistinguishable. Every operator-facing absolute timestamp in
// the cockpit routes through here so the date is always present. The zero time
// renders as an em dash rather than the Go zero-value "Jan 01 00:00:00".
func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("Jan 02 15:04:05")
}

// formatTimestampShort is the compact date+time variant for narrow table
// columns: "01-02 15:04". Still carries the date so multi-day runs stay
// unambiguous; drops seconds and the month name to fit a ~11-char cell.
func formatTimestampShort(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("01-02 15:04")
}

// beadIDSuffix strips a bead ID's leading "<projectID>-" prefix, leaving the
// project-unique suffix (e.g. "koryph-9af.6" → "9af.6"). Within a single
// project's cockpit the prefix is redundant on every row; when a column is
// too narrow for the full id it is the part worth dropping first. Returns id
// unchanged when projectID is empty or is not actually id's prefix (e.g. a
// markdown phase slug that carries no project prefix at all).
func beadIDSuffix(projectID, id string) string {
	if projectID == "" {
		return id
	}
	if s, ok := strings.CutPrefix(id, projectID+"-"); ok && s != "" {
		return s
	}
	return id
}

// truncateBeadID fits a bead ID into maxLen runes, preferring to drop the
// project prefix over cutting characters from the id itself: the suffix
// ("9af.6") is what distinguishes one bead from another within a project, so
// a rune-truncated "koryph-9a…" — which keeps the redundant prefix and cuts
// the very digits that identify the bead — is strictly worse than showing
// "9af.6" in full. Falls back to rune truncation (with ellipsis) only when
// even the bare suffix does not fit.
func truncateBeadID(projectID, id string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len([]rune(id)) <= maxLen {
		return id
	}
	short := beadIDSuffix(projectID, id)
	if len([]rune(short)) <= maxLen {
		return short
	}
	return truncate(short, maxLen)
}

// renderBreadcrumb formats the Detail panel's navigation trail: navStack
// (oldest first) followed by current. Every id is shown with its project
// prefix stripped first (beadIDSuffix); if the joined trail still doesn't
// fit maxWidth, the OLDEST entries are dropped one at a time (replaced by a
// leading "…") rather than truncating the string from the right — the
// current bead is the one thing on this line the operator actually needs
// to see, so it is the last thing ever cut, and only as a final rune-level
// fallback when even its own bare suffix doesn't fit.
func renderBreadcrumb(projectID string, navStack []string, current string, maxWidth int) string {
	full := make([]string, 0, len(navStack)+1)
	for _, id := range navStack {
		full = append(full, beadIDSuffix(projectID, id))
	}
	full = append(full, beadIDSuffix(projectID, current))

	for start := range full {
		prefix := ""
		if start > 0 {
			prefix = "… → "
		}
		crumb := prefix + strings.Join(full[start:], " → ")
		if len([]rune(crumb)) <= maxWidth {
			return crumb
		}
	}
	// Even the bare current-bead suffix doesn't fit — rune-truncate it.
	return truncate(full[len(full)-1], maxWidth)
}
