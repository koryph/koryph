// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package metrics rolls up burn and reliability baselines across managed
// projects from run ledgers (+ registry + quota snapshots). Read-only.
//
// Implementation contract (rollup.go):
//   - Collect(store *registry.Store, projectID string) (*Report, error) —
//     walk each managed project's .plan-logs/koryph/<run>/ledger.json
//     (projectID "" = all); aggregate per project and per model: slots,
//     merged, failed, blocked, attempts>1 (retries), total + mean cost,
//     review bounces; count stale agent worktrees (worktree.List where
//     branch has prefix "agent/") and orphaned agent/* branches without
//     worktrees; note runs never finalized (status running with no live
//     process — informational only, no PID probing needed here).
//   - Render(r *Report, w io.Writer) — aligned text table + a trailing
//     one-line JSON for machine consumption.
package metrics

// ModelStat aggregates slots for one model tier.
type ModelStat struct {
	Slots   int     `json:"slots"`
	CostUSD float64 `json:"cost_usd"`
	MeanUSD float64 `json:"mean_usd"`
	Merged  int     `json:"merged"`
	Failed  int     `json:"failed"`
	Retries int     `json:"retries"`
}

// ProjectStat aggregates one project.
type ProjectStat struct {
	ProjectID       string               `json:"project_id"`
	Account         string               `json:"account"`
	Runs            int                  `json:"runs"`
	UnfinalizedRuns int                  `json:"unfinalized_runs"`
	Slots           int                  `json:"slots"`
	Merged          int                  `json:"merged"`
	Failed          int                  `json:"failed"`
	Blocked         int                  `json:"blocked"`
	Retries         int                  `json:"retries"`
	ReviewBounces   int                  `json:"review_bounces"`
	CostUSD         float64              `json:"cost_usd"`
	ByModel         map[string]ModelStat `json:"by_model"`
	StaleWorktrees  int                  `json:"stale_worktrees"`
	OrphanBranches  int                  `json:"orphan_branches"`
}

// Report is the rollup result.
type Report struct {
	GeneratedAt string        `json:"generated_at"`
	Projects    []ProjectStat `json:"projects"`
	TotalUSD    float64       `json:"total_usd"`
}
