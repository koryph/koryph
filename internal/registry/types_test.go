// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package registry

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestAccountForFallsBackToFlatFields proves RuntimeAccounts is fully
// additive (koryph-v8u.5): a record with no runtime_accounts block at all —
// every record written before this bead — resolves AccountFor for any name
// (not just "claude") to the flat AccountProfile/ClaudeConfigDir/
// ExpectedIdentity/EnvPassthrough fields, so existing projects are
// unaffected.
func TestAccountForFallsBackToFlatFields(t *testing.T) {
	rec := &Record{
		AccountProfile:   "work",
		ClaudeConfigDir:  "/home/u/.claude-work",
		ExpectedIdentity: "agent@example.com",
		APIKeyEnvVar:     "MY_API_KEY",
		EnvPassthrough:   []string{"MY_PROJECT_VAR"},
	}
	want := RuntimeAccount{
		ConfigDir:        "/home/u/.claude-work",
		ExpectedIdentity: "agent@example.com",
		APIKeyEnvVar:     "MY_API_KEY",
		EnvPassthrough:   []string{"MY_PROJECT_VAR"},
	}
	if got := rec.AccountFor("claude"); !reflect.DeepEqual(got, want) {
		t.Errorf("AccountFor(claude) = %+v, want %+v (flat-field fallback)", got, want)
	}
	// The fallback is unconditional on name — a lookup for a runtime this
	// record has never heard of also falls back rather than returning a zero
	// value, since "no runtime_accounts block" carries no information about
	// which runtime the flat fields describe.
	if got := rec.AccountFor("codex"); !reflect.DeepEqual(got, want) {
		t.Errorf("AccountFor(codex) = %+v, want %+v (flat-field fallback)", got, want)
	}
}

// TestAccountForExplicitEntryWins proves an explicit runtime_accounts[name]
// entry overrides the flat-field fallback, and that a DIFFERENT runtime name
// on the same record still falls back independently.
func TestAccountForExplicitEntryWins(t *testing.T) {
	rec := &Record{
		AccountProfile:   "work",
		ClaudeConfigDir:  "/home/u/.claude-work",
		ExpectedIdentity: "agent@example.com",
		RuntimeAccounts: map[string]RuntimeAccount{
			"codex": {
				ConfigDir:        "/home/u/.codex-work",
				ExpectedIdentity: "agent@codex.example.com",
				APIKeyEnvVar:     "CODEX_API_KEY",
			},
		},
	}

	codex := rec.AccountFor("codex")
	want := RuntimeAccount{
		ConfigDir:        "/home/u/.codex-work",
		ExpectedIdentity: "agent@codex.example.com",
		APIKeyEnvVar:     "CODEX_API_KEY",
	}
	if !reflect.DeepEqual(codex, want) {
		t.Errorf("AccountFor(codex) = %+v, want explicit entry %+v", codex, want)
	}

	claude := rec.AccountFor("claude")
	wantClaude := RuntimeAccount{ConfigDir: "/home/u/.claude-work", ExpectedIdentity: "agent@example.com"}
	if !reflect.DeepEqual(claude, wantClaude) {
		t.Errorf("AccountFor(claude) = %+v, want flat-field fallback %+v (must not see codex's entry)", claude, wantClaude)
	}
}

// TestRuntimeAccountsJSONRoundTrip confirms the new field's tag and omitempty
// behavior: absent on a record with no runtime_accounts (back-compat with
// every pre-koryph-v8u.5 registry.d/*.json on disk), present and keyed by
// name when set.
func TestRuntimeAccountsJSONRoundTrip(t *testing.T) {
	rec := Record{ProjectID: "p"}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"runtime_accounts"`) {
		t.Errorf("empty RuntimeAccounts must omit the key entirely: %s", data)
	}

	rec.RuntimeAccounts = map[string]RuntimeAccount{"claude": {ConfigDir: "/x"}}
	data, err = json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Record
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.RuntimeAccounts, rec.RuntimeAccounts) {
		t.Errorf("round-trip RuntimeAccounts = %+v, want %+v", got.RuntimeAccounts, rec.RuntimeAccounts)
	}
}

// TestEffectiveHoldoutDefaultsWhenUnset is the koryph-3l1.3 acceptance test
// for AgentProxy.Holdout's tri-state resolution: nil (unset) resolves to
// DefaultHoldout; an explicit 0 stays 0 (never silently promoted to the
// default); nil receiver is safe.
func TestEffectiveHoldoutDefaultsWhenUnset(t *testing.T) {
	var nilProxy *AgentProxy
	if got := nilProxy.EffectiveHoldout(); got != DefaultHoldout {
		t.Errorf("nil AgentProxy.EffectiveHoldout() = %v, want DefaultHoldout %v", got, DefaultHoldout)
	}

	unset := &AgentProxy{BaseURL: "http://127.0.0.1:8091"}
	if got := unset.EffectiveHoldout(); got != DefaultHoldout {
		t.Errorf("unset Holdout.EffectiveHoldout() = %v, want DefaultHoldout %v", got, DefaultHoldout)
	}

	zero := 0.0
	explicitZero := &AgentProxy{BaseURL: "http://127.0.0.1:8091", Holdout: &zero}
	if got := explicitZero.EffectiveHoldout(); got != 0 {
		t.Errorf("explicit Holdout=0 .EffectiveHoldout() = %v, want 0 (must not be promoted to default)", got)
	}
}

// TestArmForDeterministic proves the koryph-3l1.3 core invariant: the SAME
// bead ID always resolves to the SAME arm, across repeated calls — this is
// what keeps a requeue/resume of the same bead from flipping arms mid-flight
// (see AgentProxy.Holdout's doc for why that would corrupt both the
// experiment and the prompt cache).
func TestArmForDeterministic(t *testing.T) {
	half := 0.5
	p := &AgentProxy{BaseURL: "http://127.0.0.1:8091", Pin: "v1", Holdout: &half}
	for _, id := range []string{"koryph-abc", "koryph-def.2", "tb1", "another-bead-id"} {
		wantProxyID, wantBaseURL := p.ArmFor(id)
		for i := 0; i < 5; i++ {
			gotProxyID, gotBaseURL := p.ArmFor(id)
			if gotProxyID != wantProxyID || gotBaseURL != wantBaseURL {
				t.Errorf("ArmFor(%q) call %d = (%q,%q), want stable (%q,%q)",
					id, i, gotProxyID, gotBaseURL, wantProxyID, wantBaseURL)
			}
		}
	}
}

// TestArmForEdgeFractions covers Holdout=0 (always proxied) and Holdout=1
// (always holdout) — the documented edge cases (AgentProxy.Holdout's doc).
func TestArmForEdgeFractions(t *testing.T) {
	p := &AgentProxy{BaseURL: "http://127.0.0.1:8091", Pin: "v1"}

	zero := 0.0
	p.Holdout = &zero
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("bead-%d", i)
		proxyID, baseURL := p.ArmFor(id)
		if proxyID != p.ID() || baseURL != p.BaseURL {
			t.Errorf("Holdout=0: ArmFor(%q) = (%q,%q), want always-proxied (%q,%q)",
				id, proxyID, baseURL, p.ID(), p.BaseURL)
		}
	}

	one := 1.0
	p.Holdout = &one
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("bead-%d", i)
		proxyID, baseURL := p.ArmFor(id)
		if proxyID != "" || baseURL != "" {
			t.Errorf("Holdout=1: ArmFor(%q) = (%q,%q), want always-holdout (\"\",\"\")", id, proxyID, baseURL)
		}
	}
}

// TestArmForNilOrUnconfiguredIsAlwaysDirect proves a nil AgentProxy or one
// with an empty BaseURL always returns the direct ("", "") pair regardless of
// Holdout — matching ID()'s and ProxyBaseURL()'s existing nil-safety, and
// confirming ArmFor never accidentally "holds out" a project that has no
// proxy configured at all (there is no experiment to run).
func TestArmForNilOrUnconfiguredIsAlwaysDirect(t *testing.T) {
	var nilProxy *AgentProxy
	if proxyID, baseURL := nilProxy.ArmFor("any-bead"); proxyID != "" || baseURL != "" {
		t.Errorf("nil AgentProxy.ArmFor() = (%q,%q), want (\"\",\"\")", proxyID, baseURL)
	}

	empty := &AgentProxy{}
	if proxyID, baseURL := empty.ArmFor("any-bead"); proxyID != "" || baseURL != "" {
		t.Errorf("empty-BaseURL AgentProxy.ArmFor() = (%q,%q), want (\"\",\"\")", proxyID, baseURL)
	}
}

// TestArmForRoughlyHonorsFraction is a statistical sanity check (not a
// determinism proof — TestArmForDeterministic covers that): across a large
// bead population, the observed holdout share should land in the
// neighborhood of the configured fraction, proving stableUnitInterval's
// spread is not pathologically skewed.
func TestArmForRoughlyHonorsFraction(t *testing.T) {
	tenth := 0.1
	p := &AgentProxy{BaseURL: "http://127.0.0.1:8091", Holdout: &tenth}
	const n = 10000
	holdout := 0
	for i := 0; i < n; i++ {
		if proxyID, _ := p.ArmFor(fmt.Sprintf("bead-%d", i)); proxyID == "" {
			holdout++
		}
	}
	frac := float64(holdout) / n
	if frac < 0.07 || frac > 0.13 {
		t.Errorf("observed holdout fraction = %.3f over %d beads, want roughly 0.10", frac, n)
	}
}
