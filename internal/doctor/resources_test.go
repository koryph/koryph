// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

// koryph-4ql.8 (docs/designs/2026-07-resource-governor.md L7 "Per-kind probe
// (opt-in)"): probe-diffing tests for DiffResourceProbe, LiveResourceHolders,
// and the checkResourceProbes doctor check. The table-driven matrix test
// covers the bead's four required scenarios end-to-end (governor.json +
// slots dir + a real `sh -c` echo probe, via Run(opts)); the narrower unit
// tests pin the shared primitives in isolation.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/govern"
)

// --- DiffResourceProbe (unit) -----------------------------------------------

func TestDiffResourceProbe_NoProbeConfigured(t *testing.T) {
	findings, err := DiffResourceProbe(context.Background(), "kind-cluster", "", nil, nil)
	if err != nil || findings != nil {
		t.Errorf("empty probeCmd: findings=%+v err=%v, want (nil, nil)", findings, err)
	}
}

func TestDiffResourceProbe_ConventionMatchOrphanFlagged(t *testing.T) {
	run := func(ctx context.Context, cmd string) (string, error) {
		return "kind-cluster-koryph-abc\n", nil
	}
	findings, err := DiffResourceProbe(context.Background(), "kind-cluster", "list", nil, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 || findings[0].Instance != "kind-cluster-koryph-abc" || findings[0].BeadID != "koryph-abc" {
		t.Errorf("findings = %+v, want one leak for koryph-abc", findings)
	}
}

func TestDiffResourceProbe_LiveLeaseNotFlagged(t *testing.T) {
	run := func(ctx context.Context, cmd string) (string, error) {
		return "kind-cluster-koryph-abc\n", nil
	}
	live := map[string]bool{"koryph-abc": true}
	findings, err := DiffResourceProbe(context.Background(), "kind-cluster", "list", live, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none (bead-id has a live lease)", findings)
	}
}

func TestDiffResourceProbe_NonMatchingNamesIgnored(t *testing.T) {
	run := func(ctx context.Context, cmd string) (string, error) {
		return "unrelated-instance\nsomething-else\n", nil
	}
	findings, err := DiffResourceProbe(context.Background(), "kind-cluster", "list", nil, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none (no name matches the kind-cluster- prefix)", findings)
	}
}

func TestDiffResourceProbe_ProbeErrorSkipsSoft(t *testing.T) {
	run := func(ctx context.Context, cmd string) (string, error) {
		return "", errors.New("boom")
	}
	findings, err := DiffResourceProbe(context.Background(), "kind-cluster", "list", nil, run)
	if err == nil {
		t.Fatal("expected the probe error to propagate to the caller for a fail-soft skip")
	}
	if findings != nil {
		t.Errorf("findings = %+v, want nil on probe error", findings)
	}
}

func TestDiffResourceProbe_BlankLinesAndDuplicatesCollapsed(t *testing.T) {
	run := func(ctx context.Context, cmd string) (string, error) {
		return "kind-cluster-a\n\nkind-cluster-a\n  \nkind-cluster-b\n", nil
	}
	findings, err := DiffResourceProbe(context.Background(), "kind-cluster", "list", nil, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 2 {
		t.Errorf("findings = %+v, want 2 (a, b) with the duplicate collapsed", findings)
	}
}

// --- LiveResourceHolders (unit) ---------------------------------------------

func TestLiveResourceHolders_NoSlotsDir(t *testing.T) {
	home := t.TempDir() // no "slots" subdir created
	holders, err := LiveResourceHolders(home+"/slots", func(int) bool { return true })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(holders) != 0 {
		t.Errorf("holders = %+v, want empty map", holders)
	}
}

func TestLiveResourceHolders_LiveAgentCounted(t *testing.T) {
	home := fabricate(t)
	slotsDir := home + "/slots"
	writeLease(t, slotsDir, govern.Lease{
		Project: "p", Bead: "b1", PID: 111, EnginePID: 222, Resources: []string{"kind-cluster"},
	})
	holders, err := LiveResourceHolders(slotsDir, func(pid int) bool { return pid == 111 })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !holders["kind-cluster"]["b1"] {
		t.Errorf("holders = %+v, want kind-cluster held by b1", holders)
	}
}

func TestLiveResourceHolders_ZombieLeaseNotCounted(t *testing.T) {
	home := fabricate(t)
	slotsDir := home + "/slots"
	writeLease(t, slotsDir, govern.Lease{
		Project: "p", Bead: "b1", PID: 111, EnginePID: 222, Resources: []string{"kind-cluster"},
	})
	// Both pids dead: a zombie lease, not a live holder.
	holders, err := LiveResourceHolders(slotsDir, func(int) bool { return false })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if holders["kind-cluster"]["b1"] {
		t.Errorf("holders = %+v, want b1 NOT counted (zombie lease)", holders)
	}
}

func TestLiveResourceHolders_PostBuildEngineAliveCounted(t *testing.T) {
	home := fabricate(t)
	slotsDir := home + "/slots"
	// Agent exited (build done), engine still alive managing review/merge —
	// mirrors checkZombieLeases' koryph-p42 precedent: still "live".
	writeLease(t, slotsDir, govern.Lease{
		Project: "p", Bead: "b1", PID: 111, EnginePID: 222, Resources: []string{"kind-cluster"},
	})
	holders, err := LiveResourceHolders(slotsDir, func(pid int) bool { return pid == 222 })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !holders["kind-cluster"]["b1"] {
		t.Errorf("holders = %+v, want b1 counted (engine alive, post-build stage)", holders)
	}
}

// --- checkResourceProbes / Run() (integration, real `sh -c` echo probes) ---

func TestCheckResourceProbes_NoResourcesSectionOK(t *testing.T) {
	home := fabricate(t)
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameResourceProbe)
	if f.Level != LevelOK {
		t.Errorf("level=%s, want ok when governor.json has no resources section: %s", f.Level, f.Message)
	}
}

func TestCheckResourceProbes_NoProbeConfiguredOK(t *testing.T) {
	home := fabricate(t)
	writeGovernorFile(t, home, govern.File{Resources: &govern.ResourcesConfig{
		Kinds: map[string]govern.ResourceKind{"kind-cluster": {Capacity: 1}}, // no Probe
	}})
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameResourceProbe)
	if f.Level != LevelOK {
		t.Errorf("level=%s, want ok when the kind has no probe configured: %s", f.Level, f.Message)
	}
}

// TestCheckResourceProbes_Matrix covers this bead's four required scenarios
// via a real `sh -c` probe (RunProbeShell, the production path) rather than
// an injected fake, so the shell-execution seam itself is exercised.
func TestCheckResourceProbes_Matrix(t *testing.T) {
	cases := []struct {
		name          string
		probeCmd      string
		lease         *govern.Lease // optional live lease
		wantWarn      bool
		wantWarnMatch string
		wantSkipped   bool
	}{
		{
			name:          "convention-matching orphan flagged",
			probeCmd:      "echo kind-cluster-koryph-abc",
			wantWarn:      true,
			wantWarnMatch: "kind-cluster-koryph-abc",
		},
		{
			name:     "convention-matching with live lease not flagged",
			probeCmd: "echo kind-cluster-koryph-abc",
			lease: &govern.Lease{
				Project: "p", Bead: "koryph-abc", PID: 111, EnginePID: 111,
				Resources: []string{"kind-cluster"},
			},
			wantWarn: false,
		},
		{
			name:     "non-matching names ignored",
			probeCmd: "echo unrelated-instance",
			wantWarn: false,
		},
		{
			name:        "probe failure is a soft skip",
			probeCmd:    "exit 1",
			wantWarn:    false,
			wantSkipped: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := fabricate(t)
			writeGovernorFile(t, home, govern.File{Resources: &govern.ResourcesConfig{
				Kinds: map[string]govern.ResourceKind{"kind-cluster": {Probe: tc.probeCmd}},
			}})
			if tc.lease != nil {
				writeLease(t, home+"/slots", *tc.lease)
			}

			o := opts(home)
			o.Alive = func(pid int) bool { return pid == 111 }
			r, err := Run(o)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			findings := findAllChecks(r, checkNameResourceProbe)

			hasWarn := false
			warnMsg := ""
			hasSkipped := false
			for _, f := range findings {
				if f.Level == LevelWarn {
					hasWarn = true
					warnMsg = f.Message
				}
				if f.Level == LevelOK && strings.Contains(f.Message, "skipped") {
					hasSkipped = true
				}
			}

			if hasWarn != tc.wantWarn {
				t.Errorf("hasWarn=%v, want %v; findings=%+v", hasWarn, tc.wantWarn, findings)
			}
			if tc.wantWarnMatch != "" && !strings.Contains(warnMsg, tc.wantWarnMatch) {
				t.Errorf("warn message = %q, want it to contain %q", warnMsg, tc.wantWarnMatch)
			}
			if hasSkipped != tc.wantSkipped {
				t.Errorf("hasSkipped=%v, want %v; findings=%+v", hasSkipped, tc.wantSkipped, findings)
			}
		})
	}
}

// TestCheckResourceProbes_InjectedRunProbe proves opts.RunProbe overrides the
// production shell runner, the same DI seam Options.Alive/LookPath use.
func TestCheckResourceProbes_InjectedRunProbe(t *testing.T) {
	home := fabricate(t)
	writeGovernorFile(t, home, govern.File{Resources: &govern.ResourcesConfig{
		Kinds: map[string]govern.ResourceKind{"docker": {Probe: "irrelevant, never actually run"}},
	}})
	o := opts(home)
	called := false
	o.RunProbe = func(ctx context.Context, cmd string) (string, error) {
		called = true
		return "docker-koryph-xyz\n", nil
	}
	r, _ := Run(o)
	if !called {
		t.Fatal("Options.RunProbe was not invoked; checkResourceProbes must use the injected runner")
	}
	f := findCheck(r, checkNameResourceProbe)
	if f.Level != LevelWarn || !strings.Contains(f.Message, "docker-koryph-xyz") {
		t.Errorf("finding = %+v, want a warn naming docker-koryph-xyz", f)
	}
}
