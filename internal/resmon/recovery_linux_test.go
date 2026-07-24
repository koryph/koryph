// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build linux

package resmon

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStopChildlessSignalsAuthenticatedLeader(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = unix.Kill(-pid, unix.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	identity := snapshotIdentity(t, pid)
	checkpointed := false
	signalled, err := StopChildless(t.Context(), pid, identity, func() error {
		checkpointed = true
		return nil
	})
	if err != nil {
		t.Fatalf("StopChildless: %v", err)
	}
	if !signalled || !checkpointed {
		t.Fatalf("StopChildless = signalled %v checkpointed %v, want true/true", signalled, checkpointed)
	}
}

func TestStopChildlessVetoesLiveChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = unix.Kill(-pid, unix.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	identity := snapshotIdentity(t, pid)
	waitForPeer(t, pid)
	checkpointed := false
	signalled, err := StopChildless(t.Context(), pid, identity, func() error {
		checkpointed = true
		return nil
	})
	if err != nil {
		t.Fatalf("StopChildless: %v", err)
	}
	if signalled || checkpointed {
		t.Fatalf("StopChildless = signalled %v checkpointed %v, want false/false", signalled, checkpointed)
	}
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatalf("leader was not resumed after veto: %v", err)
	}
}

func snapshotIdentity(t *testing.T, pid int) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		table, err := Snapshot(context.Background())
		if err == nil {
			if identity, ok := table.ProcessIdentity(pid); ok {
				return identity
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d never appeared in process snapshot", pid)
	return ""
}

func waitForPeer(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		table, err := Snapshot(context.Background())
		if err == nil {
			if has, found := table.HasCohortPeer(pid); found && has {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d never acquired a cohort peer", pid)
}
