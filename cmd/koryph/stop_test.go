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

func TestStopAllRejectsProjectAndPhase(t *testing.T) {
	isolate(t)
	runCmd("init")
	code, _, errs := runCmd("stop", "--all", "--project", "x")
	if code == 0 || !strings.Contains(errs, "neither --project nor a phase-id") {
		t.Errorf("stop --all --project x: code=%d stderr=%q", code, errs)
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
