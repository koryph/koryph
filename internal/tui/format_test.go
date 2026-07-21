// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import "testing"

func TestBeadIDSuffix(t *testing.T) {
	cases := []struct {
		projectID, id, want string
	}{
		{"koryph", "koryph-9af.6", "9af.6"},
		{"koryph", "koryph-b01", "b01"},
		// No project id known yet (before the first snapshot) — unchanged.
		{"", "koryph-9af.6", "koryph-9af.6"},
		// Id does not actually carry the project prefix (e.g. a markdown
		// phase slug) — unchanged, not mangled.
		{"koryph", "phase-1", "phase-1"},
		// Project id alone with a trailing hyphen and nothing after it must
		// not collapse to an empty string.
		{"koryph", "koryph-", "koryph-"},
	}
	for _, tc := range cases {
		if got := beadIDSuffix(tc.projectID, tc.id); got != tc.want {
			t.Errorf("beadIDSuffix(%q, %q) = %q, want %q", tc.projectID, tc.id, got, tc.want)
		}
	}
}

func TestTruncateBeadID(t *testing.T) {
	cases := []struct {
		name          string
		projectID, id string
		maxLen        int
		want          string
	}{
		{
			name:      "fits fully, no change",
			projectID: "koryph", id: "koryph-9af.6", maxLen: 14,
			want: "koryph-9af.6",
		},
		{
			name:      "too long, prefix dropped instead of rune-truncated",
			projectID: "a-very-long-project-name", id: "a-very-long-project-name-9af.6", maxLen: 14,
			want: "9af.6",
		},
		{
			name:      "suffix itself still too long, falls back to rune truncation",
			projectID: "proj", id: "proj-extremely-long-suffix-that-does-not-fit", maxLen: 10,
			want: truncate("extremely-long-suffix-that-does-not-fit", 10),
		},
		{
			name:      "no project id known — falls back to plain truncate",
			projectID: "", id: "koryph-9af.6", maxLen: 8,
			want: truncate("koryph-9af.6", 8),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateBeadID(tc.projectID, tc.id, tc.maxLen); got != tc.want {
				t.Errorf("truncateBeadID(%q, %q, %d) = %q, want %q", tc.projectID, tc.id, tc.maxLen, got, tc.want)
			}
		})
	}
}

// TestRenderBreadcrumbKeepsCurrentBeadVisible proves the breadcrumb drops
// the OLDEST entries (never the current, trailing bead) when it must shrink
// to fit, and strips the project prefix from every entry first.
func TestRenderBreadcrumbKeepsCurrentBeadVisible(t *testing.T) {
	nav := []string{"koryph-1af.1", "koryph-2bg.2", "koryph-3ch.3"}
	current := "koryph-9af.6"

	// Plenty of room: every entry shown, project-stripped.
	if got := renderBreadcrumb("koryph", nav, current, 200); got != "1af.1 → 2bg.2 → 3ch.3 → 9af.6" {
		t.Errorf("wide breadcrumb = %q", got)
	}

	// Only enough room for the last couple entries — must end with the
	// current bead, never the reverse.
	got := renderBreadcrumb("koryph", nav, current, 20)
	if got == "" {
		t.Fatal("breadcrumb must not be empty")
	}
	if got[len(got)-len("9af.6"):] != "9af.6" {
		t.Errorf("narrow breadcrumb = %q, must end with the current bead's suffix", got)
	}
	if len([]rune(got)) > 20 {
		t.Errorf("narrow breadcrumb exceeds maxWidth: %q (%d runes)", got, len([]rune(got)))
	}

	// Pathologically narrow: even the bare current suffix must be shown
	// (rune-truncated), not silently dropped.
	got = renderBreadcrumb("koryph", nav, current, 3)
	if got == "" {
		t.Error("breadcrumb must render something even at maxWidth=3")
	}
}
