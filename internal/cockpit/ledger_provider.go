// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
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
	projectID      string
	repoRoot       string
	accountProfile string // for quota config lookup; may be ""

	ls *ledger.Store
	gs *govern.Store
	bd *beads.Adapter

	// burndown cache — refreshed at burndownTTL cadence (not every 100 ms tick).
	burndownCache BurndownSnapshot
	burndownAt    time.Time

	// efficiency cache — refreshed at efficiencyTTL cadence (not every 100 ms tick).
	efficiencyCache EfficiencySnapshot
	efficiencyAt    time.Time

	// graph — shared dependency graph snapshot; refreshed at graphTTL cadence.
	graph *GraphProvider
}

// NewLedgerProvider returns a LedgerProvider for the project at repoRoot.
// accountProfile is used to load the quota config for cost projections; pass ""
// to skip quota-sourced window data.
func NewLedgerProvider(projectID, repoRoot, accountProfile string) *LedgerProvider {
	return &LedgerProvider{
		projectID:      projectID,
		repoRoot:       repoRoot,
		accountProfile: accountProfile,
		ls:             ledger.NewStore(repoRoot),
		gs:             govern.NewStore(),
		bd:             beads.New(repoRoot),
		graph:          NewGraphProvider(repoRoot, 0), // 0 → package default graphTTL
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

	// --- burndown (cached) ------------------------------------------------------
	if snap.CapturedAt.Sub(p.burndownAt) >= burndownTTL {
		p.burndownCache = p.refreshBurndown(snap.CapturedAt)
		p.burndownAt = snap.CapturedAt
	}
	snap.Burndown = p.burndownCache

	// --- efficiency (cached) ----------------------------------------------------
	if snap.CapturedAt.Sub(p.efficiencyAt) >= efficiencyTTL {
		p.efficiencyCache = p.refreshEfficiency(snap, snap.CapturedAt)
		p.efficiencyAt = snap.CapturedAt
	}
	snap.Efficiency = p.efficiencyCache

	// --- graph (cached) ---------------------------------------------------------
	snap.Graph = p.graph.Refresh(context.Background(), snap.CapturedAt)

	return snap, nil
}

// refreshBurndown builds a fresh BurndownSnapshot, soft-failing on any
// data source that is unavailable (beads absent, quota uncalibrated, etc.).
func (p *LedgerProvider) refreshBurndown(now time.Time) BurndownSnapshot {
	ctx := context.Background()

	// --- ledger history -------------------------------------------------------
	runIDs, _ := p.ls.ListRuns()
	if len(runIDs) > burndownMaxRuns {
		runIDs = runIDs[:burndownMaxRuns]
	}
	var runs []*ledger.Run
	for _, id := range runIDs {
		run, err := p.ls.LoadRun(id)
		if err == nil {
			runs = append(runs, run)
		}
	}

	// --- beads ---------------------------------------------------------------
	var readyIssues []beads.Issue
	if ri, err := p.bd.Ready(ctx, beads.ReadyOpts{}); err == nil {
		readyIssues = ri
	}

	// Collect unique epic IDs from the current run's slots and from
	// the ledger history.
	epicIDs := map[string]struct{}{}
	for _, run := range runs {
		for _, sl := range run.Slots {
			if sl != nil && sl.EpicID != "" {
				epicIDs[sl.EpicID] = struct{}{}
			}
		}
	}
	epicChildren := map[string][]beads.Issue{}
	for epicID := range epicIDs {
		if children, err := p.bd.ListChildren(ctx, epicID); err == nil {
			epicChildren[epicID] = children
		}
	}

	// --- quota config (file read only; no ccusage subprocess in the TUI) --------
	// We read the persisted Config for estimator calibration but do NOT call
	// quota.Snapshot (which runs ccusage — too slow for a 5 s TUI refresh).
	// Window data will be shown as "unknown" until a background refresh bead
	// adds it (filed as a follow-up in SUMMARY.md).
	var qcfg *quota.Config
	if p.accountProfile != "" {
		if cfg, err := quota.LoadConfig(p.accountProfile); err == nil {
			qcfg = cfg
		}
	}

	return computeBurndown(burndownInput{
		runs:         runs,
		readyIssues:  readyIssues,
		epicChildren: epicChildren,
		quotaCfg:     qcfg,
		quotaUsage:   nil, // see above
		now:          now,
	})
}

// refreshEfficiency builds a fresh EfficiencySnapshot, soft-failing on any
// data source that is unavailable.
func (p *LedgerProvider) refreshEfficiency(snap Snapshot, now time.Time) EfficiencySnapshot {
	// Load historical runs for the dispatch sparkline.
	runIDs, _ := p.ls.ListRuns()
	if len(runIDs) > efficiencyMaxRuns {
		runIDs = runIDs[:efficiencyMaxRuns]
	}
	var runs []*ledger.Run
	for _, id := range runIDs {
		run, err := p.ls.LoadRun(id)
		if err == nil {
			runs = append(runs, run)
		}
	}

	// Active slots from the current run's snapshot (already fetched above).
	var active []*ledger.Slot
	if snap.RunID != "" {
		if run, err := p.ls.LoadRun(snap.RunID); err == nil {
			active = activeSlots(run)
		}
	}

	// Quota config (file read only).
	var qcfg *quota.Config
	if p.accountProfile != "" {
		if cfg, err := quota.LoadConfig(p.accountProfile); err == nil {
			qcfg = cfg
		}
	}

	return computeEfficiency(efficiencyInput{
		runs:        runs,
		activeSlots: active,
		govStore:    p.gs,
		govSnap:     snap.Governor,
		quotaCfg:    qcfg,
		quotaUsage:  nil, // ccusage not run in TUI path
		now:         now,
	})
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
