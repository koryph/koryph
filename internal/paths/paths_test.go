// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package paths

import (
	"os"
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

func TestEnsureSocketDirRepairsExistingMode(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	dir := SocketDir("mode-test")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if got, err := EnsureSocketDir("mode-test"); err != nil || got != dir {
		t.Fatalf("EnsureSocketDir = %q, %v; want %q, nil", got, err, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("socket directory mode = %04o, want 0700", got)
	}
}

func TestEnsureSocketDirRejectsSymlink(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	dir := SocketDir("symlink-test")
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir) })
	if _, err := EnsureSocketDir("symlink-test"); err == nil {
		t.Fatal("EnsureSocketDir accepted a symlink")
	}
}
