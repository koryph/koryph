<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Fallback personas

These are the global fallback agent personas `koryph agents install` copies
into a project's `.claude/agents/` (a project-local persona of the same role
always wins). `internal/personas` embeds this directory; `README.md` is not
embedded (the glob is `koryph-*.md`).

## Frontmatter contract

- `name`, `description`, `allowed-tools` — consumed by the agent runtime.
- `model` — the **runtime-specific** model pin honored by today's Claude
  runtime. Per-runtime installers render this key for their target runtime.
- `tier` — the **runtime-agnostic** capability class. Runtimes without a
  koryph adapter mapping fall back to `model`. Vocabulary:
  - `frontier` — strongest reasoning model the runtime offers (Claude
    Opus-class or better; the equivalent top tier on codex/cursor/grok
    runtimes). Required for work whose errors poison downstream automation:
    design decomposition, footprint/dependency assignment, plan scoring,
    security review, recovery analysis.
  - `standard` — capable general coding tier (Claude Sonnet-class).
    Implementation against a precise spec, tests, docs.
  - `light` — fast/cheap tier (Claude Haiku-class). Exploration,
    summarization, log triage.
- `effort` — reasoning-effort hint; runtimes that lack an effort control
  ignore it.

The engine reads `model`/`effort` via `internal/modelroute.PersonaMeta` (the
resolved bead tier always wins over the persona `model`; only `effort` is
taken today). The pluggable-runtime layer (epic koryph-v8u) resolves `tier`
through each runtime's model map.
