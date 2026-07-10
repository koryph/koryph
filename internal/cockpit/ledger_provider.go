// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/sched"
)

// derivedRefreshTimeout bounds one background refreshDerived pass
// (koryph-b01): its bd subprocess calls (list/ready/digraph, ~9-15 s cold)
// previously ran on context.Background() and could block indefinitely under
// dolt lock contention — exactly when a run loop is hammering the same DB —
// wedging the pass forever and freezing every derived tab. Generous (bd can
// legitimately be slow) but finite; a timed-out pass keeps the previous
// caches and retries on the next TTL tick.
const derivedRefreshTimeout = 60 * time.Second

// derivedTTL is how long the beads-sourced derived sections (burndown,
// efficiency, graph, queue) are cached before the background goroutine
// recomputes them. All four share one cadence; the per-section values were
// identical (5 s) and the sections are now recomputed together.
const derivedTTL = 5 * time.Second

// agentStatus matches the shape that koryph dispatch seeds and agents rewrite
// at each step boundary ({"state","step","pct"}).
type agentStatus struct {
	State string `json:"state"`
	Step  string `json:"step"`
	Pct   int    `json:"pct"`
}

// LedgerProvider implements Provider over a project's run ledger and the
// machine-global governor. It is the primary provider used by the TUI.
//
// Refresh() is safe to call concurrently from multiple goroutines (the TUI
// tick Cmd and the manual 'r' reload Cmd can race); mu serialises access to
// mutable fields.
type LedgerProvider struct {
	// mu serialises all mutable field access — Refresh() is called from both
	// the background tick goroutine and the tea.Cmd returned by manual reload,
	// so concurrent calls are possible.
	mu sync.Mutex

	projectID      string
	repoRoot       string
	accountProfile string // for quota config lookup; may be ""

	ls *ledger.Store
	gs *govern.Store
	bd *beads.Adapter

	// Derived sections (burndown, efficiency, graph, queue) are expensive to
	// assemble: they shell out to bd, which costs several seconds on a large
	// project — bd's digraph export alone is ~9 s. They are therefore served
	// from these caches and recomputed by a single background goroutine
	// (refreshDerived) so a cold or expired cache NEVER blocks the refresh tick.
	// The cheap ledger data (slots/threads, run status, governor, events) is
	// always assembled synchronously and returned immediately; the derived
	// sections fill in a few seconds later when the background job completes.
	// All four caches advance together and are stamped with derivedAt.
	burndownCache     BurndownSnapshot
	efficiencyCache   EfficiencySnapshot
	graphCache        GraphSnapshot
	queueCache        QueueSnapshot
	derivedAt         time.Time // when the four caches above were last recomputed
	derivedRefreshing bool      // a background refreshDerived is in flight

	// derivedTimeout bounds one refreshDerived pass (koryph-b01). Set once at
	// construction (derivedRefreshTimeout); tests shrink it to exercise the
	// timeout/latch-reset path without a 60 s wait. Read by the background
	// goroutine without the lock, so it must never be mutated after Refresh
	// has first been called.
	derivedTimeout time.Duration

	// graph — shared dependency graph provider (holds its own cache + TTL).
	// Driven from refreshDerived and read synchronously by BeadDetail.
	graph *GraphProvider

	// events — live events feed collector (koryph-9af.5).
	events *eventCollector
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
		events:         newEventCollector(),
		derivedTimeout: derivedRefreshTimeout,
	}
}

// ProjectID implements Provider.
func (p *LedgerProvider) ProjectID() string { return p.projectID }

// RepoRoot implements Provider.
func (p *LedgerProvider) RepoRoot() string { return p.repoRoot }

// Refresh implements Provider. It reads the latest ledger run, all active
// slots, and the governor snapshot. Concurrent calls are safe; the mutex
// serialises execution to protect burndownCache, burndownAt, and events.
func (p *LedgerProvider) Refresh() (Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
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

	// --- derived sections (burndown, efficiency, graph, queue) ------------------
	// Served from cache and recomputed off the tick thread (see refreshDerived).
	// bd's digraph export alone is ~9 s, so computing these synchronously would
	// stall the whole snapshot — including the cheap slot/thread data above —
	// for that long on every cold or expired cache. Instead we return the last
	// good caches immediately and kick a single background recompute when they
	// are stale, so the Threads tab paints in milliseconds.
	snap.Burndown = p.burndownCache
	snap.Efficiency = p.efficiencyCache
	snap.Graph = p.graphCache
	snap.Queue = p.queueCache
	// Enrich slot display titles from the queue cache (which carries the real bd
	// titles). The ledger slot itself only knows the bead id; the Threads tab
	// shows a short description alongside the id. A bead absent from the queue —
	// a closed/terminal bead, or before the first derived refresh populates the
	// cache — keeps its id-based fallback title (titleFor).
	enrichSlotTitles(snap.Slots, snap.Queue)
	if !p.derivedRefreshing && snap.CapturedAt.Sub(p.derivedAt) >= derivedTTL {
		p.derivedRefreshing = true
		// Pass the inputs the background job needs by value; it must not read
		// p's mutable cache fields without the lock.
		go p.refreshDerived(snap.Slots, snap.Governor, snap.RunID, snap.CapturedAt)
	}

	// --- events (koryph-9af.5) --------------------------------------------------
	// Collect is called on every tick — it is cheap (diff + audit tail).
	p.events.Collect(snap)
	snap.Events = p.events.Snapshot()

	return snap, nil
}

// refreshDerived recomputes the expensive beads-sourced sections (graph,
// burndown, efficiency, queue) off the refresh tick and stores them in the
// provider caches. It is launched as a goroutine by Refresh when the caches are
// stale, guarded by derivedRefreshing so at most one runs at a time.
//
// The expensive work (bd subprocess calls, ~15 s cold) runs WITHOUT holding
// p.mu so concurrent cheap Refresh calls are never blocked; the lock is taken
// only at the end to publish the results atomically. slots/gov/runID/now are
// passed by value so the goroutine reads no mutable provider state unlocked
// (p.ls, p.bd, p.gs, p.graph are set once at construction and are safe to read).
//
// Failure containment (koryph-b01): the pass is bounded by
// derivedRefreshTimeout and the derivedRefreshing latch resets via defer —
// including on a panic, which is recovered rather than crashing the whole TUI
// — so a wedged or failed pass can never freeze the derived tabs for the rest
// of the session; the next TTL tick simply retries. A pass whose queue
// assembly failed (bd error/timeout) keeps the previous queueCache instead of
// clobbering a good tree with an empty snapshot.
func (p *LedgerProvider) refreshDerived(slots []SlotSnapshot, gov GovernorSnapshot, runID string, now time.Time) {
	defer func() {
		r := recover()
		p.mu.Lock()
		p.derivedRefreshing = false
		// Advance derivedAt even on failure so retries pace at the TTL rather
		// than hot-looping on every tick against a struggling bd.
		p.derivedAt = now
		p.mu.Unlock()
		if r != nil {
			logDerivedPanic(p.projectID, r)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), p.derivedTimeout)
	defer cancel()

	// Graph first — the queue computation consumes it.
	graphSnap := p.graph.Refresh(ctx, now)
	burndown := p.refreshBurndown(ctx, now)
	efficiency := p.refreshEfficiency(Snapshot{RunID: runID, Governor: gov}, now)
	queue, queueOK := p.refreshQueue(ctx, Snapshot{Slots: slots, Graph: graphSnap, Governor: gov, CapturedAt: now})

	p.mu.Lock()
	p.graphCache = graphSnap
	p.burndownCache = burndown
	p.efficiencyCache = efficiency
	// A failed queue assembly (bd error/timeout, queueOK=false) keeps a
	// previous NON-EMPTY tree — clobbering it with an empty snapshot would
	// silently blank the Queue tab (koryph-b01). When there is nothing to
	// protect, the stamped-but-empty snapshot publishes as usual: its
	// ComputedAt is what tells the TUI "refresh completed, genuinely no data"
	// apart from "still waiting on the first pass".
	if queueOK || p.queueCache.NodeCount == 0 {
		p.queueCache = queue
	}
	p.mu.Unlock()
}

// refreshBurndown builds a fresh BurndownSnapshot, soft-failing on any
// data source that is unavailable (beads absent, quota uncalibrated, etc.).
func (p *LedgerProvider) refreshBurndown(ctx context.Context, now time.Time) BurndownSnapshot {

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

	// Per-kind external resource ledger (koryph-4ql.1 L7, koryph-4ql.10).
	// Fail open (I6): on any error gs.Resources stays nil, matching an old
	// pre-resources snapshot — the TUI/IDE render no resources section.
	if rs, err := p.gs.ResourcesStatus(); err == nil {
		gs.Resources = convertResourceStatuses(rs)
	}
	return gs
}

// convertResourceStatuses maps govern.ResourceStatus to the cockpit-local
// ResourceSnapshot wire shape (koryph-4ql.10), the same mirroring
// refreshGovernor already does for PoolSnapshot.
func convertResourceStatuses(in []govern.ResourceStatus) []ResourceSnapshot {
	if len(in) == 0 {
		return nil
	}
	out := make([]ResourceSnapshot, 0, len(in))
	for _, rs := range in {
		var holders []ResourceHolderSnapshot
		if len(rs.Holders) > 0 {
			holders = make([]ResourceHolderSnapshot, 0, len(rs.Holders))
			for _, h := range rs.Holders {
				holders = append(holders, ResourceHolderSnapshot{
					Project:      h.Project,
					Bead:         h.Bead,
					MemReserveMB: h.MemReserveMB,
					Ramping:      h.Ramping,
				})
			}
		}
		out = append(out, ResourceSnapshot{
			Kind:           rs.Kind,
			Capacity:       rs.Capacity,
			MemMB:          rs.MemMB,
			RampSeconds:    rs.RampSeconds,
			Probe:          rs.Probe,
			Holders:        holders,
			ReservedMB:     rs.ReservedMB,
			MaterializedMB: rs.MaterializedMB,
		})
	}
	return out
}

// slotToSnapshot converts a ledger.Slot to a SlotSnapshot, reading the
// agent's status.json when StatusPath is set.
func slotToSnapshot(sl *ledger.Slot, now time.Time) SlotSnapshot {
	ss := SlotSnapshot{
		PhaseID:            sl.PhaseID,
		BeadID:             sl.BeadID,
		Title:              titleFor(sl),
		Stage:              sl.Status,
		Model:              sl.Model,
		ModelWhy:           sl.ModelWhy,
		Attempt:            sl.Attempts,
		PID:                sl.PID,
		Branch:             sl.Branch,
		Worktree:           sl.Worktree,
		CostUSD:            sl.CostUSD,
		EstimateUSD:        sl.EstimateUSD,
		GateRequeues:       sl.GateRequeues,
		MergeRequeues:      sl.MergeRequeues,
		ConflictRequeues:   sl.ConflictRequeues,
		RateLimitRequeues:  sl.RateLimitRequeues,
		BudgetKillRequeues: sl.BudgetKillRequeues,
		Terminal:           ledger.Terminal(sl.Status),
		PeakRSSMB:          sl.PeakRSSMB,
		AvgRSSMB:           sl.AvgRSSMB,
		CPUSeconds:         sl.CPUSeconds,
		IOReadMB:           sl.IOReadMB,
		IOWriteMB:          sl.IOWriteMB,
		ResourceSamples:    sl.ResourceSamples,
	}
	if sl.DispatchedAt != "" {
		if t, err := time.Parse(time.RFC3339, sl.DispatchedAt); err == nil {
			ss.DispatchedAt = t
			ss.Elapsed = now.Sub(t)
		}
	}
	if sl.FinishedAt != "" {
		if t, err := time.Parse(time.RFC3339, sl.FinishedAt); err == nil {
			ss.FinishedAt = t
			if !ss.DispatchedAt.IsZero() {
				ss.Elapsed = t.Sub(ss.DispatchedAt) // final wall time once terminal
			}
		}
	}
	ss.CPUUtilPct = cpuUtilPct(sl.CPUSeconds, ss.DispatchedAt, ss.FinishedAt, now)
	// Read live agent status file if available.
	if sl.StatusPath != "" {
		if as, err := readAgentStatus(sl.StatusPath); err == nil {
			ss.StatusJSON = as.State
			ss.StatusLine = as.Step
		}
	}
	return ss
}

// cpuUtilPct derives average CPU utilization (percent; 100 = one core saturated
// for the whole window) from cumulative CPU seconds over the slot's wall-clock
// window: dispatch → finish (terminal) or dispatch → now (live). Returns 0 when
// the window is unknown or non-positive.
func cpuUtilPct(cpuSeconds float64, started, finished, now time.Time) float64 {
	if started.IsZero() || cpuSeconds <= 0 {
		return 0
	}
	end := now
	if !finished.IsZero() {
		end = finished
	}
	wall := end.Sub(started).Seconds()
	if wall <= 0 {
		return 0
	}
	return cpuSeconds / wall * 100
}

// titleFor returns the id-based fallback display title for a slot. Refresh
// upgrades this to the real bd title via enrichSlotTitles when the bead is
// present in the queue cache.
func titleFor(sl *ledger.Slot) string {
	if sl.BeadID != "" {
		return sl.BeadID
	}
	return sl.PhaseID
}

// enrichSlotTitles overwrites each slot's Title with the real bd title from the
// queue snapshot, keyed by bead id (falling back to phase id). Slots whose bead
// is absent from the queue keep their id-based fallback title (titleFor), which
// is why callers can detect "no real title" by Title == id.
func enrichSlotTitles(slots []SlotSnapshot, qs QueueSnapshot) {
	if len(slots) == 0 || len(qs.Roots) == 0 {
		return
	}
	titles := make(map[string]string)
	collectQueueTitles(qs.Roots, titles)
	for i := range slots {
		id := slots[i].BeadID
		if id == "" {
			id = slots[i].PhaseID
		}
		if t := titles[id]; t != "" {
			slots[i].Title = t
		}
	}
}

// collectQueueTitles walks the queue tree, recording each node's bead id → title.
func collectQueueTitles(nodes []QueueNode, out map[string]string) {
	for _, n := range nodes {
		if n.Issue.Title != "" {
			out[n.Issue.ID] = n.Issue.Title
		}
		collectQueueTitles(n.Children, out)
	}
}

// refreshQueue builds a fresh QueueSnapshot. It calls bd list and bd ready
// then cross-references with the current running slots and dep graph.
// Soft-fails when bd is absent (returns a zero snapshot).
//
// ok reports whether the queue data was actually assembled (koryph-b01): a
// bd that is absent, erroring, or timed out returns ok=false so the caller
// keeps the previous cache instead of clobbering a good tree with an empty
// snapshot — an empty-because-no-issues queue still returns ok=true.
func (p *LedgerProvider) refreshQueue(ctx context.Context, snap Snapshot) (QueueSnapshot, bool) {
	if !p.bd.Available() {
		return QueueSnapshot{ComputedAt: snap.CapturedAt}, false
	}

	// All open issues.
	allIssues, err := p.bd.List(ctx)
	if err != nil {
		return QueueSnapshot{ComputedAt: snap.CapturedAt}, false
	}

	// Ready frontier.
	readyList, _ := p.bd.Ready(ctx, beads.ReadyOpts{})
	readyIDs := make(map[string]bool, len(readyList))
	for _, iss := range readyList {
		readyIDs[iss.ID] = true
	}

	// Build issue lookup for footprint computation.
	byID := make(map[string]beads.Issue, len(allIssues))
	for _, iss := range allIssues {
		byID[iss.ID] = iss
	}

	// Resolve parents referenced by an open child but absent from the open set
	// (a closed/filtered epic). Fetching their metadata lets computeQueue keep
	// the open children grouped under the epic instead of orphaning them to the
	// top level (the flat-queue regression). Bounded so a pathological graph
	// can't fan out into an unbounded burst of `bd show` calls.
	closedParents := p.resolveClosedParents(ctx, allIssues, byID)

	// Running IDs and footprints from current slots.
	runningIDs := make(map[string]bool, len(snap.Slots))
	runningFPs := make(map[string]sched.Footprint, len(snap.Slots))
	for _, sl := range snap.Slots {
		if sl.Stage != "running" && sl.Stage != "dispatching" {
			continue
		}
		id := sl.BeadID
		if id == "" {
			id = sl.PhaseID
		}
		runningIDs[id] = true
		if iss, ok := byID[id]; ok {
			runningFPs[id] = sched.FootprintFor(iss, nil)
		}
	}

	return computeQueue(queueInput{
		allIssues:     allIssues,
		readyIDs:      readyIDs,
		runningIDs:    runningIDs,
		runningFPs:    runningFPs,
		graph:         snap.Graph,
		resources:     snap.Governor.Resources,
		projectID:     p.projectID,
		closedParents: closedParents,
		now:           snap.CapturedAt,
	}), true
}

// closedParentFetchMax bounds how many `bd show` lookups refreshQueue issues
// for closed/absent parent epics in a single refresh, so a graph with many
// distinct closed parents cannot fan out into an unbounded subprocess burst.
const closedParentFetchMax = 48

// resolveClosedParents returns metadata for parent epics referenced by an open
// child but absent from allIssues (byID) — i.e. the epic has been closed while
// children remain open, so `bd list` omits it. Each is fetched via bd.Show
// (which returns closed issues), capped at closedParentFetchMax. A parent whose
// lookup fails is simply omitted; computeQueue then falls back to an ID-only
// container. Returns nil when there are no such parents (the common case), so
// the extra work is skipped entirely on a fully-open graph.
func (p *LedgerProvider) resolveClosedParents(ctx context.Context, allIssues []beads.Issue, byID map[string]beads.Issue) map[string]beads.Issue {
	// Collect distinct absent parent IDs in first-seen order.
	seen := make(map[string]struct{})
	var missing []string
	for _, iss := range allIssues {
		if iss.ParentID == "" {
			continue
		}
		if _, present := byID[iss.ParentID]; present {
			continue
		}
		if _, dup := seen[iss.ParentID]; dup {
			continue
		}
		seen[iss.ParentID] = struct{}{}
		missing = append(missing, iss.ParentID)
	}
	if len(missing) == 0 {
		return nil
	}
	if len(missing) > closedParentFetchMax {
		missing = missing[:closedParentFetchMax]
	}
	out := make(map[string]beads.Issue, len(missing))
	for _, id := range missing {
		if iss, err := p.bd.Show(ctx, id); err == nil {
			out[id] = iss
		}
	}
	return out
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

// BeadDetail assembles a BeadDetailSnapshot for beadID using the live ledger
// slot (if any) and the beads adapter. It soft-fails on every data source so
// that a partially-populated detail is always returned rather than an error.
func (p *LedgerProvider) BeadDetail(ctx context.Context, beadID string, now time.Time) BeadDetailSnapshot {
	d := BeadDetailSnapshot{
		BeadID:     beadID,
		ComputedAt: now,
	}

	// --- beads metadata -------------------------------------------------------
	if issue, err := p.bd.Show(ctx, beadID); err == nil {
		d.Title = issue.Title
		d.Description = issue.Description
		d.Notes = issue.Notes
		d.Status = issue.Status
		d.Priority = issue.Priority
		d.IssueType = issue.IssueType
		d.Labels = issue.Labels
		d.ParentID = issue.ParentID
	}

	// --- graph deps -----------------------------------------------------------
	// Graph is refreshed by the graph provider; we read it from the run ledger's
	// cached graph provider if available.
	gSnap := p.graph.Refresh(ctx, now)
	if deps, ok := gSnap.Deps[beadID]; ok {
		d.Deps = deps
	}
	// Reverse deps: find all nodes that list beadID as a dependency.
	// Sort for stable display order (map iteration is nondeterministic).
	for nodeID, nodeDeps := range gSnap.Deps {
		for _, dep := range nodeDeps {
			if dep == beadID {
				d.ReverseDeps = append(d.ReverseDeps, nodeID)
				break
			}
		}
	}
	sort.Strings(d.ReverseDeps)

	// --- slot-derived fields --------------------------------------------------
	run, err := p.ls.LoadLatest()
	if err != nil {
		return d
	}
	for _, sl := range run.Slots {
		if sl == nil || sl.BeadID != beadID {
			continue
		}
		d.Branch = sl.Branch
		d.Worktree = sl.Worktree
		d.CostUSD = sl.CostUSD
		d.EstimateUSD = sl.EstimateUSD
		d.LogPath = sl.LogPath
		d.StreamPath = sl.Stream

		// Timing + resource usage (koryph process-metrics).
		d.PeakRSSMB = sl.PeakRSSMB
		d.AvgRSSMB = sl.AvgRSSMB
		d.CPUSeconds = sl.CPUSeconds
		d.IOReadMB = sl.IOReadMB
		d.IOWriteMB = sl.IOWriteMB
		d.ResourceSamples = sl.ResourceSamples
		if sl.DispatchedAt != "" {
			if t, err := time.Parse(time.RFC3339, sl.DispatchedAt); err == nil {
				d.StartedAt = t
			}
		}
		if sl.FinishedAt != "" {
			if t, err := time.Parse(time.RFC3339, sl.FinishedAt); err == nil {
				d.FinishedAt = t
			}
		}
		d.CPUUtilPct = cpuUtilPct(sl.CPUSeconds, d.StartedAt, d.FinishedAt, now)

		// Build one AttemptRecord per attempt (we have summary counts only,
		// so synthesise a single record from the current slot state).
		rec := AttemptRecord{
			Attempt:  sl.Attempts,
			Status:   sl.Status,
			CostUSD:  sl.CostUSD,
			Model:    sl.Model,
			Branch:   sl.Branch,
			Worktree: sl.Worktree,
		}
		rec.RequeueCause = buildRequeueCause(sl)
		if sl.DispatchedAt != "" {
			if t, err2 := time.Parse(time.RFC3339, sl.DispatchedAt); err2 == nil {
				rec.DispatchedAt = t
				rec.Elapsed = elapsed(now, t)
			}
		}
		d.AttemptHistory = append(d.AttemptHistory, rec)
		break // one slot per beadID in current run
	}

	// d.Acceptance is intentionally left empty: the bd CLI does not expose
	// acceptance criteria as a separate JSON field. The View guards on non-empty,
	// so this is safe.
	return d
}

// buildRequeueCause returns the most-recent requeue cause label for a slot.
func buildRequeueCause(sl *ledger.Slot) string {
	switch {
	case sl.GateRequeues > 0:
		return "gate"
	case sl.MergeRequeues > 0:
		return "merge"
	case sl.RateLimitRequeues > 0:
		return "ratelimit"
	default:
		return ""
	}
}

// elapsed returns the duration between start and now, or 0 if start is zero.
func elapsed(now, start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	return now.Sub(start)
}
