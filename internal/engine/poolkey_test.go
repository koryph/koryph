// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"testing"

	"github.com/koryph/koryph/internal/registry"
)

// TestPoolKeyIsAccount proves the concurrency governor pool is keyed on the
// resolved account (koryph-1o2.1), so two accounts on one host — e.g. a larger
// subscription and a smaller work seat — get independent pools. The key reuses
// quotaName()'s precedence (QuotaProfile ?? AccountProfile), unifying the
// concurrency pool with the already-per-account quota ledger. A runner with no
// record keeps the empty key, which govern.NormalizeProvider maps to the default
// pool — exactly the pre-koryph-1o2.1 hardcoded constant's value.
func TestPoolKeyIsAccount(t *testing.T) {
	cases := []struct {
		name string
		rec  *registry.Record
		want string
	}{
		{"account profile", &registry.Record{AccountProfile: "work"}, "work"},
		{"quota profile overrides account", &registry.Record{AccountProfile: "personal", QuotaProfile: "max-pro"}, "max-pro"},
		{"nil record → default pool", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &runner{rec: tc.rec}
			if got := r.poolKey(); got != tc.want {
				t.Errorf("poolKey() = %q, want %q", got, tc.want)
			}
		})
	}
}
