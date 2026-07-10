// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package beads

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBd writes an executable shell script that prints versionLine for
// `<script> version` and returns its path, for pointing KORYPH_BD_BIN at.
func fakeBd(t *testing.T, versionLine string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bd")
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo '" + versionLine + "'; fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbeVersionParsesAndCompares(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantVer string
		wantOK  bool
	}{
		{"old dev build below min", "bd version 1.0.3 (dev)", "1.0.3", false},
		{"exactly min", "bd version 1.0.5 (Homebrew)", "1.0.5", true},
		{"newer patch", "bd version 1.0.9", "1.0.9", true},
		{"newer minor", "bd version 1.2.0", "1.2.0", true},
		{"much older", "bd version 0.9.1", "0.9.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KORYPH_BD_BIN", fakeBd(t, tc.line))
			info := ProbeVersion(context.Background())
			if !info.Found {
				t.Fatal("Found = false, want true (fake bd exists)")
			}
			if info.Version != tc.wantVer {
				t.Errorf("Version = %q, want %q", info.Version, tc.wantVer)
			}
			if info.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v (min %s)", info.OK, tc.wantOK, MinVersion)
			}
		})
	}
}

func TestProbeVersionNotFound(t *testing.T) {
	t.Setenv("KORYPH_BD_BIN", "/nonexistent/definitely-not-bd")
	info := ProbeVersion(context.Background())
	if info.Found || info.OK {
		t.Errorf("Found=%v OK=%v, want both false for a missing bd", info.Found, info.OK)
	}
	if !strings.Contains(info.Remediation(), "not on PATH") {
		t.Errorf("remediation = %q, want a not-found message", info.Remediation())
	}
}

func TestRemediationIsNixAware(t *testing.T) {
	nix := VersionInfo{Found: true, OK: false, Version: "1.0.3", Path: "/nix/store/abc-beads-1.0.3/bin/bd", FromNix: true}
	if r := nix.Remediation(); !strings.Contains(r, "nix environment") || !strings.Contains(r, "flake") {
		t.Errorf("nix remediation missing flake/nix guidance: %q", r)
	}
	brew := VersionInfo{Found: true, OK: false, Version: "1.0.3", Path: "/opt/homebrew/bin/bd", FromNix: false}
	r := brew.Remediation()
	if strings.Contains(r, "nix environment") {
		t.Errorf("non-nix remediation should not mention nix: %q", r)
	}
	if !strings.Contains(r, "brew") {
		t.Errorf("non-nix remediation should suggest a package upgrade: %q", r)
	}
}
