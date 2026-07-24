<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# AI runtimes: support status

> **Claude Code and Codex are supported runtime adapters.** Claude retains
> the complete legacy capability set; Codex is supported for authenticated
> workspace-write dispatch with runtime-native hooks and an advisory quota
> guard. Other runtimes remain declared-but-unimplemented adapters.

## Status by runtime

| Runtime | Status | What works today |
|---|---|---|
| **Claude Code** (`claude`) | **Supported** | Everything in this book: dispatch, personas, hooks, session resume, identity verification, quota measurement, activity streaming |
| **Codex** (`codex`) | **Supported, advisory metering** | `codex exec --json`, `CODEX_HOME` account isolation, ChatGPT/API-key CLI login, `AGENTS.md`, native `.codex/hooks.json`, native agent projections, workspace-write sandbox, exact-model and portable-equivalency routing |
| Cursor | **Declared, not dispatchable** | Runtime-neutral contract and model-routing schema only; no verified adapter yet |
| Gemini CLI | **Declared, not dispatchable** | Runtime-neutral contract and model-routing schema only; no verified adapter yet |
| Grok Builder | **Declared, not dispatchable** | Runtime-neutral contract and model-routing schema only; no verified adapter yet |
| Copilot | **Declared, not dispatchable** | Runtime-neutral contract and model-routing schema only; no verified adapter yet |
| opencode / amp | **Declared, not dispatchable** | Runtime-neutral contract and model-routing schema only; no verified adapter yet |

## Codex setup

1. Install and authenticate the Codex CLI: `codex login` (or `codex login
   --with-api-key`). Verify with `codex login status`.
2. Enroll it for a project. This audited command enables `runtimes.codex`,
   installs Codex's native assets, and optionally makes it the default:

   ```sh
   koryph project set-runtime-account <project-id> \
     --runtime codex --config-dir "${CODEX_HOME:-$HOME/.codex}" --identity auto \
     --reason "enroll Codex account"
   # Add --default to make Codex the project default; otherwise Claude remains
   # the default and beads can opt into Codex with runtime:codex.
   ```

   The equivalent initial onboarding command is:

   ```sh
   koryph project add <root> --account personal --runtime codex --identity auto
   ```

   A hand-authored configuration remains useful when you want to review it:

   ```json
   {
     "default_runtime": "codex",
     "runtimes": {
       "codex": {
         "enabled": true,
         "model_map": {
           "frontier": "gpt-5.6-terra",
           "standard": "gpt-5.6-terra",
           "light": "gpt-5.6-terra"
         },
         "effort_map": {"xhigh": "high"}
       }
     }
   }
   ```

   `config_dir` is `CODEX_HOME`. A missing or changed auth-record binding
   fails closed before dispatch.
3. For a project with both runtimes enabled, `koryph project install-assets
   <root>` refreshes every runtime projection together. It writes
   `.codex/hooks.json` and `.codex/agents/*.toml` from canonical
   `agents/*.md`. Claude's `.claude/agents/*.md` files and both runtimes'
   workflow entries are relative links to canonical `agents/*.md` and
   `commands/*.md`, respectively—edit the canonical file once and both tools
   see the change immediately.

Codex intentionally has no koryph hard spend cap or safe session-resume flag
in the current CLI invocation form, so those features are omitted and quota
throttling is advisory until a trustworthy Codex usage source is added.

## Selecting models and effort

Beads can express either concrete runtime choices or a portable equivalency:

- `model:gpt-5.6-terra` infers Codex from the registered model catalogue.
- `runtime:codex`, `model:gpt-5.6-terra`, `effort:high` selects an exact native
  model and native effort explicitly.
- `runtime:codex`, `equiv:frontier:xhigh` selects the project's Codex
  frontier mapping and translates portable `xhigh` through
  `runtimes.codex.effort_map` (the shipped default maps it to `high`).

Do not combine `equiv:` with `model:` on one bead. Existing
`model:opus|sonnet|haiku` labels remain compatible Claude selections.

The portable equivalency vocabulary is `frontier|standard|light` plus a
portable effort (`low`, `medium`, `high`, `xhigh`, `max`, or `ultra`). Each
enabled runtime maps that pair through its own `model_map` and `effort_map`.
An exact model that belongs to the built-in Codex/Claude catalogue infers its
runtime; a custom model must carry an explicit `runtime:<name>` label.

## Selecting a runtime for one run

Normal dispatch preserves each bead's `runtime:`, `model:`, and `equiv:`
labels, with `default_runtime` and the runtime-scoped project defaults filling
in anything omitted. `koryph run` adds two mutually exclusive session-only
policies when an operator needs to constrain that normal routing:

- `--runtime-only codex` runs only beads whose normal routing already resolves
  to Codex. A bead pinned to Claude (or to a Claude-only native model) remains
  ready but is recorded as skipped in the run frontier; it is never silently
  changed.
- `--runtime-equivalent codex` processes the full eligible frontier on Codex.
  Koryph first resolves every bead as declared, derives its portable
  `frontier|standard|light` capability and, when known, portable effort, then
  maps that request through `runtimes.codex.model_map` and `effort_map`. The
  target runtime's account, authentication check, quota pool, and estimate
  table are used for the run.

For example, a Claude bead carrying `runtime:claude`, `model:opus`, and
`effort:xhigh` becomes Codex's `frontier:xhigh` mapping under
`--runtime-equivalent codex` (currently `gpt-5.6-terra` with native `high`
effort). Prefer an explicit `equiv:frontier:xhigh` whenever a bead must remain
portable across runtimes.

Koryph fails closed rather than guessing when a native source model maps to
more than one portable tier or is custom/unmapped. This is common with the
current Codex default model, which intentionally serves several tiers. Replace
that source selection with `equiv:<tier>:<effort>` (or the project's
`default_equivalent`) before using `--runtime-equivalent`.

## Codex equivalent-model canary

After authenticating Codex and configuring an unambiguous portable equivalent,
run one eligible bead as a Codex-only canary:

```sh
koryph run --project <project-id> --once --max 1 \
  --runtime-equivalent codex --allow-unvalidated
```

`--runtime-equivalent codex` preserves the bead's requested capability tier
and effort while selecting that project's Codex mapping; it does not relabel
the bead. `--max 1` limits the canary to one slot. Remove
`--allow-unvalidated` once the project has reached the `validated` lifecycle
state. This shared runtime policy does not replace Claude: Claude remains
fully supported and can be selected normally for later runs.

Inspect the completed slot with `koryph status --project <project-id>`, then
confirm the landed tip has a good Git signature:

```sh
git log -1 --format='%H %G?'
```

The status must be `G`. For a candidate branch before it lands, use
`koryph signing verify --project <project-id> --branch <branch>`; koryph also
verifies signatures immediately before merging.

Blocked candidates are not discarded. Koryph records the block and preserves
the branch and worktree for inspection and recovery. In particular, a dirty
candidate is blocked with its uncommitted changes intact; commit the intended
work in that worktree before retrying rather than recreating the canary.

Projects may set defaults in either place below; native and portable defaults
are mutually exclusive at each scope:

```json
{
  "default_runtime": "codex",
  "default_equivalent": "standard:high",
  "runtimes": {
    "claude": {"enabled": true, "default_model": "sonnet"},
    "codex": {"enabled": true, "default_equivalent": "standard:xhigh"}
  }
}
```

The top-level defaults belong to `default_runtime`. A
`runtimes.<name>.default_model` or `default_equivalent` is the explicit default
for that runtime and takes precedence when a bead selects it. A command-line
`--default-model` remains the highest label-less native override for that run.

## What "alpha" means, precisely

- **The contract file works everywhere.** `AGENTS.md` — the runtime-neutral
  operating contract koryph installs at your repo root — is read natively by
  Codex, Cursor, Grok, Copilot, opencode, and amp. An interactive session in
  any of those tools will follow koryph's rules (beads-only task tracking,
  footprint labels, protected paths) when you drive it by hand.
- **The plumbing parses; unsupported adapters refuse.** `koryph.project.json`
  accepts `default_runtime` and a `runtimes{}` block, beads accept
  `runtime:<name>` labels, and the quota layer is built to bill each
  runtime against its own provider's windows. But dispatch to any runtime
  other than `claude` or `codex` is **blocked fail-closed with a clear reason** —
  koryph never silently substitutes a runtime it cannot vouch for, because
  it cannot yet verify those runtimes' identity, parse their event streams,
  or meter their quota.
- **We haven't verified these paths end-to-end.** That is the whole reason
  for the label. As adapters land and get exercised, this table will be
  updated — expect movement here in the near future.

## Why fail-closed instead of best-effort

koryph's account safety model verifies *who is running* before anything
dispatches. A best-effort "try the codex CLI and hope" path would mean
unverified identity, unmetered spend, and unparsed failure modes — three
of the exact problems koryph exists to prevent. Until an adapter can meet
the same bar as the Claude one, refusing loudly is the feature.

## The adapter seam (what an adapter is)

Runtime support is a Go interface, not a fork: `Detect`, `AuthCheck`,
`VerifyIdentity`, `Capabilities`, `Command`, `ParseEvents`,
`InstructionFile`, `AccountEnv`, `ModelMap`. Personas carry model **tiers**
(frontier / standard / light) rather than model names, and each adapter
maps tiers to its provider's models — for Claude: Opus / Sonnet / Haiku.
Capability flags (hooks, resume, sandbox, budget flags, usage source) let
the engine degrade gracefully where a runtime lacks a feature: runtimes
without hook support rely on worktree isolation and merge-time
protected-path refusal for containment instead of in-editor guards.

The shipped Claude adapter (`internal/runtime/claude/`) is the reference
implementation; the [Efficiency tab](tui.md#4-quota-windows)'s quota
windows and the status bar are already per-provider, so a new adapter's
usage reader gets its own burn bars with no cockpit changes.

## Help us get there

Runtime adapters are one of the highest-leverage places to contribute, and
we actively welcome the help:

1. Read the `Runtime` interface and the Claude adapter as the reference.
2. Open a [feature request](https://github.com/koryph/koryph/issues/new/choose)
   naming the runtime, so the work is visible and not duplicated.
3. Expect the review bar to be about safety, not polish: identity
   verification and fail-closed behavior come first, capability flags
   second, everything else after.

See [Community & contributing](../community.md) for the contribution
ground rules (DCO, signing, the gate).
