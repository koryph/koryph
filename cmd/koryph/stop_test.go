// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"strings"
	"testing"
)

func TestStopAllEmptyRegistry(t *testing.T) {
	isolate(t)
	runCmd("init")
	code, out, _ := runCmd("stop", "--all")
	if code != 0 {
		t.Fatalf("stop --all: code %d out %q", code, out)
	}
	if !strings.Contains(out, "0 agent(s) across 0 project(s)") {
		t.Errorf("stop --all on empty registry: %q", out)
	}
}

func TestStopAllRejectsPhaseID(t *testing.T) {
	isolate(t)
	runCmd("init")
	code, _, errs := runCmd("stop", "--all", "somebead")
	if code == 0 || !strings.Contains(errs, "takes no phase-id") {
		t.Errorf("stop --all somebead: code=%d stderr=%q", code, errs)
	}
}

// --all --project scopes the sweep to one project instead of the whole
// registry. An unknown project id is a not-found error, not the old
// "takes neither --project nor a phase-id" rejection.
func TestStopAllScopedToProject(t *testing.T) {
	isolate(t)
	runCmd("init")
	code, _, errs := runCmd("stop", "--all", "--project", "nope")
	if code == 0 || !strings.Contains(errs, "not found") {
		t.Errorf("stop --all --project nope: code=%d stderr=%q", code, errs)
	}
}

func TestStopRequiresProjectOrAll(t *testing.T) {
	isolate(t)
	runCmd("init")
	code, _, errs := runCmd("stop", "somebead")
	if code == 0 || !strings.Contains(errs, "--project is required") {
		t.Errorf("stop without --project/--all: code=%d stderr=%q", code, errs)
	}
}
