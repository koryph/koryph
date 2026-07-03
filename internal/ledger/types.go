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
	RunRunning     = "running"
	RunPausedQuota = "paused-quota"
	RunDrained     = "drained"
	RunDone        = "done"
	RunAborted     = "aborted"
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

	// GateRequeues and MergeRequeues count requeues already spent on,
	// respectively, a post-rebase gate failure and a merge error — each
	// budgeted 2 (koryph-2im.6), still bounded by Attempts < MaxAttempts.
	// Additive: a Slot decoded from an old ledger that predates these fields
	// unmarshals them to zero, which behaves exactly like "none spent yet."
	GateRequeues  int `json:"gate_requeues,omitempty"`
	MergeRequeues int `json:"merge_requeues,omitempty"`

	ReviewIters  int    `json:"review_iters,omitempty"`
	DispatchedAt string `json:"dispatched_at,omitempty"`
	MergedAt     string `json:"merged_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	Note         string `json:"note,omitempty"`
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
}

// Decision is one recovery classification outcome.
type Decision struct {
	PhaseID string
	Action  string // reattach|requeue-resume|requeue-fresh|blocked|skip
	Reason  string
}

// MaxAttempts is the re-dispatch budget per slot.
const MaxAttempts = 3
