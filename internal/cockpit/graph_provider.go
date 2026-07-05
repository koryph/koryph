// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"context"
	"sync"
	"time"

	"github.com/koryph/koryph/internal/beads"
)

const (
	// graphTTL is how long the dependency graph snapshot is cached between
	// bd calls. bd list --format digraph is cheap but not free; 5 s matches
	// the burndown cache cadence.
	graphTTL = 5 * time.Second
)

// GraphProvider reads the beads dependency graph on demand and caches the
// result for graphTTL before the next bd call. It is embedded in LedgerProvider
// and its snapshot is delivered via Snapshot.Graph so that multiple TUI tabs
// (queue, detail) share a single read without independently calling bd.
//
// GraphProvider is goroutine-safe; Refresh holds mu for the duration of any
// cache miss so concurrent callers (the 100 ms tick and the async BeadDetail
// goroutine) do not race on g.cache / g.at.
type GraphProvider struct {
	mu    sync.Mutex
	bd    *beads.Adapter
	ttl   time.Duration
	cache GraphSnapshot
	at    time.Time
}

// NewGraphProvider returns a GraphProvider that reads from the beads repo at
// repoRoot. ttl sets the cache duration; pass 0 to use the package default
// (graphTTL).
func NewGraphProvider(repoRoot string, ttl time.Duration) *GraphProvider {
	if ttl <= 0 {
		ttl = graphTTL
	}
	return &GraphProvider{
		bd:  beads.New(repoRoot),
		ttl: ttl,
	}
}

// Refresh returns a cached GraphSnapshot or re-reads from bd if the cache has
// expired. now is the caller's notion of the current time (passed in so the
// provider does not call time.Now() itself, making tests deterministic).
//
// A bd error is treated as a soft failure: the last valid cache is returned
// (which may be the zero value on the first call). The caller can detect a
// zero snapshot via GraphSnapshot.NodeCount == 0.
func (g *GraphProvider) Refresh(ctx context.Context, now time.Time) GraphSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.at.IsZero() && now.Sub(g.at) < g.ttl {
		return g.cache
	}

	deps, err := g.bd.DepDigraph(ctx)
	if err != nil || deps == nil {
		// Soft-fail: return the cached (possibly zero) snapshot unchanged.
		return g.cache
	}

	// Count edges and collect all node IDs (sources + unique targets).
	nodeSet := make(map[string]struct{}, len(deps))
	edges := 0
	for src, blockers := range deps {
		nodeSet[src] = struct{}{}
		for _, blocker := range blockers {
			nodeSet[blocker] = struct{}{}
		}
		edges += len(blockers)
	}

	g.cache = GraphSnapshot{
		Deps:       deps,
		NodeCount:  len(nodeSet),
		EdgeCount:  edges,
		ComputedAt: now,
	}
	g.at = now
	return g.cache
}
