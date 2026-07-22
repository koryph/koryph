<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# AI runtimes: support status

> **The honest status line: Claude Code is koryph's production runtime.
> Every other runtime is alpha — declared in the architecture, not yet
> verified end-to-end.** We intend to grow past a single vendor, and the
> seams for that are real and shipped; but today koryph is heavily focused
> on Claude Code, and we would rather say so than let you discover it
> mid-wave.

## Status by runtime

| Runtime | Status | What works today |
|---|---|---|
| **Claude Code** (`claude`) | **Supported** | Everything in this book: dispatch, personas, hooks, session resume, identity verification, quota measurement, activity streaming |
| Codex | **Alpha** | `AGENTS.md` operating contract; config/label plumbing parses — dispatch is refused fail-closed |
| Cursor | **Alpha** | Same as Codex |
| Gemini CLI | **Alpha** | Same as Codex |
| Grok Builder | **Alpha** | Same as Codex |
| Copilot | **Alpha** | Same as Codex |
| opencode / amp | **Alpha** | Same as Codex |

## What "alpha" means, precisely

- **The contract file works everywhere.** `AGENTS.md` — the runtime-neutral
  operating contract koryph installs at your repo root — is read natively by
  Codex, Cursor, Grok, Copilot, opencode, and amp. An interactive session in
  any of those tools will follow koryph's rules (beads-only task tracking,
  footprint labels, protected paths) when you drive it by hand.
- **The plumbing parses; the engine refuses.** `koryph.project.json`
  accepts `default_runtime` and a `runtimes{}` block, beads accept
  `runtime:<name>` labels, and the quota layer is built to bill each
  runtime against its own provider's windows. But dispatch to any runtime
  other than `claude` is **blocked fail-closed with a clear reason** —
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
