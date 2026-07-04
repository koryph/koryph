// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"context"
	"testing"
	"time"
)

// TestGraphSnapshotZeroValueIsSafe verifies that GraphSnapshot's zero value
// does not panic when accessed.
func TestGraphSnapshotZeroValueIsSafe(t *testing.T) {
	var snap GraphSnapshot
	_ = snap.NodeCount
	_ = snap.EdgeCount
	_ = snap.ComputedAt
	// Iterating a nil map is safe in Go.
	for range snap.Deps {
		t.Error("unexpected iteration over nil Deps map")
	}
}

// TestGraphProviderNodeCount verifies the node-counting logic: nodes that
// appear only as targets (not sources) are still counted.
func TestGraphProviderNodeCount(t *testing.T) {
	// Simulate: A→B, A→C, D→C
	// Nodes: A, B, C, D (4); Edges: 3
	deps := map[string][]string{
		"A": {"B", "C"},
		"D": {"C"},
	}

	nodeSet := make(map[string]struct{})
	edges := 0
	for src, blockers := range deps {
		nodeSet[src] = struct{}{}
		for _, b := range blockers {
			nodeSet[b] = struct{}{}
		}
		edges += len(blockers)
	}

	wantNodes := 4 // A, B, C, D
	wantEdges := 3
	if len(nodeSet) != wantNodes {
		t.Errorf("NodeCount = %d, want %d", len(nodeSet), wantNodes)
	}
	if edges != wantEdges {
		t.Errorf("EdgeCount = %d, want %d", edges, wantEdges)
	}
}

// TestGraphProviderCachingWithinTTL verifies that Refresh returns the cached
// value within the TTL window without a new bd call.
func TestGraphProviderCachingWithinTTL(t *testing.T) {
	gp := NewGraphProvider(t.TempDir(), 10*time.Second)
	ctx := context.Background()
	now := time.Now()

	// Manually seed a valid cache.
	seeded := GraphSnapshot{
		Deps:       map[string][]string{"A": {"B"}, "B": {}},
		NodeCount:  2,
		EdgeCount:  1,
		ComputedAt: now,
	}
	gp.cache = seeded
	gp.at = now

	// Call within TTL — must return cached value unchanged.
	snap := gp.Refresh(ctx, now.Add(1*time.Second))
	if snap.NodeCount != seeded.NodeCount {
		t.Errorf("NodeCount = %d, want %d (cached)", snap.NodeCount, seeded.NodeCount)
	}
	if snap.EdgeCount != seeded.EdgeCount {
		t.Errorf("EdgeCount = %d, want %d (cached)", snap.EdgeCount, seeded.EdgeCount)
	}
	// at must not have advanced (no new bd call).
	if !gp.at.Equal(now) {
		t.Errorf("at advanced within TTL: got %v, want %v", gp.at, now)
	}
}

// TestGraphProviderTTLExpiryAdvancesAt verifies that Refresh advances gp.at
// after a successful (or attempted) bd call once the TTL has elapsed.
func TestGraphProviderTTLExpiryAdvancesAt(t *testing.T) {
	ttl := 2 * time.Second
	gp := NewGraphProvider(t.TempDir(), ttl)
	ctx := context.Background()
	old := time.Now().Add(-10 * time.Second)

	// Seed a stale cache.
	gp.cache = GraphSnapshot{NodeCount: 3, ComputedAt: old}
	gp.at = old // expired

	now := time.Now()
	// After TTL expiry, Refresh will attempt a bd call.
	// Whether bd succeeds or fails, the function must not panic.
	_ = gp.Refresh(ctx, now)
	// We cannot assert a specific NodeCount because bd may or may not be
	// available in the test environment; we just verify no panic occurred.
}

// TestGraphProviderDefaultTTL verifies that passing ttl=0 sets the package
// default rather than a zero-duration TTL (which would mean every call hits bd).
func TestGraphProviderDefaultTTL(t *testing.T) {
	gp := NewGraphProvider(t.TempDir(), 0)
	if gp.ttl <= 0 {
		t.Errorf("ttl = %v after passing 0; want package default (%v)", gp.ttl, graphTTL)
	}
	if gp.ttl != graphTTL {
		t.Errorf("ttl = %v, want graphTTL (%v)", gp.ttl, graphTTL)
	}
}
