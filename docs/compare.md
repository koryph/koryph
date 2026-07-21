<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# How koryph compares

> **Snapshot: July 2026.** Agent orchestration is the fastest-moving corner
> of the industry; treat every claim below as dated the day it was written.
> Corrections are welcome — [file a docs issue](community.md#filing-an-issue).

koryph is a local-first, open-source, single-binary **software factory**:
it plans work into a dependency-aware task graph, *prevents* merge conflicts
by scheduling declared footprints, enforces review plus your own green gate
before fast-forward merges, governs cost, rate limits, and subscription
burn, recovers failed sessions with escalation to stronger models, and
carries the result through to signed, attested releases.

No tool we know of ships more than a couple of pieces of that combination —
but many ship *one* piece extremely well, and several are better than
koryph at things koryph doesn't attempt. Here's the honest map.

## The short version

- **Building on Anthropic's stack?** Claude Code's own subagents and
  workflows are powerful *within one session*; Claude Managed Agents is
  cloud infrastructure *for building* orchestrators. koryph is the layer
  that survives session death: the persistent task graph, the merge
  discipline, the governance, the releases.
- **Want an agent workbench with great UX?** Conductor and Sculptor are
  polished manual multiplexers — you fan out the tasks, you merge the
  results. koryph automates precisely the parts they leave to you.
- **Want maximum-velocity agent swarms?** Gas Town runs bigger fleets with
  a wilder culture and more runtimes. koryph trades that reach for
  determinism: no LLM in the control plane, conflicts prevented rather
  than triaged, spend governed rather than survived.
- **Living inside GitHub/enterprise?** Agent HQ, Devin, and Factory offer
  cloud scale, SSO, and audit trails koryph doesn't play in — at the price
  of your code executing on someone else's compute, under someone else's
  meter.

## Anthropic's own stack

**[Claude Managed Agents](https://platform.claude.com/docs/en/managed-agents/overview)**
(public beta, April 2026) is a server-hosted agent harness: you define
agents, Anthropic runs them in managed sandboxes with checkpointing,
tracing, and scheduling, billed per-token plus per-session-hour on the API.
It is *infrastructure to build an orchestrator on* — there is no task
graph, no git-awareness, no merge pipeline, no release engineering, and it
doesn't run on the flat-rate subscriptions koryph is built to ride.
Complementary more than competitive: koryph could someday dispatch *to* it;
today koryph's local, subscription-first model is close to its opposite.

**Claude Code itself** now fans out subagents and scripted multi-agent
workflows within a session. koryph deliberately builds *on* that rather
than competing with it: what Claude Code does not provide is a persistent
cross-session task graph, worktree-isolated branch/merge discipline, an
enforced review→gate→fast-forward pipeline, account pinning, multi-day
fleet operation, quota governance across projects, or a release train.
That gap is exactly koryph's job description.

## Gas Town — the nearest neighbor

[Gas Town](https://github.com/gastownhall/gastown) (Steve Yegge, MIT,
~17k stars) is the most philosophically adjacent tool — not least because
**koryph uses Gas Town's own task substrate,
[beads](https://github.com/gastownhall/beads)**. Same database, opposite
orchestration philosophy:

| | Gas Town | koryph |
|---|---|---|
| Coordinator | An LLM (the Mayor) decides what runs | A deterministic scheduler over declared footprints |
| Merge conflicts | Detected reactively in a bors-style merge queue (the Refinery) | Prevented proactively — overlapping writers are never dispatched together |
| Quality gate | Merge-queue verification | Review findings + *your* gate commands + fast-forward-only merges |
| Spend | Concurrency caps; famously heavy burn at full throttle | AIMD governors, subscription-burn calibration, budget caps, a quota ladder |
| Releases | — | Signed, attested, immutable release train |
| Runtimes | Many agent CLIs | Claude Code (others [alpha](user-guide/runtimes.md)) |
| Culture | Velocity, personality, scale | Discipline, determinism, shippability |

Gas Town is ahead of koryph on community, Windows support, and runtime
breadth, and its ecosystem is evolving fast (Gas City, the Wasteland,
hosted options). If you want the biggest possible swarm and enjoy the
chaos, run Gas Town. If you want the same task substrate with a control
plane that behaves the same way twice, run koryph.

## Agenc — the command center

[Agenc](https://github.com/mieubrisse/agenc) (AGPL-3.0) is "the CEO command
center for your fleet of Claudes": a tmux-based cockpit for firing off
disposable missions in full repo clones and hot-switching between live
sessions. It optimizes for velocity and feel with a human in the loop;
koryph optimizes for correctness, safety, and shippability without one.
The design differences are deliberate on both sides: clones vs worktrees,
shared credentials vs pinned fail-closed identity, push-to-main vs
review→gate→fast-forward, git-as-coordination vs a central scheduler.
We study it anyway — its conversational configuration and frictionless
ad-hoc dispatch are genuinely good ideas.

## The wider field

- **[Conductor](https://www.conductor.build/)** — polished macOS app running
  parallel Claude/Codex/Cursor agents in git worktrees with a
  review-and-merge UI. A manual multiplexer with superb UX; no planning,
  gating, governance, recovery, or releases.
- **[Sculptor](https://imbue.com/sculptor/)** (Imbue) — parallel agents in
  Docker containers with one-click sync into your local repo. Container
  isolation is a real alternative to worktrees for *environment*
  conflicts; merge discipline still lands on the human.
- **[claude-squad](https://github.com/smtg-ai/claude-squad)** (AGPL) — the
  minimal ancestor: tmux + worktree session manager for several agent
  CLIs. Simplicity is the feature.
- **vibe-kanban** (Bloop, Apache-2.0, 27k stars) — kanban-as-orchestrator
  for ten-plus runtimes with built-in review. **Sunsetting**: the company
  shut down in April 2026. A loud lesson in the economics of free local
  tooling — and part of why koryph is a no-SaaS project rather than a
  startup.
- **[Cursor](https://cursor.com/) background/parallel agents** — up to
  eight worktree agents plus cloud VMs returning PRs, metered in plan
  credits. Deep IDE integration; shallow orchestration; a vendor credit
  economy rather than governance of accounts you own.
- **[OpenAI Codex](https://openai.com/codex/) cloud tasks** — sandboxes at
  enormous scale producing PRs. A task-runner, not a factory: no
  dependency planning, no merge discipline beyond PRs, code executes on
  OpenAI's cloud.
- **[GitHub Agent HQ / Copilot coding agent](https://github.blog/news-insights/company-news/welcome-home-agents/)**
  — assign issues to third-party agents inside GitHub, gated by GitHub's
  own branch protections. The enterprise default if your org lives there;
  orchestration is issue-assignment, the gate is forge-bound, and the
  compute is Microsoft's.
- **[Devin](https://cognition.ai/)** (Cognition) — the most productized
  autonomous engineer, metered in opaque ACUs. Ahead on autonomy polish
  and enterprise sales; the anti-koryph on transparency and locality.
- **[OpenHands](https://github.com/openhands)** (MIT, ~80k stars) — the
  biggest open-source coding agent, scaling by "more sessions" with a
  planning-mode beta; not a governed fleet, and drifting cloud-ward.
- **[Factory](https://factory.ai/)** — enterprise spec-driven droids with
  SSO/audit/on-prem. Owns the compliance lane koryph doesn't enter.
- **[Google Jules](https://jules.google/)** — async cloud agent, VM per
  task, PR out. Minimal overlap beyond "parallel agents making PRs".

## Capability matrix

Legend: ✓ yes · ~ partial · ✗ no. Cloud vendors (Codex, Agent HQ, Devin,
Jules) are collapsed to their strongest representative, Agent HQ.

| Capability | koryph | Managed Agents | Claude Code | Gas Town | agenc | Conductor | Agent HQ | OpenHands |
|---|---|---|---|---|---|---|---|---|
| Local-first — code never leaves your machine | ✓ | ✗ | ✓ | ✓ | ✓ | ✓ | ✗ | ~ |
| Open source | ✓ | ✗ | ✗ | ✓ | ✓ | ✗ | ✗ | ✓ |
| Persistent dependency-aware task graph | ✓ | ✗ | ✗ | ✓ | ✗ | ✗ | ✗ | ~ |
| Proactive conflict-free parallelism | ✓ | ✗ | ✗ | ~ | ✗ | ✗ | ✗ | ✗ |
| Worktree isolation | ✓ | ✗ | ✗ | ✓ | ✗ | ✓ | ✗ | ~ |
| Enforced review + user-defined merge gate | ✓ | ✗ | ✗ | ~ | ✗ | ~ | ~ | ~ |
| Operator-tunable cost/rate governors | ✓ | ~ | ~ | ~ | ✗ | ✗ | ~ | ~ |
| Subscription-burn awareness | ✓ | ✗ | ~ | ✗ | ✗ | ✗ | ✗ | ✗ |
| Session recovery + model escalation | ✓ | ~ | ~ | ~ | ~ | ✗ | ~ | ~ |
| Repo hygiene enforcement (signing, protected paths, postures) | ✓ | ✗ | ✗ | ✗ | ✗ | ✗ | ~ | ✗ |
| Signed releases / SBOM / SLSA | ✓ | ✗ | ✗ | ✗ | ✗ | ✗ | ~ | ✗ |
| Multi-runtime dispatch | [alpha](user-guide/runtimes.md) | n/a | ✗ | ✓ | ✗ | ✓ | ✓ | ✓ |

## Where others are honestly ahead

- **Cloud scale and durability** — Managed Agents, Codex, and Devin run
  fleets no laptop can, with checkpointed sessions that outlive your
  hardware. Closing this gap is an open aspiration (a Kubernetes operator
  is on the [wish list](features.md#looking-ahead)) — but honestly priced:
  beyond one machine, orchestration is pay-per-token territory, because a
  single laptop can already exhaust a typical subscription allocation.
- **UI polish and onboarding** — Conductor, Sculptor, and Cursor are
  smoother on minute one. koryph's `adopt` wizard narrows this gap; a
  TUI is still a TUI.
- **Enterprise governance** — Agent HQ, Factory, and Devin have SSO, org
  policy, and audit trails. koryph's audit story is local files and git
  history — inspectable, but not a compliance product.
- **Runtime breadth** — Gas Town and others dispatch many agent CLIs
  today; koryph [refuses what it can't verify](user-guide/runtimes.md),
  which currently means Claude Code only.
- **Community mass** — several of these projects have thousands of stars
  and contributors. koryph is early. (You could
  [help with that](community.md).)

## The seat koryph occupies

The middle of this market is consolidating fast: independent orchestration
SaaS has died off, vendors are pulling orchestration into clouds you don't
control, and the wildest local experiment has moved into maintenance mode.
What's left underserved is exactly one seat: **the disciplined local
factory** — open source, deterministic, subscription-powered, that
prevents conflicts instead of triaging them, governs spend instead of
surviving it, and doesn't stop at "PR opened" but carries work through to
a signed, attested release. That's the seat koryph is built for.

*What it costs you:* your machine's capacity as the ceiling, Claude Code as
the (current) runtime, and a terminal-first workflow. If those trade the
right way for you, [start here](user-guide/quickstart.md).
