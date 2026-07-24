// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketDirIsIndependentOfDeepTMPDIR(t *testing.T) {
	t.Setenv("KORYPH_HOME", "/var/folders/very-long-koryph-home")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "a", "deep", "phase", "directory", "that", "must", "not", "reach", "the", "socket"))
	got := SocketDir("test-ssh-agent:/very/deep/phase")
	if strings.HasPrefix(got, t.TempDir()) {
		t.Fatalf("SocketDir = %q, must not use TMPDIR", got)
	}
	if len(filepath.Join(got, "signing.sock")) >= 104 {
		t.Fatalf("socket path = %q, exceeds macOS limit", filepath.Join(got, "signing.sock"))
	}
	if want := SocketDir("test-ssh-agent:/very/deep/phase"); got != want {
		t.Errorf("SocketDir = %q, want deterministic %q", got, want)
	}
}

func TestSigningDirUsesShortSocketRoot(t *testing.T) {
	t.Setenv("KORYPH_HOME", "/a/koryph/home")
	if got, want := SigningDir(), SocketDir("signing"); got != want {
		t.Errorf("SigningDir = %q, want %q", got, want)
	}
}
