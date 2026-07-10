<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Architecture

koryph is an AI software factory built on three pillars — **Build** (the agent
factory), **Protect** (hygiene as code), and **Ship** (the release train). The
document below maps the Build pillar in depth, because that is the engine's hot
path; Protect and Ship are covered in [Signing](user-guide/signing.md),
[Releasing projects](user-guide/releasing-projects.md), and the repo-settings
IaC under `.github/`. For the end-to-end journey across all three, see
[Zero to shipped](user-guide/zero-to-shipped.md).

At its core koryph drives autonomous coding agents through a repeating **wave
loop**: it reads ready work from beads, schedules a conflict-free batch,
dispatches each bead to a headless agent runtime (the `claude` CLI by default)
running in an isolated git worktree, polls the agents to completion, reviews
and merges the green ones, and closes the bead. Every stage is a distinct Go
package so it can be swapped, mocked, or re-entered on recovery without
dragging the rest of the pipeline along.

See the [enhancement roadmap](https://github.com/koryph/koryph/blob/main/docs/designs/2026-07-enhancement-roadmap.md)
(kept in-repo, not in the published book) for design rationale and migration
history.

## Component map

Data flows left-to-right through the wave; the quota governor, ledger, and
registry audit are cross-cutting and touch every dispatch.

```mermaid
flowchart LR
  subgraph cli[cmd/koryph]
    RUN[koryph run]
  end

  subgraph engine[internal/engine · wave loop]
    REG[registry lookup]
    VER[account verify<br/>fail-closed]
    SCAN[beads frontier scan]
    SCHED[sched<br/>footprint batching]
    GOV{{quota governor<br/>OK·Warn·Drain·Stop}}
    PROMPT[promptc<br/>cache-stable prompt]
    DISP[dispatch<br/>claude CLI · subscription-first]
    POLL[poll<br/>heartbeat + manifest]
    STAGES[stages<br/>post-implement pipeline]
    REVIEW[review<br/>security / merge-readiness]
    MERGE[merge<br/>rebase · green gate · ff-merge]
    CLOSE[bd close]
  end

  RUN --> REG --> VER --> SCAN --> SCHED --> PROMPT --> DISP --> POLL --> STAGES --> REVIEW --> MERGE --> CLOSE
  GOV -. gates dispatch/width .-> SCHED
  GOV -. preflight refuse .-> DISP

  REGST[(~/.koryph<br/>registry · quota · audit)]
  LEDGER[(.plan-logs<br/>ledger + manifest v2)]
  BEADS[(.beads<br/>Dolt task graph)]
  WT[[project worktrees]]

  REG --- REGST
  GOV --- REGST
  VER --- REGST
  SCAN --- BEADS
  CLOSE --- BEADS
  DISP --- WT
  STAGES --- WT
  MERGE --- WT
  DISP --- LEDGER
  POLL --- LEDGER
  MERGE --- LEDGER
```

## Module map

| Path | Role |
|---|---|
| `cmd/koryph` | CLI entry point — key verbs: `run`, `project`, `init`, `onboard`, `validate`, `quota`, `batch`, `stop`, `tail`, `nudge`, `merge`, `land`, `review-pr`, `pr-sync`, `signing`, `governor`, `doctor`, `metrics`, `agents`, `commands`, `rules` (non-exhaustive; `ops.go` is a source file, not a command) |
| `internal/engine` | wave loop (scan → batch → preflight → dispatch → poll → stages → review → merge → record) |
| `internal/registry` | multi-project registry + audit log (`~/.koryph`, git-backed) |
| `internal/account` | Claude env construction + fail-closed identity verification |
| `internal/dispatch` | dispatch backend (headless `claude` CLI, subscription-first) |
| `internal/anthro` | direct Anthropic API + Message Batches (explicit only) |
| `internal/beads` | bd adapter (ready graph, labels, merge slot, children) |
| `internal/sched` | footprint conflict coloring + wave building |
| `internal/ledger` | run ledger + checkpoint manifest v2 + resume classification |
| `internal/worktree` | worktree lifecycle (ensure/bootstrap/remove) |
| `internal/merge` | rebase → green gate → ff-merge + protected paths |
| `internal/quota` | per-account usage windows + Warn/Drain/Stop governor (cost) |
| `internal/govern` | machine-global concurrency cap across projects (rate-limit safety); optional AIMD adaptive overlay with settle windows, circuit breaker, and dispatch smoothing |
| `internal/modelroute` | stage/label model resolution + rationale |
| `internal/promptc` | cache-stable prompt compiler |
| `internal/review` | optional security-reviewer / merge-readiness pass |
| `internal/stage` | post-implement pipeline stages (docs/test/…) run in-worktree before merge |
| `internal/version` | `engine_version` pinning (semver-minimum satisfaction) |
| `internal/project` | per-project adapter config (`koryph.project.json`) |
| `internal/onboard` | project onboarding/migration (dry-run first) |
| `internal/scaffold` | hash-aware installer for embedded `.claude` assets (force-guarded) |
| `internal/commands` | embedded `koryph-*` Claude slash commands + installer |
| `internal/rules` | hook scripts + additive `.claude/settings.json` merge (enforcement wiring) |
| `hooks/` | shipped Claude Code hooks (agent-boundary guard, worktree guard) |
| `agents/` | global fallback personas for projects with no local `.claude/agents/*` |

## One wave, end-to-end

```mermaid
sequenceDiagram
  participant E as engine
  participant Q as quota governor
  participant G as concurrency governor
  participant A as account/registry
  participant S as sched
  participant D as dispatch (claude CLI)
  participant W as worktree
  participant M as merge
  participant B as beads

  E->>A: registry.Get(project) + version.Satisfied
  E->>A: account.VerifyExpected(profile, expected)
  alt identity mismatch / unverifiable
    A-->>E: error → ExitFatal (whole wave blocked)
  end
  loop each wave until drained / quota pause
    E->>Q: governor() → level, calibrated, usage
    Q-->>E: level (OK/Warn/Drain/Stop) + ScaleSlots width
    E->>G: RefreshDemand → EffectiveCap (static or AIMD)
    E->>B: adapter.Ready(parent) → frontier
    E->>S: BuildWave(issues, max=min(width,cap), active=in-flight footprints) → items
    opt calibrated & enforcing
      E->>Q: Preflight(usage, estimate)
      Q-->>E: refuse → no new dispatch this wave
    end
    loop each item (staggered)
      E->>G: Acquire lease (fair-share + smoothing)
      alt denied (cap/fair-share/smoothing/breaker)
        G-->>E: defer item to next tick
      else granted
        E->>D: promptc.Compile + backend.Dispatch(spec)
        D->>A: re-verify identity (belt-and-braces)
        D->>W: launch headless claude in worktree branch
        E->>B: adapter.Claim(bead) · write ledger slot + manifest v2
      end
    end
    E->>D: poll every poll_seconds — status.json heartbeat + git commits
    D-->>E: agent process exits
    opt AIMD adaptive on
      E->>G: ReportRateLimit if rate-limited death detected
    end
    E->>G: Release lease
    E->>E: review (Opus) — blocking findings?
    alt blocking & ReviewIters < 2
      E->>D: requeue with reviewPath (--resume session)
    else clean
      E->>M: merge-slot mutex → protected-path check → rebase → green gate → ff-merge
      alt gate red / conflict / protected
        M-->>E: slot Failed/Conflict — worktree kept, bead left open
      else green
        M->>B: ff-merge landed → bd close
      end
    end
  end
```

## Wave vs rolling dispatch modes

```mermaid
flowchart TD
  subgraph wave["wave mode (default)"]
    direction TB
    WS[Scan frontier] --> WB[Build batch\nup to width]
    WB --> WD[Dispatch all items]
    WD --> WP[Poll until ALL slots idle]
    WP --> WM[Merge / close each]
    WM --> WS
  end

  subgraph rolling["rolling mode (dispatch_mode: rolling)"]
    direction TB
    RS[Scan frontier] --> RB[Build batch from\nfree capacity]
    RB --> RD[Dispatch free slots\nwith in-flight gating]
    RD --> RP[Poll tick — poll_seconds]
    RP --> RC{any slot freed?}
    RC -- yes --> RS
    RC -- no --> RP
    RS --> RM[Merge / close\ncompleted slots]
    RM --> RS
  end
```

**Key difference**: in rolling mode each poll tick recomputes free capacity and refills
immediately, so a slot that lands early does not sit idle while its wave-mates run.
In-flight footprints are passed to `BuildWave` on every refill tick so new candidates
cannot conflict with already-running beads.

## Adaptive governor (AIMD) state

```mermaid
stateDiagram-v2
  [*] --> Closed : adaptive enabled\n(DynamicCap = seed)
  Closed --> Closed : quiet for probeInterval (5 min)\n→ DynamicCap += 1 (up to HardMax)
  Closed --> Closed : rate-limit event received\n→ DynamicCap ÷= 2 (or 4 on burst)\n   SettleUntil = now + settle_seconds
  Closed --> Open : rate-limit at floor (cap=1)\nOR 3 decreases in 10 min
  Open --> HalfOpen : break_seconds elapsed\n→ admit ONE probe lease
  HalfOpen --> Closed : probe Release — no rate-limit\n→ DynamicCap = 1, reset reopen count
  HalfOpen --> Open : probe ReportRateLimit\n→ break duration doubles (≤ 3600 s)
  Open --> Open : rate-limit events counted only\n(admission already 0)
  HalfOpen --> Open : probe lease disappears\n(crash timeout 30 min)\n→ conservative re-open
```

While `BreakerState = open`, `Acquire` denies every new lease machine-wide (running
agents are never interrupted). A settle window freezes cap changes in both directions
for `settle_seconds` after any `DynamicCap` change, so a burst of concurrent
rate-limit events halves the cap once rather than once each. Dispatch smoothing adds
a `min_dispatch_interval_seconds` jittered spacing between admitted dispatches to
prevent thundering-herd refills when the cap rises. All three mechanisms are
Adaptive-gated — zero effect when the overlay is off.

## State ownership

koryph is deliberate about *where* each kind of state lives and how durable
it is. Three stores plus the worktrees, with no overlap:

| Layer | Owns | Lifetime / sync |
|---|---|---|
| `~/.koryph/` | Project registry (`registry.d/<id>.json`), account map, per-account quota calibration, cross-project run index, `audit.jsonl` | Itself a git repo — every mutation is an atomic write + audit append + commit, reversible |
| `<project>/.plan-logs/` | Run ledgers, checkpoint manifests (`koryph/<run>/<bead>/manifest.json`), per-dispatch `status.json` / `SUMMARY.md` / `session.log` | Repo-local; records *where things stand*, but the durable checkpoint is the worktree commit, not the manifest |
| `<project>/.beads/` | Task/plan state, dependency graph, `koryph-plan` blocks, merge/model/risk labels | Project-local Dolt DB; syncs cross-machine via its own Dolt remote — never through worktree git merges |
| `<project>` worktrees | In-flight agent work (committed + uncommitted) | Ephemeral; only as durable as its last commit; never removed while dirty without approval |

Rule of thumb: **cross-project** state lives in `~/.koryph/`;
**per-project durable** state lives in beads and `.plan-logs/`; **in-flight**
state lives in the worktree and is only as durable as its last commit.

## The wave loop

`engine.Run` sets up once (registry lookup, version check, identity
verification, run lock) and then calls `loop`, which repeats until the frontier
drains, the governor pauses on quota, the context is cancelled, or `--once`
settles exactly one wave.

**Two dispatch loop variants** are selected by `dispatch_mode` in
`koryph.project.json` (overridable per run with `--dispatch-mode`):

- **`wave`** (default) — dispatch a batch, then wait for **every** slot in it
  to land before scanning again. Simple and predictable; a slot that frees
  early idles until its wave-mates finish.
- **`rolling`** — continuously refills: every poll tick recomputes free
  capacity from the count of currently-running slots and tops off any slot that
  freed without waiting for the rest of the batch. A slot that lands early is
  refilled on the next tick.

Both modes share the same scan/preflight/dispatch/poll/merge primitives; only
when the next scan happens differs. `--once` always runs one single-pass wave
and exits, in either mode.

Each iteration:

1. **Govern.** `governor()` loads quota config and snapshots usage, returning a
   `Level` and whether the account is `calibrated`. The billing-guard mode is
   resolved, and `ScaleSlots` may shrink the wave width below the configured
   maximum as usage climbs.
2. **Scan the frontier.** `beads.Ready` returns issues with no open blockers,
   optionally scoped to a `--parent` epic.
3. **Build the wave.** `sched.BuildWave` filters to eligible, dispatchable
   issues and greedily packs a conflict-free batch up to the width (see
   *footprint batching*). In rolling mode the active in-flight footprints are
   passed as `sched.Opts.Active` so freshly-built batches never clash with
   already-running beads.
4. **Preflight.** In loop mode on a calibrated, enforcing governor,
   `quota.Preflight` can refuse the whole wave if its estimated spend would
   breach the drain fraction.
5. **Dispatch.** For each item (optionally staggered by
   `dispatch_stagger_seconds`), `dispatchBead` routes a model, ensures a
   worktree + bootstrap, compiles a prompt, launches the backend, claims the
   bead, and writes a ledger slot + manifest.
6. **Poll.** `pollUntilIdle` ticks every `poll_sec` (default 10, configurable
   via `poll_seconds` in `koryph.project.json` or `KORYPH_POLL_SEC`), reading
   each slot's `status.json` heartbeat and counting git commits ahead of the
   base branch until every slot reaches a terminal state.
7. **Stages, review, merge, record.** A completed slot first runs any configured
   post-implement `pipeline` stages (docs/test/…) in its worktree; then clean
   slots are reviewed and merged. Requeues refresh the worktree onto current main
   first, so a retry never runs a stale checkout. The ledger and manifest are
   updated so a later `--resume` can re-classify anything left running.

**Footprint batching.** A bead's *footprint* is split into **read** and
**write** token sets. Two footprints conflict only when they share a token *and*
at least one side holds it as a write (RWMutex semantics: two readers of the
same token co-run without conflict). Tokens are derived in precedence order:
`fp:read:<token>` labels → read tokens; `fp:<token>` labels → write tokens
(existing grammar, unchanged); `area:*` labels mapped through the project's
`AreaMap` → write tokens; else `TokenUnknown` (always a write, serializing
unlabeled beads). `BuildWave` greedily colors the frontier: two beads whose
footprints conflict never land in the same wave, and in rolling mode a
candidate conflicting with *any in-flight bead* is additionally deferred until
that bead lands. Epics, features, decisions, merge-requests, `no-dispatch` /
`refactor-core` / `gt:*`-gated issues, already-active beads, and containers
with open children are deferred with a recorded reason.

## Account safety model

Account selection is the first gate, not an afterthought. Before any state is
touched — no lock, no run dir, no worktrees — `account.VerifyExpected` reads
the profile's `.claude.json`, extracts `oauthAccount.emailAddress`, and
compares it case-insensitively against the registry's `ExpectedIdentity`. A
missing file, unparseable JSON, empty email, or mismatch **fails closed**: the
run exits fatal rather than dispatching under a guessed account.

The environment is built explicitly by `account.Env`, never inherited from
ambient shell state. The child environment is built from a credential-free
**allowlist** (`account.ChildEnv`): only known-safe operational variables pass
through, so tokens (`GH_TOKEN`, `VAULT_TOKEN`, `AWS_*`) and the operator's
ambient `SSH_AUTH_SOCK` are dropped by omission. It then injects only
`CLAUDE_CONFIG_DIR` for a work/custom profile (a personal profile leaves it
unset and never points at `~/.claude`), `ANTHROPIC_API_KEY` when billing is
`BillingAPIKey`, and the **scoped signing socket** (a koryph-managed ssh-agent
holding only the commit-signing key). The dispatch backend re-verifies identity
per dispatch as belt-and-braces, recording the `VerifiedIdentity` on the ledger
slot. Headless agents run `--permission-mode dontAsk`, and the guard hooks
(agent-boundary + worktree) — installed under `KORYPH_HOME`, **outside** any
agent's writable worktree, so an agent cannot neuter its own guards —
deterministically block an agent from `git checkout main`, `git merge`,
`git push`, `bd close`, touching another worktree, or writing koryph's own
enforcement surface (`hooks/`, `.claude/`, `agents/`).

## Billing & quota governance

Every account carries two rolling usage windows: a 5-hour window (`Window5h`,
aligned to a fixed UTC grid) and a 7-day `Weekly` window. Each has a
`CeilingUSD` calibrated from the user's observed `/usage` percentage.
`Fraction()` is spent ÷ ceiling; an unmeasurable window reports `1.0` so the
governor **fails closed** rather than over-spending blind. Usage is measured by
`quota.Snapshot`, which prefers the `ccusage` CLI and falls back to scanning
local transcript `*.jsonl` files, and finally to `Source="unavailable"`.

The **machine-global concurrency governor** (`internal/govern`) is a separate,
orthogonal gate: it bounds the *number* of agents running across all projects
and processes, so independent `koryph run` invocations cannot collectively
breach the Claude API rate limits (429s). See
[docs/developer-guide/global-governor.md](developer-guide/global-governor.md)
for the full design. The short form: a flock-guarded `governor.json` stores the
cap; each engine acquires a lease per dispatch and releases it on slot
completion; fair-share allocation rotates the remainder across all projects with
ready work. An optional **AIMD adaptive overlay** (enabled with
`koryph governor set --adaptive`) turns the static cap into a congestion
controller — additive increase every 5 minutes of quiet, multiplicative
decrease on a rate-limit signal — hardened by settle windows, a circuit
breaker, and dispatch smoothing (see `koryph governor set --help` for all
knobs). Both governors gate every dispatch: the cost governor gates by dollars,
the concurrency governor gates by rate-limit safety; a dispatch proceeds only
when *both* allow it.

The governor maps the higher of the two window fractions to a level:

| Level | Fraction | Effect |
|---|---|---|
| `LevelOK` | `< 0.80` | Full-width dispatch |
| `LevelWarn` | `≥ WarnFraction` (0.80) | Log a warning; `ScaleSlots` starts shrinking width |
| `LevelDrain` | `≥ DrainFraction` (0.90) | No new dispatch; finish active slots |
| `LevelStop` | `≥ StopFraction` (0.95) | Pause the run (or, with explicit opt-in, switch to API-key billing) |

An account whose ceilings are both zero is *uncalibrated*: the governor
short-circuits to advisory `LevelOK` without probing usage.

**Billing-guard modes.** `guardMode` decides whether these throttling
constraints are *enforced* or merely *advisory*, with precedence: run flag
(`--no-billing-guard`) > project registry (`billing_guard=advisory`) > baseline
(an uncalibrated governor is advisory). In advisory mode the governor measures
and logs but never blocks dispatch and never switches billing. Enforce is the
default.

**Subscription-first.** Dispatch runs on the account's subscription by default
(`BillingSubscription`). Per-token API spend engages *only* at `LevelStop`,
*only* with `--allow-api-spend`, a registry `api_fallback=explicit`, and a
resolvable `APIKeyEnvVar` — logged loudly as the sole path to metered spend.
Message Batches (`internal/anthro`) is a separate, manual entry point: it
requires a purpose-named `KORYPH_BATCH_API_KEY` (it refuses ambient
`ANTHROPIC_API_KEY`) plus per-invocation confirmation, and is never invoked by
the loop, scheduler, or recovery.

## Model routing

`modelroute.Resolve` picks a tier per dispatch. The tiers are `TierHaiku`,
`TierSonnet`, `TierOpus`, and `TierFable`. Stage defaults: planning/design/
scoring/review → **Opus**; implement/docs/test → **Sonnet**; explore/debug →
**Haiku**. Precedence, highest first:

1. explicit `--model` flag
2. stage-scoped label `model:<stage>:<tier>`
3. plain label `model:<tier>`
4. run default (`--default-model`)
5. stage default

**Opus is the ceiling.** Escalation (`RecoveryUpgrade`, gated by
`EscalationTier`) always targets `TierOpus` and never Fable; that path is
structurally excluded. The engine escalates in-run: when a bead-fault requeue
(gate failure, review bounce, conflict, crash — never a transient merge error,
rate limit, or budget kill) is about to burn the FINAL `MaxAttempts` attempt on
a haiku/sonnet slot, that last attempt runs on Opus instead — provided Opus is
in the project's `AllowedModels`. The decision is recorded in the slot's model
rationale (`escalated from sonnet after 2 bead-fault attempts (…)`), rendered
as a trailing `↑` in the TUI's Model column and emitted as
`engine.slot.escalated` telemetry. Retries below the final attempt keep the
first attempt's model frozen (koryph-ehx). **Fable is explicit-only.** It resolves
only when the tier is Fable *and* the source was an explicit label/flag *and*
`TierFable` is in the project's `AllowedModels` (the default allowlist —
haiku/sonnet/opus — deliberately omits it). Persona frontmatter can contribute
an `effort` hint, but the resolved tier always wins over any persona `model`.
Every resolution carries a human-readable `Rationale` (e.g. `label
model:plan:opus`, `stage default (implement)`), recorded on the slot and
manifest.

## Recovery & native session resume

Each dispatch writes a **checkpoint manifest v2** (`manifest.json`) alongside
the ledger slot, capturing the session id, worktree, branch, base commit,
attempt, recovery tier, and merge policy. On `--resume`, `ledger.Classify`
inspects each non-terminal slot and probes the world to choose an action:

| Action | Condition | Behavior |
|---|---|---|
| `ActionSkip` | slot already terminal | leave recorded |
| `ActionReattach` | PID alive | keep polling |
| `ActionRequeueResume` | dead, commits present | re-dispatch resuming the session |
| `ActionRequeueFresh` | dead, no commits | fresh dispatch |
| `ActionBlocked` | attempts ≥ max | stop retrying |

**Checkpoint-with-the-work.** Git commits inside the worktree are the primary,
durable checkpoint; the manifest records *where things stand*, but recovery
trusts committed repo state over manifest claims when they disagree. When a
slot with prior commits is requeued, the manifest's `SessionID` drives a
**native resume** — the backend launches `claude --resume <id> --fork-session`
so the agent continues its own session rather than starting cold. A slot with
no commits is re-dispatched fresh. The recovery *tier* (`rt:0`..`rt:3`, label
overrides `risk_tier_default`) is recorded on the manifest to govern how
aggressively work is retried. Stuck detection (`stuck_sec`, default 900)
flags a slot informationally without killing the poll when it shows no sign
of life: no agent-written heartbeat, no commit, **and no process-cohort CPU
activity** — an agent blocked inside one long tool call (a build, a
Playwright e2e) burns CPU and is treated as alive even though it cannot
heartbeat. Slots with declared `res:*` resources get 4× the threshold
(known-slow external-resource work); the progress line says the process is
alive and that koryph is deliberately not interrupting it.

## Review, merge policies & protected paths

After an agent exits cleanly, an optional **review** pass runs the
`security-reviewer` persona (on Opus) diffing the branch against its base and
returning strict JSON `{blocking, findings}`. Review is best-effort: any
failure returns a degraded, non-blocking verdict so it can never wedge the
engine. Blocking findings **requeue** the slot with the review path attached
(the agent resumes to address them), up to 2 iterations; after that the policy
is forced to `manual` so nothing auto-merges unreviewed.

`merge.Merge` lands a green branch under a **bd merge-slot mutex** (one merge at
a time). The sequence: a **protected-path check** (`git diff --name-only
base...branch`) rejects the merge outright if it touches any protected path;
then `git fetch` + `git rebase` onto `origin/<default>` (or local default with
no remote); then the **green gate** runs each `cfg.Gate` command sequentially
via `sh -c` (wrapped in `direnv exec` when available) — any non-zero exit aborts
the merge and discards the dirtied tree; then `git merge --ff-only` (or
`--squash`); optional push; and worktree + branch cleanup (skipped if the tree
stays dirty). Results are `merged`, `conflict`, `gate-failed`, `protected`, or
`error`.

**PR path (`OpenPR`).** For a protected default branch (`merge_policy: pr`),
`merge.Merge` shares that entire preflight (mutex, protected-path check,
signature verification, sync, rebase, gate) and then **diverges** after the
gate: instead of the ff-merge it pushes `agent/<bead-id>` to origin and opens a
PR against the default branch through a `PROpener` (the `gh` CLI by default; an
interface so tests inject a fake). The worktree and branch are **kept** — the
default branch is never touched — so a later fast-forward landing step
(`koryph-ufy.4`) can resume them. Extra results: `pr-opened` (with the PR URL
and number), plus `pr-no-remote` / `pr-no-gh` when the prerequisites are absent
(the engine blocks the bead cleanly and keeps the branch for a `--resume`). The
engine parks the slot in the `pr-opened` ledger status; agents themselves never
push — the push and PR creation live in this engine merge path.

`DefaultProtected` covers `CLAUDE.md`, `MEMORY.md`, `CLAUDE-ACCOUNTS.md`,
`koryph.project.json`, `.claude/`, `.beads/`, `scripts/lib/`,
`.pre-commit-config.yaml`, `.gitignore`, `.github/CODEOWNERS`, and `.envrc`;
projects add `Extra` paths. A trailing `/` matches a subtree recursively.
Merge **policy** (`merge:auto` / `merge:manual` / `merge:pr` label on the epic,
else project config) decides whether a green branch merges automatically, waits
for a human, or opens a PR.

The routine CI/build subset (`LiftableProtected`: `.github/`, `Makefile`) is
**operator-liftable**: `koryph merge --allow-protected` / `koryph land
--allow-protected` land a legitimate CI or build bead through the normal
lock-safe, ledger-reconciling merge path instead of forcing a bare `git
merge` behind the loop's back. The lift is deliberately narrow: governance
defaults (`.claude/`, `.beads/`, `koryph.project.json`, hooks, agents, …) and
the project's `Extra` paths refuse even under the flag, and the engine's
auto-merge path can never set it — dispatched agents do not get to lift their
own sandbox.

## Versioning

The engine pins itself to each project. `project.Config.EngineVersion`
expresses a minimum, e.g. `0.2+` or `>=0.2.3`. `version.Satisfied` normalizes
both sides (strips leading `v`, trailing `+`, `>=`) and compares major.minor.
patch componentwise; an empty requirement is always satisfied. If the running
`koryph` doesn't satisfy the project's `engine_version`, `Run` exits fatal with
an upgrade instruction — a project can require newer engine semantics without
risking an older binary silently mis-driving it. The pinned `EngineVersion`
also flows into `promptc.Compile`, whose cache-stable preamble depends only on
that version — so the prompt cache stays warm across every dispatch of one
engine version, and rotates deliberately when the engine bumps.
