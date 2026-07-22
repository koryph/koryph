<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Zero to shipped

This chapter walks the whole journey koryph exists to serve: from a git repo
to built, signed, released software, with autonomous coding agents doing the
building and koryph enforcing the discipline that makes that safe. Every step
below is a **shipped command** you can run today. Where a smoother front door
is planned but not yet built, it is marked as such — nothing here oversells.

The journey spans koryph's three pillars:

- **Build** — plan work into beads, then run waves of agents against it.
- **Protect** — pin repository hygiene as committed IaC and enforce signing.
- **Ship** — set up the release train and publish signed, provenanced builds.

You do not need all three on day one. A project can stop after Build and add
Protect and Ship later; each pillar is opt-in and independently useful.

---

## The front door today (and the one coming)

Today koryph starts from an **existing git repository** — one you already
created (`git init`, `gh repo create`, or a clone). You bring the repo; koryph
registers it, scaffolds its agent contract, and takes over from there.

> **Planned, not yet shipped:** a single `koryph new <name> --lang …` command
> that creates the GitHub repo, seeds the license/REUSE/README/CI skeleton,
> initialises beads, applies a hygiene posture, and wires signing and the
> release train in one shot. It does not exist yet — see the
> [software-factory design](https://github.com/koryph/koryph/blob/main/docs/designs/2026-07-software-factory.md)
> §3.1. Until it lands, the steps below are the real front door.

---

## Stage 1 — Adopt the project (Build)

Point koryph at your repo and let the wizard take it to a green
`koryph validate` in one run — installing prerequisites, initializing beads,
deriving the account/gate/forge/area_map, installing the agent scaffolding,
and validating:

```sh
koryph adopt ~/src/myproject
```

`adopt` detects what's already in place, prints a plan naming each step and
why it's needed, asks for consent in three scopes (the repo-wide plan, each
system-level install, and the derived account/gate/forge values), then
executes and finishes by running `koryph validate` in-process — promoting
the record `registered → migrated` on the first green pass. See
[koryph adopt](adopt.md) for the full phase-by-phase walkthrough.

**Alternative — drive it by hand:** `koryph project add` is the lower-level
primitive `adopt` sequences. It writes a machine-local registry record under
`~/.koryph` and installs the agent scaffolding — `AGENTS.md` (the
runtime-neutral operating contract), fallback personas, and — for Claude
Code — the `koryph-*` slash commands, hooks, and permission baseline — but
does not initialize beads or infer `gate`/`forge`/`area_map` for you:

```sh
koryph project add ~/src/myproject \
  --account personal \
  --identity you@example.com
```

Then confirm the pre-dispatch gate passes:

```sh
koryph validate myproject
```

`validate` checks hooks, adapter config, beads initialisation, and account
identity, promoting the record `registered → migrated` on the first green pass.

See [Quickstart](quickstart.md) for the full asset table and both paths'
validation output.

---

## Stage 2 — Turn intent into a bead graph (Build)

Waves dispatch **beads** (issues in the project's local beads/Dolt database).
`bd ready` must be non-empty or the loop has nothing to build. Three shipped
paths get you there:

- **From an idea in your head** — just describe what you want to build,
  change, or fix in an agent session: the installed `koryph-intent.sh`
  hook detects work-shaped prompts and routes the session to the right
  planning command. Or run `/koryph-design "<ask>"` directly — it writes a
  repo-grounded design doc, waits for your approval, then decomposes it.
- **From a design doc** — run `/koryph-plan <doc>` in your editor (or the
  `koryph-plan` skill) to decompose a design into a filed, conflict-aware bead
  graph with footprints and dependencies wired.
- **From existing markdown** — run `/koryph-import [path]` (the `koryph-import`
  skill) to convert `ROADMAP.md`, `TODO.md`, or inline TODO/FIXME clusters
  into that same footprint-labelled corpus. See [Intake](intake.md).

You can also file a single well-formed issue with `/koryph-issue "<desc>"`.
Footprint labels (`area:*`, `fp:read:*`) are what let the scheduler run
conflict-free work in parallel — the planning commands assign them for you.
The whole prompt-to-beads pipeline, including the design-approval stop and
a worked example, is in [From prompt to beads](describing-work.md).

---

## Stage 3 — Run waves (Build)

Dry-run a single wave first to see exactly what would dispatch:

```sh
koryph run --project myproject --once --dry-run
```

Remove `--dry-run` to fire a live wave, and drop `--once` to loop until the
ready-graph drains:

```sh
koryph run --project myproject --once            # one live wave
koryph run --project myproject --review --auto-merge   # loop, review + merge
```

Each dispatch runs in an isolated worktree under the verified account; koryph
polls to completion, then reviews, rebases, runs the green gate, and
fast-forward-merges the green branches. Watch progress with `koryph board`
and `koryph status --project myproject`. See [Running waves](running-waves.md)
for `--resume`, `--max`, and auto-merge policy, and
[Billing & quota](billing-and-quota.md) before enabling `--allow-api-spend`.

---

## Stage 4 — Pin repository hygiene (Protect)

koryph treats GitHub security settings as **infrastructure as code**:
branch-protection rulesets and repo settings live as committed JSON under
`.github/`, so the enforced posture is reviewable and reproducible. Diff the
live repo against the committed IaC, then apply:

```sh
koryph repo check                        # exit 1 on drift between live GitHub settings and .github IaC
koryph repo apply                        # diff-first → snapshot → apply; rollback with:
koryph repo rollback                     # restore live state to the most recent pre-apply snapshot
koryph repo rollback --to 2026-07-04T16  # restore to a specific snapshot (timestamp prefix)
```

For repos without their own committed IaC, named **posture profiles** apply
the same discipline anywhere — the built-in `oss-solo-maintainer` profile
carries a 1-approval + signed-commits + secret-scanning baseline:

```sh
koryph posture list                                            # built-ins + ~/.koryph/postures
koryph posture check oss-solo-maintainer --repo O/R --no-fail  # see what would change
koryph posture apply oss-solo-maintainer --repo O/R            # diff-first, then apply
```

Repo-local `.github/` IaC, when present, always overrides the profile — an
ejected repo stays sovereign. A project can pin its profile in the `posture`
block of `koryph.project.json`; `koryph doctor` then reports drift. See
[Posture profiles](postures.md).

`koryph doctor --project myproject` reports configuration drift as part of the
same hygiene story.

---

## Stage 5 — Enforce commit signing (Protect)

koryph can require SSH-signed, DCO-signed-off commits, serving the signing key
from a vault so it never lives loose on disk:

```sh
koryph signing setup     # provision / register the signing key from the vault
koryph signing enable --project myproject
koryph signing status
```

Once enabled, unsigned or unsigned-off commits are refused by the local hooks
and the merge gate. See [Signing](signing.md) for the vault flow, key
rotation, and verification.

---

## Stage 6 — Set up the release train (Ship)

Give the project a language-agnostic release pipeline with one command:

```sh
# Mode A — generic build commands (any language)
koryph release setup --project myproject --mode commands

# Mode B — GoReleaser (Go projects with .goreleaser.yaml)
koryph release setup --project myproject --mode goreleaser
```

`release setup` renders the release-please config and the release-train
workflow, then prints the remaining human steps (provision the bot, set
repository secrets, add the bot to branch-protection bypass, push to open the
first Release PR). It is idempotent for the workflow and config files. See
[Releasing projects](releasing-projects.md) for the build contract and both
modes in depth.

After merge, publication is always the **last** step: nothing is published
until the tag, artifacts, SBOM, and SLSA provenance are all attached, so every
release locks a complete, immutable asset set.

---

## Stage 7 — Provision the release bot (Ship)

The release bot is a vault-backed identity that lets the release train push
tags and manage releases without a human PAT. On GitHub it is a **GitHub App**;
on GitLab it is a **project access token** (see [Choosing a forge](forges.md)).
The commands below show the GitHub App flow. Create it once per account:

```sh
koryph bot create --name mylogin-release-bot
koryph bot attach --project myproject     # sets RELEASE_BOT_APP_ID + PRIVATE_KEY secrets
koryph bot check --project myproject
```

`koryph release setup … --bot` runs create + attach for you in one step. On a
headless machine, pass `--headless` to `bot create` and open the printed URL
from another device. The private key PEM is never printed to the terminal.

If you skip the bot, the release train still works with a graceful fallback —
see [The release bot](release-bot.md) for the three replication models
(personal, guest org, owned org) and the fallback behaviour.

---

## The whole journey, at a glance

| Pillar | Stage | Command(s) |
|---|---|---|
| Build | Adopt | `koryph adopt` (or `koryph project add`, `koryph validate`) |
| Build | Plan | `/koryph-design`, `/koryph-plan`, `/koryph-import`, `/koryph-issue` |
| Build | Run | `koryph run --project … [--review --auto-merge]` |
| Protect | Hygiene | `koryph repo check` / `apply`, `koryph posture apply`, `koryph doctor` |
| Protect | Signing | `koryph signing setup / enable / status` |
| Ship | Release | `koryph release setup --project … --mode …` |
| Ship | Bot | `koryph bot create / attach / check` |

That is the factory: an existing repo in, signed and released software out,
with agents building under discipline the whole way. The remaining gap — a
one-command `koryph new` front door and cross-repo posture profiles — is
tracked in the [software-factory design](https://github.com/koryph/koryph/blob/main/docs/designs/2026-07-software-factory.md);
everything above ships today.
