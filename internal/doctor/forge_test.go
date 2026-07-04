// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
)

func TestCheckForge(t *testing.T) {
	cases := []struct {
		name      string
		cfg       *project.Config
		wantLevel Level
		wantMsg   string
	}{
		{
			name:      "nil config degrades gracefully",
			cfg:       nil,
			wantLevel: LevelOK,
			wantMsg:   "skipped",
		},
		{
			name:      "empty forge defaults to github",
			cfg:       &project.Config{Forge: ""},
			wantLevel: LevelOK,
			wantMsg:   "forge: github",
		},
		{
			name:      "explicit github",
			cfg:       &project.Config{Forge: "github"},
			wantLevel: LevelOK,
			wantMsg:   "forge: github",
		},
		{
			name:      "gitlab",
			cfg:       &project.Config{Forge: "gitlab"},
			wantLevel: LevelOK,
			wantMsg:   "forge: gitlab",
		},
		{
			name:      "unrecognised forge is warn",
			cfg:       &project.Config{Forge: "bitbucket"},
			wantLevel: LevelWarn,
			wantMsg:   "bitbucket",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := checkForge(tc.cfg)
			if f.Check != checkNameForge {
				t.Errorf("Check = %q, want %q", f.Check, checkNameForge)
			}
			if f.Level != tc.wantLevel {
				t.Errorf("Level = %q, want %q (message: %q)", f.Level, tc.wantLevel, f.Message)
			}
			if tc.wantMsg != "" && !strings.Contains(f.Message, tc.wantMsg) {
				t.Errorf("Message = %q, want it to contain %q", f.Message, tc.wantMsg)
			}
		})
	}
}
