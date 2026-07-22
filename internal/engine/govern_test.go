// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/quota"
)

// TestGlobalGovernorDefersWhenCapFull proves the wave loop honors the machine
// global concurrency cap (koryph-1xk): with the single global slot already
// held by another project's agent, this project's ready bead is deferred rather
// than dispatched.
func TestGlobalGovernorDefersWhenCapFull(t *testing.T) {
	f := newFixture(t, fixOpts{})

	// Cap the machine at 1 and consume that slot with a live foreign lease
	// (keyed to this test process's pid so it is not pruned as dead).
	gs := govern.NewStore()
	if err := gs.SetCap(fixtureAccount, 1); err != nil {
		t.Fatal(err)
	}
	if err := gs.Hold(govern.Lease{
		Project: "other", Bead: "x1", PID: os.Getpid(), EnginePID: os.Getpid(),
		Provider: fixtureAccount,
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got.Dispatched != 0 {
		t.Errorf("Dispatched = %d, want 0 (global cap full)", got.Dispatched)
	}
	if !strings.Contains(out.String(), "global governor cap or memory floor reached") {
		t.Errorf("expected a deferral log line; got:\n%s", out.String())
	}
	// The bead was neither claimed nor given a worktree.
	if log := f.bdLog(t); strings.Contains(log, "update tb1 --claim") {
		t.Errorf("bead claimed despite deferral:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(f.wtRoot, "agent-tb1")); !os.IsNotExist(err) {
		t.Errorf("worktree created despite deferral: stat err = %v", err)
	}
}

// TestGlobalGovernorDispatchesWhenSlotFree confirms the same fixture dispatches
// normally once the global slot is available (cap not exceeded).
func TestGlobalGovernorDispatchesWhenSlotFree(t *testing.T) {
	newFixture(t, fixOpts{}) // sets KORYPH_HOME + registry the run needs
	gs := govern.NewStore()
	if err := gs.SetCap(fixtureAccount, 1); err != nil { // cap 1, but no lease held → one free slot
		t.Fatal(err)
	}

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 1 || got.Merged != 1 {
		t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged", got)
	}
	// The slot was released at merge — no lingering lease for this project.
	_, leases, _, err := gs.Snapshot(fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range leases {
		if l.Project == "proj" {
			t.Errorf("global slot for proj not released after merge: %+v", l)
		}
	}
}

// TestGlobalGovernorDefersUsingQuotaSeededCap is the koryph-1o2.3 admission
// regression: with NO explicit `governor set --account` cap for this
// account, a persisted quota.Config.MaxThreads seed must still be the cap
// admission enforces — not silently ignored in favor of
// govern.DefaultMaxGlobalAgents (8). A foreign lease alone would never fill
// the default cap of 8, so this only defers if the seed (1) actually won.
func TestGlobalGovernorDefersUsingQuotaSeededCap(t *testing.T) {
	newFixture(t, fixOpts{})
	if _, err := quota.SetMaxThreads(fixtureAccount, 1); err != nil {
		t.Fatal(err)
	}

	gs := govern.NewStore()
	if err := gs.Hold(govern.Lease{
		Project: "other", Bead: "x1", PID: os.Getpid(), EnginePID: os.Getpid(),
		Provider: fixtureAccount,
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 0 {
		t.Errorf("Dispatched = %d, want 0 (quota-seeded cap of 1 already held by another project)", got.Dispatched)
	}
}

// TestGlobalGovernorExplicitCapWinsOverQuotaSeed proves precedence: an
// explicit `governor set --account` cap always wins over the quota seed,
// even when the seed is smaller (koryph-1o2.3, precedence level 1 vs 2).
func TestGlobalGovernorExplicitCapWinsOverQuotaSeed(t *testing.T) {
	newFixture(t, fixOpts{})
	if _, err := quota.SetMaxThreads(fixtureAccount, 1); err != nil {
		t.Fatal(err)
	}
	gs := govern.NewStore()
	if err := gs.SetCap(fixtureAccount, 5); err != nil {
		t.Fatal(err)
	}
	if err := gs.Hold(govern.Lease{
		Project: "other", Bead: "x1", PID: os.Getpid(), EnginePID: os.Getpid(),
		Provider: fixtureAccount,
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 1 {
		t.Errorf("Dispatched = %d, want 1 (explicit cap 5 > 1 active foreign lease; must win over the smaller quota seed)", got.Dispatched)
	}
}
