// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package ledger owns the per-run ledger and the per-slot checkpoint
// manifest (schema v2). Both are repo files under
// <repo>/.plan-logs/koryph/<run-id>/ — checkpoints live with the work.
//
// Implementation contract (store.go, classify.go):
//   - Store.NewRun / LoadRun / LoadLatest (via the `latest` symlink).
//   - Every mutation is read-modify-write through fsx.WriteJSONAtomic and
//     refreshes UpdatedAt. Only the koryph process writes the ledger.
//   - Store.SetSlot / UpdateSlot(runID, phaseID, mutate func(*Slot)).
//   - Manifest persisted per slot at <run>/<phase>/manifest.json.
//   - Classify(run, probe) []Decision — recovery classification:
//     terminal statuses are skipped;
//     PID alive → reattach;
//     PID dead + commits>0 → requeue with ResumeSHA;
//     PID dead + no commits → requeue fresh;
//     attempts >= MaxAttempts → blocked.
//   - FinalizeRun marks a run terminal (fixes the stale-"running" bug).
package ledger

import "github.com/koryph/koryph/internal/sched"

// Slot statuses (superset of the bash engine's, kept wire-compatible).
const (
	SlotQueued       = "queued"
	SlotDispatching  = "dispatching"
	SlotRunning      = "running"
	SlotStuck        = "stuck"
	SlotReview       = "review"
	SlotMergePending = "merge-pending"
	SlotMerged       = "merged"
	SlotPROpened     = "pr-opened"
	SlotDone         = "done"
	SlotFailed       = "failed"
	SlotConflict     = "conflict"
	SlotBlocked      = "blocked"
)

// Run statuses.
const (
	RunRunning       = "running"
	RunPausedQuota   = "paused-quota"
	RunHardStopQuota = "hard-stop-quota" // hard stop: agents interrupted, worktrees preserved
	RunDrained       = "drained"
	RunDone          = "done"
	RunAborted       = "aborted"
)

// Terminal reports whether a slot status is terminal. pr-opened is terminal
// for the run: the agent is done and the slot freed; the branch parks (worktree
// kept) until a separate landing step fast-forwards the PR.
func Terminal(status string) bool {
	switch status {
	case SlotMerged, SlotPROpened, SlotDone, SlotFailed, SlotConflict, SlotBlocked, SlotMergePending:
		return true
	}
	return false
}

// Run is one koryph run over one project.
type Run struct {
	SchemaVersion int              `json:"schema_version"`
	RunID         string           `json:"run_id"`
	ProjectID     string           `json:"project_id"`
	EngineVersion string           `json:"engine_version"`
	StartedAt     string           `json:"started_at"`
	UpdatedAt     string           `json:"updated_at"`
	Status        string           `json:"status"`
	Wave          int              `json:"wave"`
	Source        string           `json:"source"` // bd|markdown
	Slots         map[string]*Slot `json:"slots"`

	// PatrolEvents is the chronological history of periodic in-loop health
	// patrol runs for this run. Appended by the engine's health patrol
	// (koryph-gus); absent in older ledgers.
	PatrolEvents []PatrolEvent `json:"patrol_events,omitempty"`
}

// Slot is one dispatched work item within a run.
type Slot struct {
	PhaseID  string `json:"phase_id"` // bead id (bd) or phase slug (markdown)
	BeadID   string `json:"bead_id,omitempty"`
	EpicID   string `json:"epic_id,omitempty"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`

	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name,omitempty"`
	Agent       string `json:"agent"`
	Model       string `json:"model"`
	ModelWhy    string `json:"model_rationale,omitempty"`
	Effort      string `json:"effort,omitempty"`

	// Runtime is the runtime (internal/runtime.Runtime.Name()) this slot
	// dispatched under (koryph-v8u.3): "claude" today, always — the engine
	// blocks any other resolved runtime rather than dispatching it. Additive:
	// a Slot decoded from a ledger that predates this field unmarshals it to
	// "", which every reader should treat as "claude" (the only runtime that
	// ever existed before this field was added).
	Runtime string `json:"runtime,omitempty"`

	AccountProfile   string `json:"account_profile"`
	ClaudeConfigDir  string `json:"claude_config_dir,omitempty"`
	VerifiedIdentity string `json:"verified_identity,omitempty"`
	VerifiedAt       string `json:"verified_at,omitempty"`
	BillingMode      string `json:"billing_mode"`

	PID        int     `json:"pid,omitempty"`
	Stream     string  `json:"stream,omitempty"`
	StatusPath string  `json:"status_path,omitempty"`
	LogPath    string  `json:"log_path,omitempty"`
	Status     string  `json:"status"`
	Attempts   int     `json:"attempts"`
	Commits    int     `json:"commits"`
	LastCommit string  `json:"last_commit,omitempty"`
	ResumeSHA  string  `json:"resume_sha,omitempty"`
	CostUSD    float64 `json:"cost_usd"`

	// EstimateUSD is the dispatch-time cost estimate stamped at the moment the
	// slot was first created (koryph-6bl). Additive: a Slot decoded from a
	// ledger that predates this field unmarshals it to 0 (= unknown / skip in
	// error statistics). For requeued slots this holds the estimate for the
	// most-recent attempt; it is NOT accumulated across attempts (unlike
	// CostUSD, which accumulates).
	EstimateUSD float64 `json:"estimate_usd,omitempty"`

	// GateRequeues and MergeRequeues count requeues already spent on,
	// respectively, a post-rebase gate failure and a merge error — each
	// budgeted 2 (koryph-2im.6), still bounded by Attempts < MaxAttempts.
	// Additive: a Slot decoded from an old ledger that predates these fields
	// unmarshals them to zero, which behaves exactly like "none spent yet."
	GateRequeues  int `json:"gate_requeues,omitempty"`
	MergeRequeues int `json:"merge_requeues,omitempty"`
	// ConflictRequeues counts rebase-conflict requeues (koryph-3as): a merge
	// conflict re-dispatches the agent to resolve CONFLICT.md in its own
	// worktree instead of stranding the bead in a terminal conflict slot.
	ConflictRequeues int `json:"conflict_requeues,omitempty"`

	ReviewIters  int    `json:"review_iters,omitempty"`
	DispatchedAt string `json:"dispatched_at,omitempty"`
	MergedAt     string `json:"merged_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	Note         string `json:"note,omitempty"`

	// RateLimitRequeues counts requeues spent on a classified rate-limit/
	// overload death (koryph-2im.4, docs/designs/2026-07-scheduler-throughput.md
	// L5) — bounded independently of Attempts, since the failure is
	// environmental (the API throttled us), not the bead's fault. Additive: a
	// Slot decoded from a ledger that predates this field unmarshals it to
	// zero, which behaves exactly like "none spent yet."
	RateLimitRequeues int `json:"rate_limit_requeues,omitempty"`

	// Footprint is the RW conflict footprint computed at dispatch time
	// (koryph-2im.3, docs/designs/2026-07-scheduler-throughput.md L2 footprint
	// persistence). runner.activeFootprints prefers this persisted value over
	// recomputing from the bead's current labels — exact in-flight gating
	// across requeues (threaded forward, see engine.dispatchReq.footprint),
	// resume adoption, and label edits mid-run (a relabel after dispatch must
	// not retroactively change what a LIVE slot is understood to conflict
	// with). Additive: a Slot decoded from a ledger that predates this field
	// unmarshals it to nil, which falls back to the pre-koryph-2im.3
	// recompute-from-labels chain exactly as before.
	Footprint *sched.Footprint `json:"footprint,omitempty"`
}

// PatrolFinding is one in-loop health check result from a periodic patrol run.
type PatrolFinding struct {
	Check   string `json:"check"`
	Level   string `json:"level"` // "ok" | "warn"
	Message string `json:"message"`
	Fixed   bool   `json:"fixed,omitempty"`
}

// PatrolEvent records one complete health patrol run in the run ledger so
// post-mortems have full health history alongside slot events.
type PatrolEvent struct {
	At       string          `json:"at"`
	Findings []PatrolFinding `json:"findings,omitempty"`
}

// PlanState tracks structured-plan progress inside a manifest.
type PlanState struct {
	CurrentStep      string   `json:"current_step,omitempty"`
	CompletedSteps   []string `json:"completed_steps,omitempty"`
	InvalidatedSteps []string `json:"invalidated_steps,omitempty"`
}

// Manifest is the per-slot checkpoint (schema v2). It carries everything a
// resumed koryph or a next-window recovery needs.
type Manifest struct {
	SchemaVersion   int       `json:"schema_version"`
	ProjectID       string    `json:"project_id"`
	BeadID          string    `json:"bead_id"`
	EpicID          string    `json:"epic_id,omitempty"`
	AccountProfile  string    `json:"account_profile"`
	ClaudeConfigDir string    `json:"claude_config_dir,omitempty"`
	SessionID       string    `json:"session_id"`
	SessionName     string    `json:"session_name,omitempty"`
	Model           string    `json:"model"`
	ModelWhy        string    `json:"model_rationale,omitempty"`
	WorktreePath    string    `json:"worktree_path"`
	Branch          string    `json:"branch"`
	BaseCommit      string    `json:"base_commit"`
	HeadCommit      string    `json:"head_commit,omitempty"`
	Attempt         int       `json:"attempt"`
	ExecutionState  string    `json:"execution_state"`
	LeaseOwner      string    `json:"lease_owner,omitempty"`
	LeaseExpiresAt  string    `json:"lease_expires_at,omitempty"`
	Plan            PlanState `json:"structured_plan"`
	ChangedFiles    []string  `json:"changed_files,omitempty"`
	PatchFiles      []string  `json:"patch_files,omitempty"`
	WIPCommit       string    `json:"optional_wip_commit,omitempty"`
	CommandsRun     []string  `json:"commands_run,omitempty"`
	TestsRun        []string  `json:"tests_run,omitempty"`
	LatestTest      string    `json:"latest_test_result,omitempty"`
	ReviewStatus    string    `json:"review_status,omitempty"`
	OpenQuestions   []string  `json:"open_questions,omitempty"`
	NextAction      string    `json:"next_action,omitempty"`
	QuotaSnapshot   any       `json:"quota_snapshot,omitempty"`
	PromptCache     string    `json:"prompt_cache_policy,omitempty"`
	BatchAllowed    bool      `json:"batch_mode_allowed"`
	RecoveryConf    string    `json:"recovery_confidence,omitempty"`
	RecoveryTier    int       `json:"recovery_policy_tier"`
	MergePolicy     string    `json:"merge_policy,omitempty"`
	AutoMerge       bool      `json:"auto_merge_allowed"`
	BillingMode     string    `json:"billing_mode"`
	BootstrapCmds   []string  `json:"bootstrap_commands,omitempty"`
	UpdatedAt       string    `json:"updated_at"`

	// Runtime is the runtime this slot dispatched under (koryph-v8u.3),
	// mirroring Slot.Runtime — see its doc for the additive/"" == "claude"
	// contract.
	Runtime string `json:"runtime,omitempty"`
}

// Decision is one recovery classification outcome.
type Decision struct {
	PhaseID string
	Action  string // reattach|requeue-resume|requeue-fresh|blocked|skip
	Reason  string
}

// MaxAttempts is the re-dispatch budget per slot.
const MaxAttempts = 3
