// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"testing"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// TestBillingForAPIKeyAccountBillsFromWaveOne is the koryph-i3b.4 acceptance
// test: a first-class api-key account (registry.AuthModeAPIKey) must bill
// api-key from wave 1, LEVEL-INDEPENDENT — unlike the legacy break-glass
// fallback, which only fires at governor stop. billingFor is exercised
// directly (no full Run()), mirroring TestGuardModeAdvisoryWhenRuntimeHasNoUsageSource's
// narrow-unit-test style.
func TestBillingForAPIKeyAccountBillsFromWaveOne(t *testing.T) {
	r := &runner{
		rec:        &registry.Record{},
		authMode:   registry.AuthModeAPIKey,
		credential: "sk-resolved-test-key",
	}
	for _, level := range []quota.Level{quota.LevelOK, quota.LevelWarn, quota.LevelThrottle, quota.LevelDrain, quota.LevelStop} {
		mode, key := r.billingFor(level)
		if mode != account.BillingAPIKey {
			t.Errorf("level %v: billingFor mode = %q, want %q (api-key account bills from wave 1)", level, mode, account.BillingAPIKey)
		}
		if key != "sk-resolved-test-key" {
			t.Errorf("level %v: billingFor key = %q, want the resolved credential", level, key)
		}
	}
}

// TestBillingForSubscriptionLegacyFallbackUnchanged pins the pre-koryph-i3b
// behavior for a subscription account (the default, empty authMode): billing
// is subscription EXCEPT at governor stop, and only then when the operator
// explicitly opted into the triple-AND fallback (AllowAPISpend + registry
// APIFallback=="explicit" + a resolvable APIKeyEnvVar) — the exact shape
// billingFor had before this bead, now reachable only via this path.
func TestBillingForSubscriptionLegacyFallbackUnchanged(t *testing.T) {
	t.Run("not at stop stays subscription even with fallback configured", func(t *testing.T) {
		r := &runner{
			rec:  &registry.Record{APIFallback: "explicit", APIKeyEnvVar: "KORYPH_TEST_FALLBACK_KEY"},
			opts: Options{AllowAPISpend: true},
		}
		t.Setenv("KORYPH_TEST_FALLBACK_KEY", "sk-fallback")
		mode, key := r.billingFor(quota.LevelWarn)
		if mode != account.BillingSubscription || key != "" {
			t.Fatalf("billingFor(LevelWarn) = (%q, %q), want (%q, \"\")", mode, key, account.BillingSubscription)
		}
	})

	t.Run("stop with full opt-in switches to api-key", func(t *testing.T) {
		r := &runner{
			rec:  &registry.Record{APIFallback: "explicit", APIKeyEnvVar: "KORYPH_TEST_FALLBACK_KEY"},
			opts: Options{AllowAPISpend: true},
		}
		t.Setenv("KORYPH_TEST_FALLBACK_KEY", "sk-fallback")
		mode, key := r.billingFor(quota.LevelStop)
		if mode != account.BillingAPIKey || key != "sk-fallback" {
			t.Fatalf("billingFor(LevelStop) = (%q, %q), want (%q, %q)", mode, key, account.BillingAPIKey, "sk-fallback")
		}
	})

	t.Run("stop without AllowAPISpend stays subscription", func(t *testing.T) {
		r := &runner{
			rec: &registry.Record{APIFallback: "explicit", APIKeyEnvVar: "KORYPH_TEST_FALLBACK_KEY"},
		}
		t.Setenv("KORYPH_TEST_FALLBACK_KEY", "sk-fallback")
		mode, key := r.billingFor(quota.LevelStop)
		if mode != account.BillingSubscription || key != "" {
			t.Fatalf("billingFor(LevelStop, no AllowAPISpend) = (%q, %q), want (%q, \"\")", mode, key, account.BillingSubscription)
		}
	})

	t.Run("stop without registry opt-in stays subscription", func(t *testing.T) {
		r := &runner{
			rec:  &registry.Record{}, // APIFallback != "explicit"
			opts: Options{AllowAPISpend: true},
		}
		mode, key := r.billingFor(quota.LevelStop)
		if mode != account.BillingSubscription || key != "" {
			t.Fatalf("billingFor(LevelStop, no registry opt-in) = (%q, %q), want (%q, \"\")", mode, key, account.BillingSubscription)
		}
	})
}

// TestBillingForAPIKeyAccountTakesPriorityOverLegacyFallback proves the two
// paths coexist without cross-talk: an api-key account with a resolved
// credential wins even when the legacy fallback's own conditions also happen
// to be satisfied (belt-and-braces registry misconfiguration) — first-class
// mode is checked first and does not fall through.
func TestBillingForAPIKeyAccountTakesPriorityOverLegacyFallback(t *testing.T) {
	r := &runner{
		rec: &registry.Record{
			APIFallback:  "explicit",
			APIKeyEnvVar: "KORYPH_TEST_FALLBACK_KEY",
		},
		opts:       Options{AllowAPISpend: true},
		authMode:   registry.AuthModeAPIKey,
		credential: "sk-first-class",
	}
	t.Setenv("KORYPH_TEST_FALLBACK_KEY", "sk-fallback-should-not-win")
	mode, key := r.billingFor(quota.LevelStop)
	if mode != account.BillingAPIKey || key != "sk-first-class" {
		t.Fatalf("billingFor = (%q, %q), want the first-class credential (%q, %q), not the legacy fallback",
			mode, key, account.BillingAPIKey, "sk-first-class")
	}
}

// TestGovernorGateAPIKeyAccountStaysBilledUnderAdvisoryGuard is the
// koryph-i3b.4 coexistence regression for governorGate itself: an
// uncalibrated (hence advisory) governor must NOT reset a first-class
// api-key account back to subscription billing the way it resets the legacy
// fallback — the advisory branch's "never switch billing" comment predates
// this bead and described only the legacy path, which never had a standing
// non-subscription billing mode to preserve.
func TestGovernorGateAPIKeyAccountStaysBilledUnderAdvisoryGuard(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	r := &runner{
		rec:        &registry.Record{AccountProfile: "authmode-test-acct"},
		authMode:   registry.AuthModeAPIKey,
		credential: "sk-advisory-test",
		rt:         runtimetest.Stub{Caps: runtime.Capabilities{UsageSource: true}},
		store:      ledger.NewStore(t.TempDir()),
	}
	g := r.governorGate(context.Background())
	if r.billing != account.BillingAPIKey || r.apiKey != "sk-advisory-test" {
		t.Fatalf("governorGate left billing = (%q, %q), want (%q, %q) — advisory guard must not reset a first-class api-key account",
			r.billing, r.apiKey, account.BillingAPIKey, "sk-advisory-test")
	}
	if !g.allowDispatch {
		t.Fatalf("govGate.allowDispatch = false, want true (uncalibrated advisory account should not block)")
	}
}
