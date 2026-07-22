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

// TestAuthModeCredentialJSONRoundTrip is the koryph-i3b.1 acceptance test
// for auth_mode/credential/identity_fingerprint: absent (omitempty) on a
// record with no non-subscription config — the back-compat shape of every
// pre-koryph-i3b registry.d/*.json on disk — and round-trips exactly when
// set.
func TestAuthModeCredentialJSONRoundTrip(t *testing.T) {
	rec := Record{ProjectID: "p"}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"auth_mode"`, `"credential"`, `"identity_fingerprint"`} {
		if strings.Contains(string(data), key) {
			t.Errorf("empty auth fields must omit %s entirely: %s", key, data)
		}
	}

	rec.AuthMode = AuthModeAPIKey
	rec.Credential = &Credential{
		Source:   CredentialSourceVault,
		Provider: "protonpass",
		KeyRef:   "Anthropic API Key",
	}
	rec.IdentityFingerprint = "sha256:ab34"
	data, err = json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Record
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.AuthMode != rec.AuthMode {
		t.Errorf("AuthMode round-trip = %q, want %q", got.AuthMode, rec.AuthMode)
	}
	if !reflect.DeepEqual(got.Credential, rec.Credential) {
		t.Errorf("Credential round-trip = %+v, want %+v", got.Credential, rec.Credential)
	}
	if got.IdentityFingerprint != rec.IdentityFingerprint {
		t.Errorf("IdentityFingerprint round-trip = %q, want %q", got.IdentityFingerprint, rec.IdentityFingerprint)
	}
}

// TestEffectiveAuthModeDefaultsToSubscription proves migration for existing
// records (koryph-i3b.1 acceptance criteria): a record with no auth_mode at
// all — every registry.d/*.json written before this bead — resolves to
// AuthModeSubscription via EffectiveAuthMode, and an explicit non-default
// mode is never overridden.
func TestEffectiveAuthModeDefaultsToSubscription(t *testing.T) {
	legacy := &Record{ProjectID: "p"}
	if got := legacy.EffectiveAuthMode(); got != AuthModeSubscription {
		t.Errorf("EffectiveAuthMode() on legacy record = %q, want %q", got, AuthModeSubscription)
	}

	explicit := &Record{ProjectID: "p", AuthMode: AuthModeOAuthToken}
	if got := explicit.EffectiveAuthMode(); got != AuthModeOAuthToken {
		t.Errorf("EffectiveAuthMode() with explicit mode = %q, want %q (must not be overridden)", got, AuthModeOAuthToken)
	}
}

// TestEffectivePromptCachePolicyDefaultsToOn is the koryph-6au acceptance
// test for the re-introduced field's tri-state resolution: an unset field
// (every record written before the field existed) resolves to "on" and reports
// enabled; an explicit "off" is honored and disables; an explicit "on" stays
// enabled. Callers must read the accessor, never the raw field.
func TestEffectivePromptCachePolicyDefaultsToOn(t *testing.T) {
	legacy := &Record{ProjectID: "p"}
	if got := legacy.EffectivePromptCachePolicy(); got != PromptCacheOn {
		t.Errorf("EffectivePromptCachePolicy() on legacy record = %q, want %q", got, PromptCacheOn)
	}
	if !legacy.PromptCacheEnabled() {
		t.Errorf("PromptCacheEnabled() on legacy record = false, want true (default on)")
	}

	off := &Record{ProjectID: "p", PromptCachePolicy: PromptCacheOff}
	if off.PromptCacheEnabled() {
		t.Errorf("PromptCacheEnabled() with explicit off = true, want false")
	}
	if got := off.EffectivePromptCachePolicy(); got != PromptCacheOff {
		t.Errorf("EffectivePromptCachePolicy() with explicit off = %q, want %q", got, PromptCacheOff)
	}

	on := &Record{ProjectID: "p", PromptCachePolicy: PromptCacheOn}
	if !on.PromptCacheEnabled() {
		t.Errorf("PromptCacheEnabled() with explicit on = false, want true")
	}
}

// TestAccountForCarriesAuthFields proves AccountFor's flat-field fallback
// (koryph-v8u.5's pattern) extends to the new auth fields: a record with no
// runtime_accounts block synthesizes AuthMode/Credential/IdentityFingerprint
// from the flat Record fields, same as ConfigDir/ExpectedIdentity above.
func TestAccountForCarriesAuthFields(t *testing.T) {
	rec := &Record{
		AccountProfile:      "work",
		AuthMode:            AuthModeAPIKey,
		Credential:          &Credential{Source: CredentialSourceEnv, EnvVar: "KORYPH_ANTHROPIC_KEY"},
		IdentityFingerprint: "sha256:ab34",
	}
	got := rec.AccountFor("claude")
	if got.AuthMode != rec.AuthMode {
		t.Errorf("AccountFor.AuthMode = %q, want %q", got.AuthMode, rec.AuthMode)
	}
	if !reflect.DeepEqual(got.Credential, rec.Credential) {
		t.Errorf("AccountFor.Credential = %+v, want %+v", got.Credential, rec.Credential)
	}
	if got.IdentityFingerprint != rec.IdentityFingerprint {
		t.Errorf("AccountFor.IdentityFingerprint = %q, want %q", got.IdentityFingerprint, rec.IdentityFingerprint)
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
