// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
)

// agentKeyLifetimeSec bounds fallback-loaded keys in the system agent so a
// vault-fetched key does not outlive the workday.
const agentKeyLifetimeSec = "3600"

// agentTimeout bounds one agent-facing invocation.
const agentTimeout = 60 * time.Second

// EnsureAgent makes the signing key available to the system SSH agent
// (SSH_AUTH_SOCK). Mode gitsign needs no agent and returns nil.
//
//   - Providers with an agent_load template (protonpass default:
//     `pass-cli ssh-agent load`) run it — the private key moves from the
//     vault straight into the agent and never surfaces here. The
//     alternative for Proton Pass is `pass-cli ssh-agent start` (Proton
//     Pass AS the agent); point SSH_AUTH_SOCK at its --socket-path and
//     EnsureAgent's load becomes a no-op template.
//   - file/command providers (no agent_load) fetch the key and pipe it to
//     `ssh-add -t 3600 -` on stdin: memory only, never on disk.
func EnsureAgent(ctx context.Context, v *VaultConfig, cfg *Config) error {
	if cfg.EffectiveMode() != ModeSSH {
		return nil
	}
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		return fmt.Errorf("signing: no SSH agent (SSH_AUTH_SOCK is unset) — start one with `eval \"$(ssh-agent -s)\"` " +
			"or run Proton Pass as your agent via `pass-cli ssh-agent start --socket-path ...`")
	}
	return loadKey(ctx, v, cfg, nil)
}

// loadKey moves the signing key into an SSH agent. env is the environment for
// the load commands: nil inherits the parent (the ambient SSH_AUTH_SOCK);
// non-nil targets a specific socket (the koryph scoped agent). The private key
// never touches disk — the agent_load template moves it vault→agent directly,
// and the fetch fallback pipes it to `ssh-add -` over stdin only.
func loadKey(ctx context.Context, v *VaultConfig, cfg *Config, env []string) error {
	pt := v.Providers[cfg.Provider]
	if len(pt.AgentLoad) > 0 {
		argv := ExpandArgv(pt.AgentLoad, cfg.KeyRef)
		res, err := execx.Run(ctx, execx.Cmd{Name: argv[0], Args: argv[1:], Env: env, Timeout: agentTimeout})
		if err != nil {
			return fmt.Errorf("signing: agent load via %s: %w", argv[0], err)
		}
		if res.ExitCode != 0 {
			hint := ""
			if pt.LoginHint != "" {
				hint = fmt.Sprintf(" (not logged in? run `%s` first)", pt.LoginHint)
			}
			return fmt.Errorf("signing: %s exited %d%s: %s",
				argv[0], res.ExitCode, hint, strings.TrimSpace(res.Stderr))
		}
		return nil
	}

	key, err := v.Fetch(ctx, cfg.Provider, cfg.KeyRef)
	if err != nil {
		return err
	}
	// ssh-add requires the PEM to end with a newline; the key stays in
	// memory and is handed to the agent over stdin only.
	material := strings.TrimRight(string(key), "\n") + "\n"
	res, err := execx.Run(ctx, execx.Cmd{
		Name: "ssh-add", Args: []string{"-t", agentKeyLifetimeSec, "-"},
		Env: env, Stdin: material, Timeout: agentTimeout,
	})
	if err != nil {
		return fmt.Errorf("signing: ssh-add: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("signing: ssh-add exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// AgentKeys lists the public keys currently held by the system SSH agent,
// normalized to "type blob" (comment stripped). An unreachable or empty
// agent yields an empty list.
func AgentKeys(ctx context.Context) []string {
	res, err := execx.Run(ctx, execx.Cmd{Name: "ssh-add", Args: []string{"-L"}, Timeout: agentTimeout})
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

// AgentReady reports whether the system SSH agent holds pubKey (matched on
// "type blob"; the comment is ignored).
func AgentReady(ctx context.Context, pubKey string) bool {
	want := keyBlob(pubKey)
	if want == "" {
		return false
	}
	for _, k := range AgentKeys(ctx) {
		if k == want {
			return true
		}
	}
	return false
}

// keyBlob normalizes an SSH public key line to its first two fields
// ("ssh-ed25519 AAAA..."), or "" when the line is not a key.
func keyBlob(line string) string {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 2 || !strings.HasPrefix(f[0], "ssh-") && !strings.HasPrefix(f[0], "ecdsa-") && !strings.HasPrefix(f[0], "sk-") {
		return ""
	}
	return f[0] + " " + f[1]
}
