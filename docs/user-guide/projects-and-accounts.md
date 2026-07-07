<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Projects and Accounts

Every repository koryph manages is described by two artefacts: a
**project config** (`koryph.project.json`) checked into the repo root, and a
**registry record** kept in `~/.koryph/registry.d/<project-id>.json`.
The registry record also binds the **account** that dispatched agents run under.

---

## koryph.project.json

Created by `koryph onboard`, validated by `koryph validate`.

| Field | Type | Default | Purpose |
|---|---|---|---|
| `schema_version` | int | 1 | Format version. Always `1`. |
| `project_id` | string | — | Slug (`[a-z0-9-]+`); must match the registry record. |
| `work_source` | string | `"bd"` | `"bd"` (beads ready-graph, preferred) or `"markdown"` (legacy plans dir). |
| `plans_dir` | string | — | Required when `work_source` is `"markdown"`. |
| `gate` | array | — | Shell commands run inside the worktree after rebase and before merge. At least one required. |
| `footprint` | array | — | Conflict-domain rules: `{pattern, tokens}`. Tokens prefixed `HOT:` conflict with every worktree sharing that token. |
| `area_map` | object | — | Expands `area:<x>` bead labels to footprint tokens when no `fp:*` label is present. |
| `stages` | object | — | Maps pipeline stage names to `.claude/agents/` persona names. |
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
| `max_concurrent_slots` | int | 3 | Wave width cap for this project. |
| `dispatch_stagger_seconds` | int | 8 | Seconds between agent launches within a wave. |
| `poll_seconds` | int | 10 | Poll-tick interval (seconds) for reading slot `status.json` heartbeats. 0 uses the engine default (10 s). Overridden by `KORYPH_POLL_SEC` or `Options.PollSec` at the programmatic call site. |
| `dispatch_mode` | string | `"wave"` | `"wave"` or `"rolling"`. Rolling continuously refills a slot as it frees up instead of waiting for the whole batch; see [Running Waves](running-waves.md#dispatch-mode-wave-vs-rolling). `--dispatch-mode` on `koryph run` overrides this per run. |

**Conventional commits are enforced by default.** With `commit_style` unset or
`"conventional"`, the merge and PR paths validate every commit subject in
`<default>..<branch>` against the Conventional Commits grammar
(`type(scope): subject`; types `feat|fix|docs|chore|refactor|test|ci|build|perf|style`)
before any merge or PR. A non-conforming subject bounces the bead back to the
implementer once — like a gate failure — and blocks it if the violation persists;
nothing non-conventional lands. Set `commit_style` to `"none"` to opt out, or
`"custom"` (with `commit_template`) to govern messages by a project template
instead.

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
| `persona` | Override the `.claude/agents/` agent. Default: the stage's namespaced engine persona (e.g. `docs → koryph-feature-docs-author`). |
| `model` | Override the model tier (must be in the registry `allowed_models`). Default: the stage's engine default. |
| `effort` | Reasoning-effort hint. |
| `prompt` | Extra stage-specific instructions appended to the built stage prompt. |
| `optional` | `true` → a failed stage logs and continues. Default `false` → a failed stage **blocks the bead** (never auto-merges past incomplete pipeline work). |

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
| `api_fallback` | `"off"` \| `"explicit"` — whether direct API key use is allowed. |
| `api_key_env_var` | Env-var **name** holding the key (never the key value itself). |
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
| `expected_identity` | Email that must be logged in at dispatch time. Must be a valid email address. |

### Profiles

**`personal`** — `CLAUDE_CONFIG_DIR` is **unset** in the child process.
Claude uses its default profile (`~/.claude.json`). Never point a personal
profile explicitly at `~/.claude`: that resolves to a freshly-hashed keychain
entry and produces a blank profile.

**`work`** (or custom) — `CLAUDE_CONFIG_DIR` is set to `claude_config_dir`
(convention: `~/.claude-work`). That directory holds its own `.claude.json`
and a separate keychain entry, keeping work and personal sessions isolated.

### Identity verification — fail closed

Before every dispatch, koryph reads `<configDir||$HOME>/.claude.json`,
extracts `oauthAccount.emailAddress`, and compares it case-insensitively to
`expected_identity`. Any of the following **refuses dispatch immediately**:

- The `.claude.json` file cannot be read.
- The JSON is unparseable.
- `oauthAccount.emailAddress` is empty (not logged in).
- The email does not match `expected_identity`.
- `expected_identity` in the registry is empty.

There is no fallback path. Fix the root cause (`claude auth login`, or update
the record via `koryph project set-account`) then retry.

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
