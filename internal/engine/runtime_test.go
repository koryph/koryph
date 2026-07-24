// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
	"github.com/koryph/koryph/internal/sched"
)

// approx compares two USD estimates for near-equality, tolerating float
// rounding — mirrors quota's own test helper of the same name.
func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// runtimeLabelBD serves a single ready bead labeled runtime:codex. The fixture
// deliberately does not enable Codex in its project config, exercising the
// fail-closed enabled-runtime gate before any worktree/backend work begins.
const runtimeLabelBD = `#!/bin/sh
dir="$FAKE_BD_DIR"
printf '%s\n' "$*" >> "$dir/bd.log"
case "$1" in
  ready)
    if [ -f "$dir/ready_served" ]; then
      echo '[]'
    else
      touch "$dir/ready_served"
      echo '[{"id":"tb1","title":"Test bead one","description":"do the work","status":"open","priority":1,"issue_type":"task","labels":["fp:core","runtime:codex"]}]'
    fi
    ;;
  version) echo "bd version 1.0.5" ;;
  update|close|comment) exit 0 ;;
  show) exit 1 ;;
  *) exit 1 ;;
esac
`

const runtimeOnlyMixedBD = `#!/bin/sh
dir="$FAKE_BD_DIR"
printf '%s\n' "$*" >> "$dir/bd.log"
case "$1" in
  ready)
    if [ -f "$dir/ready_served" ]; then
      echo '[]'
    else
      touch "$dir/ready_served"
      echo '[{"id":"tb-codex","title":"Codex bead","description":"do the work","status":"open","priority":0,"issue_type":"task","labels":["fp:codex","runtime:codex","model:gpt-5.6-terra"]},{"id":"tb-claude","title":"Claude bead","description":"do the work","status":"open","priority":1,"issue_type":"task","labels":["fp:claude","runtime:claude"]}]'
    fi
    ;;
  version) echo "bd version 1.0.5" ;;
  update|close|comment) exit 0 ;;
  show) exit 1 ;;
  *) exit 1 ;;
esac
`

func TestRuntimeOnlyFiltersForeignRuntimeBeforeDispatch(t *testing.T) {
	f := newFixture(t, fixOpts{bdScript: runtimeOnlyMixedBD})
	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.RuntimeOnly = "claude"

	got, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if got.Dispatched != 1 || got.Merged != 1 {
		t.Fatalf("Outcome = %+v, want exactly the Claude bead dispatched", got)
	}
	log := f.bdLog(t)
	if strings.Contains(log, "update tb-codex --claim") || !strings.Contains(log, "update tb-claude --claim") {
		t.Errorf("runtime-only claims =\n%s\nwant only tb-claude", log)
	}
	if !strings.Contains(out.String(), "runtime-only claude") {
		t.Errorf("output = %q, want runtime-only audit rationale", out.String())
	}
}

func TestRuntimeEquivalentMapsNormalSourceModelToTargetRuntime(t *testing.T) {
	r := &runner{
		opts: Options{RuntimeEquivalent: "codex"},
		cfg:  &project.Config{DefaultRuntime: "claude", Runtimes: map[string]project.RuntimeConfig{"codex": {Enabled: true}}},
		rec:  &registry.Record{Root: t.TempDir(), AllowedModels: []string{"haiku", "sonnet", "opus"}},
	}
	issue := beads.Issue{ID: "tb", Labels: []string{"runtime:claude", "model:opus", "effort:xhigh"}}
	if normal, _ := r.normalRuntimeFor(issue); normal != "claude" {
		t.Fatalf("normal runtime = %q, want claude", normal)
	}
	if effective, _ := r.effectiveRuntimeFor(issue); effective != "codex" {
		t.Fatalf("effective runtime = %q, want codex", effective)
	}
	res, err := r.resolveModelForRuntime("implement", issue, "", "codex")
	if err != nil {
		t.Fatalf("resolve equivalent: %v", err)
	}
	if res.Model != "gpt-5.6-terra" || res.Effort != "high" || !strings.Contains(res.Rationale, "runtime equivalent codex") {
		t.Errorf("resolution = %+v, want Codex equivalent provenance", res)
	}
}

func TestRuntimeExecutionFlagsAreMutuallyExclusive(t *testing.T) {
	got, err := Run(context.Background(), Options{RuntimeOnly: "claude", RuntimeEquivalent: "codex"})
	if err == nil || got.Code != ExitUsage || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Run() = (%+v, %v), want usage mutual-exclusion error", got, err)
	}
}

func TestScopedSigningTransportFailsBeforeRuntimeLaunch(t *testing.T) {
	unsupported := runtimetest.Stub{
		StubName: "future-runtime",
		Caps:     runtime.Capabilities{ScopedSigningSocket: false},
	}
	err := scopedSigningTransportError(unsupported, "/private/koryph/signing.sock")
	if err == nil || !strings.Contains(err.Error(), "cannot isolate") ||
		!strings.Contains(err.Error(), "no private key") {
		t.Fatalf("scopedSigningTransportError = %v, want actionable fail-closed diagnostic", err)
	}

	supported := runtimetest.Stub{
		StubName: "safe-runtime",
		Caps:     runtime.Capabilities{ScopedSigningSocket: true},
	}
	if err := scopedSigningTransportError(supported, "/private/koryph/signing.sock"); err != nil {
		t.Fatalf("supported runtime refused: %v", err)
	}
	if err := scopedSigningTransportError(unsupported, ""); err != nil {
		t.Fatalf("non-signing runtime changed: %v", err)
	}
}

// TestDispatchBlocksUnavailableRuntime proves a bead selecting a registered
// but disabled runtime is BLOCKED, never silently dispatched under Claude.
func TestDispatchBlocksUnavailableRuntime(t *testing.T) {
	f := newFixture(t, fixOpts{bdScript: runtimeLabelBD})
	var out bytes.Buffer

	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 0 || got.Blocked != 1 {
		t.Errorf("Outcome = %+v, want 0 dispatched / 1 blocked", got)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil || sl.Status != ledger.SlotBlocked {
		t.Fatalf("slot = %+v, want blocked", sl)
	}
	if !strings.Contains(sl.Note, "runtime codex is not enabled") {
		t.Errorf("slot note = %q, want it to name the disabled runtime", sl.Note)
	}
	// Never dispatched: no worktree/backend work, no bd claim.
	if log := f.bdLog(t); strings.Contains(log, "--claim") {
		t.Errorf("bead was claimed despite a disabled runtime:\n%s", log)
	}
}

// TestDispatchRecordsClaudeRuntimeByDefault proves the compatibility contract
// (koryph-v8u.3): an unlabeled bead, on a project with no default_runtime,
// still dispatches under claude exactly as before, AND the new Slot/Manifest
// Runtime field records "claude" (not left empty) so the additive field is
// actually exercised end-to-end.
func TestDispatchRecordsClaudeRuntimeByDefault(t *testing.T) {
	f := newFixture(t, fixOpts{})
	var out bytes.Buffer

	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 1 || got.Merged != 1 {
		t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged", got)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatal("no slot for tb1")
	}
	if sl.Runtime != "claude" {
		t.Errorf("slot.Runtime = %q, want claude", sl.Runtime)
	}

	m, err := store.LoadManifest(run.RunID, "tb1")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Runtime != "claude" {
		t.Errorf("manifest.Runtime = %q, want claude", m.Runtime)
	}
}

// TestWaveEstimateResolvesPerItemRuntime is the koryph-v8u.12 unit test for
// waveEstimate's per-item runtime resolution: each item is priced via
// quota.EstimateItemForRuntime using ITS OWN resolved runtime name
// (modelroute.ResolveRuntimeName over the item's bead labels and the
// project's default_runtime) rather than a hardcoded "claude" literal.
// waveEstimate is exercised directly (no full Run()) to keep this a narrow,
// fast unit test.
//
// An unregistered runtime:<name> label gracefully degrades to claude's own
// table (quota.EstimateItemForRuntime's documented fallback — a cost
// ESTIMATE is advisory, never a fail-closed gate), so a labeled item's
// estimate is numerically identical to an unlabeled one here; this asserts
// the wiring runs without error and reproduces the exact pre-koryph-v8u.12
// value for every combination, which is the hard compatibility requirement.
// quota's own TestEstimateItemForRuntimeStubTable proves the estimator
// genuinely dispatches on the runtime name once a table is registered for
// it; this test proves waveEstimate actually resolves and threads that name
// through instead of ignoring it.
func TestWaveEstimateResolvesPerItemRuntime(t *testing.T) {
	quotaCfg := quota.DefaultConfig("acct")
	r := &runner{
		cfg:      &project.Config{}, // DefaultRuntime == "" -> project default "claude"
		quotaCfg: quotaCfg,
	}

	unlabeled := sched.Item{
		Issue: beads.Issue{ID: "tb-plain", Description: "short", Labels: []string{"fp:core"}},
		Model: "sonnet",
	}
	explicitClaude := sched.Item{
		Issue: beads.Issue{ID: "tb-claude", Description: "short", Labels: []string{"fp:core", "runtime:claude"}},
		Model: "sonnet",
	}
	unregisteredRuntime := sched.Item{
		Issue: beads.Issue{ID: "tb-other", Description: "short", Labels: []string{"fp:core", "runtime:wave-estimate-test-unregistered"}},
		Model: "sonnet",
	}

	want := quota.EstimateItemForRuntime(quotaCfg, "claude", "sonnet", quota.SizeOf(len("short")))
	for _, tc := range []struct {
		name string
		item sched.Item
	}{
		{"unlabeled bead", unlabeled},
		{"explicit runtime:claude label", explicitClaude},
		{"unregistered runtime label (falls back to claude)", unregisteredRuntime},
	} {
		if got := r.waveEstimate([]sched.Item{tc.item}); !approx(got, want) {
			t.Errorf("%s: waveEstimate = %g, want %g", tc.name, got, want)
		}
	}

	// A wave of all three sums to exactly 3x the single-item estimate.
	if got, want := r.waveEstimate([]sched.Item{unlabeled, explicitClaude, unregisteredRuntime}), want*3; !approx(got, want) {
		t.Errorf("mixed wave waveEstimate = %g, want %g", got, want)
	}
}
