// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// TestMergeErrorRetryable pins the requeue-budget decision for a merge error
// (koryph-3fs, budget raised to 2 by koryph-2im.6): the first and second
// merge-error requeues retry, the third blocks — and MaxAttempts still caps
// regardless of how much of the requeue budget remains.
func TestMergeErrorRetryable(t *testing.T) {
	cases := []struct {
		name string
		slot ledger.Slot
		want bool
	}{
		{"fresh slot (0 requeues) retries", ledger.Slot{Attempts: 0, MergeRequeues: 0}, true},
		{"one requeue spent still retries", ledger.Slot{Attempts: 1, MergeRequeues: 1}, true},
		{"budget exhausted (2 requeues) blocks", ledger.Slot{Attempts: 2, MergeRequeues: 2}, false},
		{"exhausted attempts block despite budget remaining",
			ledger.Slot{Attempts: ledger.MaxAttempts, MergeRequeues: 0}, false},
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

// TestGateRequeueRetryable is the gate-failed mirror of
// TestMergeErrorRetryable (koryph-2im.6): first and second gate-failure
// requeues retry, the third blocks, and MaxAttempts still caps.
func TestGateRequeueRetryable(t *testing.T) {
	cases := []struct {
		name string
		slot ledger.Slot
		want bool
	}{
		{"fresh slot (0 requeues) retries", ledger.Slot{Attempts: 0, GateRequeues: 0}, true},
		{"one requeue spent still retries", ledger.Slot{Attempts: 1, GateRequeues: 1}, true},
		{"budget exhausted (2 requeues) blocks", ledger.Slot{Attempts: 2, GateRequeues: 2}, false},
		{"exhausted attempts block despite budget remaining",
			ledger.Slot{Attempts: ledger.MaxAttempts, GateRequeues: 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sl := tc.slot
			if got := gateRequeueRetryable(&sl); got != tc.want {
				t.Errorf("gateRequeueRetryable(%+v) = %v, want %v", tc.slot, got, tc.want)
			}
		})
	}
}

// TestCommitStyleRetryableUnchanged pins commit-style's budget at exactly 1
// (koryph-2im.6 explicitly leaves this path unchanged): a single bounce, then
// block, regardless of the (now-unused-for-gate/merge) requeue counters.
func TestCommitStyleRetryableUnchanged(t *testing.T) {
	cases := []struct {
		name string
		slot ledger.Slot
		want bool
	}{
		{"fresh slot bounces", ledger.Slot{Attempts: 0, Note: ""}, true},
		{"unrelated note still bounces", ledger.Slot{Attempts: 1, Note: "blocking review findings"}, true},
		{"already commit-style-requeued blocks", ledger.Slot{Attempts: 1, Note: commitStyleRequeueNote}, false},
		{"exhausted attempts block", ledger.Slot{Attempts: ledger.MaxAttempts, Note: ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sl := tc.slot
			if got := commitStyleRetryable(&sl); got != tc.want {
				t.Errorf("commitStyleRetryable(%+v) = %v, want %v", tc.slot, got, tc.want)
			}
		})
	}
}
