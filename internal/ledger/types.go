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

	// Frontier is the most recent wave's per-candidate dispatch verdict — every
	// ready bead the scheduler considered and why it was or was not dispatched —
	// so `koryph status --frontier` can explain the frontier instead of an
	// operator reverse-engineering it from a truncated "deferred N beads, +26
	// more" log line (D7/D9). Overwritten each wave; absent in older ledgers.
	Frontier *FrontierSnapshot `json:"frontier,omitempty"`
}

// FrontierSnapshot is the scheduler's per-candidate verdict for one wave.
type FrontierSnapshot struct {
	At      string          `json:"at"`
	Wave    int             `json:"wave"`
	Entries []FrontierEntry `json:"entries,omitempty"`
}

// FrontierEntry is one ready bead's dispatch verdict for the wave.
type FrontierEntry struct {
	BeadID  string `json:"bead_id"`
	Title   string `json:"title,omitempty"`
	Verdict string `json:"verdict"`          // "dispatched" | "deferred" | "skipped"
	Reason  string `json:"reason,omitempty"` // why, for deferred/skipped
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

	// ModelActual is the model that ACTUALLY served the most recent attempt
	// (koryph-qf6.2), reduced from the result line's modelUsage object and
	// normalized to a tier when the id names one (raw id otherwise). Model
	// above is what dispatch REQUESTED; the two diverge when the CLI's
	// hardcoded --fallback-model downgrades a session mid-flight, and any
	// outcome dataset keyed on Model alone mis-attributes those sessions.
	// Snapshotted per attempt like DeathReason (NOT accumulated); empty when
	// the stream carried no modelUsage (crash before a result line, or a
	// ledger predating this field).
	ModelActual string `json:"model_actual,omitempty"`

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

	// ProxyID is the proxy identity this slot dispatched through
	// (koryph-3l1.1, registry.AgentProxy.ID(): "<base_url>" or
	// "<base_url>#<pin>"), stamped at dispatch time; empty means direct — EITHER
	// no agent_proxy was configured for the project at all, OR one was
	// configured and this slot was assigned to the holdout arm
	// (registry.AgentProxy.ArmFor, koryph-3l1.3, design §3 L6): the holdout
	// arm deliberately reuses the exact same "" value as "no proxy" because it
	// IS a direct dispatch — that is what makes it a valid control population
	// for the estimator's calibKey segmentation ("tier:size@proxyID") and for
	// quota.RecordForProxy/EstimateItemForRuntimeProxy, both of which are
	// wired to pass this value starting with koryph-3l1.3. See
	// ProxyConfigured below for how a reader distinguishes "no experiment
	// running" from "this bead landed in the holdout arm of one."
	ProxyID string `json:"proxy_id,omitempty"`

	// ProxyConfigured records whether the project had a non-nil agent_proxy
	// configured at THIS slot's dispatch time (koryph-3l1.3, design §3 L6),
	// independent of which arm ProxyID above says the slot landed in. A
	// two-arm experiment report needs this to tell "no proxy was ever
	// configured for this project" (ProxyConfigured==false, ProxyID=="") apart
	// from "a proxy was configured and this bead was the holdout arm's
	// direct-dispatch control" (ProxyConfigured==true, ProxyID==="") — both
	// dispatch identically (no ANTHROPIC_BASE_URL override), but only the
	// latter belongs in the standing-canary comparison; the former predates
	// (or postdates) any experiment and would silently inflate the holdout
	// arm's bead count if not excluded. Additive: a Slot decoded from a
	// ledger that predates this field unmarshals it to false, which correctly
	// excludes every pre-koryph-3l1.3 slot from the two-arm report (no
	// experiment could have been running before this field existed).
	ProxyConfigured bool `json:"proxy_configured,omitempty"`

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

	// InputTokens/OutputTokens/CacheReadTokens/CacheCreationTokens are the
	// per-slot token composition (koryph-77r.1, design
	// docs/designs/2026-07-token-economy.md §3 L1), parsed off the stream-json
	// result line's usage block (or, when absent, the session-transcript
	// fallback — see internal/engine's completeSlot). They ACCUMULATE across
	// requeues exactly like CostUSD, so total token spend per bead is never
	// lost when a slot is replaced on requeue (see requeueSlot/
	// requeueRateLimited threading accumulatedCostUSD). Additive: a Slot
	// decoded from a ledger that predates these fields unmarshals them to 0.
	InputTokens         int64 `json:"input_tokens,omitempty"`
	OutputTokens        int64 `json:"output_tokens,omitempty"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`

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

	// LastActivityAt is the wall-clock instant the slot last showed real work —
	// the freshest of stream growth, the agent heartbeat, cohort CPU, and
	// commits — re-derived from ground truth on every poll tick (see
	// slotActivityAt). Distinct from UpdatedAt, which only records that the
	// engine polled: an operator (and the stuck check) can tell "we looked" from
	// "it did something." RFC3339 UTC; additive, absent in old ledgers.
	LastActivityAt string `json:"last_activity_at,omitempty"`

	// FinishedAt is the wall-clock instant this slot's agent process stopped —
	// stamped when the slot goes terminal (completeSlot), independent of
	// MergedAt (which is set only on a successful merge). With DispatchedAt it
	// bounds the slot's wall-clock duration for the efficiency dashboard. RFC3339
	// UTC; empty while the slot is still live. Additive: absent in old ledgers.
	FinishedAt string `json:"finished_at,omitempty"`

	// Resource-usage aggregates for this slot's agent process cohort (the Setsid
	// session rooted at PID, sampled by internal/resmon on the engine poll tick —
	// koryph process-metrics). They let the cockpit report per-bead memory, CPU,
	// and I/O so orchestration can be calibrated against real consumption. All
	// are best-effort: zero when no sample has landed yet, when the run predates
	// this field, or when the platform can't supply a metric (macOS has no
	// cgo-free per-process disk I/O, so IORead/WriteMB stay 0 there). They
	// reflect the whole process tree/group, not just PID, and are overwritten
	// (not accumulated) each tick from the sampler's in-memory running Usage.
	PeakRSSMB       int     `json:"peak_rss_mb,omitempty"`      // max resident memory observed
	AvgRSSMB        int     `json:"avg_rss_mb,omitempty"`       // mean resident memory across samples
	CPUSeconds      float64 `json:"cpu_seconds,omitempty"`      // cumulative CPU time (user+system)
	IOReadMB        float64 `json:"io_read_mb,omitempty"`       // cumulative disk bytes read (Linux only)
	IOWriteMB       float64 `json:"io_write_mb,omitempty"`      // cumulative disk bytes written (Linux only)
	ResourceSamples int     `json:"resource_samples,omitempty"` // number of resmon readings folded in

	// RateLimitRequeues counts requeues spent on a classified rate-limit/
	// overload death (koryph-2im.4, docs/designs/2026-07-scheduler-throughput.md
	// L5) — bounded independently of Attempts, since the failure is
	// environmental (the API throttled us), not the bead's fault. Additive: a
	// Slot decoded from a ledger that predates this field unmarshals it to
	// zero, which behaves exactly like "none spent yet."
	RateLimitRequeues int `json:"rate_limit_requeues,omitempty"`

	// BudgetKillRequeues counts warm-resume requeues spent on a classified
	// budget-kill death (koryph-77r.10, design
	// docs/designs/2026-07-token-economy.md recovery-economics follow-up):
	// unlike RateLimitRequeues, a budget-kill is bead-specific, not
	// environmental, so this DOES count toward Attempts too (Attempts
	// increments normally on a budget-killed requeue) — this counter only
	// bounds the separate, much tighter warm-resume-then-park budget (see
	// engine's budgetKillRequeueBudget). Additive: a Slot decoded from a
	// ledger that predates this field unmarshals it to zero.
	BudgetKillRequeues int `json:"budget_kill_requeues,omitempty"`

	// TurnExhaustedRequeues counts FRESH-session requeues spent on a classified
	// turn-ceiling death (koryph-840): the engine interrupted the agent for
	// running past Config.PerAgentMaxTurns turns and re-dispatched it with a
	// brand-new session (no --resume) so it stops re-reading its accreted
	// tool-result history every turn. Like BudgetKillRequeues this is
	// bead-specific (not environmental), so it DOES count toward Attempts too;
	// this counter only bounds the separate, tighter fresh-restart-then-park
	// budget (see engine's turnExhaustedRequeueBudget). Additive: a Slot
	// decoded from a ledger that predates this field unmarshals it to zero.
	TurnExhaustedRequeues int `json:"turn_exhausted_requeues,omitempty"`

	// DeathReason records completeSlot's classification of this slot's most
	// recent attempt death when it is not an ordinary crash/no-commit exit
	// (koryph-77r.10) — e.g. "budget-killed". Snapshotted per attempt (like
	// Note), NOT accumulated across requeues: a fresh dispatch's Slot starts
	// with DeathReason == "" until its own attempt dies. Additive: a Slot
	// decoded from a ledger that predates this field unmarshals it to "".
	DeathReason string `json:"death_reason,omitempty"`

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

	// Resources is the RESOLVED external resource kinds this slot's agent
	// declared (koryph-4ql.3, docs/designs/2026-07-resource-governor.md L2/L3),
	// frozen at dispatch exactly like Footprint. The engine resolves the bead's
	// `res:*` labels once via sched.ResourcesFor and persists the resulting
	// tokens here (not the labels), so a relabel mid-run cannot re-price a live
	// slot (I8). runner.activeResources prefers this persisted value over
	// recomputing from labels — and here persistence is LOAD-BEARING, not merely
	// a fast path: unlike Footprint (whose terminal fallback is the conservative
	// domain:unknown), the resource fallback degrades to the maximally-PERMISSIVE
	// empty set (L1's inverted default — undeclared means "agent + worktree
	// only"), so a slot whose bead can no longer be recovered contributes no
	// holdings. Threaded through every requeue via engine.dispatchReq.resources
	// and re-attached to the govern lease by holdGlobalSlot. Additive: a Slot
	// decoded from a ledger that predates this field unmarshals it to nil, which
	// is resource-free — exactly today's behavior (I9).
	Resources []string `json:"resources,omitempty"`

	// BeadLabels, SizeClass, and IssueType are the bead's similarity features
	// frozen at FIRST dispatch (koryph-qf6.3), exactly like Footprint above: a
	// relabel mid-run must not retroactively change what a live slot is
	// understood to look like, and the learner that joins outcomes to features
	// (koryph-qf6.6) needs the features the bead was ROUTED with, not whatever
	// it carries later. BeadLabels is the raw label set (area:*, model:*, …),
	// SizeClass is quota.SizeOf's S/M/L description bucket, IssueType is the
	// bd issue type. Threaded through every requeue via
	// engine.dispatchReq.features (see featuresFromSlot). Additive: a Slot
	// decoded from a ledger that predates these fields unmarshals them to
	// zero values, which readers must treat as "features unknown".
	BeadLabels []string `json:"bead_labels,omitempty"`
	SizeClass  string   `json:"size_class,omitempty"`
	IssueType  string   `json:"issue_type,omitempty"`

	// MemReserveMB is this slot's resolved memory reservation in MB (koryph-4ql.3,
	// L5): Σ mem_mb over its declared kinds (machine ledger override → project
	// vocabulary), frozen at dispatch alongside Resources per I8. Reserved from
	// available memory while the lease ramps so admission sees declared future
	// demand rather than only the current reading. 0 = undeclared (degrades to
	// the koryph-930 floor, today's behavior). Additive: a Slot decoded from a
	// ledger that predates this field unmarshals it to zero.
	MemReserveMB int `json:"mem_reserve_mb,omitempty"`
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
	SchemaVersion   int    `json:"schema_version"`
	ProjectID       string `json:"project_id"`
	BeadID          string `json:"bead_id"`
	EpicID          string `json:"epic_id,omitempty"`
	AccountProfile  string `json:"account_profile"`
	ClaudeConfigDir string `json:"claude_config_dir,omitempty"`
	SessionID       string `json:"session_id"`
	SessionName     string `json:"session_name,omitempty"`
	Model           string `json:"model"`
	ModelWhy        string `json:"model_rationale,omitempty"`
	// ModelActual mirrors Slot.ModelActual (koryph-qf6.2) — see its doc.
	// Refreshed by checkpointSlot at terminal transitions; empty on a
	// manifest that predates the field or an attempt with no result line.
	ModelActual    string    `json:"model_actual,omitempty"`
	WorktreePath   string    `json:"worktree_path"`
	Branch         string    `json:"branch"`
	BaseCommit     string    `json:"base_commit"`
	HeadCommit     string    `json:"head_commit,omitempty"`
	Attempt        int       `json:"attempt"`
	ExecutionState string    `json:"execution_state"`
	LeaseOwner     string    `json:"lease_owner,omitempty"`
	LeaseExpiresAt string    `json:"lease_expires_at,omitempty"`
	Plan           PlanState `json:"structured_plan"`
	ChangedFiles   []string  `json:"changed_files,omitempty"`
	PatchFiles     []string  `json:"patch_files,omitempty"`
	WIPCommit      string    `json:"optional_wip_commit,omitempty"`
	CommandsRun    []string  `json:"commands_run,omitempty"`
	TestsRun       []string  `json:"tests_run,omitempty"`
	LatestTest     string    `json:"latest_test_result,omitempty"`
	ReviewStatus   string    `json:"review_status,omitempty"`
	OpenQuestions  []string  `json:"open_questions,omitempty"`
	NextAction     string    `json:"next_action,omitempty"`
	QuotaSnapshot  any       `json:"quota_snapshot,omitempty"`
	BatchAllowed   bool      `json:"batch_mode_allowed"`
	RecoveryConf   string    `json:"recovery_confidence,omitempty"`
	RecoveryTier   int       `json:"recovery_policy_tier"`
	MergePolicy    string    `json:"merge_policy,omitempty"`
	AutoMerge      bool      `json:"auto_merge_allowed"`
	BillingMode    string    `json:"billing_mode"`
	// ProxyID mirrors Slot.ProxyID (koryph-3l1.1) — see its doc.
	ProxyID       string   `json:"proxy_id,omitempty"`
	BootstrapCmds []string `json:"bootstrap_commands,omitempty"`
	UpdatedAt     string   `json:"updated_at"`

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
