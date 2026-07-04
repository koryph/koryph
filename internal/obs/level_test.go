// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"log/slog"
	"testing"
)

func TestLevelTrace(t *testing.T) {
	if LevelTrace >= slog.LevelDebug {
		t.Errorf("LevelTrace (%d) must be less than LevelDebug (%d)", LevelTrace, slog.LevelDebug)
	}
}

func TestLevelString(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  string
	}{
		{LevelTrace, "TRACE"},
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARN"},
		{slog.LevelError, "ERROR"},
	}
	for _, tc := range cases {
		got := LevelString(tc.level)
		if got != tc.want {
			t.Errorf("LevelString(%d) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		input string
		want  slog.Level
		ok    bool
	}{
		{"trace", LevelTrace, true},
		{"TRACE", LevelTrace, true},
		{"debug", slog.LevelDebug, true},
		{"DEBUG", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"INFO", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"WARNING", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"ERROR", slog.LevelError, true},
		{"unknown", slog.LevelInfo, false},
		{"", slog.LevelInfo, false},
	}
	for _, tc := range cases {
		got, ok := ParseLevel(tc.input)
		if ok != tc.ok {
			t.Errorf("ParseLevel(%q) ok=%v, want %v", tc.input, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}
