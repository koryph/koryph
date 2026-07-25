<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# AGENTS.md — koryph operating contract
<!-- koryph-clause:repository/v1 -->

The **canonical, runtime-neutral operating contract** every agent follows in
this repository. It owns repository conventions only. Role behavior,
worktree/phase boundaries, result commands, and task criteria are injected by
their separate named clauses.

## Capability tiers (not model names)

Koryph sizes work by runtime-agnostic tier:

- **frontier** — strongest reasoning tier; required where an error poisons
  downstream automation: decomposition, footprint/dependency/resource
  assignment, plan scoring, security review, and recovery analysis.
- **standard** — implementation against a precise specification, focused
  tests, and documentation.
- **light** — exploration, summarization, and log triage.

Beads whose acceptance criteria require a running service or environment carry
a `res:<kind>` label for each kind. Footprints protect the merge; resources
protect the machine. Undeclared resources risk host thrashing; over-declared
resources only reduce parallelism.

## Task tracking: beads only

All durable work lives in **beads** (`bd`) — never TodoWrite or markdown TODO
lists. The Koryph orchestrator owns claim, dependency, status, and closure
mutations for dispatched work. Interactive planning sessions use `bd prime` for
the full reference and persist durable insight with `bd remember` rather than a
MEMORY.md file.

## From intent to beads

When the operator describes a feature-sized or multi-part change, route it
through the planning front door:

- `/koryph-design <ask>` — create a repository-grounded design and decompose
  it after approval.
- `/koryph-plan <doc>` — decompose an existing design.
- `/koryph-import [path]` — import an existing roadmap.
- `/koryph-issue <desc>` — file one small, self-contained fix or chore.

On runtimes without slash-command support, use the corresponding canonical
workflow under `commands/`. Questions and explicitly requested trivial edits
need no planning Bead.

## Verification ownership

Dispatched workers run focused checks scoped to their task and changed
packages. Koryph's validation service owns broad repository validation and its
durable evidence. Do not duplicate broad validation in worker sessions.

## Commits

- Use Conventional Commits:
  `type(scope): imperative subject`.
- Include a DCO sign-off on every commit.
- Use SSH signing when configured.

Commit coherent checkpoints; uncommitted work is not recoverable after an
interrupted run.

## Repository protection configuration

Protected paths and merge policy are defined in `koryph.project.json`, not in
personas or copied examples. Koryph enforces that configuration at its
containment and merge boundaries.

## Non-interactive shell

Always pass non-interactive flags so an aliased tool cannot wait on a prompt:
`rm -f`, `rm -rf`, `cp -f`, `mv -f`; `ssh`/`scp -o BatchMode=yes`;
`apt-get -y`.

## Output economy

For a focused command whose output would dominate the transcript, use:

```sh
hooks/koryph-spill.sh <label> -- <command…>
```

The wrapper captures full stdout and stderr, prints a bounded summary,
preserves the exit code, and ends with `full output: <path>`. Read that file
when the summary is insufficient.
