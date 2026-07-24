// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build linux

package resmon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const processStopTimeout = time.Second

// StopChildless safely interrupts an authenticated process only when its live
// cohort is empty. A pidfd pins the kernel process object across every check.
// SIGSTOP then prevents the leader from spawning a gate/test child between the
// fresh cohort snapshot and SIGTERM. SIGTERM and SIGCONT are both sent through
// that same pidfd, so neither can target a recycled numeric PID.
//
// beforeSignal persists the caller's recovery intent while the authenticated
// leader is frozen. It must succeed before SIGTERM is sent. signalled is false
// when a peer vetoes recovery or the process cannot be authenticated.
func StopChildless(ctx context.Context, pid int, wantIdentity string, beforeSignal func() error) (signalled bool, err error) {
	if pid <= 0 || wantIdentity == "" {
		return false, fmt.Errorf("stop childless: invalid process identity")
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if err == unix.ENOSYS || err == unix.EINVAL {
			return false, fmt.Errorf("%w: pidfd_open: %v", ErrStableProcessHandleUnavailable, err)
		}
		return false, fmt.Errorf("stop childless: pidfd_open(%d): %w", pid, err)
	}
	defer unix.Close(pidfd)

	if err := unix.PidfdSendSignal(pidfd, unix.SIGSTOP, nil, 0); err != nil {
		return false, fmt.Errorf("stop childless: freeze pid %d: %w", pid, err)
	}
	frozen := true
	defer func() {
		if frozen {
			_ = unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0)
		}
	}()

	stopCtx, cancel := context.WithTimeout(ctx, processStopTimeout)
	defer cancel()
	if err := waitProcessStopped(stopCtx, pid, wantIdentity); err != nil {
		return false, err
	}

	table, err := Snapshot(stopCtx)
	if err != nil || table == nil {
		if err == nil {
			err = fmt.Errorf("empty process table")
		}
		return false, fmt.Errorf("stop childless: frozen cohort snapshot: %w", err)
	}
	if !table.MatchesProcess(pid, wantIdentity) {
		return false, nil
	}
	hasPeer, found := table.HasCohortPeer(pid)
	if !found || hasPeer {
		return false, nil
	}
	if beforeSignal == nil {
		return false, fmt.Errorf("stop childless: missing recovery checkpoint")
	}
	if err := beforeSignal(); err != nil {
		return false, fmt.Errorf("stop childless: checkpoint recovery: %w", err)
	}
	if err := unix.PidfdSendSignal(pidfd, unix.SIGTERM, nil, 0); err != nil {
		return false, fmt.Errorf("stop childless: SIGTERM pid %d: %w", pid, err)
	}
	if err := unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0); err != nil {
		// SIGTERM was already sent to the stable handle, so preserve signalled
		// even if an external actor killed the process before it could resume.
		// Keep frozen true so the deferred best-effort SIGCONT retries once.
		return true, fmt.Errorf("stop childless: SIGCONT pid %d: %w", pid, err)
	}
	frozen = false
	return true, nil
}

func waitProcessStopped(ctx context.Context, pid int, wantIdentity string) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, identity, err := readProcessStateIdentity(pid)
		if err != nil {
			return fmt.Errorf("stop childless: read frozen pid %d: %w", pid, err)
		}
		if identity != wantIdentity {
			return fmt.Errorf("stop childless: pid %d identity changed", pid)
		}
		if state == "T" || state == "t" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stop childless: waiting for pid %d to freeze: %w", pid, ctx.Err())
		case <-ticker.C:
		}
	}
}

func readProcessStateIdentity(pid int) (state, identity string, err error) {
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", err
	}
	rparen := strings.LastIndexByte(string(content), ')')
	if rparen < 0 || rparen+2 >= len(content) {
		return "", "", fmt.Errorf("malformed /proc stat")
	}
	rest := strings.Fields(string(content)[rparen+2:])
	if len(rest) == 0 {
		return "", "", fmt.Errorf("missing process state")
	}
	_, _, _, identity, ok := parseStatCPUWithBirth(string(content))
	if !ok || identity == "" {
		return "", "", fmt.Errorf("missing process identity")
	}
	return rest[0], identity, nil
}
