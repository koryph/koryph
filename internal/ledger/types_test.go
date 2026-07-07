// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"encoding/json"
	"testing"
)

// TestSlotOldLedgerCompat proves the koryph-2im.6 requeue-budget counters are
// additive: a Slot JSON blob captured before GateRequeues/MergeRequeues
// existed must still unmarshal cleanly, with both fields reading as zero —
// which is exactly the "no requeues spent yet" state the counters' consuming
// code (internal/engine's gateRequeueRetryable/mergeErrorRetryable) expects.
func TestSlotOldLedgerCompat(t *testing.T) {
	const oldSlotJSON = `{
		"phase_id": "tb1",
		"bead_id": "tb1",
		"branch": "koryph/tb1",
		"worktree": "/tmp/tb1",
		"status": "blocked",
		"attempts": 2,
		"commits": 3,
		"note": "gate failed after requeue: boom"
	}`

	var sl Slot
	if err := json.Unmarshal([]byte(oldSlotJSON), &sl); err != nil {
		t.Fatalf("unmarshal pre-koryph-2im.6 Slot JSON: %v", err)
	}
	if sl.GateRequeues != 0 {
		t.Errorf("GateRequeues = %d, want 0 (zero value) for an old ledger without the field", sl.GateRequeues)
	}
	if sl.MergeRequeues != 0 {
		t.Errorf("MergeRequeues = %d, want 0 (zero value) for an old ledger without the field", sl.MergeRequeues)
	}
	// A zero-valued counter must behave like "no requeues spent" downstream:
	// still within budget (2), so a slot recovered from an old ledger is
	// still eligible for a gate/merge-error requeue rather than wrongly
	// treated as budget-exhausted.
	if sl.GateRequeues >= 2 {
		t.Error("zero-value GateRequeues must read as within budget, not exhausted")
	}

	// Round-trip: marshaling a zero-valued counter omits it (omitempty), so
	// re-marshaling an old slot produces JSON indistinguishable from the
	// original absence of the field.
	out, err := json.Marshal(&sl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var rt map[string]any
	if err := json.Unmarshal(out, &rt); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if _, ok := rt["gate_requeues"]; ok {
		t.Error("gate_requeues should be omitted (omitempty) when zero")
	}
	if _, ok := rt["merge_requeues"]; ok {
		t.Error("merge_requeues should be omitted (omitempty) when zero")
	}
}

// TestSlotProxyIDAdditiveCompat proves ProxyID (koryph-3l1.1) is additive
// exactly like GateRequeues/MergeRequeues above: a Slot JSON blob captured
// before the field existed unmarshals it to "" (direct — no agent proxy),
// which is the exact "no proxy" value quota's calibKey treats as the legacy
// "tier:size" population (never "@"-suffixed). omitempty keeps a
// direct-dispatch slot's re-marshaled JSON indistinguishable from an old
// ledger that never had the field at all.
func TestSlotProxyIDAdditiveCompat(t *testing.T) {
	const oldSlotJSON = `{
		"phase_id": "tb2",
		"bead_id": "tb2",
		"branch": "koryph/tb2",
		"worktree": "/tmp/tb2",
		"status": "running",
		"billing_mode": "subscription"
	}`

	var sl Slot
	if err := json.Unmarshal([]byte(oldSlotJSON), &sl); err != nil {
		t.Fatalf("unmarshal pre-koryph-3l1.1 Slot JSON: %v", err)
	}
	if sl.ProxyID != "" {
		t.Errorf("ProxyID = %q, want \"\" (zero value) for an old ledger without the field", sl.ProxyID)
	}

	out, err := json.Marshal(&sl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var rt map[string]any
	if err := json.Unmarshal(out, &rt); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if _, ok := rt["proxy_id"]; ok {
		t.Error("proxy_id should be omitted (omitempty) when empty")
	}

	// A non-empty ProxyID survives round-trip verbatim (the format callers
	// will feed into quota.RecordForProxy/EstimateItemForRuntimeProxy).
	sl.ProxyID = "http://127.0.0.1:8091#v3"
	out, err = json.Marshal(&sl)
	if err != nil {
		t.Fatalf("marshal with ProxyID: %v", err)
	}
	var sl2 Slot
	if err := json.Unmarshal(out, &sl2); err != nil {
		t.Fatalf("unmarshal with ProxyID: %v", err)
	}
	if sl2.ProxyID != sl.ProxyID {
		t.Errorf("ProxyID roundtrip = %q, want %q", sl2.ProxyID, sl.ProxyID)
	}
}
