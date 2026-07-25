// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/promptc"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

func TestRepositoryContractFollowsRuntimeCapability(t *testing.T) {
	root := t.TempDir()
	want := promptc.RepositoryClauseMarker + "\nRepository rules."
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	projected, err := repositoryContractForRuntime(root, runtimetest.Stub{})
	if err != nil {
		t.Fatal(err)
	}
	if projected != want {
		t.Fatalf("projected contract = %q, want %q", projected, want)
	}

	native, err := repositoryContractForRuntime(root, runtimetest.Stub{
		Caps: runtime.Capabilities{RepositoryInstructions: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if native != "" {
		t.Fatalf("native runtime received duplicate repository contract %q", native)
	}
}

func TestRepositoryContractFailsClosed(t *testing.T) {
	root := t.TempDir()
	if _, err := repositoryContractForRuntime(root, runtimetest.Stub{}); err == nil {
		t.Fatal("missing AGENTS.md was accepted")
	}
	if err := os.WriteFile(
		filepath.Join(root, "AGENTS.md"),
		[]byte(promptc.RepositoryClauseMarker+"\n"+promptc.EngineClauseMarker),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryContractForRuntime(root, runtimetest.Stub{}); err == nil ||
		!strings.Contains(err.Error(), "semantic clause") {
		t.Fatalf("foreign semantic owner error = %v", err)
	}
}

func TestMissingRepositoryContractTripsCircuitAndTerminatesRun(t *testing.T) {
	f := newFixture(t, fixOpts{})
	if err := os.Remove(filepath.Join(f.repo, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.repo, "add", "-A")
	runGit(t, f.repo, "commit", "--no-verify", "-m", "test: remove repository contract")

	var out bytes.Buffer
	var tripwires []SafetyTripwire
	opts := baseOptions(&out)
	opts.OnSafetyTripwire = func(event SafetyTripwire) {
		tripwires = append(tripwires, event)
	}
	got, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if got.Reason != "engine-invariant" || got.Dispatched != 0 {
		t.Fatalf("outcome = %+v, want one bounded engine-invariant stop\n%s", got, out.String())
	}
	if len(tripwires) != 1 ||
		tripwires[0].Kind != SafetyTripwireEngineInvariant ||
		tripwires[0].BeadID != "tb1" ||
		!strings.Contains(tripwires[0].Detail, "AGENTS.md") {
		t.Fatalf("tripwires = %+v, want one repository-contract invariant", tripwires)
	}
	run, err := ledger.NewStore(f.repo).LoadRun(got.RunID)
	if err != nil {
		t.Fatal(err)
	}
	slot := run.Slots["tb1"]
	if slot == nil || slot.Status != ledger.SlotBlocked ||
		slot.OutcomeClass != string(OutcomeEngineInvariant) {
		t.Fatalf("slot = %+v, want blocked engine invariant", slot)
	}
	if claims := strings.Count(f.bdLog(t), "update tb1 --claim"); claims > 1 {
		t.Fatalf("bead was redispatched %d times:\n%s", claims, f.bdLog(t))
	}
}
