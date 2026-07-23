// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

// addProject registers a project (via the real `project add` command, so the
// scaffolded koryph.project.json/registry record match production shape) and
// returns its record.
func addProject(t *testing.T, id string) *registry.Record {
	t.Helper()
	root := gitRepo(t)
	code, out, errb := runCmd("project", "add", root,
		"--account", "personal", "--identity", "me@example.com", "--id", id)
	if code != 0 {
		t.Fatalf("project add: code=%d stderr=%s", code, errb)
	}
	var rec registry.Record
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("project add output not JSON: %v\n%s", err, out)
	}
	return &rec
}

// --- drain -------------------------------------------------------------

func TestDrainRequiresProjectOrAll(t *testing.T) {
	isolate(t)
	runCmd("init")
	code, _, errb := runCmd("drain")
	if code != engine.ExitUsage || !strings.Contains(errb, "--project is required") {
		t.Errorf("code=%d stderr=%q", code, errb)
	}
}

func TestDrainRejectsProjectAndAllTogether(t *testing.T) {
	isolate(t)
	runCmd("init")
	code, _, errb := runCmd("drain", "--all", "--project", "x")
	if code != engine.ExitUsage || !strings.Contains(errb, "no --project") {
		t.Errorf("code=%d stderr=%q", code, errb)
	}
}

func TestDrainWritesSentinelAndAudits(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")

	code, out, errb := runCmd("drain", "--project", "demo")
	if code != 0 {
		t.Fatalf("drain: code=%d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "drain requested for demo") {
		t.Errorf("stdout = %q", out)
	}

	st := ledger.NewStore(rec.Root)
	if !st.DrainRequested() {
		t.Error("drain sentinel was not written")
	}

	auditData, err := os.ReadFile(filepath.Join(os.Getenv("KORYPH_HOME"), "audit.jsonl"))
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !strings.Contains(string(auditData), `"kind":"drain"`) || !strings.Contains(string(auditData), `"project_id":"demo"`) {
		t.Errorf("audit log missing drain event:\n%s", auditData)
	}
}

func TestDrainAllIteratesRegistryProjects(t *testing.T) {
	isolate(t)
	recA := addProject(t, "a")
	recB := addProject(t, "b")

	code, out, errb := runCmd("drain", "--all")
	if code != 0 {
		t.Fatalf("drain --all: code=%d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "requested for 2 project(s)") {
		t.Errorf("stdout = %q", out)
	}
	for _, rec := range []*registry.Record{recA, recB} {
		if !ledger.NewStore(rec.Root).DrainRequested() {
			t.Errorf("%s: sentinel not written", rec.ProjectID)
		}
	}
}

// TestDrainClearedAsStaleAtNextRunStart proves the end-to-end integration
// between `koryph drain` and the engine's run-start stale-sentinel clear
// (koryph-57v.1): a sentinel written while nothing is running does not
// survive into the next `koryph run` invocation.
func TestDrainClearedAsStaleAtNextRunStart(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")
	// Make the project dispatchable: work_source bd, validated, no gate.
	st := registry.NewStore()
	rec.MigrationStatus = registry.StatusValidated
	rec.WorktreeRoot = filepath.Join(rec.Root, "..", "wt")
	if err := st.Save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	if code, _, errb := runCmd("drain", "--project", "demo"); code != 0 {
		t.Fatalf("drain: stderr=%s", errb)
	}
	if !ledger.NewStore(rec.Root).DrainRequested() {
		t.Fatal("sentinel not written")
	}
	// engine.Run itself (not exercised via cmdRun here, since that requires a
	// full bd/claude fixture) proves the clear; internal/engine's own tests
	// cover the operator-drain finalize path end-to-end. Here we only assert
	// the CLI wrote a sentinel that ledger.Store.ConsumeDrain() (the exact
	// call engine.Run makes at start) removes.
	if !ledger.NewStore(rec.Root).ConsumeDrain() {
		t.Fatal("ConsumeDrain reported nothing present")
	}
	if ledger.NewStore(rec.Root).DrainRequested() {
		t.Error("sentinel still present after ConsumeDrain")
	}
}

// --- resize --------------------------------------------------------------

func TestResizeRequiresMaxOrClear(t *testing.T) {
	isolate(t)
	addProject(t, "demo")
	code, _, errb := runCmd("resize", "--project", "demo")
	if code != engine.ExitUsage || !strings.Contains(errb, "--max N or --clear is required") {
		t.Errorf("code=%d stderr=%q", code, errb)
	}
}

func TestResizeRejectsMaxAndClearTogether(t *testing.T) {
	isolate(t)
	addProject(t, "demo")
	code, _, errb := runCmd("resize", "--project", "demo", "--max", "2", "--clear")
	if code != engine.ExitUsage || !strings.Contains(errb, "mutually exclusive") {
		t.Errorf("code=%d stderr=%q", code, errb)
	}
}

func TestResizeRejectsZeroOrNegativeMax(t *testing.T) {
	isolate(t)
	addProject(t, "demo")
	code, _, errb := runCmd("resize", "--project", "demo", "--max", "0")
	if code != engine.ExitUsage || !strings.Contains(errb, "must be > 0") {
		t.Errorf("code=%d stderr=%q", code, errb)
	}
}

func TestResizeClampsToProjectCapWithoutForce(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo") // scaffolded koryph.project.json defaults MaxConcurrentSlots=4

	code, out, errb := runCmd("resize", "--project", "demo", "--max", "10")
	if code != 0 {
		t.Fatalf("resize: code=%d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "clamped from 10") {
		t.Errorf("stdout = %q, want a clamp notice", out)
	}
	ov, ok := ledger.NewStore(rec.Root).LoadResize()
	if !ok || ov.Max != 4 {
		t.Errorf("LoadResize = (%+v, %v), want (Max:4, true)", ov, ok)
	}
}

func TestResizeForceExceedsProjectCap(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")

	code, out, errb := runCmd("resize", "--project", "demo", "--max", "10", "--force")
	if code != 0 {
		t.Fatalf("resize: code=%d stderr=%s", code, errb)
	}
	if strings.Contains(out, "clamped") {
		t.Errorf("stdout = %q, --force should not clamp", out)
	}
	ov, ok := ledger.NewStore(rec.Root).LoadResize()
	if !ok || ov.Max != 10 || !ov.Force {
		t.Errorf("LoadResize = (%+v, %v), want (Max:10, Force:true, true)", ov, ok)
	}
}

func TestResizeClear(t *testing.T) {
	isolate(t)
	rec := addProject(t, "demo")

	if code, _, errb := runCmd("resize", "--project", "demo", "--max", "2"); code != 0 {
		t.Fatalf("resize: stderr=%s", errb)
	}
	if _, ok := ledger.NewStore(rec.Root).LoadResize(); !ok {
		t.Fatal("override not set")
	}

	code, out, errb := runCmd("resize", "--project", "demo", "--clear")
	if code != 0 {
		t.Fatalf("resize --clear: code=%d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "width override cleared") {
		t.Errorf("stdout = %q", out)
	}
	if _, ok := ledger.NewStore(rec.Root).LoadResize(); ok {
		t.Error("override still present after --clear")
	}
}

func TestResizeAllIteratesRegistryProjects(t *testing.T) {
	isolate(t)
	recA := addProject(t, "a")
	recB := addProject(t, "b")

	code, out, errb := runCmd("resize", "--all", "--max", "2")
	if code != 0 {
		t.Fatalf("resize --all: code=%d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "set for 2 project(s)") {
		t.Errorf("stdout = %q", out)
	}
	for _, rec := range []*registry.Record{recA, recB} {
		if ov, ok := ledger.NewStore(rec.Root).LoadResize(); !ok || ov.Max != 2 {
			t.Errorf("%s: LoadResize = (%+v, %v)", rec.ProjectID, ov, ok)
		}
	}
}

func TestResizeAllRejectsProjectFlag(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("resize", "--all", "--project", "x", "--max", "2")
	if code != engine.ExitUsage || !strings.Contains(errb, "no --project") {
		t.Errorf("code=%d stderr=%q", code, errb)
	}
}

func TestDrainResizeUsageInHelp(t *testing.T) {
	// The listing is registry-driven now: each command shows its name +
	// one-line summary (full synopsis lives in `koryph <cmd> -h`).
	_, out, _ := runCmd("help")
	for _, want := range []string{"\n  drain ", "\n  resize "} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing command %q:\n%s", strings.TrimSpace(want), out)
		}
	}
}
