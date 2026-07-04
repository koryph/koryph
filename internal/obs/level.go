// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import "log/slog"

// LevelTrace is a custom slog level below Debug, following the slog
// convention of negative values for finer-grained levels. Handlers that
// do not explicitly handle TRACE treat it as a verbose debug event.
const LevelTrace slog.Level = slog.LevelDebug - 4

// LevelNames maps each level value to its display string for handlers
// that format their own level labels.
var LevelNames = map[slog.Level]string{
	LevelTrace:      "TRACE",
	slog.LevelDebug: "DEBUG",
	slog.LevelInfo:  "INFO",
	slog.LevelWarn:  "WARN",
	slog.LevelError: "ERROR",
}

// LevelString returns the canonical name for level l.
// For levels not in LevelNames the slog default is used.
func LevelString(l slog.Level) string {
	if s, ok := LevelNames[l]; ok {
		return s
	}
	return l.String()
}

// ParseLevel converts a case-insensitive level name to a slog.Level.
// Returns (level, true) on success; (slog.LevelInfo, false) on unknown input.
func ParseLevel(s string) (slog.Level, bool) {
	switch {
	case equalFold(s, "trace"):
		return LevelTrace, true
	case equalFold(s, "debug"):
		return slog.LevelDebug, true
	case equalFold(s, "info"):
		return slog.LevelInfo, true
	case equalFold(s, "warn"), equalFold(s, "warning"):
		return slog.LevelWarn, true
	case equalFold(s, "error"):
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// equalFold is a minimal ASCII case-insensitive compare to avoid importing strings.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}
