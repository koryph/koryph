<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Projects and Accounts

Every repository koryph manages is described by two artefacts: a
**project config** (`koryph.project.json`) checked into the repo root, and a
**registry record** kept in `~/.koryph/registry.d/<project-id>.json`.
The registry record also binds the **account** that dispatched agents run under.

---

## koryph.project.json

Created by `koryph project add` or `koryph adopt` — `koryph onboard` is a
separate, read-only inspection tool and never writes this file — validated
by `koryph validate`.

| Field | Type | Default | Purpose |
|---|---|---|---|
| `schema_version` | int | 1 | Format version. Always `1`. |
| `project_id` | string | — | Slug (`[a-z0-9-]+`); must match the registry record. |
| `work_source` | string | `"bd"` | `"bd"` (beads ready-graph, preferred) or `"markdown"` (legacy plans dir). |
| `plans_dir` | string | — | Required when `work_source` is `"markdown"`. |
| `gate` | array | — | Shell commands run inside the worktree after rebase and before merge. At least one required. |
| `footprint` | array | — | Conflict-domain rules: `{pattern, tokens}`. Tokens prefixed `HOT:` conflict with every worktree sharing that token. |
| `area_map` | object | — | Expands `area:<x>` bead labels to footprint tokens when no `fp:*` label is present. |
| `stages` | object | — | Maps pipeline stage names to canonical `agents/` persona names. |
| `tiers` | object | — | Maps model tier names to persona names for tier-driven dispatch. |
| `pipeline` | array | — | Ordered post-implement stages (`{name, persona?, model?, effort?, prompt?, optional?}`) run in the worktree before review/merge. See below. |
| `bootstrap` | array | — | Commands run in a fresh or re-attached worktree before the agent starts (e.g. `pnpm install --frozen-lockfile`). |
| `protected_paths` | array | — | Paths that, if touched by a worktree diff, block merge. Extends the engine default list. |
| `validation` | array | — | Extra commands run by `koryph validate` beyond engine checks. |
| `engine_version` | string | any | Minimum engine required: `"0.2+"`, `">=0.2.0"`, or a bare version. |
| `commit_style` | string | `"conventional"` | `"conventional"`, `"custom"`, or `"none"`. `conventional` (also when empty) is **mechanically enforced** at merge/PR time; `custom` defers to `commit_template`; `none` opts out of enforcement. |
| `commit_template` | string | — | Required when `commit_style` is `"custom"`. |
| `merge_policy` | string | — | Default merge behaviour: `"manual"`, `"auto"`, or `"pr"`. Required. |
| `risk_tier_default` | int | 2 | Recovery tier (0–3) for beads without an `rt:*` label. |
| `max_concurrent_slots` | int | 4 | Wave width cap for this project (koryph-4rk6.4 laptop-safe default; was 3). |
| `dispatch_stagger_seconds` | int | 8 | Seconds between agent launches within a wave. |
| `poll_seconds` | int | 10 | Poll-tick interval (seconds) for reading slot `status.json` heartbeats. 0 uses the engine default (10 s). Overridden by `KORYPH_POLL_SEC` or `Options.PollSec` at the programmatic call site. |
| `dispatch_mode` | string | `"wave"` | `"wave"` or `"rolling"`. Rolling continuously refills a slot as it frees up instead of waiting for the whole batch; see [Running Waves](running-waves.md#dispatch-mode-wave-vs-rolling). `--dispatch-mode` on `koryph run` overrides this per run. |
| `review` | object | — | Post-implementation reviewer timeout: `{timeout_seconds?}`. See [Agent timeouts](#agent-timeouts) below. |

**Conventional commits are enforced by default.** With `commit_style` unset or
`"conventional"`, the merge and PR paths validate every commit subject in
`<default>..<branch>` against the Conventional Commits grammar
(`type(scope): subject`; types `feat|fix|docs|chore|refactor|revert|test|ci|build|perf|style`)
before any merge or PR. A non-conforming subject bounces the bead back to the
implementer once — like a gate failure — and blocks it if the violation persists;
nothing non-conventional lands. Set `commit_style` to `"none"` to opt out, or
`"custom"` (with `commit_template`) to govern messages by a project template
instead.

### Agent timeouts

koryph uses **one** agent-facing wall timeout, defaulting to **1200 s (20
minutes)**, for every agent it spawns on your behalf: the post-implementation
reviewer (`--review`), each post-implement pipeline stage, and epic validation.
(Implementer agents themselves are budget-bound, not wall-bound, and are not
affected.) That single default is overridable at three levels, in strict
precedence:

**bead → project → system → built-in (1200)**

| Level | Where | How |
|---|---|---|
| **bead** | a bead label | `timeout:<seconds>` — a bare label (like `model:opus`). Applies to that bead's reviewer/stage timeouts. |
| **project** | `koryph.project.json` | `review.timeout_seconds`, a pipeline stage's `timeout_sec`, `epic_validation.timeout_seconds`. |
| **system** | `~/.koryph/config.json` | `default_timeout_seconds` — a machine-wide default for every project that sets nothing. |
| **built-in** | — | 1200 s when nothing above is set. |

There is **no upper ceiling**: any level may set a value larger than the
default when a genuinely large change needs more room. Only positive values
count — `0`/absent means "not set at this level", so the next level down wins.

The break-glass `KORYPH_REVIEW_TIMEOUT_SEC` environment variable still overrides
the **reviewer** timeout at runtime, above the whole hierarchy — the same
convention as `KORYPH_POLL_SEC` over `poll_seconds`.

A review that times out degrades, and with `--review` a degraded verdict blocks
the merge (koryph never auto-merges unreviewed work); the surfaced reason
suggests raising the timeout or splitting the change into smaller beads. A
pipeline stage that times out **after** committing degrades gracefully (its
completed work lands, a follow-up is flagged) rather than parking the bead.

```json
{ "review": { "timeout_seconds": 1800 } }
```

```jsonc
// ~/.koryph/config.json — a machine-wide default for every project
{ "default_timeout_seconds": 1800 }
```

> **Deprecated:** `review.max_timeout_seconds` (the old escalation ceiling) is
> ignored — koryph now runs a single timeout with no escalation. Existing files
> still parse; `koryph doctor` NOTEs a lingering value. Remove it and use
> `review.timeout_seconds`.

### Minimal example

```json
{
  "schema_version": 1,
  "project_id": "myproject",
  "work_source": "bd",
  "gate": ["make lint", "make test"],
  "merge_policy": "manual",
  "risk_tier_default": 2
}
```

### Editor validation (JSON Schema)

A JSON Schema for this file is published at
[`docs/schema/koryph.project.schema.json`](../schema/koryph.project.schema.json).
It is **generated from the Go `project.Config` struct** — field descriptions
come from the struct doc comments and the enums/ranges (`merge_policy`,
`merge_method`, `commit_style`, `work_source`, `risk_tier_default`, …) mirror
the loader's validation — so it cannot drift: a `go test` regenerates and
diffs it, and running `go generate ./internal/project` refreshes it after any
`Config` change.

Point your editor at it for inline completion, hover docs, and validation:

```json
{
  "$schema": "https://koryph.dev/schemas/config",
  "schema_version": 1,
  "project_id": "myproject"
}
```

or reference the committed file directly (`"$schema": "./docs/schema/koryph.project.schema.json"`).
The VS Code extension contributes it via `jsonValidation` automatically — you
get inline completion, hover docs, and error highlighting for any `koryph.project.json`
file without extra setup.

To open the config with a single command, run **Koryph: Edit Project Config**
from the VS Code command palette (`⇧⌘P` / `Ctrl+Shift+P`). The extension
locates the `koryph.project.json` in your workspace, opens it with schema
validation active, and surfaces the run-start caveat as a notification.

> **Run-start caveat:** config is read **once at run start** — edits apply on
> the **next `koryph run`**, not to the currently active engine.

> **Registry-record fields** (account identity, allowed models, billing guard)
> are stored in `~/.koryph/registry.d/<project-id>.json` and are managed
> exclusively by `koryph project` CLI commands.  Do **not** edit those files by
> hand — they are git-committed by the store and hand edits will be overwritten
> on the next `koryph project set` or `koryph project set-account` call.

### Conflict domains

`footprint` contains `{pattern, tokens}` rules. Patterns use doublestar-lite
globs (`*` within a segment, `**` across). Tokens prefixed `HOT:` cause
koryph to refuse scheduling two agents that share that token in the same
wave. `area_map` is shorthand: label a bead `area:api` and koryph expands
it to `area_map["api"]`'s token list — no explicit `fp:*` label required.

### Post-implement pipeline stages

By default a bead flows `implement → (review) → merge`. `pipeline` inserts
extra stages that run **sequentially in the same worktree**, after the
implementer's commits land and before review/merge — each a persona agent that
may add its own commits (docs, tests, changelog, …):

```json
{
  "pipeline": [
    { "name": "docs", "prompt": "Update the user-guide chapter for this change." },
    { "name": "test", "model": "opus", "optional": false }
  ]
}
```

| Stage field | Purpose |
|---|---|
| `name` | Stage id (required, unique). Known ids — `docs`, `test`, … — inherit a default persona and model tier; `implement`/`review` are reserved. |
| `persona` | Override the canonical `agents/` persona. Default: the stage's namespaced engine persona (e.g. `docs → koryph-feature-docs-author`). |
| `model` | Override the model tier (must be in the registry `allowed_models`). Default: the stage's engine default. |
| `effort` | Reasoning-effort hint. |
| `prompt` | Extra stage-specific instructions appended to the built stage prompt. |
| `optional` | `true` → a failed stage logs and continues. Default `false` → a failed stage **blocks the bead** (never auto-merges past incomplete pipeline work). |
| `timeout_sec` | Per-stage wall-clock deadline in seconds — the project level of the [agent-timeout hierarchy](#agent-timeouts). `≤ 0` (or omitted) falls through to the machine-wide `default_timeout_seconds` and then the built-in default (1200 s); a bead `timeout:<seconds>` label overrides it. Raise it for a legitimately slow stage rather than letting it time out. |

```json
{ "pipeline": [ { "name": "docs", "timeout_sec": 1200 } ] }
```

Each stage is rebased onto the current default branch immediately before it
runs, so a docs/changelog stage writes against the latest tree — this keeps
shared-file writes from colliding at merge time. If a **required** stage times
out but the bead's implementer work is already committed, the engine **degrades
rather than parks**: it lands the completed work and records a `followup:<stage>`
label plus a comment on the bead so the skipped stage is picked up later, instead
of stranding correct commits behind a slow enhancement stage.

Stages honor the same account, billing, signing, and budget guarantees as the
implementer, and their cost counts toward the quota governor. A bead bounced by
review re-runs the whole pipeline on the updated code.

---

## The registry record

`~/.koryph` is a git repository. Every mutation writes a record atomically,
appends an audit event, and produces a conventional commit, so the full change
history is `git log ~/.koryph`.

| Field | Purpose |
|---|---|
| `project_id` | Slug; matches `koryph.project.json`. |
| `name` | Display name. |
| `root` | Absolute path to the repository. Must exist and contain `.git`. |
| `remote` | Git remote URL (optional). |
| `default_branch` | Branch to merge into (typically `main`). |
| `beads_root` | Path to `.beads` (usually `<root>/.beads`). |
| `beads_status` | `none` \| `initialized` \| `hardened`. |
| `beads_hooks_status` | `none` \| `wired`. |
| `koryph_engine_version` | Engine version at last validation. |
| `migration_status` | Lifecycle gate — see below. |
| `worktree_root` | Where worktrees are created (default: `<parent>/<repo>-worktrees`). |
| `active_sessions` | Run IDs currently dispatched against this project. |
| `allowed_models` | Model tiers permitted, e.g. `["opus","sonnet","haiku"]`. |
| `planner_model` | Default planner tier (default `"opus"`). |
| `impl_model` | Default implementer tier (default `"sonnet"`). |
| `recovery_model_policy` | Always `"upgrade-opus"`; Fable is never used for recovery. |
| `batch_policy` | `"deny"` \| `"explicit"` — whether the Batch API is available. |
| `agent_mcp` | `"inherit"` (default, also when empty) \| `"strict"` — MCP loading for dispatched implementer agents. `"strict"` passes `--strict-mcp-config` so the agent loads **no** ambient MCP servers, trimming the re-read-every-turn prompt prefix; koryph implementer personas use only file/bash tools, so this is a pure context-economy win. Leave unset unless a project's agents genuinely call an MCP. See [Context economy](context-economy.md). |
| `api_fallback` | `"off"` \| `"explicit"` — whether direct API key use is allowed. |
| `api_key_env_var` | Env-var **name** holding the key (never the key value itself). |
| `prompt_cache_policy` | `"on"` (default, also when empty) \| `"off"` — whether koryph places a 1h extended-TTL prompt-cache breakpoint after the byte-identical shared prompt prefix (engine preamble + project block) on the request paths it owns. Consumed today by `koryph batch run --project <id>`, which defaults `--cache-prefix` from this field. The wave-loop `claude -p` dispatch manages its own cache TTL and is unaffected. |
| `billing_guard` | `"enforce"` (default) or `"advisory"` — whether the quota governor blocks or only warns. Automatically advisory while the account is uncalibrated. |
| `quota_profile` | Quota governor bucket (defaults to `account_profile`). |
| `visibility_sync` | `"off"` (GitHub/Linear sync is a later phase). |
| `agent_proxy` | Optional local interception-proxy config (`base_url`, `health`, `pin`, `stats`, `holdout`). Absent = direct dispatch (no `ANTHROPIC_BASE_URL` override). `base_url` is validated at load as an `http://` loopback address. See [Headroom integration](headroom-integration.md) for the full field reference, doctor's four proxy checks, and the holdout workflow. |

### Migration lifecycle

```
registered → inventoried → migrated → validated
```

Only `validated` records are eligible for dispatch. `koryph validate
<project-id>` promotes a record from `registered` → `migrated` on a green
gate pass. To reach `validated`, run a canary wave first (`koryph run --project
<project-id> --once --allow-unvalidated`), then re-run `koryph validate
<project-id>` — the record is promoted only when the latest run has at least
one merged slot and no failures.

---

## Account model

Each record carries an **account triple** that determines which Claude login
dispatched agents run under.

| Field | Meaning |
|---|---|
| `account_profile` | `"personal"`, `"work"`, or a custom label. |
| `claude_config_dir` | Path to a dedicated Claude config directory. Empty string = personal (do **not** set explicitly to `~/.claude`). |
| `expected_identity` | Email that must be logged in at dispatch time (auth_mode `subscription`). Must be a valid email address. For `api-key`/`oauth-token` modes this becomes a free-form display label — no `@` required, never itself verified. |
| `auth_mode` | `"subscription"` (default, empty means subscription) \| `"api-key"` \| `"oauth-token"`. See [Authentication modes](authentication-modes.md). |
| `credential` | Only set for `api-key`/`oauth-token` modes: `{source: "vault"\|"env", provider, key_ref, env_var}` — how to resolve the long-lived credential. `env_var`/the vault item must never resolve to the literal string `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`. |
| `identity_fingerprint` | Only set for `api-key`/`oauth-token` modes: a non-secret `sha256:<prefix>` of the resolved credential, recorded at registration and re-derived at every dispatch to detect a swapped key/token. |

### Profiles

**`personal`** — `CLAUDE_CONFIG_DIR` is **unset** in the child process.
Claude uses its default profile (`~/.claude.json`). Never point a personal
profile explicitly at `~/.claude`: that resolves to a freshly-hashed keychain
entry and produces a blank profile.

**`work`** (or custom) — `CLAUDE_CONFIG_DIR` is set to `claude_config_dir`
(convention: `~/.claude-work`). That directory holds its own `.claude.json`
and a separate keychain entry, keeping work and personal sessions isolated.

### Identity verification — fail closed

This section describes the default `auth_mode: subscription` check. Before
every dispatch, koryph reads `<configDir||$HOME>/.claude.json`, extracts
`oauthAccount.emailAddress`, and compares it case-insensitively to
`expected_identity`. Any of the following **refuses dispatch immediately**:

- The `.claude.json` file cannot be read.
- The JSON is unparseable.
- `oauthAccount.emailAddress` is empty (not logged in).
- The email does not match `expected_identity`.
- `expected_identity` in the registry is empty.

There is no fallback path. Fix the root cause (`claude auth login`, or update
the record via `koryph project set-account`) then retry.

An `api-key`/`oauth-token` account has no `.claude.json` to check at all —
identity verification instead resolves the credential, compares its
fingerprint against `identity_fingerprint`, and probes it live against
Anthropic; see [Authentication modes](authentication-modes.md) for that
mode-specific check.

### Changing the account triple (set-account)

The account triple is **immutable via normal `koryph project set`**. It moves
only through `koryph project set-account`, which:

1. Requires a non-empty `--reason` string.
2. Records the previous values of all three fields in `audit.jsonl`.
3. Resets `migration_status` to `registered`, requiring re-validation before
   the next dispatch.
4. Commits to the registry git repo:
   `feat(registry): set-account <id> <old-profile>-><new-profile>`.

To inspect the audit trail:

```sh
git log ~/.koryph                              # full change log
grep set-account ~/.koryph/audit.jsonl | jq   # account change events only
```

**Billing modes** follow the account triple. When `api_fallback` is `"off"`
(default), `ANTHROPIC_API_KEY` is scrubbed from the child environment and
agents use the account's OAuth subscription. When `api_fallback` is
`"explicit"` and the caller passes an explicit allow flag, the key named by
`api_key_env_var` is injected instead.
