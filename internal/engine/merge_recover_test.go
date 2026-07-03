// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// TestMergeErrorRetryable pins the self-heal decision for a merge error
// (koryph-3fs): retry once from a fresh slot, then block. A slot already
// marked with mergeErrorRequeueNote, or one that has exhausted MaxAttempts,
// must not loop.
func TestMergeErrorRetryable(t *testing.T) {
	cases := []struct {
		name string
		slot ledger.Slot
		want bool
	}{
		{"fresh slot retries", ledger.Slot{Attempts: 0, Note: ""}, true},
		{"unrelated note still retries", ledger.Slot{Attempts: 1, Note: "blocking review findings"}, true},
		{"already merge-requeued blocks", ledger.Slot{Attempts: 1, Note: mergeErrorRequeueNote}, false},
		{"exhausted attempts block", ledger.Slot{Attempts: ledger.MaxAttempts, Note: ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sl := tc.slot
			if got := mergeErrorRetryable(&sl); got != tc.want {
				t.Errorf("mergeErrorRetryable(%+v) = %v, want %v", tc.slot, got, tc.want)
			}
		})
	}
}
