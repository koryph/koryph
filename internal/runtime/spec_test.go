// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeFinalOutputPathIsRunPrivateSibling(t *testing.T) {
	phase := filepath.Join(t.TempDir(), "run-1", "bead-1")
	got := RuntimeFinalOutputPath(phase)
	want := filepath.Join(
		filepath.Dir(phase), ".runtime-output", "bead-1", RuntimeFinalOutputFileName,
	)
	if got != want {
		t.Fatalf("RuntimeFinalOutputPath = %q, want %q", got, want)
	}
	rel, err := filepath.Rel(phase, got)
	if err != nil {
		t.Fatal(err)
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("runtime output remained inside worker-writable phase: %q", got)
	}
}
