// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"errors"
	"log/slog"
	"testing"
)

func TestRunAttrs(t *testing.T) {
	attrs := RunAttrs("run-123", "myproject")
	if len(attrs) != 2 {
		t.Fatalf("RunAttrs len = %d, want 2", len(attrs))
	}
	checkAttr(t, attrs[0], KeyRunID, "run-123")
	checkAttr(t, attrs[1], KeyProject, "myproject")
}

func TestRunAttrsOmitEmpty(t *testing.T) {
	attrs := RunAttrs("", "")
	if len(attrs) != 0 {
		t.Errorf("RunAttrs with empty args: len=%d, want 0", len(attrs))
	}
}

func TestBeadAttrs(t *testing.T) {
	attrs := BeadAttrs("bead-abc", 3)
	if len(attrs) != 2 {
		t.Fatalf("BeadAttrs len = %d, want 2", len(attrs))
	}
	checkAttr(t, attrs[0], KeyBeadID, "bead-abc")
	if attrs[1].Key != KeyAttempt {
		t.Errorf("BeadAttrs[1].Key = %q, want %q", attrs[1].Key, KeyAttempt)
	}
}

func TestForgeAttrs(t *testing.T) {
	attrs := ForgeAttrs("anthropic", "claude-opus-4", "koryph-implementer")
	if len(attrs) != 3 {
		t.Fatalf("ForgeAttrs len = %d, want 3", len(attrs))
	}
	checkAttr(t, attrs[0], KeyProvider, "anthropic")
	checkAttr(t, attrs[1], KeyModel, "claude-opus-4")
	checkAttr(t, attrs[2], KeyPersona, "koryph-implementer")
}

func TestErrAttr(t *testing.T) {
	a := Err(errors.New("boom"))
	if a.Key != KeyError {
		t.Errorf("Err key = %q, want %q", a.Key, KeyError)
	}
	if a.Value.String() != "boom" {
		t.Errorf("Err value = %q, want boom", a.Value.String())
	}
}

func TestErrNil(t *testing.T) {
	a := Err(nil)
	if a.Key != "" {
		t.Errorf("Err(nil) should return zero attr, got key=%q", a.Key)
	}
}

func TestPhaseAttr(t *testing.T) {
	a := Phase("dispatch")
	checkAttr(t, a, KeyPhase, "dispatch")
}

// checkAttr is a small test helper.
func checkAttr(t *testing.T, a slog.Attr, wantKey, wantVal string) {
	t.Helper()
	if a.Key != wantKey {
		t.Errorf("attr key = %q, want %q", a.Key, wantKey)
	}
	if a.Value.String() != wantVal {
		t.Errorf("attr[%q] value = %q, want %q", wantKey, a.Value.String(), wantVal)
	}
}
