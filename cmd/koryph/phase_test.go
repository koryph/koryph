// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
)

func TestPhaseLabelAddRequiresDispatchEnvironment(t *testing.T) {
	t.Setenv("KORYPH_PHASE_DIR", "")
	t.Setenv("KORYPH_DIR", "")
	t.Setenv("KORYPH_PHASE_ID", "")
	code, _, errb := runCmd("phase", "request", "label-add", "--label", "area:docs", "--timeout", "1ms")
	if code != engine.ExitFatal || errb == "" {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
}

func TestPhaseLabelAddRejectsControlLabel(t *testing.T) {
	code, _, _ := runCmd("phase", "request", "label-add", "--label", "model:frontier")
	if code != engine.ExitUsage {
		t.Fatalf("code=%d, want usage", code)
	}
}

func TestPhaseBlockWritesStructuredStatus(t *testing.T) {
	dir := t.TempDir()
	status := filepath.Join(dir, "status.json")
	if err := os.WriteFile(status, []byte(`{"state":"running","step":"work","pct":25}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_DIR", dir)
	t.Setenv("KORYPH_PHASE_ID", "bead-1")
	t.Setenv("KORYPH_STATUS_PATH", status)
	code, out, errb := runCmd("phase", "block", "--capability", "runtime-canary", "--detail", "profile unavailable")
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errb)
	}
	data, err := os.ReadFile(status)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || !containsAll(string(data), `"block_kind": "capability"`, `"capability": "runtime-canary"`) {
		t.Fatalf("status=%s", data)
	}
}

func containsAll(s string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(s, value) {
			return false
		}
	}
	return true
}
