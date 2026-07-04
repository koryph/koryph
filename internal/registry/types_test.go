// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package registry

import (
	"encoding/json"
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
