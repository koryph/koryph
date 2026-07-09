// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/govern"
)

// resBead is one synthetic frontier entry for the resource loop tests: an id,
// a priority (lower dispatches first), and its literal label set.
type resBead struct {
	id       string
	priority int
	labels   []string
}

// resourceBDScript renders a fake `bd` serving a fixed frontier, dropping a bead
// from `ready` once it has been claimed or closed (the multiBeadBDScript
// approach) so a rolling loop sees a claimed bead disappear on the next tick.
// Unlike multiBeadBDScript it takes an arbitrary label set per bead, so a bead
// can declare a res:<kind> label.
func resourceBDScript(beads []resBead) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("dir=\"$FAKE_BD_DIR\"\n")
	b.WriteString("printf '%s\\n' \"$*\" >> \"$dir/bd.log\"\n")
	b.WriteString("log=\"$dir/bd.log\"\n")
	b.WriteString("case \"$1\" in\n")
	b.WriteString("  ready)\n")
	b.WriteString("    items=\"\"\n")
	for _, s := range beads {
		var labels []string
		for _, l := range s.labels {
			labels = append(labels, fmt.Sprintf(`\"%s\"`, l))
		}
		entry := fmt.Sprintf(`{\"id\":\"%s\",\"title\":\"%s\",\"description\":\"x\",\"status\":\"open\",\"priority\":%d,\"issue_type\":\"task\",\"labels\":[%s]}`,
			s.id, s.id, s.priority, strings.Join(labels, ","))
		fmt.Fprintf(&b, "    if ! grep -q '^update %s --claim$' \"$log\" 2>/dev/null && ! grep -q '^close %s ' \"$log\" 2>/dev/null; then\n", s.id, s.id)
		b.WriteString("      if [ -n \"$items\" ]; then items=\"$items,\"; fi\n")
		fmt.Fprintf(&b, "      items=\"$items%s\"\n", entry)
		b.WriteString("    fi\n")
	}
	b.WriteString("    echo \"[$items]\"\n")
	b.WriteString("    ;;\n")
	b.WriteString("  version) echo \"bd version 1.0.5\" ;;\n")
	b.WriteString("  update|close|comment) exit 0 ;;\n")
	b.WriteString("  show) exit 1 ;;\n")
	b.WriteString("  *) exit 1 ;;\n")
	b.WriteString("esac\n")
	return b.String()
}

// holdForeignResource seeds a live foreign-project lease holding kind at the
// default capacity 1, so this project's declaring bead is denied at Acquire.
func holdForeignResource(t *testing.T, kind string) {
	t.Helper()
	gs := govern.NewStore()
	if err := gs.Hold(govern.Lease{
		Project: "other", Bead: "foreign-1", PID: os.Getpid(), EnginePID: os.Getpid(),
		Resources: []string{kind},
	}); err != nil {
		t.Fatalf("seed foreign lease: %v", err)
	}
}

// TestWaveLoopResourceSkipContinues proves the wave loop's per-bead skip
// (koryph-4ql.3, design L3): a higher-priority bead denied on a capacity-1
// resource kind held by another project SKIPS, and the lightweight bead behind
// it still dispatches and merges in the SAME wave — not a batch-break.
func TestWaveLoopResourceSkipContinues(t *testing.T) {
	newFixture(t, fixOpts{
		claudeScript: slowClaudeScript,
		bdScript: resourceBDScript([]resBead{
			// Distinct fp:* tokens so the two beads do NOT footprint-conflict
			// (an undeclared bead is domain:unknown, which serializes) — the
			// point here is the RESOURCE skip, not a footprint deferral.
			{id: "heavy-1", priority: 1, labels: []string{"fp:hvy", "res:kind-cluster"}},
			{id: "light-1", priority: 2, labels: []string{"fp:lgt"}},
		}),
	})
	holdForeignResource(t, "kind-cluster")

	var out bytes.Buffer
	opts := baseOptions(&out) // Once=true → wave loop
	opts.DispatchMode = "wave"
	got, err := Run(context.Background(), opts)
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Exactly the lightweight bead dispatched+merged; the resource-heavy one was
	// skipped, not batch-broken (which would have dispatched neither).
	if got.Dispatched != 1 || got.Merged != 1 {
		t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged (light-1 through, heavy-1 skipped)", got)
	}
	if !strings.Contains(out.String(), "bead light-1: dispatched") {
		t.Errorf("light-1 was not dispatched behind the skipped bead:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "heavy-1: deferred — resource kind-cluster at capacity") {
		t.Errorf("heavy-1 deferral (resource + holder) not logged:\n%s", out.String())
	}
	if strings.Contains(out.String(), "bead heavy-1: dispatched") {
		t.Errorf("heavy-1 dispatched despite a full capacity-1 kind:\n%s", out.String())
	}
}

// TestRollingLoopResourceSkipContinues is TestWaveLoopResourceSkipContinues for
// the rolling loop: identical per-bead skip semantics, driven through the
// continuous-refill path. The run never drains (heavy-1 stays resource-blocked),
// so it runs in a goroutine and is cancelled once both outcomes are observed.
func TestRollingLoopResourceSkipContinues(t *testing.T) {
	newFixture(t, fixOpts{
		claudeScript: slowClaudeScript,
		bdScript: resourceBDScript([]resBead{
			// Distinct fp:* tokens so the two beads do NOT footprint-conflict
			// (an undeclared bead is domain:unknown, which serializes) — the
			// point here is the RESOURCE skip, not a footprint deferral.
			{id: "heavy-1", priority: 1, labels: []string{"fp:hvy", "res:kind-cluster"}},
			{id: "light-1", priority: 2, labels: []string{"fp:lgt"}},
		}),
	})
	holdForeignResource(t, "kind-cluster")

	out := &syncBuf{}
	opts := baseOptions(nil)
	opts.Out = out
	opts.Once = false
	opts.DispatchMode = "rolling"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, opts)
		close(done)
	}()

	ok := waitForCondition(10*time.Second, func() bool {
		s := out.String()
		return strings.Contains(s, "bead light-1: dispatched") &&
			strings.Contains(s, "heavy-1: deferred — resource kind-cluster at capacity")
	})
	cancel()
	<-done
	if !ok {
		t.Errorf("rolling loop did not both dispatch light-1 and defer heavy-1 on resource capacity:\n%s", out.String())
	}
	if strings.Contains(out.String(), "bead heavy-1: dispatched") {
		t.Errorf("heavy-1 dispatched despite a full capacity-1 kind:\n%s", out.String())
	}
}

// TestWaveLoopBoundaryPacing proves the wave-mode boundary pacing (koryph-4ql.3,
// design L3/R3): a boundary that dispatches nothing (its sole bead is
// resource-blocked) with nothing active must SLEEP one poll tick rather than
// re-scan in a tight loop. With a 1s poll tick, a ~1.3s window admits only a
// couple of deferral re-scans, not the hundreds an un-paced hot-spin would
// produce.
func TestWaveLoopBoundaryPacing(t *testing.T) {
	newFixture(t, fixOpts{
		claudeScript: slowClaudeScript,
		bdScript: resourceBDScript([]resBead{
			{id: "heavy-1", priority: 1, labels: []string{"res:kind-cluster"}},
		}),
	})
	holdForeignResource(t, "kind-cluster")

	out := &syncBuf{}
	opts := baseOptions(nil)
	opts.Out = out
	opts.Once = false
	opts.DispatchMode = "wave"
	opts.PollSec = 1 // one-tick pacing granularity
	ctx, cancel := context.WithTimeout(context.Background(), 1300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, opts)
		close(done)
	}()
	<-done

	deferrals := strings.Count(out.String(), "heavy-1: deferred — resource kind-cluster at capacity")
	if deferrals == 0 {
		t.Fatalf("expected at least one resource deferral; got none:\n%s", out.String())
	}
	// Un-paced, the loop would re-scan as fast as bd ready returns (hundreds of
	// times in 1.3s). Paced at 1s/tick, only ~1-2 boundaries occur.
	if deferrals > 5 {
		t.Errorf("wave loop hot-spun: %d resource deferrals in ~1.3s (want <= 5, one per poll tick)", deferrals)
	}
}
