<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Intent routing: from a described ask to dispatch-shaped beads

Status: shipped 2026-07-21.

## Problem

An adopted repo's `CLAUDE.md` predates koryph, or doesn't exist at all.
Nothing in the project's own instruction files tells an agent session that
implementation work in a koryph-managed repo must become **beads** — so an
operator who types "add rate limiting to the API server" gets an agent that
starts editing files ad hoc, bypassing footprints, resources, dependency
wiring, and the wave loop entirely. The planning skills that do this
correctly (`/koryph-plan`, `/koryph-import`, `/koryph-issue`) already ship,
but they assume the operator (a) knows they exist and (b) already has a
design doc or markdown corpus. There was no front door for an ask that
arrives as prose, and no runtime mechanism that surfaces the front door.

## Design

Three layers, all installed by the standard asset sequence
(`adopt.InstallAssets` — `koryph adopt` and `koryph project add`), none
depending on repo-side instruction files:

### L1 — `hooks/koryph-intent.sh` (UserPromptSubmit router)

Detection cannot live in `CLAUDE.md`, so it arrives at runtime, per prompt.
The hook reads the submitted prompt and, when it matches a work-intent
heuristic (build/change/fix verb vocabulary plus want-phrases, ≥24 chars,
not `/`-, `!`-, or `#`-prefixed), injects a <1KB rubric mapping the ask to
the right planning command. Invariants:

- **I1 fail-open** — always exit 0; missing `jq`, unparsable stdin, or any
  internal error injects nothing and never blocks a prompt.
- **I2 no recursion** — silent when `KORYPH_PHASE_ID` or
  `KORYPH_SPAWN_KIND` is set: a dispatched agent's work already IS a bead.
- **I3 byte-frugal** — questions and routed prefixes inject zero bytes;
  the rubric itself stays under 1KB (token-economy discipline,
  2026-07-token-economy.md).
- **I4 advisory** — never blocks or rewrites the prompt; the rubric tells
  the model to ignore the map for questions and trivial direct edits, so
  an over-trigger costs bytes, never correctness.

Wired by `internal/rules` (marker `koryph-intent.sh`, same
`${KORYPH_HOME}/hooks/` central-install pattern as the guards); ships via
the existing `hooks/*.sh` embed, so no install-list change.

### L2 — `/koryph-design` (the prose front door)

`internal/commands/koryph-design.md`. Routes first (small fix →
`/koryph-issue`; existing doc → `/koryph-plan`; existing markdown →
`/koryph-import`), then: clarify the ask, ground it in the actual repo
(symbols, `area_map`, `resources` vocabulary, prior beads), design with an
explicit extension seam, write a self-contained design doc under
`docs/designs/`, **stop for operator review**, and only then hand off to
`/koryph-plan` for decomposition into an epic + children carrying
footprint (`area:*`/`fp:*`), resource (`res:*`), and routing labels with
dependency edges. Frontier-tier gated like `/koryph-plan`, with the same
`koryph-architect` delegation escape hatch.

### L3 — `AGENTS.md` template section

"From intent to beads" in `internal/agentsmd/template.md`: the same
routing table as the rubric, for runtimes without hook support (Codex,
Cursor, Grok, …) — those read `AGENTS.md` natively and can follow the
command prompt files under `.claude/commands/` directly.

## Alternatives considered

- **A managed CLAUDE.md block** — rejected: koryph deliberately does not
  own `CLAUDE.md` (bd manages its own block there; koryph's contract file
  is `AGENTS.md`), and a repo-side file still can't fire per-prompt.
- **SessionStart injection of the rubric** — rejected: pays the byte cost
  in every session including the majority that never describe new work;
  UserPromptSubmit pays only on matching prompts and arrives exactly when
  the intent does.
- **LLM-based intent classification in the hook** — rejected: a hook must
  be fast, deterministic, and fail-open; a keyword heuristic plus an
  advisory rubric (I4) gets the same routing outcome since the model makes
  the final call anyway.

## Acceptance

- `hooks/intent_test.go` — rubric on work-intent prompts (valid hook JSON,
  all four commands named, ≤1KB), silence on questions / routed prefixes /
  short prompts / dispatch sessions, fail-open on malformed stdin and
  missing jq.
- `internal/rules/rules_test.go` — `koryph-intent` installs to the central
  HooksDir and wires as `UserPromptSubmit` in merged settings.
- `internal/commands/install_test.go` — `koryph-design` ships in the
  embedded command set.
