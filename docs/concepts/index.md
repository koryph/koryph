<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Concepts

This track explains the ideas koryph is built on — independent of any
particular command. The [user guide](../user-guide/installation.md) tells you
*what to type*; this section tells you *why it works*, in the order the ideas
build on each other. For all of them chained into one loop — prompt to
design to beads to merge to release — see
[The lifecycle](lifecycle.md).

## Work: beads and the ready-graph

koryph does not maintain its own task tracker. Work lives in
[beads](https://github.com/gastownhall/beads): every task is an issue (a
"bead") in a per-project database that travels with the repo through its git
remote, so every collaborator — human or agent — shares the same graph.
Beads declare dependencies on each other, and the set of beads whose
dependencies are all satisfied is the **ready-graph**: the frontier of work
that could start right now. That frontier, not a human dispatcher, is what
feeds the scheduler. Planning skills (`/koryph-plan`, `/koryph-import`,
`/koryph-replan`) exist to turn designs and prompts into beads shaped
correctly for dispatch — typed, scoped, footprinted, and dependency-linked.

→ **[Work: beads and the ready-graph](beads.md)**

## Parallelism: footprints

Two agents editing the same file produce a merge conflict; the fix is to know
*before dispatch* what each task will touch. Every bead carries a
**footprint** — a set of labels naming the areas of the codebase it reads and
writes (`area:sched`, `fp:read:go:signing`, …). Two beads conflict when they
share a token and at least one writes it; readers happily co-run. The
scheduler only dispatches mutually conflict-free work, which is what makes
"run five agents at once" safe rather than reckless. An unlabeled bead falls
into a catch-all token that conflicts with everything — honest labeling is
what buys parallelism.

→ **[Parallelism: footprints](footprints.md)**

## Time: rolling dispatch

The naive way to run a fleet is in waves: dispatch N tasks, wait for all N to
finish, repeat. The problem is that the whole wave then moves at the speed of
its slowest member. koryph's default is **rolling dispatch**: as each agent
finishes and merges, the scheduler immediately re-scans the ready-graph and
fills the free slot with the next conflict-free bead. New work becomes
eligible mid-flight (dependencies resolve, footprints free up), so the fleet
stays saturated without ever violating footprint safety.

→ **[Time: rolling dispatch](rolling-dispatch.md)**

## Safety: worktrees, protected paths, and the green gate

Each agent works in its **own git worktree** — an isolated checkout on its
own branch. Your working copy is never touched, and a misbehaving agent can
be discarded without cleanup. Finished branches pass through a pipeline:
review (findings block until addressed), rebase onto current `main`, then the
**green gate** — the project's own build/test/lint commands. Only a gate-green
branch fast-forwards onto `main`. Two hard lines exist regardless of gate
results: **protected paths** (CI workflows, hooks, policy files — merges
touching them are refused and a human lands them deliberately) and signed
commits.

→ **[Safety: worktrees, protected paths, and the green gate](worktrees.md)**

## Money: governors, quota, and subscription-first

Autonomous agents can spend faster than you can watch. koryph is
**subscription-first**: dispatch rides the flat-rate CLI subscription, and
per-token API spend requires explicit opt-in. Concurrency runs through
**per-provider governors** — each provider (Anthropic, others as adapters
arrive) gets its own cap that *adapts*: rate-limit responses halve it
immediately (with settle windows and circuit breakers to prevent thrashing),
and sustained success probes it back up. On top sits **quota tracking**:
koryph calibrates observed spend against your plan's windows and can throttle
or pause dispatch as a window fills. The result is a fleet that runs at the
edge of what your subscription allows and never past it.

→ **[Money: governors, quota, and subscription-first](governors.md)**

## People: accounts, identity, and personas

Every project pins the **account** its agents run under, and identity is
verified fail-closed before dispatch — agents never run under whatever
happens to be logged in. **Personas** describe the kind of worker a task
needs (implementer, reviewer, architect, …) and carry a **model tier**
(frontier / standard / light) rather than a hard-coded model name; each
runtime maps tiers to its own models. That is what keeps koryph
runtime-neutral: Claude Code and Codex today, other agent CLIs through the same
adapter seam — support for those is
[alpha](../user-guide/runtimes.md), stated plainly.

→ **[People: accounts, identity, and personas](accounts.md)**

## Hygiene: postures and vaults

Repo security settings are configuration, not clicks. A **posture** is a
named, versioned bundle of branch-protection rulesets, repo settings, and
scanner presets that koryph can diff and apply against a live repo
(`koryph posture`, with the built-in `oss-solo-maintainer` profile as the
opinionated default). Secrets and signing keys resolve through the **vault**
layer — Proton Pass, 1Password, macOS Keychain, or a passphrase-encrypted
file — fetched on demand, never plaintext by default. A key protected by a
passphrase on disk is treated as what it is: the same posture as a normal
`~/.ssh` key, worth an informational note, not an alarm.

→ **[Hygiene: postures and vaults](postures.md)**

## Shipping: the release train and the supply chain

A release should be an outcome, not a project. Conventional commits
accumulate into a Release PR (release-please); merging it triggers
gate-before-tag, an artifact build (GoReleaser or your own commands — the
contract works for any language), and a **draft-until-complete** release:
binaries, checksums, SPDX SBOMs, cosign signatures, and SLSA build
provenance all attach *before* publication, so immutable releases only ever
lock a complete set. A one-click **release bot** keeps Release-PR checks
flowing; documented fallbacks cover the no-bot case. Anyone can verify what
you shipped — see [Verifying a release](../user-guide/supply-chain.md).

→ **[Shipping: the release train and the supply chain](release-train.md)**

## The fence: ejectability and the opinionation boundary

Two principles bound everything above. The **opinionation boundary**: koryph
holds opinions about *process* (signing, gates, provenance, footprinted
planning) and none about *your application* (no frameworks, no layouts, no
dependencies). And **ejectability**: everything koryph produces is standard,
inspectable material — an ordinary git repo, GitHub-native settings, plain
workflows, standard release assets. Delete koryph and nothing breaks; you
lose the factory, not the product. A third, operational principle follows the
same spirit: **capabilities live in the binary** — one installed `koryph` is
the complete product, with no repo clone or script kit required.

→ **[The fence: ejectability and the opinionation boundary](ejectability.md)**
