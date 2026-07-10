// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package execx

import (
	"strings"
	"testing"
)

// TestGateEnvExcludesSecrets is the security invariant for the green-gate
// environment (koryph security audit): a gate subprocess compiles and runs
// agent-authored code, so it must never inherit the orchestrator's ambient
// credentials, while still carrying the build/test toolchain it needs.
func TestGateEnvExcludesSecrets(t *testing.T) {
	// Orchestrator secrets that must be scrubbed.
	secrets := []string{
		"GH_TOKEN", "GITHUB_TOKEN", "COSIGN_PASSWORD", "KORYPH_PASSPHRASE",
		"ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
		"NPM_TOKEN", "OP_SERVICE_ACCOUNT_TOKEN",
	}
	for _, s := range secrets {
		t.Setenv(s, "sensitive-value")
	}
	// Toolchain vars that must survive.
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/koryph")
	t.Setenv("GOCACHE", "/tmp/gocache")
	t.Setenv("LC_ALL", "en_US.UTF-8")

	env := GateEnv()
	names := map[string]bool{}
	for _, kv := range env {
		if name, _, ok := strings.Cut(kv, "="); ok {
			names[name] = true
		}
	}

	for _, s := range secrets {
		if names[s] {
			t.Errorf("gate env leaked orchestrator secret %s", s)
		}
	}
	for _, keep := range []string{"PATH", "HOME", "GOCACHE", "LC_ALL"} {
		if !names[keep] {
			t.Errorf("gate env dropped required toolchain var %s", keep)
		}
	}
}
