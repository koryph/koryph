<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Vibe-code with discipline

**koryph turns AI coding agents into a software factory.** Point it at a git
repo and it plans the work, runs a fleet of agents in parallel without merge
conflicts, keeps them inside budget and policy, merges only what passes your
gate — and ships signed, attested releases at the end. One static binary. No
SaaS. Nothing to unsubscribe from.

[**Install**](user-guide/installation.md) ·
[**Quickstart**](user-guide/quickstart.md) ·
[**Features**](features.md) ·
[**Zero to shipped**](user-guide/zero-to-shipped.md) ·
[**How koryph compares**](compare.md) ·
[**Download**](https://github.com/koryph/koryph/releases/latest)

[Using an AI agent? Point it at llms.txt →](llms.txt){ .md-button }

---

## Why koryph exists

Tools like Claude Code changed the constraint. An AI agent writes a feature in
minutes — so the bottleneck is no longer typing code, it is everything around
the code: keeping five agents from trampling each other, keeping spend inside
your subscription, keeping unreviewed changes off `main`, keeping commits
signed and settings hardened, and turning the result into a release someone
else can actually trust and install.

Vibe coding without that process is how you get a repo full of merge
conflicts, a surprise bill, and an unshippable pile of code. Doing that
process *by hand* is how you lose the speed you just gained.

koryph is the missing assembly line. It carries the process — planning,
parallelism, review, budgets, hygiene, releases — so you and your agents can
carry the ideas. It is opinionated about **process**, never about your
application: no frameworks chosen for you, no folder layouts, no lock-in.

## The three pillars

**Build — the agent factory.** koryph reads your project's task graph
([beads](https://github.com/gastownhall/beads)), batches conflict-free work by
each task's declared *footprint*, and dispatches headless agents into isolated
git worktrees — in parallel, continuously, under the correct account. Finished
work goes through review → rebase → your green gate → fast-forward merge.
Nothing lands that doesn't pass.

**Protect — hygiene as code.** Branch-protection rulesets, repo settings, and
security posture live as committed JSON you can check and apply
(`koryph repo check|apply`), with named profiles like the built-in
`oss-solo-maintainer`. Commit signing is enforced from vault-served keys.
Protected paths keep agents away from your CI, hooks, and policy files.
`koryph doctor` catches drift before it bites.

**Ship — the release train.** Conventional commits drive versioning for any
language. Releases are draft-until-complete and immutable: binaries,
checksums, SBOMs, cosign signatures, and SLSA build provenance all attach
before anything publishes. A vault-backed release bot keeps PR checks
flowing — with graceful fallbacks when you can't install one.

## New in this release

- **[`koryph adopt`](user-guide/adopt.md)** — one command takes any existing
  repo to a fully-onboarded, validated project: detect → plan → consent →
  execute → verify. Adoption is now the ten-minute front door.
- **[Intent routing](user-guide/describing-work.md)** — describe what you
  want built in a normal Claude session; koryph routes the ask through
  `/koryph-design` into a reviewed design doc and an implementable,
  conflict-aware bead graph.
- **[Recovery & escalation](user-guide/recovery.md), hardened** — stall
  detection, a dead-agent patrol, fault-only escalation to the frontier
  tier, and operator overrides that a running loop respects instead of
  clobbers (`koryph merge --close-bead`, `koryph inject`,
  `koryph status --frontier`).
- **[Terminal cockpit](user-guide/tui.md), overhauled** — a live activity
  tail of each agent's thinking and tool calls, structured stall flags,
  accurate fleet/quota readouts, and a queue that explains *why* every bead
  is or isn't dispatching.

The full history is in the
[CHANGELOG](https://github.com/koryph/koryph/blob/main/CHANGELOG.md).

## Datasheet

| | Feature | What you get |
|---|---|---|
| **Adopt** | `koryph adopt` wizard | Any existing repo to a green `koryph validate` in one consented run; re-run any time as a health check |
| **Plan** | Intent routing | Work-shaped prompts route to the right planning command instead of ad-hoc implementation |
| | Design funnel | `/koryph-design` writes a repo-grounded design doc, stops for your approval, then decomposes it |
| | Planning skills | `/koryph-plan`, `/koryph-import`, `/koryph-issue` — designs, roadmaps, and prompts become a correctly-footprinted, dependency-aware task graph |
| **Build** | Footprint scheduler | Tasks declare what they touch; only conflict-free work runs in parallel — no merge-conflict roulette |
| | Rolling dispatch | Slots refill continuously as work finishes; the fleet never idles waiting for a "wave" to end |
| | Worktree isolation | Every agent works in its own git worktree; your checkout is never touched |
| | Review pipeline | Reviewer findings block the merge until addressed; then rebase, gate, fast-forward |
| | Green gate | Your own build/test/lint commands are the merge gate — if it's red, it doesn't land |
| | Resource governor | Beads declare machine demand (`res:kind-cluster`, `res:docker`); counted capacities keep two 6 GB dev stacks from ever co-dispatching |
| | Cost governors | Per-provider concurrency caps that adapt to rate limits (AIMD + circuit breakers), plus subscription-burn tracking and quota calibration |
| | Recovery & escalation | Stalls detected, failures classified and retried; a genuine fault's final attempt escalates to the frontier tier — and koryph learns which work to start there |
| | Epic validation | After an epic's last child lands, a validator vets the union against the design — gaps become follow-up beads, then a docs update, then the epic closes |
| | Account safety | Each project pins its account; identity is verified fail-closed before any dispatch |
| | Multi-runtime | Runtime-neutral core with personas and model tiers (frontier / standard / light); **Claude Code is the production runtime — others are [alpha](user-guide/runtimes.md)**, refused fail-closed rather than guessed at |
| **Protect** | Posture profiles | Repo hygiene as named, applyable configuration — rulesets, settings, secret scanning, org-level rules |
| | Signing, vault-served | SSH commit signing with keys resolved on demand from Proton Pass, 1Password, macOS Keychain, or an encrypted file — never plaintext by default |
| | Protected paths | Merges that touch CI, hooks, or policy files are refused; a human lands those |
| | Doctor | One command reports drift across settings, signing, credentials, release infra, and DNS |
| | Scanner fragments | Opt-in gitleaks, vulnerability scanning, and license-allowlist presets |
| **Ship** | Release train | release-please + GoReleaser (or your own build commands) behind one contract that works for any language |
| | Supply chain | SBOM (SPDX), cosign keyless signatures, SLSA Build L3 provenance, immutable draft-until-complete releases — [verifiable by anyone](user-guide/supply-chain.md) |
| | Release bot | A vault-backed bot identity provisioned in one browser click (`koryph bot create`) — a GitHub App or a GitLab access token, [chosen per project](user-guide/forges.md) — so Release PRs/MRs trigger checks unaided |
| | Docs publishing | Zensical/MkDocs book published to your forge's Pages (GitHub Pages, GitLab Pages) on every docs push, custom domain and HTTPS included |
| **Operate** | Terminal cockpit | `koryph tui` — live threads, burndown ETAs, event feed, governor/quota gauges, and an activity tail of each agent's thinking; plus `board`, `roster`, and a VS Code extension |
| | Everything in the binary | Provisioning, hygiene, validation, releases — a brew-style install is the complete product; scripts are shims |

## What koryph is *not*

- **Not a SaaS.** There is no account, no telemetry, no server. Everything
  runs on your machine against your git remotes and your AI subscriptions.
- **Not a framework.** koryph never chooses your language, layout, or
  dependencies. Delete koryph and your repo is still a perfectly ordinary
  repo — you lose the factory, not the product.
- **Not a replacement for judgment.** The gate, the review stage, and
  protected paths exist precisely so that speed never outruns your standards.

## Get started

Adoption is two commands and about ten minutes:

```sh
# 1. Install — single static binary, no runtime needed
brew install koryph/tap/koryph

# 2. Adopt any existing repo: the wizard detects what's missing, shows a
#    plan, asks consent, and finishes with a green `koryph validate`
koryph adopt ~/src/myproject

# 3. Describe what you want built (in an agent session), then run the loop
koryph run --project myproject --once --auto-merge --review
```

Continue with the [quickstart](user-guide/quickstart.md), or read
[Zero to shipped](user-guide/zero-to-shipped.md) for the full journey — plan,
build, protect, release. Prefer to drive every step by hand? The wizard's
lower-level verbs are all [still there](user-guide/quickstart.md#alternative-register-manually-with-project-add).

This book is layered so you can stop reading whenever you have enough:
[Features](features.md) for the map, [Concepts](concepts/index.md) for the
ideas, the **user guide** for operators and collaborators, and the
**developer guide** for contributors to koryph itself.

## Project

- [How koryph compares](compare.md) — the 2026 agent-orchestration
  landscape, honestly mapped.
- [Security](security.md) — how koryph secures itself and your machines,
  and how to report a vulnerability.
- [Community & contributing](community.md) — filing issues, contributing
  code, and the Apache-2.0 licensing model.

## The name

**koryph** comes from the Ancient Greek κορυφαῖος (*koryphaios*) — the leader
of the chorus in classical Greek drama. The koryphaios stood at the head of the
chorus and spoke on its behalf whenever it took part in the action: one voice
fronting many performers moving in step. The root κορυφή (*koryphē*) means
"crest" or "summit", and the word lives on in several modern languages as a
term for the leading figure in a field.

That is exactly this tool's job: one process fronting a fleet of autonomous
coding agents — queueing their work, dispatching them in parallel, and
speaking for them at the merge. It is pronounced **KOR-iff**.

**For AI agents and tools:** a machine-readable index of the canonical docs
(llmstxt.org format) is published at [`/llms.txt`](llms.txt) — ingest it to map
the project and its operating contract.
