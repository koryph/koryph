// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"os"
	"syscall"
	"testing"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/signing/signingtest"
)

// shortKoryphHome sets KORYPH_HOME to a short temp path — Unix domain sockets
// cap at ~104 chars, and the default $TMPDIR on macOS is too long for
// <home>/signing/agent.sock.
func shortKoryphHome(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kry")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("KORYPH_HOME", dir)
}

// TestScopedAgentLifecycle exercises the koryph scoped signing agent end to end
// with a real (throwaway) ssh-agent and a generated key — no vault, no operator
// state. It proves: startScopedAgent brings the socket live at
// paths.SigningAgentSock(); a key loaded via the scoped SSH_AUTH_SOCK is
// readable there; ScopedAgentReady/agentHasKeyOn report it; and the scoped
// socket is isolated from the operator's ambient agent.
func TestScopedAgentLifecycle(t *testing.T) {
	signingtest.RequireTools(t, "ssh-agent", "ssh-add", "ssh-keygen")
	ctx := context.Background()

	// A separate ambient agent stands in for the operator's SSH_AUTH_SOCK; the
	// scoped socket must NOT be it.
	signingtest.SpawnAgent(t)

	// Point KORYPH_HOME (and thus paths.SigningAgentSock) at a short temp dir.
	shortKoryphHome(t)
	sock := paths.SigningAgentSock()

	if scopedSockLive(ctx, sock) {
		t.Fatal("scoped socket reported live before the agent was started")
	}
	pid, err := startScopedAgent(ctx, sock)
	if err != nil {
		t.Fatalf("startScopedAgent: %v", err)
	}
	if pid > 0 {
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
	}
	if !scopedSockLive(ctx, sock) {
		t.Fatal("scoped socket not live after startScopedAgent")
	}

	// Generate a key and load it into the SCOPED socket only (simulating the
	// provider load, which loadKey performs with sockEnv).
	privPath, pubKey := signingtest.GenKey(t)
	res, err := execx.Run(ctx, execx.Cmd{
		Name: "ssh-add", Args: []string{"-t", agentKeyLifetimeSec, privPath},
		Env: sockEnv(sock), Timeout: agentTimeout,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("ssh-add into scoped socket: err=%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr)
	}

	if !agentHasKeyOn(ctx, sock, pubKey) {
		t.Error("scoped socket does not report the loaded key")
	}
	if !ScopedAgentReady(ctx, pubKey) {
		t.Error("ScopedAgentReady = false after loading the key into the scoped socket")
	}

	// Isolation: the operator's ambient agent must NOT hold the scoped key.
	if AgentReady(ctx, pubKey) {
		t.Error("operator ambient agent holds the scoped signing key; scoped socket is not isolated")
	}
	// A different key must not be reported ready.
	_, otherPub := signingtest.GenKey(t)
	if ScopedAgentReady(ctx, otherPub) {
		t.Error("ScopedAgentReady = true for a key that was never loaded")
	}
}

// TestScopedSockEnvReplacesAmbient guards that sockEnv points tools at the
// scoped socket, never the operator's ambient one.
func TestScopedSockEnvReplacesAmbient(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/operator-ambient.sock")
	env := sockEnv("/koryph/scoped.sock")
	var count int
	var val string
	for _, kv := range env {
		if len(kv) > len("SSH_AUTH_SOCK=") && kv[:len("SSH_AUTH_SOCK=")] == "SSH_AUTH_SOCK=" {
			count++
			val = kv[len("SSH_AUTH_SOCK="):]
		}
	}
	if count != 1 || val != "/koryph/scoped.sock" {
		t.Errorf("SSH_AUTH_SOCK = %q x%d, want the scoped socket exactly once", val, count)
	}
}
