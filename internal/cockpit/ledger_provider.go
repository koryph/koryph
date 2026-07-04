// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
)

// agentStatus matches the shape that koryph dispatch seeds and agents rewrite
// at each step boundary ({"state","step","pct"}).
type agentStatus struct {
	State string `json:"state"`
	Step  string `json:"step"`
	Pct   int    `json:"pct"`
}

// LedgerProvider implements Provider over a project's run ledger and the
// machine-global governor. It is the primary provider used by the TUI.
type LedgerProvider struct {
	projectID string
	repoRoot  string

	ls *ledger.Store
	gs *govern.Store
}

// NewLedgerProvider returns a LedgerProvider for the project at repoRoot.
func NewLedgerProvider(projectID, repoRoot string) *LedgerProvider {
	return &LedgerProvider{
		projectID: projectID,
		repoRoot:  repoRoot,
		ls:        ledger.NewStore(repoRoot),
		gs:        govern.NewStore(),
	}
}

// ProjectID implements Provider.
func (p *LedgerProvider) ProjectID() string { return p.projectID }

// Refresh implements Provider. It reads the latest ledger run, all active
// slots, and the governor snapshot.
func (p *LedgerProvider) Refresh() (Snapshot, error) {
	snap := Snapshot{
		ProjectID:  p.projectID,
		CapturedAt: time.Now(),
	}

	// --- ledger -----------------------------------------------------------------
	run, err := p.ls.LoadLatest()
	if err == nil {
		snap.RunID = run.RunID
		snap.RunStatus = run.Status
		snap.Wave = run.Wave

		now := snap.CapturedAt
		slots := make([]SlotSnapshot, 0, len(run.Slots))
		for _, sl := range run.Slots {
			if sl == nil {
				continue
			}
			ss := slotToSnapshot(sl, now)
			slots = append(slots, ss)
		}
		sort.Slice(slots, func(i, j int) bool {
			return slots[i].PhaseID < slots[j].PhaseID
		})
		snap.Slots = slots
	}
	// A missing ledger is not an error — project may not have started a run.

	// --- governor ---------------------------------------------------------------
	snap.Governor = p.refreshGovernor()

	return snap, nil
}

// refreshGovernor reads the machine-global governor state.
func (p *LedgerProvider) refreshGovernor() GovernorSnapshot {
	gs := GovernorSnapshot{Pools: map[string]PoolSnapshot{}}

	pools, err := p.gs.Pools()
	if err != nil {
		return gs
	}
	for _, pool := range pools {
		ps, err := p.gs.PoolStatus(pool)
		if err != nil {
			continue
		}
		cfg := ps.AIMD
		dynamicCap := cfg.DynamicCap
		if dynamicCap <= 0 {
			dynamicCap = cfg.MaxGlobalAgents
		}
		if dynamicCap <= 0 {
			dynamicCap = govern.DefaultMaxGlobalAgents
		}
		gs.Pools[pool] = PoolSnapshot{
			Provider:     pool,
			Cap:          cfg.MaxGlobalAgents,
			Dynamic:      dynamicCap,
			Adaptive:     cfg.Adaptive,
			Leases:       len(ps.Leases),
			BreakerState: cfg.BreakerState,
		}
	}
	// Ensure the default pool is always present even if governor.json is missing.
	if _, ok := gs.Pools[govern.DefaultPool]; !ok {
		gs.Pools[govern.DefaultPool] = PoolSnapshot{
			Provider: govern.DefaultPool,
			Cap:      govern.DefaultMaxGlobalAgents,
			Dynamic:  govern.DefaultMaxGlobalAgents,
		}
	}
	return gs
}

// slotToSnapshot converts a ledger.Slot to a SlotSnapshot, reading the
// agent's status.json when StatusPath is set.
func slotToSnapshot(sl *ledger.Slot, now time.Time) SlotSnapshot {
	ss := SlotSnapshot{
		PhaseID:     sl.PhaseID,
		BeadID:      sl.BeadID,
		Title:       titleFor(sl),
		Stage:       sl.Status,
		Model:       sl.Model,
		Attempt:     sl.Attempts,
		PID:         sl.PID,
		Branch:      sl.Branch,
		Worktree:    sl.Worktree,
		CostUSD:     sl.CostUSD,
		EstimateUSD: sl.EstimateUSD,
	}
	if sl.DispatchedAt != "" {
		if t, err := time.Parse(time.RFC3339, sl.DispatchedAt); err == nil {
			ss.DispatchedAt = t
			ss.Elapsed = now.Sub(t)
		}
	}
	// Read live agent status file if available.
	if sl.StatusPath != "" {
		if as, err := readAgentStatus(sl.StatusPath); err == nil {
			ss.StatusJSON = as.State
			ss.StatusLine = as.Step
		}
	}
	return ss
}

// titleFor returns the best available display title for a slot.
func titleFor(sl *ledger.Slot) string {
	if sl.BeadID != "" {
		return sl.BeadID // TUI may enrich later with bd title cache
	}
	return sl.PhaseID
}

// readAgentStatus reads the agent's status.json file.
func readAgentStatus(path string) (agentStatus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agentStatus{}, err
	}
	var as agentStatus
	if err := json.Unmarshal(data, &as); err != nil {
		return agentStatus{}, err
	}
	return as, nil
}
