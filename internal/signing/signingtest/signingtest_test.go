// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signingtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestSpawnAgentUsesConfiguredSocket(t *testing.T) {
	RequireTools(t, "ssh-agent")
	socket := filepath.Join(shortSocketDir(t), "s0")
	t.Setenv(testAgentSocketsEnv, socket)
	SpawnAgent(t)
	if got := os.Getenv("SSH_AUTH_SOCK"); got != socket {
		t.Errorf("SSH_AUTH_SOCK = %q, want configured test socket %q", got, socket)
	}
}

func TestSpawnAgentUsesNextFreeConfiguredSocket(t *testing.T) {
	RequireTools(t, "ssh-agent")
	dir := shortSocketDir(t)
	busy, free := filepath.Join(dir, "s0"), filepath.Join(dir, "s1")
	out, err := exec.Command("ssh-agent", "-a", busy, "-s").Output()
	if err != nil {
		t.Fatalf("start busy agent: %v", err)
	}
	pidM := pidRe.FindStringSubmatch(string(out))
	if pidM == nil {
		t.Fatalf("cannot parse busy agent output: %q", out)
	}
	pid, err := strconv.Atoi(pidM[1])
	if err != nil {
		t.Fatalf("parse busy agent pid %q: %v", pidM[1], err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
	t.Setenv(testAgentSocketsEnv, strings.Join([]string{busy, free}, string(filepath.ListSeparator)))
	SpawnAgent(t)
	if got := os.Getenv("SSH_AUTH_SOCK"); got != free {
		t.Errorf("SSH_AUTH_SOCK = %q, want next free configured socket %q", got, free)
	}
}

// shortSocketDir avoids macOS's short Unix socket-path limit when the test
// process inherits a deeply nested phase-local TMPDIR.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "koryph-socket-")
	if err != nil {
		t.Fatalf("create short socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
