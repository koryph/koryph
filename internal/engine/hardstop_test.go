// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
)

// TestInterruptActiveSlotsSignalsProcessGroup is the koryph audit finding
// #31 fix: a hard --budget stop must reach the WHOLE agent process group
// (agents are launched Setsid — dispatch.StopGraceful's contract), not just
// the leader pid, or the leader's still-alive subprocess children are
// orphaned rather than stopped and keep running (and spending) past the cap.
// Before the fix, interruptActiveSlots called syscall.Kill(pid, SIGTERM)
// directly on the leader pid alone.
func TestInterruptActiveSlotsSignalsProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.run.Slots = map[string]*ledger.Slot{
		"running":  {PhaseID: "running", Status: ledger.SlotRunning, PID: pid},
		"nopid":    {PhaseID: "nopid", Status: ledger.SlotDispatching, PID: 0},
		"terminal": {PhaseID: "terminal", Status: ledger.SlotMerged, PID: 999999},
	}

	r.interruptActiveSlots()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("Wait: %v (want SIGTERM exit)", err)
		}
		ws, ok := ee.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
			t.Errorf("running slot's process not terminated by SIGTERM: %v", ee)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("running slot's process did not exit after interruptActiveSlots (SIGTERM)")
	}
}
