// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"os"
	"strings"
	"testing"
)

func TestCapabilityHoldPersistsAndBoundsChangedEvidence(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SetCapabilityHold(CapabilityHold{
		BeadID: "bead-1", Capability: "network", EvidenceHash: "old", RetryLimit: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ConsumeCapabilityRetry("bead-1", "old"); err != nil || ok {
		t.Fatalf("unchanged retry = %v, %v", ok, err)
	}
	if ok, err := store.ConsumeCapabilityRetry("bead-1", "new"); err != nil || !ok {
		t.Fatalf("changed retry = %v, %v", ok, err)
	}
	if ok, err := store.ConsumeCapabilityRetry("bead-1", "newer"); err != nil || ok {
		t.Fatalf("over-budget retry = %v, %v", ok, err)
	}
	hold, ok, err := store.LoadCapabilityHold("bead-1")
	if err != nil || !ok || hold.RetryCount != 1 || hold.EvidenceHash != "new" {
		t.Fatalf("hold = %+v, %v, %v", hold, ok, err)
	}
}

func TestCapabilityHoldStoresOnlyDigestsForOperatorAndProbe(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SetCapabilityHold(CapabilityHold{
		BeadID: "bead-secret", Capability: "network", EvidenceHash: "baseline",
	}); err != nil {
		t.Fatal(err)
	}
	const operatorSecret = "token-super-secret"
	const probeSecret = "https://user:password@example.test"
	if err := store.RequestCapabilityRetry("bead-secret", operatorSecret); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCapabilityProbe("bead-secret", "network", probeSecret, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.capabilityHoldsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), operatorSecret) || strings.Contains(string(raw), probeSecret) {
		t.Fatalf("capability hold leaked raw evidence: %s", raw)
	}
	hold, ok, err := store.LoadCapabilityHold("bead-secret")
	if err != nil || !ok || len(hold.OperatorHash) != 64 || len(hold.ProbeHash) != 64 {
		t.Fatalf("hold = %+v, %v, %v", hold, ok, err)
	}
}

func TestSetCapabilityHoldPreservesSpentBudget(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SetCapabilityHold(CapabilityHold{
		BeadID: "bead-1", Capability: "network", EvidenceHash: "one", RetryLimit: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ConsumeCapabilityRetry("bead-1", "two"); err != nil || !ok {
		t.Fatalf("retry = %v, %v", ok, err)
	}
	if err := store.SetCapabilityHold(CapabilityHold{
		BeadID: "bead-1", Capability: "network", EvidenceHash: "three", RetryLimit: 1,
	}); err != nil {
		t.Fatal(err)
	}
	hold, _, err := store.LoadCapabilityHold("bead-1")
	if err != nil || hold.RetryCount != 1 {
		t.Fatalf("hold = %+v, %v", hold, err)
	}
}
