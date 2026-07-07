// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package project

import "testing"

func TestCopyrightConfigResolvers(t *testing.T) {
	tests := []struct {
		name        string
		c           *CopyrightConfig
		wantText    string
		wantLicense string
	}{
		{"nil → built-in default", nil, "(c) 2026 The Koryph Developers", "Apache-2.0"},
		{"empty → built-in default", &CopyrightConfig{}, "(c) 2026 The Koryph Developers", "Apache-2.0"},
		{"holder only keeps default year", &CopyrightConfig{Holder: "Acme, Inc."}, "(c) 2026 Acme, Inc.", "Apache-2.0"},
		{"full override", &CopyrightConfig{Holder: "The Foo Authors", Year: "2024-2026", License: "MIT"}, "(c) 2024-2026 The Foo Authors", "MIT"},
		{"license only", &CopyrightConfig{License: "GPL-3.0-or-later"}, "(c) 2026 The Koryph Developers", "GPL-3.0-or-later"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.FileCopyrightText(); got != tc.wantText {
				t.Errorf("FileCopyrightText() = %q, want %q", got, tc.wantText)
			}
			if got := tc.c.LicenseID(); got != tc.wantLicense {
				t.Errorf("LicenseID() = %q, want %q", got, tc.wantLicense)
			}
		})
	}
}
