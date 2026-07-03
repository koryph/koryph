// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// TypeScript transcriptions of koryph's on-disk JSON schemas. These mirror the
// Go source of truth so the extension can read state files without a daemon:
//   - internal/ledger/types.go   (Run / Slot / Manifest, schema v2)
//   - internal/registry/types.go (Record, Event)
//   - internal/govern/types.go   (Config, Lease, Demand)
//   - internal/quota/types.go    (Config, Usage, Window)
//
// Single-writer discipline (Decision 3): the extension is READ-ONLY on every
// one of these files. Field names track the Go `json:"..."` tags exactly.
//
// Every top-level document carries a `schema_version`. Readers MUST route
// unknown versions through `guardSchemaVersion` so a future engine bump
// degrades to raw-JSON display instead of crashing the extension.

// ---------------------------------------------------------------------------
// Schema-version contract
// ---------------------------------------------------------------------------

/** Ledger Run/Manifest schema this build was transcribed against. */
export const LEDGER_SCHEMA_VERSION = 2;
/** Registry Record schema this build was transcribed against. */
export const REGISTRY_SCHEMA_VERSION = 1;
/** Quota Config schema this build was transcribed against (quota.ConfigSchemaVersion). */
export const QUOTA_CONFIG_SCHEMA_VERSION = 1;

/**
 * A parse result that either yields a typed value or preserves the raw JSON
 * for graceful degradation. `known` is false when the document's
 * `schema_version` is newer than this build understands (or, for records that
 * predate versioning, when the value is absent and cannot be assumed).
 */
export interface Parsed<T> {
  /** True when `value` was produced by a version this build understands. */
  known: boolean;
  /** The declared schema_version (0 when absent). */
  schemaVersion: number;
  /** The typed projection. Present even when `known` is false (best-effort). */
  value: T;
  /** The raw parsed JSON, always retained for raw-display fallback. */
  raw: unknown;
  /** Human-readable note when degraded (undefined when `known`). */
  degradedReason?: string;
}

/**
 * Guard a parsed document against its expected schema version.
 *
 * - Equal version           → known.
 * - Older, non-zero version  → known (fields are additive; older docs load).
 * - Absent (0) version       → known only when `assumeUnversioned` is set
 *                              (some records predate versioning and backfill).
 * - Newer version            → degraded: caller should show raw JSON.
 */
export function guardSchemaVersion<T>(
  raw: unknown,
  value: T,
  expected: number,
  opts: { assumeUnversioned?: boolean } = {},
): Parsed<T> {
  const declared = readSchemaVersion(raw);
  if (declared === 0) {
    if (opts.assumeUnversioned) {
      return { known: true, schemaVersion: 0, value, raw };
    }
    return {
      known: false,
      schemaVersion: 0,
      value,
      raw,
      degradedReason: 'missing schema_version',
    };
  }
  if (declared > expected) {
    return {
      known: false,
      schemaVersion: declared,
      value,
      raw,
      degradedReason: `schema_version ${declared} newer than supported ${expected}`,
    };
  }
  return { known: true, schemaVersion: declared, value, raw };
}

/** Extract a numeric `schema_version` from an arbitrary JSON value (0 if absent). */
export function readSchemaVersion(raw: unknown): number {
  if (raw && typeof raw === 'object' && 'schema_version' in raw) {
    const v = (raw as Record<string, unknown>).schema_version;
    if (typeof v === 'number' && Number.isFinite(v)) {
      return v;
    }
  }
  return 0;
}

// ---------------------------------------------------------------------------
// ledger — internal/ledger/types.go
// ---------------------------------------------------------------------------

/** Slot statuses (superset of the bash engine's, wire-compatible). */
export const SlotStatus = {
  Queued: 'queued',
  Dispatching: 'dispatching',
  Running: 'running',
  Stuck: 'stuck',
  Review: 'review',
  MergePending: 'merge-pending',
  Merged: 'merged',
  PROpened: 'pr-opened',
  Done: 'done',
  Failed: 'failed',
  Conflict: 'conflict',
  Blocked: 'blocked',
} as const;
export type SlotStatus = (typeof SlotStatus)[keyof typeof SlotStatus];

/** Run statuses. */
export const RunStatus = {
  Running: 'running',
  PausedQuota: 'paused-quota',
  Drained: 'drained',
  Done: 'done',
  Aborted: 'aborted',
} as const;
export type RunStatus = (typeof RunStatus)[keyof typeof RunStatus];

/** Terminal slot statuses (mirrors ledger.Terminal). */
const TERMINAL_STATUSES = new Set<string>([
  SlotStatus.Merged,
  SlotStatus.PROpened,
  SlotStatus.Done,
  SlotStatus.Failed,
  SlotStatus.Conflict,
  SlotStatus.Blocked,
  SlotStatus.MergePending,
]);

/** Reports whether a slot status is terminal (mirrors ledger.Terminal). */
export function isTerminal(status: string): boolean {
  return TERMINAL_STATUSES.has(status);
}

/** ledger.Slot — one dispatched work item within a run. */
export interface Slot {
  phase_id: string;
  bead_id?: string;
  epic_id?: string;
  branch: string;
  worktree: string;

  session_id: string;
  session_name?: string;
  agent: string;
  model: string;
  model_rationale?: string;
  effort?: string;

  account_profile: string;
  claude_config_dir?: string;
  verified_identity?: string;
  verified_at?: string;
  billing_mode: string;

  pid?: number;
  stream?: string;
  status_path?: string;
  log_path?: string;
  status: string;
  attempts: number;
  commits: number;
  last_commit?: string;
  resume_sha?: string;
  cost_usd: number;

  review_iters?: number;
  dispatched_at?: string;
  merged_at?: string;
  updated_at?: string;
  note?: string;
}

/** ledger.Run — one koryph run over one project. */
export interface Run {
  schema_version: number;
  run_id: string;
  project_id: string;
  engine_version: string;
  started_at: string;
  updated_at: string;
  status: string;
  wave: number;
  source: string; // bd | markdown
  slots: Record<string, Slot>;
}

/** ledger.PlanState — structured-plan progress inside a manifest. */
export interface PlanState {
  current_step?: string;
  completed_steps?: string[];
  invalidated_steps?: string[];
}

/** ledger.Manifest — the per-slot checkpoint (schema v2). */
export interface Manifest {
  schema_version: number;
  project_id: string;
  bead_id: string;
  epic_id?: string;
  account_profile: string;
  claude_config_dir?: string;
  session_id: string;
  session_name?: string;
  model: string;
  model_rationale?: string;
  worktree_path: string;
  branch: string;
  base_commit: string;
  head_commit?: string;
  attempt: number;
  execution_state: string;
  lease_owner?: string;
  lease_expires_at?: string;
  structured_plan: PlanState;
  changed_files?: string[];
  patch_files?: string[];
  optional_wip_commit?: string;
  commands_run?: string[];
  tests_run?: string[];
  latest_test_result?: string;
  review_status?: string;
  open_questions?: string[];
  next_action?: string;
  quota_snapshot?: unknown;
  prompt_cache_policy?: string;
  batch_mode_allowed: boolean;
  recovery_confidence?: string;
  recovery_policy_tier: number;
  merge_policy?: string;
  auto_merge_allowed: boolean;
  billing_mode: string;
  bootstrap_commands?: string[];
  updated_at: string;
}

// ---------------------------------------------------------------------------
// registry — internal/registry/types.go
// ---------------------------------------------------------------------------

/** registry.Record — one managed project. (Named ProjectRecord to avoid
 * shadowing TypeScript's built-in `Record<K, V>` utility type.) */
export interface ProjectRecord {
  schema_version: number;
  project_id: string;
  name: string;
  root: string;
  remote?: string;
  default_branch: string;

  beads_root?: string;
  beads_status: string;
  beads_hooks_status: string;
  dolt_mode?: string;
  dolt_remote_ref?: string;

  koryph_engine_version?: string;
  migration_status: string;

  account_profile: string;
  claude_config_dir?: string;
  expected_identity: string;
  direnv_expected?: string;

  allowed_models: string[];
  planner_model: string;
  impl_model: string;
  recovery_model_policy: string;

  batch_policy: string;
  api_fallback: string;
  api_key_env_var?: string;
  prompt_cache_policy: string;
  billing_guard?: string;

  worktree_root?: string;
  active_sessions?: string[];

  quota_profile?: string;
  visibility_sync: string;

  env_passthrough?: string[];

  created_at: string;
  updated_at: string;
}

/** registry.Event — one append-only audit entry (audit.jsonl). */
export interface Event {
  at: string;
  kind: string; // register|update|set-account|dispatch|validate|onboard|quota|merge
  project_id?: string;
  actor?: string;
  detail?: unknown;
}

// ---------------------------------------------------------------------------
// govern — internal/govern/types.go
// ---------------------------------------------------------------------------

/** govern.DefaultMaxGlobalAgents — cap used when governor.json is absent. */
export const DEFAULT_MAX_GLOBAL_AGENTS = 4;

/** govern.Config — machine-wide concurrency governor config (governor.json). */
export interface GovernorConfig {
  max_global_agents: number;
}

/** govern.Lease — one running agent holding a global slot. */
export interface Lease {
  project: string;
  bead: string;
  pid: number; // agent process id
  engine_pid: number; // owning koryph run pid
  model?: string;
  acquired_at: string; // RFC3339
}

/** govern.Demand — a project's "I have ready work" heartbeat. */
export interface Demand {
  project: string;
  engine_pid: number;
  updated_at: string; // RFC3339
}

// ---------------------------------------------------------------------------
// quota — internal/quota/types.go
// ---------------------------------------------------------------------------

/** quota.Level — the governor verdict. */
export const QuotaLevel = {
  OK: 'ok',
  Warn: 'warn', // >= 0.80
  Drain: 'drain', // >= 0.90
  Stop: 'stop', // >= 0.95
} as const;
export type QuotaLevel = (typeof QuotaLevel)[keyof typeof QuotaLevel];

/** quota thresholds (fractions of the calibrated ceiling). */
export const WARN_FRACTION = 0.8;
export const DRAIN_FRACTION = 0.9;
export const STOP_FRACTION = 0.95;

/** quota.Window — one measured usage window. */
export interface Window {
  hours: number;
  spent_usd: number;
  ceiling_usd: number;
  source: string; // ccusage | jsonl-scan | unavailable
  approx: boolean;
}

/** quota.Usage — a per-account snapshot. */
export interface Usage {
  account: string;
  at: string;
  window_5h: Window;
  weekly: Window;
}

/** quota.Config — per-account governor config + calibration state. */
export interface QuotaConfig {
  schema_version?: number;
  account: string;
  window_ceiling_usd: number;
  weekly_ceiling_usd: number;
  plan_tier?: string;
  per_agent_max_usd: number;
  per_tier_usd: Record<string, number>;
  size_multiplier: Record<string, number>;
  safety_margin: number;
  calibration?: Record<string, number>;
}

/**
 * quota.Window.Fraction — spent/ceiling, failing closed (1.0) when
 * unmeasurable. Mirrors the Go method so the extension colors status bars the
 * same way the engine gates dispatch.
 */
export function windowFraction(w: Window | undefined): number {
  if (!w || w.ceiling_usd <= 0 || w.source === 'unavailable') {
    return 1.0;
  }
  return w.spent_usd / w.ceiling_usd;
}

/**
 * quota.State — level from the max of window & weekly fractions. Mirrors the
 * governor's ok/warn/drain/stop banding.
 */
export function quotaLevel(u: Usage | undefined): QuotaLevel {
  const frac = Math.max(windowFraction(u?.window_5h), windowFraction(u?.weekly));
  if (frac >= STOP_FRACTION) {
    return QuotaLevel.Stop;
  }
  if (frac >= DRAIN_FRACTION) {
    return QuotaLevel.Drain;
  }
  if (frac >= WARN_FRACTION) {
    return QuotaLevel.Warn;
  }
  return QuotaLevel.OK;
}

// ---------------------------------------------------------------------------
// board — cmd/koryph/run.go boardEntry (`koryph board --json`)
// ---------------------------------------------------------------------------

/** boardEntry — one project's line on the board. */
export interface BoardEntry {
  project_id: string;
  migration_status: string;
  account: string;
  run_id?: string;
  run_status?: string;
  slots?: Record<string, number>;
  live_pids: number;
}
