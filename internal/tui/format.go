// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import "time"

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
