// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/paths"
)

// The koryph scoped signing agent is a dedicated ssh-agent (socket at
// paths.SigningAgentSock) that holds ONLY the commit-signing key. Dispatched
// agents are pointed at it via SSH_AUTH_SOCK so they can sign commits without
// ever reaching the operator's ambient agent and its other (personal/prod)
// keys. `koryph signing enable` populates it (the sole vault entry point); the
// engine only VERIFIES it (never touches the vault).

// EnsureScopedAgent makes the koryph scoped signing agent exist and hold the
// signing key, loading it via the same vault path as EnsureAgent but targeting
// the scoped socket. It fails closed if the provider did not place the key in
// the scoped socket. mode != ssh is a no-op.
func EnsureScopedAgent(ctx context.Context, v *VaultConfig, cfg *Config) error {
	if cfg.EffectiveMode() != ModeSSH {
		return nil
	}
	if err := paths.EnsureSigningDir(); err != nil {
		return fmt.Errorf("signing: secure scoped agent dir: %w", err)
	}
	sock := paths.SigningAgentSock()
	if !scopedSockLive(ctx, sock) {
		pid, err := startScopedAgent(ctx, sock)
		if err != nil {
			return err
		}
		if pid > 0 {
			_ = os.WriteFile(scopedPIDPath(), []byte(strconv.Itoa(pid)), 0o600)
		}
	}
	if err := loadKey(ctx, v, cfg, sockEnv(sock)); err != nil {
		return fmt.Errorf("signing: loading key into koryph scoped agent: %w", err)
	}
	// Fail closed: the provider MUST have honored the scoped SSH_AUTH_SOCK.
	// If not (e.g. a vault CLI that writes only to the ambient agent), the
	// scoped socket stays empty and dispatched agents could not sign — surface
	// it here, loudly, rather than at dispatch time.
	if cfg.PublicKey != "" && !agentHasKeyOn(ctx, sock, cfg.PublicKey) {
		return fmt.Errorf(
			"signing: koryph scoped agent %s did not receive the signing key after load — "+
				"the %q provider may ignore SSH_AUTH_SOCK; see docs/user-guide/signing.md",
			sock, cfg.Provider)
	}
	return nil
}

// ScopedAgentReady reports whether the koryph scoped signing agent holds pubKey.
// Used by the engine's run-time signing preflight (never touches the vault).
func ScopedAgentReady(ctx context.Context, pubKey string) bool {
	if err := paths.EnsureSigningDir(); err != nil {
		return false
	}
	return agentHasKeyOn(ctx, paths.SigningAgentSock(), pubKey)
}

// scopedPIDPath is where EnsureScopedAgent records the scoped agent's pid so it
// can be terminated later (StopScopedAgent, tests).
func scopedPIDPath() string { return filepath.Join(paths.SigningDir(), "agent.pid") }

// StopScopedAgent terminates the koryph scoped signing agent recorded by
// EnsureScopedAgent, if any. Best-effort: a missing pidfile or dead process is
// not an error.
func StopScopedAgent() error {
	if err := paths.EnsureSigningDir(); err != nil {
		return fmt.Errorf("signing: secure scoped agent dir: %w", err)
	}
	data, err := os.ReadFile(scopedPIDPath())
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err == nil && pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	_ = os.Remove(scopedPIDPath())
	_ = os.Remove(paths.SigningAgentSock())
	return nil
}

// sockEnv builds the environment for an ssh-agent/ssh-add invocation that must
// target a specific socket: the parent env with SSH_AUTH_SOCK replaced, so the
// binaries still resolve on PATH and can reach $HOME, but the ambient agent is
// not touched.
func sockEnv(sock string) []string {
	return append(execx.BaseEnv("SSH_AUTH_SOCK"), "SSH_AUTH_SOCK="+sock)
}

// scopedSockLive reports whether an ssh-agent is answering on sock.
// `ssh-add -l` exits 0 (has keys) or 1 (agent up, no keys) when connected, and
// 2 when it cannot reach an agent.
func scopedSockLive(ctx context.Context, sock string) bool {
	res, err := execx.Run(ctx, execx.Cmd{
		Name: "ssh-add", Args: []string{"-l"}, Env: sockEnv(sock), Timeout: agentTimeout,
	})
	if err != nil {
		return false
	}
	return res.ExitCode == 0 || res.ExitCode == 1
}

// scopedPIDRe extracts the daemon pid from `ssh-agent -a` stdout.
var scopedPIDRe = regexp.MustCompile(`SSH_AGENT_PID=([0-9]+)`)

// startScopedAgent starts a daemonized ssh-agent bound to sock (0700 dir),
// clearing a stale socket file first. It returns the agent pid (0 if it could
// not be parsed) so callers/tests can terminate it.
func startScopedAgent(ctx context.Context, sock string) (int, error) {
	if err := paths.EnsureSocketPathDir(filepath.Dir(sock)); err != nil {
		return 0, fmt.Errorf("signing: scoped agent dir: %w", err)
	}
	if _, err := os.Stat(sock); err == nil {
		_ = os.Remove(sock) // a stale socket blocks `ssh-agent -a`
	}
	res, err := execx.Run(ctx, execx.Cmd{
		Name: "ssh-agent", Args: []string{"-a", sock}, Timeout: agentTimeout,
	})
	if err != nil {
		return 0, fmt.Errorf("signing: start scoped ssh-agent: %w", err)
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf("signing: ssh-agent -a exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	pid := 0
	if m := scopedPIDRe.FindStringSubmatch(res.Stdout); m != nil {
		pid, _ = strconv.Atoi(m[1])
	}
	return pid, nil
}

// agentHasKeyOn reports whether the agent at sock holds pubKey (matched on
// "type blob"; comment ignored).
func agentHasKeyOn(ctx context.Context, sock, pubKey string) bool {
	want := keyBlob(pubKey)
	if want == "" {
		return false
	}
	for _, k := range agentKeysOn(ctx, sock) {
		if k == want {
			return true
		}
	}
	return false
}

// agentKeysOn lists the public keys held by the agent at sock, normalized to
// "type blob". An unreachable or empty agent yields an empty list.
func agentKeysOn(ctx context.Context, sock string) []string {
	res, err := execx.Run(ctx, execx.Cmd{
		Name: "ssh-add", Args: []string{"-L"}, Env: sockEnv(sock), Timeout: agentTimeout,
	})
	if err != nil || res.ExitCode != 0 {
		return nil
	}
	var keys []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if b := keyBlob(line); b != "" {
			keys = append(keys, b)
		}
	}
	return keys
}
