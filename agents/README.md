<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Fallback personas

These are the global fallback agent personas `koryph agents install` copies
into a project's `.claude/agents/` (a project-local persona of the same role
always wins). `internal/personas` embeds this directory (`//go:embed *.md`,
which also embeds this README — it carries no `tier`/`model` frontmatter, so
the installer's per-runtime rendering below always leaves it verbatim).

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

The engine reads `model`/`effort`/`tier` via `internal/modelroute.PersonaMeta`.
The pluggable-runtime layer (epic koryph-v8u) resolves `tier` through each
runtime's model map.

## Installer rendering (koryph-v8u.12)

`koryph agents install --runtime <name>` renders these frontmatter files for
a non-Claude target BEFORE they ever reach `internal/modelroute`: it rewrites
each persona's `model:` scalar through `<name>`'s `runtime.Runtime.ModelMap`,
keyed by that same persona's `tier:` scalar, so a codex/cursor/grok project
lands with its own model pin instead of a Claude model name it cannot honor.
`--runtime` unset (or `"claude"`) is a verbatim, byte-identical copy — no
rendering pass runs at all for claude, which is the default and the
compatibility contract every pre-koryph-v8u.12 install depends on. A persona
with no `tier:`, or whose tier the target runtime's `ModelMap` does not
cover, is installed UNCHANGED (still carrying the Claude `model:` pin) rather
than having a value fabricated; the installer notes which personas landed
this way. An unregistered `--runtime` name is a hard error (fail closed) —
see `internal/personas.InstallForRuntime`.

## Resolution precedence (koryph-v8u.10)

For a persona-run stage, the implement-stage model is chosen in this order:

1. a bead `model:<tier>` label (`model:opus`, `model:implement:opus`, ...) —
   wins unconditionally, unchanged from before this bead.
2. this stage's persona `tier` scalar, resolved through the active runtime's
   model map (today: the hardcoded Claude map — frontier→opus, standard→
   sonnet, light→haiku; a project may override any entry via
   `koryph.project.json`'s `model_map`). `fable` is never an implicit
   mapping target; a project may explicitly re-map `frontier` to `fable`,
   but `modelroute.Resolve`'s fable guard still requires an explicit
   selection source before that takes effect.
3. this stage's persona `model` scalar (the legacy Claude pin) — the
   fallback when the persona carries no `tier`, or its `tier` is unmapped.
4. the engine's hardcoded per-stage default (plan/design/score/review →
   opus; implement/docs/test → sonnet; explore/debug → haiku).

`effort` is unaffected by this ordering: it is always taken from the
resolved persona's frontmatter when the bead/run did not already set one.
