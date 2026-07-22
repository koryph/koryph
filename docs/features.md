<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Features

Every major koryph capability on one page, grouped in the order you meet
them: adopt a repo, plan work, build in parallel, gate what lands, govern
the spend, recover from failure, operate the fleet, protect the repo, ship
a release. Each entry is a summary with a link to the chapter that operates
it — this page is the map, not the territory.

New to koryph? Read the [Concepts overview](concepts/index.md) for the ideas
in order, or [The lifecycle](concepts/lifecycle.md) for how these features
chain into one loop from prompt to release. Items marked **new** shipped in
the current release.

---

## Adopt — one command to a working factory

- **The `koryph adopt` wizard** *(new)* — point koryph at any existing git
  repo and one command takes it to a green `koryph validate`: detect →
  plan → consent → execute → verify. It installs missing prerequisites with
  consent, initialises and hardens beads, derives the account, gate, forge,
  and `area_map`, installs the agent scaffolding, and offers one signed
  adoption commit. Re-running it is a free health check.
  → [koryph adopt](user-guide/adopt.md)
- **The `/koryph-adopt` skill** *(new)* — once one repo is adopted, an agent
  session can drive the same wizard conversationally for any other repo.
  → [koryph adopt](user-guide/adopt.md#the-koryph-adopt-skill)
- **Agent-drivable onboarding** — `koryph adopt <root> --yes --json` is the
  whole onboarding runbook in one non-interactive command that fails closed
  on anything ambiguous; [`llms.txt`](llms.txt) carries the manual fallback
  so any AI session can adopt koryph unassisted.
  → [Quickstart](user-guide/quickstart.md)
- **Lower-level verbs stay put** — `koryph project add`, `install-assets`,
  `validate`, and `doctor` keep their contracts; `adopt` sequences them.
  → [Projects & accounts](user-guide/projects-and-accounts.md)

## Plan — describe it, get a dispatchable task graph

- **Intent routing** *(new)* — describe what you want to build, change, or
  fix in a normal agent session; the installed `koryph-intent.sh` hook
  detects work-shaped prompts and routes the session to the right planning
  command instead of letting it implement ad hoc. Advisory, fail-open,
  byte-frugal.
  → [From prompt to beads](user-guide/describing-work.md)
- **`/koryph-design`** *(new)* — the front door for feature-sized asks: it
  clarifies the ask, grounds it in your actual repo, writes a design doc,
  **stops for your approval**, then hands off to decomposition.
  → [From prompt to beads](user-guide/describing-work.md#koryph-design)
- **Planning skills** — `/koryph-plan` decomposes a design doc into an epic
  plus dependency-linked, footprint-labelled child beads; `/koryph-import`
  converts existing `ROADMAP.md`/`TODO.md` corpora; `/koryph-issue` files a
  single well-formed issue; `koryph plan` analyses a corpus for
  conflicts.
  → [From prompt to beads](user-guide/describing-work.md)
- **Beads and the ready-graph** — work lives in
  [beads](https://github.com/gastownhall/beads), a dependency-aware issue
  database that travels with the repo through its own git remote. The set of
  unblocked beads is the frontier the scheduler feeds from — no human
  dispatcher.
  → [Work: beads and the ready-graph](concepts/beads.md)
- **Issue intake** — pull GitHub issues (and other trackers) into the
  planning funnel as beads.
  → [Intake](user-guide/intake.md)

## Build — a fleet without merge conflicts

- **Footprint scheduler** — every bead declares what it touches
  (`area:*`, `fp:read:*` labels); only mutually conflict-free work
  dispatches together, which is what makes "run eight agents at once" safe
  rather than reckless.
  → [Parallelism: footprints](concepts/footprints.md)
- **Rolling dispatch** — slots refill continuously as work finishes; the
  fleet never idles waiting for the slowest member of a wave.
  → [Time: rolling dispatch](concepts/rolling-dispatch.md)
- **Worktree isolation** — each agent works in its own git worktree on its
  own branch; your checkout is never touched, and a misbehaving agent can be
  discarded without cleanup.
  → [Safety: worktrees and the green gate](concepts/worktrees.md)
- **Personas and model tiers** — tasks name the kind of worker they need
  (implementer, reviewer, architect, validator) and a tier
  (frontier / standard / light) rather than a hard-coded model; each runtime
  maps tiers to its own models.
  → [People: accounts and personas](concepts/accounts.md)
- **Runtime-neutral core** — the adapter seam, `runtime:<name>` labels, and
  per-provider quota blocks are built; **Claude Code is the only production
  runtime today** and everything else is alpha — dispatch to an unshipped
  runtime is refused fail-closed rather than guessed at.
  → [AI runtimes: support status](user-guide/runtimes.md)

## Gate — nothing lands that doesn't pass

- **Review pipeline** — every finished branch gets a one-shot reviewer whose
  findings block the merge until addressed; then rebase onto current `main`.
  → [Running waves](user-guide/running-waves.md#review-bounces)
- **The green gate** — your project's own commands are the merge gate. A
  real example, verbatim from koryph's own `koryph.project.json`:

    ```json
    "gate": [
      "test -z \"$(gofmt -l .)\"",
      "go build ./...",
      "go vet ./...",
      "go test ./...",
      "make lint",
      "make reuse"
    ]
    ```

    If any command exits non-zero, the branch does not land. The gate is
    yours: swap in `npm test`, `cargo clippy`, `pytest` — koryph never
    chooses your toolchain.
    → [Safety: worktrees and the green gate](concepts/worktrees.md)
- **Merge policies** — `auto` (fast-forward when review is clean), `manual`
  (operator lands it), or `pr` (push the branch and open a PR for protected
  default branches, landed later with `koryph land`, fast-forward only).
  Epic labels override project config per subtree.
  → [Running waves](user-guide/running-waves.md#merge-policies)
- **Protected paths** — merges touching CI workflows, hooks, or policy files
  are refused outright regardless of gate results; a human lands those
  deliberately.
  → [Safety: worktrees and the green gate](concepts/worktrees.md)
- **Merge reconcilers** — derived artifacts (lockfiles, generated indexes)
  collide at merge even when their inputs don't; declared reconcilers let
  those residual collisions self-heal.
  → [Merge reconcilers](user-guide/merge-reconcilers.md)
- **Epic validation** — after the last child of an epic merges, a
  frontier-tier validator reviews the union of everything that shipped for
  completeness (did it meet the design, in letter and spirit?) and
  structural health (duplication, architecture drift). Gaps become follow-up
  beads and re-enter the loop; a passing epic files a docs-update bead
  before it closes.
  → [Epic validation](user-guide/epic-validation.md)

## Govern — the machine, the money, and the rate limits

- **Resource governor** — footprints protect the merge; resources protect
  the machine. Beads declare external runtime demand (`res:kind-cluster`,
  `res:docker`, `res:dev-server`); each kind has a counted capacity on this
  host, so two 6&nbsp;GB dev clusters never co-dispatch, and leak detection
  attributes anything left behind.
  → [Machine: resources](concepts/resources.md)
- **Memory admission** — dispatch subtracts every ramping lease's declared
  memory reservation before admitting the next agent, so a wave can't pass
  the free-RAM check and then thrash the host mid-provision.
  → [Machine: resources](concepts/resources.md#reservation-aware-memory-admission-and-the-ramp-window)
- **Adaptive concurrency governors** — per-provider, per-account pools with
  AIMD adaptation: rate-limit responses halve the cap immediately (with
  settle windows and circuit breakers to prevent thrashing); sustained
  success probes it back up. The fleet runs at the edge of what your
  provider allows and never past it.
  → [Money: governors and quota](concepts/governors.md)
- **Subscription-first billing** — dispatch rides your flat-rate CLI
  subscription; per-token API spend requires explicit opt-in and only after
  the subscription window is exhausted.
  → [Billing & quota](user-guide/billing-and-quota.md)
- **Quota tracking and calibration** — live burn against your plan's 5-hour
  and weekly windows, measured from a background transcript scan and
  calibrated against observed usage; a governor ladder warns at 90%,
  throttles at 94%, gracefully stops at 97%, and hard-stops at 99% — so the
  fleet never torches an allocation you needed for tomorrow.
  → [Billing & quota](user-guide/billing-and-quota.md)
- **Context economy** — token telemetry, cache-hit tripwires, prompt-prefix
  hygiene, and output caps keep agent context lean so quota goes to real
  work.
  → [Context economy](user-guide/context-economy.md)

## Recover — failure is an input, not an outage

- **Stall and death detection** — structured heartbeat monitoring flags a
  silent agent within minutes, and a health patrol sweeps for dead agents
  and stuck claims on a fixed cadence, auto-fixing what it safely can.
  *(new: patrol sweep, stale-park detection)*
  → [Recovery & escalation](user-guide/recovery.md)
- **Classified retries** — every requeue carries its cause (gate, merge,
  conflict, rate-limit, budget-kill) with a bounded retry budget; a
  budget-killed agent warm-resumes its own session instead of starting over.
  → [Recovery & escalation](user-guide/recovery.md)
- **Escalation to stronger models** — when a genuine fault is about to burn
  a bead's final attempt on a cheap tier, that attempt runs on the frontier
  tier instead, and the escalation is recorded as durable provenance.
  Escalation counts *faults*, never environment noise. *(new: fault-only
  counting)*
  → [Recovery & escalation](user-guide/recovery.md#escalation)
- **Learned model routing** — `koryph models` mines escalation history
  and pre-labels similar work to start on the stronger tier directly;
  enable `adaptive_escalation` to run the pass at every wave boundary.
  → [Recovery & escalation](user-guide/recovery.md#learned-model-labels)
- **Operator overrides that stick** *(new)* — `koryph merge --close-bead` on
  a live loop records your manual merge in an override sidecar the engine
  folds in (instead of clobbering your hand-work); `koryph inject` adds a
  bead to a running loop without a restart; `koryph status --frontier` shows
  exactly why each ready bead did or didn't dispatch last wave.
  → [Recovery & escalation](user-guide/recovery.md#the-operators-hand)

## Operate — watch and steer, from any terminal

- **Terminal cockpit** *(overhauled this release)* — `koryph tui` is a full
  cockpit over SSH: live threads with stall flags and escalation markers,
  epic burndown with P50/P90 ETAs, a filterable event feed, governor and
  quota gauges, estimator calibration, token economy, a hierarchical queue,
  and a live activity tail that follows an agent's thinking and tool calls
  in real time.
  → [Terminal cockpit (TUI)](user-guide/tui.md)
- **One-shot views** — `koryph board` (fleet overview), `koryph roster`
  (per-bead lifecycle), `koryph status [--frontier]`, `koryph tail`.
  → [Quickstart](user-guide/quickstart.md#step-3-read-the-board)
- **Live steering** — `koryph nudge` (drop a note into a running agent's
  inbox), `stop` (graceful, never SIGKILL), `drain` (wind down), `resize`
  (change concurrency mid-run).
  → [Running waves](user-guide/running-waves.md)
- **Doctor** — one command reports drift across settings, signing,
  credentials, release infra, zombie leases, orphan worktrees, and stranded
  epics — with `--fix` for what's safely automatic.
  → [Doctor](user-guide/doctor.md)
- **Observability** — structured JSONL logs, traces, and metrics under
  `~/.koryph/telemetry/`, queryable with jq/DuckDB, with optional OTLP
  export. No telemetry ever leaves your machine otherwise.
  → [Observability](user-guide/observability.md)
- **VS Code extension** — the same cockpit data in your editor: tree view,
  transcripts, quota status bar.
  → [VS Code extension](user-guide/vscode-extension.md)

## Protect — hygiene as code

- **Account safety** — each project pins the account its agents run under;
  identity is verified fail-closed before any dispatch, never inherited from
  whatever shell happens to be logged in.
  → [People: accounts and personas](concepts/accounts.md)
- **Posture profiles** — branch protection, repo settings, and scanner
  presets as named, diffable, applyable bundles (`koryph posture`), with the
  built-in `oss-solo-maintainer` profile as the opinionated default.
  → [Posture profiles](user-guide/postures.md)
- **Repo settings as IaC** — rulesets and repo settings live as committed
  JSON; `koryph repo check` exits non-zero on drift, `apply` is diff-first
  with snapshots and rollback.
  → [Zero to shipped](user-guide/zero-to-shipped.md#stage-4-pin-repository-hygiene-protect)
- **Vault-served signing** — SSH commit signing with keys resolved on demand
  from Proton Pass, 1Password, macOS Keychain, or an encrypted file — never
  plaintext on disk by default.
  → [Signing](user-guide/signing.md)
- **Agent containment** — dispatched agents get a credential-free,
  allowlisted environment; worktree and boundary guard hooks confine them to
  their own tree and deny orchestrator-only operations. Defense in depth,
  stated honestly: hooks are controls, not a sandbox.
  → [Security](security.md)

## Ship — releases someone else can trust

- **The release train** — conventional commits accumulate into a Release PR;
  merging it triggers gate-before-tag, an artifact build (GoReleaser or your
  own commands — any language), and a draft-until-complete release.
  → [Shipping: the release train](concepts/release-train.md)
- **Supply chain by default** — SPDX SBOMs, keyless cosign signatures, and
  SLSA build provenance attach before anything publishes; releases are
  immutable and [verifiable by anyone](user-guide/supply-chain.md).
  → [Verifying a release](user-guide/supply-chain.md)
- **The release bot** — a vault-backed bot identity provisioned in one
  browser click so Release PR checks flow unaided, with graceful fallbacks
  when you can't install one.
  → [Release bot](user-guide/release-bot.md)
- **CI setup** — `koryph ci setup` renders forge-native pipelines that run
  your gate on every PR, for GitHub or GitLab.
  → [CI pipeline setup](user-guide/ci-setup.md)
- **Docs publishing** — a Zensical/MkDocs book published to your forge's
  Pages on every docs push, custom domain included.
  → [Release pipeline setup](user-guide/release.md)

---

## Customize any of it

koryph is opinionated about process, never about your project — and every
opinion above has a dial. The checked-in `koryph.project.json` carries your
gate commands, `area_map`, protected paths, merge policy, concurrency cap,
per-stage personas and model tiers, resource vocabulary, epic-validation
rounds and validator model, and adaptive-escalation thresholds. Machine-side,
`~/.koryph/governor.json` sets per-provider caps, resource capacities, and
memory floors, and posture profiles are plain JSON you can fork.
See [Projects & accounts](user-guide/projects-and-accounts.md) for the full
schema, and [Epic validation](user-guide/epic-validation.md#configuration)
for a fully-worked gating config.

## Looking ahead

> **Aspirations, not commitments.** Everything above this section ships
> today; everything below is direction. Nothing here oversells.

- **koryph across a cluster.** Today koryph's ceiling is one machine — the
  governor's capacity ledger, the resource kinds, and the worktrees are all
  host-scoped. We aspire to a **Kubernetes operator** that runs koryph
  across a cluster: fleets scheduled over nodes, resource kinds mapped to
  cluster capacity, the same footprint and gate discipline at rack scale.
  Be clear-eyed about the economics before wanting this: **a single laptop
  can already exhaust a typical subscription plan's allocation**, so
  cluster-scale koryph is inherently a **pay-per-token** proposition. The
  operator will be the right tool for two kinds of users — those running
  **their own GPUs** with self-hosted models, and those with the **budget
  to pay per token on frontier models**. For everyone else, koryph's
  subscription-first defaults will keep protecting the flat-rate case, and
  API spend will always be explicit opt-in.
- **More runtimes, verified.** The adapter seam is built and the alpha
  table is public — see [AI runtimes: support status](user-guide/runtimes.md).
  We intend to grow past a single vendor as fast as adapters can clear the
  safety bar, and [contributions are welcome](community.md).
- **A greenfield front door.** `koryph adopt` onboards existing repos; a
  planned `koryph new` will scaffold repo, license, CI, beads, posture,
  signing, and release train in one shot — tracked in the open in
  `docs/designs/`.
- **Evolving with the providers.** The AI vendors are moving fast — hosted
  agent harnesses, session checkpointing, new quota models. koryph will
  evolve as their tools evolve; the constants are the discipline (footprints,
  gates, provenance) and the fence (local-first, ejectable, yours).

## Where to next

- **Try it** — [Installation](user-guide/installation.md) →
  [Quickstart](user-guide/quickstart.md): install, `koryph adopt`, first
  dry-run wave, in about ten minutes.
- **Understand it** — the [Concepts track](concepts/index.md) teaches the
  ideas in dependency order; [The lifecycle](concepts/lifecycle.md) shows
  them as one loop.
- **Compare it** — [How koryph compares](compare.md) maps the 2026 agent
  orchestration landscape honestly.
