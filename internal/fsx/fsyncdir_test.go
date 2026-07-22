// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFsyncDirSucceedsOnRealDir is the koryph audit finding #32 fix's happy
// path: WriteAtomic's rename is followed by an fsync of the PARENT
// directory, so the directory-entry change (not just the file's data) is
// durable across a crash — without it, a crash between rename() and the next
// unrelated fsync of that directory can resurrect the pre-rename state even
// though the caller observed a successful write.
func TestFsyncDirSucceedsOnRealDir(t *testing.T) {
	if err := fsyncDir(t.TempDir()); err != nil {
		t.Errorf("fsyncDir(real dir) = %v, want nil", err)
	}
}

// TestFsyncDirBestEffortOnMissingDir proves a directory that cannot even be
// opened (already removed, permissions) is treated as best-effort — nothing
// stronger is achievable, and the rename this follows has already succeeded —
// rather than turning into a hard WriteAtomic failure.
func TestFsyncDirBestEffortOnMissingDir(t *testing.T) {
	if err := fsyncDir(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Errorf("fsyncDir(missing dir) = %v, want nil (best effort)", err)
	}
}

// TestWriteAtomic_ParentDirFsynced is a regression guard proving WriteAtomic
// itself still succeeds end-to-end now that it fsyncs the parent directory
// after rename — a failure here would mean the durability fix broke the
// existing write path rather than just hardening it.
func TestWriteAtomic_ParentDirFsynced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "durable.txt")
	if err := WriteAtomic(path, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("content = %q, want %q", got, "payload")
	}
}
