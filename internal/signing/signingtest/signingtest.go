// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package signingtest provides hermetic SSH fixtures for signing tests: a
// real throwaway ssh-agent, a generated ed25519 keypair, and git config
// isolation. No network, no vault, no user state.
package signingtest

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// RequireTools skips the test unless every named binary is on PATH.
func RequireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("required tool %q not on PATH", n)
		}
	}
}

var (
	sockRe = regexp.MustCompile(`SSH_AUTH_SOCK=([^;]+);`)
	pidRe  = regexp.MustCompile(`SSH_AGENT_PID=([0-9]+);`)
)

// SpawnAgent starts a REAL ssh-agent for the test, exports
// SSH_AUTH_SOCK/SSH_AGENT_PID via t.Setenv, and kills the agent in cleanup.
func SpawnAgent(t *testing.T) {
	t.Helper()
	RequireTools(t, "ssh-agent")
	out, err := exec.Command("ssh-agent", "-s").Output()
	if err != nil {
		t.Fatalf("ssh-agent -s: %v", err)
	}
	sockM := sockRe.FindStringSubmatch(string(out))
	pidM := pidRe.FindStringSubmatch(string(out))
	if sockM == nil || pidM == nil {
		t.Fatalf("cannot parse ssh-agent output: %q", out)
	}
	pid, err := strconv.Atoi(pidM[1])
	if err != nil {
		t.Fatalf("ssh-agent pid %q: %v", pidM[1], err)
	}
	t.Setenv("SSH_AUTH_SOCK", strings.TrimSpace(sockM[1]))
	t.Setenv("SSH_AGENT_PID", pidM[1])
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
}

// GenKey generates an unencrypted ed25519 keypair in a temp dir and returns
// the private key path and the public key line ("ssh-ed25519 AAAA... comment").
func GenKey(t *testing.T) (privPath, pubKey string) {
	t.Helper()
	RequireTools(t, "ssh-keygen")
	dir := t.TempDir()
	privPath = filepath.Join(dir, "id_ed25519")
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "koryph-test", "-f", privPath).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := exec.Command("cat", privPath+".pub").Output()
	if err != nil {
		t.Fatalf("read pubkey: %v", err)
	}
	return privPath, strings.TrimSpace(string(pub))
}

// AddKey loads a private key file into the test agent via ssh-add.
func AddKey(t *testing.T, privPath string) {
	t.Helper()
	RequireTools(t, "ssh-add")
	if out, err := exec.Command("ssh-add", "-q", privPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-add %s: %v\n%s", privPath, err, out)
	}
}

// IsolateGit points git's global/system config at nonexistent files so host
// settings (signing, hooks, prompts) cannot leak into the test.
func IsolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "no-system"))
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}
