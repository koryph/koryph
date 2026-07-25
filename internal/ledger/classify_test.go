// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyTable(t *testing.T) {
	run := &Run{
		Slots: map[string]*Slot{
			// alive PID → reattach
			"alive": {PhaseID: "alive", Status: SlotRunning, PID: 4242},
			// dead + recorded commits → requeue-resume (reason names commit)
			"resume": {PhaseID: "resume", Status: SlotRunning, PID: 7, Commits: 3, LastCommit: "cafef00d"},
			// dead + no commits → requeue-fresh
			"fresh": {PhaseID: "fresh", Status: SlotRunning, PID: 7, Commits: 0},
			// attempts exhausted → blocked (precedence over liveness)
			"blocked": {PhaseID: "blocked", Status: SlotRunning, PID: 7, Attempts: MaxAttempts},
			// completion-ready dead candidates finalize before attempt exhaustion.
			"finalize": {
				PhaseID: "finalize", Status: SlotReview, PID: 7, Commits: 2,
				DispatchBaseSHA: "base", DispatchGeneration: "dispatch",
				CandidateGeneration: "generation", CandidateResultPath: "result.json",
			},
			"merging": {
				PhaseID: "merging", Status: SlotMerging, PID: 7, Commits: 2,
				DispatchBaseSHA: "base", DispatchGeneration: "dispatch",
				CandidateGeneration: "generation", CandidateResultPath: "result.json",
			},
			"finalize-max": {
				PhaseID: "finalize-max", Status: SlotRunning, PID: 7, Commits: 2, Attempts: MaxAttempts,
				DispatchBaseSHA: "base", DispatchGeneration: "dispatch",
				CandidateGeneration: "generation", CandidateResultPath: "result.json",
			},
			// terminal → skip
			"merged": {PhaseID: "merged", Status: SlotMerged},
			// stuck + dead + Commits==0 but branch has commits via probe → requeue-resume
			"fallback": {PhaseID: "fallback", Status: SlotStuck, PID: 7, Branch: "has-commits"},
		},
	}
	p := Probe{
		Alive: func(pid int) bool { return pid == 4242 },
		CommitCount: func(branch string) (int, error) {
			if branch == "has-commits" {
				return 5, nil
			}
			return 0, nil
		},
		CompletionReady: func(sl *Slot) bool {
			return sl.Status == SlotReview || sl.Status == SlotMerging || sl.PhaseID == "finalize-max"
		},
	}

	got := Classify(run, p)

	want := map[string]string{
		"alive":        ActionReattach,
		"resume":       ActionRequeueResume,
		"fresh":        ActionRequeueFresh,
		"blocked":      ActionBlocked,
		"finalize":     ActionFinalize,
		"merging":      ActionFinalize,
		"finalize-max": ActionFinalize,
		"merged":       ActionSkip,
		"fallback":     ActionRequeueResume,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d decisions, want %d: %+v", len(got), len(want), got)
	}
	byPhase := map[string]Decision{}
	for _, d := range got {
		byPhase[d.PhaseID] = d
	}
	for phase, action := range want {
		d, ok := byPhase[phase]
		if !ok {
			t.Errorf("no decision for phase %q", phase)
			continue
		}
		if d.Action != action {
			t.Errorf("phase %q: action = %q, want %q (reason %q)", phase, d.Action, action, d.Reason)
		}
	}

	// requeue-resume reason must reference the last commit.
	if r := byPhase["resume"].Reason; !strings.Contains(r, "cafef00d") {
		t.Errorf("resume reason missing last commit: %q", r)
	}
	// fallback resume was derived from the probe (5 commits).
	if r := byPhase["fallback"].Reason; !strings.Contains(r, "5") {
		t.Errorf("fallback reason missing probe commit count: %q", r)
	}

	// Output must be sorted by PhaseID for determinism.
	for i := 1; i < len(got); i++ {
		if got[i-1].PhaseID > got[i].PhaseID {
			t.Fatalf("output not sorted by PhaseID: %v", got)
		}
	}
}

func TestClassifyDoesNotFinalizeAdvisoryLegacyArtifacts(t *testing.T) {
	run := &Run{Slots: map[string]*Slot{
		"legacy": {
			PhaseID: "legacy", Status: SlotRunning, PID: 7, Commits: 1,
			StatusPath: "status.json",
		},
		"legacy-review": {
			PhaseID: "legacy-review", Status: SlotReview, PID: 7, Commits: 1,
			CandidateGeneration: "candidate-only", CandidateResultPath: "result.json",
		},
	}}
	got := Classify(run, Probe{
		CompletionReady: func(*Slot) bool { return true },
	})
	if len(got) != 2 || got[0].Action != ActionRequeueResume || got[1].Action != ActionRequeueResume {
		t.Fatalf("legacy advisory candidate = %+v, want preserved requeue-resume", got)
	}
}

func TestLegacySlotJSONDecodesWithZeroTypedLifecycle(t *testing.T) {
	var slot Slot
	if err := json.Unmarshal([]byte(`{
		"phase_id":"legacy","status":"running","attempts":2,
		"commits":1,"note":"old string policy"
	}`), &slot); err != nil {
		t.Fatal(err)
	}
	if slot.PhaseID != "legacy" || slot.Attempts != 2 || slot.CandidateGeneration != "" ||
		slot.CandidateResultPath != "" || slot.DispatchBaseSHA != "" || slot.DispatchGeneration != "" ||
		slot.OutcomeClass != "" || slot.Retry != (RetryCounters{}) {
		t.Fatalf("legacy slot decoded incompatibly: %+v", slot)
	}
}

func TestClassifyNilAliveTreatsAsDead(t *testing.T) {
	run := &Run{Slots: map[string]*Slot{
		"x": {PhaseID: "x", Status: SlotRunning, PID: 1, Commits: 0},
	}}
	got := Classify(run, Probe{}) // both funcs nil
	if len(got) != 1 || got[0].Action != ActionRequeueFresh {
		t.Fatalf("nil Alive should treat PID as dead → requeue-fresh, got %+v", got)
	}
}

func TestClassifyIdentityAwareProbeRejectsRecycledPID(t *testing.T) {
	run := &Run{Slots: map[string]*Slot{
		"reused": {PhaseID: "reused", Status: SlotRunning, PID: 4242},
	}}
	got := Classify(run, Probe{
		Alive:     func(int) bool { return true },
		AliveSlot: func(*Slot) bool { return false },
	})
	if len(got) != 1 || got[0].Action != ActionRequeueFresh {
		t.Fatalf("Classify = %+v, want recycled PID requeue-fresh", got)
	}
}

func TestClassifyNilRun(t *testing.T) {
	if got := Classify(nil, Probe{}); got != nil {
		t.Fatalf("Classify(nil) = %v, want nil", got)
	}
}

// TestRunDead is the run-level liveness table (koryph-oixo): a status=running
// run whose engine is dead is a phantom; every other combination is not.
func TestRunDead(t *testing.T) {
	cases := []struct {
		name        string
		run         *Run
		engineAlive bool
		want        bool
	}{
		{"running + engine dead → phantom", &Run{Status: RunRunning}, false, true},
		{"running + engine alive → live", &Run{Status: RunRunning}, true, false},
		{"done + engine dead → terminal, not phantom", &Run{Status: RunDone}, false, false},
		{"drained + engine dead → terminal", &Run{Status: RunDrained}, false, false},
		{"aborted + engine dead → terminal", &Run{Status: RunAborted}, false, false},
		// Intentionally-parked states are NOT phantoms even with no live engine:
		// they await resume, they did not crash.
		{"paused-quota + engine dead → parked, not phantom", &Run{Status: RunPausedQuota}, false, false},
		{"hard-stop-quota + engine dead → parked", &Run{Status: RunHardStopQuota}, false, false},
		{"nil run → false", nil, false, false},
	}
	for _, c := range cases {
		if got := RunDead(c.run, c.engineAlive); got != c.want {
			t.Errorf("%s: RunDead = %v, want %v", c.name, got, c.want)
		}
	}
}
