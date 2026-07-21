<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Internal Packages

One section per `internal/` package. Import paths are
`github.com/koryph/koryph/internal/<name>`.

## account

Constructs the child environment for a dispatch and verifies the logged-in
Claude identity, fail-closed. Supports `subscription` (OAuth) and `api-key`
billing modes.

- **`Profile`** — resolved account context (config dir, expected email)
- **`Identity`** — email address read from `.claude.json`
- **`BillingMode`** — `"subscription"` | `"api-key"`
- **`Env(p, billing, apiKey)`** — full `[]string` child env; scrubs/re-injects credentials
- **`Verify(ctx, p)`** / **`VerifyExpected(ctx, p, email)`** — read and compare identity
- **`Discover(ctx)`** — enumerate candidate profiles (`~/.claude.json`, `~/.claude-*/`) with verified identity + provenance, for the adopt wizard's account proposal

## adopt

Orchestration logic for the `koryph adopt` wizard (design:
docs/designs/2026-07-adopt.md): detect → plan → confirm → execute → verify,
sequencing the same primitives `project add` uses. The CLI glue (flags,
prompts, streaming) lives in `cmd/koryph/adopt.go`; everything here is pure
or unit-testable.

- **`Detect(ctx, root)`** — read-only `Snapshot` (tools, platform, beads state, account candidates, gate/forge/area_map proposals, registry/config state)
- **`BuildPlan(snap)`** — the ordered `[]Step` plan (states `done|needed|offer|blocked`, each with a why)
- **`ResolveAccountNonInteractive`** / **`ResolveGateNonInteractive`** / **`ResolveForgeNonInteractive`** — fail-closed value resolvers for `--yes`/non-TTY
- **`ExecuteBeads`** / **`RegisterAndConfigure`** / **`InstallAssets`** / **`CommitAdoption`** — idempotent execute-phase steps

## agentjson

Helpers for parsing Claude CLI JSON-envelope output: the outer
`{result, is_error}` envelope whose `result` field carries the model's text,
which itself is expected to be strict JSON. The single authoritative
implementation shared by `internal/review` and `internal/epicreview` so
escape/`is_error` edge-case fixes propagate to every caller.

- **`ParseEnvelope(out)`** — unwrap the CLI envelope, error on `is_error`
- **`SelectJSON(s, requiredKeys...)`** — pick the first *valid* JSON block
  carrying the schema keys (skips non-JSON brace tokens the model quoted)
- **`JSONBlocks`** / **`FirstJSONBlock`** / **`FencedJSONBlocks`** — balanced-block extraction
- **`Tail(s, n)`** — bound an error/log excerpt

## agentsmd

Installs the koryph operating contract as `AGENTS.md` at a managed project's
root — the canonical, runtime-neutral instruction file read natively by Codex,
Cursor, Grok, Copilot, opencode, amp, and (as a fallback) Claude Code; the
cross-runtime counterpart of `CLAUDE.md`. Installed unconditionally during
`project add` with the same hash-aware overwrite policy as
`scaffold.CopyEmbed` (identical → no-op, differing → skipped unless force).

- **`Install(root, force)`** — write `<root>/AGENTS.md`; returns a `scaffold.Action*` constant
- **`Template()`** — the embedded contract bytes

## anthro

Thin wrapper around `anthropic-sdk-go` for the two operations koryph uses
directly: single-message inference and pre-flight cost estimation.

- **`Client`** — holds SDK client; `NewClient(keyEnvVar)` resolves the key from env
- **`MsgReq`** — request envelope (model, messages, max tokens)
- **`Usage`** — input/output token counts; **`BatchResult`** — aggregate batch outcome
- **`EstimateUSD(reqs)`** — cost estimate for a slice of `MsgReq`

## beads

Adapts the `bd` CLI. The **only** package that shells out to `bd`; all other
packages consume its types.

- **`Adapter`** — wraps a repo root; `New(repoRoot)` is the constructor
- **`Issue`** — parsed beads issue (ID, title, status, labels, …)
- **`ReadyOpts`** — filters for `bd ready` queries

## bot

Implements the GitHub App Manifest flow for the `koryph bot` command family:
app creation (the localhost-redirect manifest dance), credential persistence,
and installation URL generation. Credentials live at
`~/.koryph/bots/<name>.json` (mode 0600) in **pointer mode** (a vault
`Provider` + `KeyRef`; `ResolveKey` fetches the PEM at JWT-mint time) or
legacy **inline mode** (PEM in the file, opt-in via `--plaintext`). GitLab has
no App identity, so the `*GitLab` variants run a project/group access-token
flow instead.

- **`Create`** / **`Attach`** / **`Check`** / **`List`** / **`Load`** — the `koryph bot` verbs (GitHub)
- **`CreateGitLab`** / **`AttachGitLab`** / **`CheckGitLab`** — the token-flow equivalents
- **`ResolveKey`** — fetch the private key from its vault pointer
- State: `~/.koryph/bots/<name>.json`

## ciinstall

Forge-native CI asset installation shared by `koryph ci setup` and future
installers. `Install` renders an asset kind through a `forge.CIService` and
writes it to the forge-native path (`gate` → `.github/workflows/koryph-gate.yml`
on GitHub, `.koryph/ci/koryph-gate.yml` includable fragment on GitLab);
`Check` compares the on-disk asset against the current render and reports
drift. Both are idempotent.

- **`Install(...)`** / **`Check(...)`** — the stable API; import, never copy
- **`KindPath(forgeName, kind)`** — forge-native destination for a kind
- **`AllKinds`** / action constants (`installed`/…)

## cockpit

The read-only data layer shared by the TUI and the VS Code extension:
assembles a per-project `Snapshot` from the run ledger, the beads adapter
(cached at a coarser TTL than the refresh tick), and the quota config —
file reads only, cheap enough to call every 100 ms. Also computes the
burndown, efficiency, and queue views, surfacing P50/P90 projections and an
explicit "insufficient history" state below `MinSamples` observations.

- **`Provider`** — `Refresh() (Snapshot, error)`; **`DetailProvider`** — optional per-bead detail
- **`NewLedgerProvider`** / **`NewGraphProvider`** — production constructors
- **`Snapshot`** — slots, queue, burndown, efficiency, events for one project

## commands

Ships the `koryph-*` Claude slash commands (`//go:embed koryph-*.md`) and
installs them into a project's `.claude/commands/`. Installed at onboarding so
koryph semantics hold whether `koryph` is run explicitly or implied by a
prompt.

- **`FS`** — embedded slash-command templates
- **`Install(root, force)`** — copy commands via `scaffold.CopyEmbed`

## dispatch

Launches one bead as a detached, headless `claude` process inside its
worktree and tracks the resulting PID/stream.

- **`Spec`** — everything needed to launch one agent (bead, env, paths)
- **`Handle`** — live process reference (PID, stream path, start time)
- **`Backend`** / **`CLIBackend`** — interface + production implementation over the `claude` binary
- **`ParseResultCost(streamPath)`** — extract USD spend from transcript
- **`Alive(pid)`** / **`StopGraceful(pid)`** — process lifecycle helpers

## doctor

Runs system-health checks against the `~/.koryph` state tree (global mode)
and per-project checks (project mode). All I/O and OS interactions are
injected so checks are unit-testable without touching the real filesystem or
spawning processes. Checks include zombie leases, orphan worktrees, GC
footprint, CI-gate and posture drift, asset drift, proxy loopback safety,
bot credentials, and unvalidated epics; some support `--fix`.

- **`Run(opts)`** / **`RunProject(opts)`** — execute checks, return a `*Report`
- **`Finding`** — one result (`Check`, `Level`, `Message`, `Fixed`); levels `ok`/`warn`/`error`
- **`Matrix`** — the integration-status matrix behind `koryph doctor --matrix`

## engine

The wave loop: scan → batch → preflight → dispatch → poll → stages → review →
merge. Main orchestration entry point called by `cmd/`.

- **`Options`** — full run config (project ID, wave width, dispatch mode, auto-merge, review flags, …)
- **`Outcome`** — summary counts (dispatched, merged, failed, blocked)
- **`Run(ctx, opts)`** — blocking entry point; returns when the run terminates

Internal files: `poll.go` (heartbeat/completion, rate-limit requeue, AIMD
signal forwarding), `recover.go` (resume logic), `wave.go` (engine-side wave
assembly, rolling-mode refill loop, in-flight footprint map), `pipeline.go`
(post-implement stage execution), `govern.go` (AIMD report-rate-limit helper).
Requeues refresh the worktree onto current main first (rebuild when no commits,
rebase when there are) so a retry never runs a stale checkout.

`DispatchMode` (from `Options` or `project.Config.DispatchMode`) selects
`wave` (wait-for-batch, default) or `rolling` (continuous-refill). Both modes
share poll primitives; rolling additionally passes active in-flight footprints
(`sched.Opts.Active`) to every `BuildWave` call so freshly-picked candidates
cannot clash with running beads. Poll interval is `poll_seconds` from project
config (default 10), overridden by `KORYPH_POLL_SEC` or `Options.PollSec`.

## epicreview

Whole-epic implementation validation (see
`docs/designs/2026-07-epic-validation.md`): after the last child of an epic
closes, `Validate` runs the `koryph-epic-validator` persona (opus by default,
with exponential-backoff retries) over the union of the epic's merged work and
returns a `Verdict` — met, gaps, and/or structural findings. `Act` applies a
verdict deterministically: stamps `validation:passed`/`parked`/`degraded`
labels, files gap and structural follow-up beads with round labels, files the
docs-update bead, and closes the epic when appropriate. Both the engine hook
and `koryph epic validate` call this package so the two paths cannot drift.

- **`Validate(ctx, Opts)`** — run the frontier validator, return a `Verdict`
- **`Act(ctx, BeadStore, ActOpts, Verdict)`** — deterministic verdict actuation
- **`BeadStore`** — the bd-verb subset `Act` needs (`*beads.Adapter` satisfies it)
- **`RoundLabel`** / **`DetectNextRound`** / **`LoadPriorVerdicts`** — round bookkeeping
- Labels: `LabelPassed`, `LabelParked`, `LabelDegraded`, `LabelStructural`, `LabelDocs`, `LabelNoValidate`

## execx

Runs external commands with an explicit working directory and environment.
All engine subprocess calls go through here.

- **`Cmd`** — command spec (bin, args, dir, env, timeout); **`Result`** — exit code + output
- **`Run(ctx, c)`** / **`MustSucceed(ctx, c)`** — execute; latter errors on non-zero exit
- **`BaseEnv(remove...)`** — current environment with named vars stripped
- **`LookPath(name)`** — reports whether `name` is on `$PATH`

## forge

The contract between koryph and hosted git forge services (GitHub, GitLab, …).
Contract-only: the `Forge` interface, `Capabilities` flags, and per-domain
service interfaces (`RepoService`, `ProtectionService`, `PRService`,
`SecretsService`, `ReleaseService`, `CIService`, `BotService`). Providers live
under `internal/forge/github/` and `internal/forge/gitlab/` and self-register
into `Default` via `init()`. Only the edges of the loop talk to a forge —
everything git-native (worktrees, merges, signing, the green gate) stays
forge-neutral.

- **`Forge`** / **`Capabilities`** — provider identity + feature flags
  (`DraftReleases`, `Rulesets`, `AppIdentity`, `WorkflowDispatch`, …); callers
  branch on capabilities, never provider names
- **`Default`** — the global `Registry`; **`ErrUnsupported`** — operation absent on this forge
- **`SniffRemote`** — detect the forge from a git remote URL

## fsx

Small filesystem helpers shared across the engine. All writes are atomic
(write-to-temp + rename) to prevent torn files on crash.

- **`WriteAtomic`** / **`WriteJSONAtomic`** — atomic byte-slice and JSON writes
- **`ReadJSON(path, v)`** — unmarshal file into `v`
- **`AppendLine(path, line)`** — append one newline-terminated record; **`Exists(path)`** — stat check

## gc

Data lifecycle management for koryph outputs: compress/delete old run
phase-dirs, size-rotate `audit.jsonl`/`runs.jsonl` (default retention:
forever), leave telemetry to `internal/obs` and posture snapshots exempt by
design. Config surface is `~/.koryph/retention.json` with per-project
overrides in `<repo>/.koryph/retention.json`; `"never"` is accepted for every
retention value. gc refuses to touch any run whose ledger shows non-terminal
slots, the active run, or the `latest` symlink target. See the
[gc user guide](../user-guide/gc.md).

- **`Run(Options)`** — apply the policy (honours `DryRun`); returns per-class `Result`
- **`Footprint(repoRoot)`** — reclaimable bytes without deleting (health patrol input)
- **`LoadConfig(repoRoot)`** — global + project overlay with defaults applied
- **`Config`** / **`RunDirPolicy`** / **`RotatePolicy`** — the retention.json schema
  (incl. `GCAuto`, the opt-in health-patrol auto-gc flag)
- State: `~/.koryph/retention.json`, `<repo>/.koryph/retention.json`

## govern

Machine-global concurrency governor: a cross-process cap on concurrently running
agents (across all projects) so independent `koryph run` invocations cannot
collectively breach the Claude API rate limits. Coordinates via lease + demand
files under `~/.koryph/slots` guarded by a flock — no daemon. See
[global-governor.md](global-governor.md).

- **`Store`** — `Acquire` (cap-checked reserve) / `Release` / `Prune` /
  `RefreshDemand` / `DropDemand` / `Snapshot` / `Cap` / `SetCap` /
  `SetAdaptiveCap` / `ReportRateLimit` / `EffectiveCap` / `AIMDStatus`
- **`Config`** — serialised `governor.json`; includes AIMD overlay fields
  (`Adaptive`, `HardMax`, `DynamicCap`) and settle/breaker/smoothing fields
  (`SettleSeconds`, `SettleUntil`, `BreakSeconds`, `BreakerState`,
  `MinDispatchIntervalSeconds`, …)
- **`Lease`** / **`Demand`** — on-disk records
- Fair-share: `floor(cap/n)` per demander, rotating remainder (no starvation)

**AIMD adaptive overlay** (`--adaptive`). When enabled, `EffectiveCap` floats
between 1 and `HardMax` instead of pinning to `MaxGlobalAgents`: +1 every
5 minutes of quiet (additive probe) / halve on a rate-limit signal (multiplicative
decrease). Hardened by three koryph-2im.11 mechanisms — all Adaptive-gated:

- **Settle window** (`SettleSeconds`, default 120 s): freezes further cap changes in
  either direction after any `DynamicCap` change; the probe clock anchors on settle
  expiry.
- **Burst-scaled decrease**: ≥3 distinct `(project, bead)` rate-limit events within
  30 s → divide by 4 instead of 2.
- **Circuit breaker** (`BreakerState`): opens when the cap is already at the floor or
  on 3 decreases within 10 minutes; denies all new admission machine-wide for
  `BreakSeconds` (default 300 s, doubling per consecutive re-open up to 3600 s);
  transitions half-open → admits exactly one probe → closes on clean release /
  re-opens on probe rate-limit.
- **Dispatch smoothing** (`MinDispatchIntervalSeconds`, default 3 s): machine-wide
  minimum inter-dispatch spacing, jittered ±50%, to prevent thundering-herd refills.

## intake

Polls a project's external issue tracker for trigger-labeled issues and files
one planning bead per issue, idempotently (a bead carrying the
`gh-<owner>/<repo>#<number>` external-ref is skipped). Every ingested bead is
labeled `no-dispatch` — an ingested issue is planning input a human or planner
must triage first; intake never mutates tracker state except the opt-in
comment-back. Sources: GitHub (via the `gh` CLI, never a raw token), Linear,
and JIRA, plus a multi-source runner.

- **`Run(ctx, Options)`** — one GitHub intake pass; **`RunLinear`** / **`RunJIRA`** / **`RunMulti`**
- **`Source`** — the pluggable issue-tracker provider interface
- Defaults: trigger label `triage`, limit 20

## ledger

Owns the per-run ledger (JSONL on disk) and per-slot checkpoints. Classifies
live runs to recommend the next orchestrator action.

- **`Store`** — ledger DB; `NewStore(repoRoot)` with per-slot `Lock`
- **`Run`** / **`Slot`** — top-level run and per-agent records
- **`Manifest`** — immutable dispatch record; **`PlanState`** — wave snapshot
- **`Decision`** — recommended action (merge, retry, abandon, …); **`Probe`** — current observations
- **`Terminal(status)`** — true if status is a final state
- **`Classify(run, probe)`** — returns `[]Decision` for the orchestrator

## merge

Lands a finished agent branch on the default branch after running the
configured green gate.

- **`Opts`** / **`Result`** — merge configuration and outcome
- **`SlotLocker`** — interface abstracting per-slot locking (for tests)
- **`Protected(diffPaths, extra)`** — returns protected paths hit by a diff
- **`Merge(ctx, o)`** — main entry: run gate → ff-merge (squash optional)
- **`RunGate(ctx, dir, cmds)`** — execute green-gate commands; returns `ok`, output
- **`Reconciler`** / **`reconcileRebase`** — auto-heal a pre-merge rebase
  conflict confined entirely to configured generated files (a migrations
  lockfile, a secrets baseline) by regenerating each from the post-merge tree
  and continuing, instead of aborting; all-or-nothing and gated
  (docs/user-guide/merge-reconcilers.md)
- **`runMergePrepare`** — post-rebase, pre-gate normalization of the rebased
  tree (renumber a migration to tip at merge time); koryph commits any change so
  it rides the ff-merge and is gated

## metrics

Rolls up burn-rate and reliability baselines from the run ledger for
reporting and quota decisions.

- **`ModelStat`** / **`ProjectStat`** / **`Report`** — nested stats types
- **`Collect(store, projectID)`** — read ledger, compute `*Report`
- **`Render(r, w)`** — pretty-print report to an `io.Writer`

## modellearn

Closes the escalation feedback loop (koryph-qf6.6): mines run ledgers for
beads that only merged after their final attempt escalated to a stronger tier,
aggregates that evidence by the similarity features frozen on each slot (area
label + size bucket), and recommends a starting tier for future beads sharing
those features. The actuator is deliberately a bead label, not a routing-table
entry: `Apply` stamps `model:<tier>` plus a `model-learned:<date>` provenance
marker on matching ready beads; any pre-existing `model:*` label wins, making
re-apply idempotent and human overrides durable.

- **`Collect`** / **`Recommend`** / **`Apply`** — mine evidence → propose tiers → label beads
- **`DefaultMinEvidence`** (2) — minimum escalated-then-merged count per bucket
- **`ProvenancePrefix`** — `model-learned:`

## modelroute

Resolves a (stage, bead-labels, run-defaults, project-config) tuple to a
`(model, effort)` pair, with persona-file overrides and recovery upgrades.
Precedence (koryph-v8u.10): bead `model:<tier>` label > persona `tier` (via
the active runtime's model map, project-overridable) > persona `model`
(legacy pin) > hardcoded stage default — see agents/README.md's "Resolution
precedence" section.

- **`Req`** / **`Resolution`** — request and resolved `(model, effort)`;
  `Req.RepoRoot`/`Req.ModelMap` opt a caller into the persona-tier step
- **`Resolve(r)`** — main entry; consults label rules, then persona tier/
  model, then defaults
- **`PersonaFor(stage, stages)`** — picks persona name from stage map
- **`RecoveryUpgrade(current)`** — the escalation target (always opus)
- **`EscalationTier(current, allowed)`** — allowlist-checked gate the engine
  consults before escalating a final bead-fault attempt (koryph-qf6.4);
  refuses opus/fable/unknown inputs
- **`TierForModelID(id)`** — normalizes a concrete model id (a result line's
  `modelUsage` key) to its tier for actual-model attribution (koryph-qf6.2)
- **`PersonaMeta(repoRoot, persona)`** — reads persona file →
  `(model, effort, tier)`

## netx

Shared network-address predicates used across koryph's security gates.
Centralised so independent copies cannot drift (design I4: loopback-only
routing for dispatched-agent Anthropic traffic).

- **`IsLoopbackHost(host)`** — the single authoritative loopback predicate
  (`localhost`, `127.0.0.0/8`, `::1`, IPv4-mapped forms); used by both the
  registry load-time validation and the doctor proxy check

## obs

koryph's observability foundation: a custom TRACE slog level, per-component
loggers with independently-settable minimum levels, and a swappable handler
pipeline (console / JSON / text / multi / OTLP-HTTP). Configured by
`~/.koryph/observability.json` with on-demand reload (no restart) and env
overrides (`KORYPH_LOG_LEVEL`, `KORYPH_LOG_FORMAT`, `KORYPH_OTEL_ENDPOINT`).
A central `RedactingHandler` scrubs every record so no secret reaches a
handler; canonical attribute keys (`run_id`, `bead_id`, `model_actual`, …)
keep logs, spans, and metrics correlated. Also owns telemetry-file rotation
and pruning (`PruneFromConfig`).

- **`Init(cfg, handler)`** / **`LoadConfig`** / **`ReloadConfig`** — startup + live reload
- **`For(component)`** — a component-scoped `*slog.Logger`
- **`RunAttrs`** / **`BeadAttrs`** / **`ForgeAttrs`** / **`Err`** — canonical attribute helpers
- **`RedactAttr`** / **`RedactValue`** — exported for no-secret-leak assertions
- State: `~/.koryph/observability.json`, `~/.koryph/telemetry/`

## onboard

Inspects a repository, registers it in the registry, and validates that all
prerequisites (hooks, beads, account identity) are satisfied.

- **`Inventory`** — survey result (remotes, hooks, beads status, worktrees)
- **`RegisterOpts`** — identity, billing, and flag overrides for `Register`
- **`Validation`** / **`Check`** — validate output (slice of named pass/fail checks)
- **`Inspect(ctx, root)`** — non-mutating survey
- **`Register(ctx, store, inv, opts)`** — write `registry.Record` from inventory
- **`Validate(ctx, store, projectID, w)`** — check all prerequisites, print results

## paths

Resolves all koryph machine-local state locations from `$KORYPH_HOME`
(default: `~/.koryph`). No exported types; all functions return `string`.

`KoryphHome` · `RegistryDir` · `QuotaDir` · `AuditLog` · `RunsIndex` ·
`PlanLogs(repoRoot)` · `KoryphRoot(repoRoot)`

## personas

Installs the fallback Claude sub-agent persona files (embedded from
`agents/` in the binary) into a project's `.claude/agents` using the shared
scaffold hash-aware, force-guarded copy policy — no network access at onboard
time. For non-Claude runtimes, `InstallForRuntime` rewrites each persona's
`model:` frontmatter through the target runtime's `ModelMap`, keyed by the
persona's `tier:` scalar, so a codex/cursor/grok project never receives a
Claude model name it cannot honor. (Rendering personas for those runtimes
works today; dispatching to them does not — the engine blocks non-Claude
runtimes fail-closed. See
[AI runtimes: support status](../user-guide/runtimes.md).)

- **`Install(root, force)`** — byte-identical copy (equivalent to runtime `"claude"`)
- **`InstallForRuntime(root, force, runtimeName)`** — tier-mapped render; also
  reports untiered personas

## plan

Corpus-level plan analysis. `Audit` performs a deterministic, read-only
conflict analysis of a project's open bead corpus under the current sched
rules (`FootprintFor` + `Conflicts`), surfacing unlabeled beads (the
`domain:unknown` serializers), non-dispatchable ready beads, dependency-
unordered conflicting pairs, and achievable vs. potential parallel width.

- **`AuditReport`** — the JSON-marshalable result (behind `koryph plan audit`)
- **`ConflictPair`** / **`WidthReport`** / **`ItemSummary`** / **`SkipSummary`** — report parts

## posture

Desired-state checking and applying for repository hygiene: branch-protection
rulesets (`.github/rulesets/*.json`) and administrative settings
(`repo-settings.json`), delegated to `gh api` passthrough — no token
management in-package. Named posture profiles compile forge-neutral `Intents`
to forge-native files (`CompileGitHub`); fragments, org-level rulesets,
snapshots, and rollback round out the `koryph posture`/`koryph repo` surface.
Posture snapshots are never auto-deleted (exempt from gc by design).

- **`CheckRulesets`** / **`ApplyRulesets`** / **`CheckSettings`** / **`ApplySettings`** — diff-first check/apply
- **`CheckOrgRulesets`** / **`ApplyOrgRulesets`** — org-level equivalents
- **`CompileGitHub(intents, params, ghDir)`** — profile → GitHub-native desired state
- **`CaptureSnapshot`** / **`Rollback`** — pre-apply state capture + restore
- **`Source`** — desired-state provider seam (`LocalSource` reads `.github/`)

## project

Loads the per-project adapter configuration (`koryph.project.json`).

- **`Config`** — wave width, green-gate commands, footprint rules, dispatch mode,
  poll interval (`PollSeconds`), and AIMD-adjacent knobs
- **`FootprintRule`** — path-pattern → conflict-scope mapping
- **`Default(projectID)`** / **`Load(repoRoot)`** — sensible defaults or parse from disk

Key scheduler fields: `DispatchMode` (`"wave"` | `"rolling"`, default `"wave"`),
`PollSeconds` (poll tick override; 0 → engine default 10 s), `MaxConcurrentSlots`
(wave-width cap per project), `DispatchStaggerSeconds` (inter-agent launch spacing).

## promptc

Compiles the dispatch prompt in a cache-stable, deterministic order so
prompt-cache hits are maximised across re-dispatches.

- **`Input`** — all variable sections (task, plan, project context, …)
- **`Compile(in)`** — assemble final prompt string
- **`Preamble(engineVersion)`** — static koryph-protocol preamble

## procx

Small OS-process primitives shared across the recovery, governor, and health
paths — factored out so the signal-0 liveness probe has exactly one
implementation instead of the four byte-identical copies that had accreted in
ledger, govern, doctor, and dispatch.

- **`Alive(pid)`** — is pid a live process? (POSIX kill(pid, 0): nil → alive,
  EPERM → alive but not ours, ESRCH/other → dead)

Reads/writes no files.

## quota

Per-account usage governor. Estimates wave cost, tracks rolling-window spend,
and gates or scales dispatch.

- **`Config`** — daily/monthly caps and thresholds per account
- **`Usage`** / **`Window`** / **`Level`** — spend snapshot, rolling measurement, `"ok"` | `"warn"` | `"drain"` | `"stop"`
- **`State(u, cfg)`** — derive `Level`; **`ScaleSlots(u, max)`** — reduce wave width under pressure
- **`Preflight(u, estimateUSD, cfg)`** — gate a wave before dispatch
- **`EstimateItem`** / **`EstimateWave`** — pre-flight USD estimates (claude's per-tier base prices)
- **`EstimateItemForRuntime`** / **`EstimateWaveForRuntime`** / **`DefaultConfigForRuntime`** — the same
  estimates, namespaced by runtime name (koryph-v8u.12): each runtime gets its own default per-tier USD
  base table (only `claude`'s carries real numbers today), selected by the runtime a bead actually
  resolves under (`modelroute.ResolveRuntimeName`); an unrecognized runtime name degrades to claude's
  table rather than erroring (an estimate is advisory governor input, never a fail-closed dispatch gate).
  Calibration keys (`"<tier>:<size>"`) are deliberately **not** runtime-namespaced — only claude
  dispatches have ever recorded calibration, so existing `~/.koryph/quota/*.json` files keep estimating
  exactly as before.
- **`Record`** / **`LoadConfig`** / **`SaveConfig`** — persist actual spend and config

## registry

Central multi-project registry stored under `paths.RegistryDir()`.
One JSON file per project.

- **`Store`** — registry root; `NewStore()` / `NewStoreAt(home)`
- **`Record`** — full project registration (ID, root, account, billing, hooks, …)
- **`Event`** — audit-log entry written on every mutation

Key `Store` methods: `Get`, `Put`, `Delete`, `List`, `All`.

## release

Implements `koryph release setup` — rendering and installing the
forge-specific release pipeline plus release-please config/manifest into a
target project — and `koryph release kick`, the bot-less fallback that
close+reopens the open Release PR so GitHub fires check workflows under the
user's real `gh` auth. The caller workflow is rendered via the project's
forge CI service (`forge.CIService.Render("caller")`); the release-please
config and manifest come from templates embedded in this package. The
manifest is written once and never overwritten.

- **`Setup(repoRoot, rc, initialVersion)`** / **`SetupForge(..., ci)`** — install the pipeline files
- **`Kick`** (via `KickOptions`/`KickResult`) — close+reopen the Release PR, optional `--wait` check polling
- **`ReleasePRLabel`** — `autorelease: pending`, the Release PR detection label

## resmon

Samples the OS resource usage (CPU time, resident memory, and — where the
platform exposes it — disk I/O) of an agent process tree, so the engine can
record per-bead efficiency metrics and the cockpit can surface avg/peak memory
and CPU per bead. Callers take ONE process-table `Snapshot` per tick and
aggregate the subtree rooted at each slot's PID — one syscall sweep regardless
of slot count. Build-tagged backends: linux reads `/proc`, darwin shells out
to `ps` (no per-process disk I/O there), other platforms report unavailable.

- **`Snapshot()`** — one whole-machine process table (`ProcTable`)
- **`ProcTable`** / **`Sample`** / **`Usage`** — table, per-process reading, per-slot aggregate

## review

Runs a read-only post-implementation review pass before a branch is merged.

- **`Opts`** — branch, project root, model, and the timeout budget
  (`TimeoutSec` starting deadline, `MaxTimeoutSec` escalation ceiling)
- **`Finding`** — one review comment (file, line, severity, message)
- **`Verdict`** — pass/fail + `[]Finding` (`TimedOut` flags a deadline kill)
- **`Review(ctx, o)`** — launch reviewer agent, collect `Verdict`

**Timeout budget.** Each attempt runs under a wall-clock deadline. It starts at
`Opts.TimeoutSec` (resolved: `KORYPH_REVIEW_TIMEOUT_SEC` env > project
`review.timeout_seconds` > 600s default) and, when an attempt is killed for
running out of time, the retry loop **doubles** the deadline toward
`Opts.MaxTimeoutSec` before the next attempt — so a large diff gets
progressively more room. `review.MaxTimeoutSec` (1200s / 20 min) is the hard
ceiling: no env override, project config, or escalation may exceed it, and every
resolved value is clamped to it (`resolveTimeouts`). `internal/project` mirrors
the ceiling as `project.ReviewTimeoutHardCapSec`; an `internal/engine` drift
guard asserts the two stay equal. A rate/usage limit (the other transient
failure) leaves the timeout unchanged — only the exponential backoff grows.

## runtime

Defines the pluggable agent-runtime contract (koryph-v8u.1): the runtime
interface, `Capabilities` flags, a normalized event envelope, and a
`Registry` — as a pure addition that deliberately imports nothing from
`internal/dispatch`/`internal/account`. Every type is a small local mirror of
the corresponding dispatch/account field set (e.g. `dispatch.Spec` ↔
`runtime.DispatchSpec`, with the mapping documented in doc comments) so a
second adapter can exist without wiring the contract to Claude's shape. The
Claude adapter lives in `internal/runtime/claude`; `runtimetest` holds shared
conformance fixtures.

- **`Capabilities`** — feature flags (`Personas`, `ModelSelect`, `EffortFlag`, `Resume`, `BudgetFlag`, …)
- **`DispatchSpec`** / **`Profile`** / **`BillingMode`** — runtime-neutral request mirrors
- **`NewRegistry()`** — named-runtime registry; each runtime carries its own model map

## sched

Builds conflict-free waves from the beads ready frontier, respecting
footprint rules and the wave-width cap.

- **`Item`** / **`Wave`** — selected issues + deferred/blocked explanations
- **`Footprint`** — RW conflict surface (`Reads []string`, `Writes []string`); **`Reason`** — why deferred/blocked
- **`FootprintFor(issue, cfg)`** — derive `Footprint` from labels + project rules
- **`Conflicts(a, b)`** — true iff footprints share a token **and** at least one side holds it as a write (RWMutex semantics: two readers co-run)
- **`Eligible(issue, activeIDs)`** — can this issue be dispatched now?
- **`BuildWave(...)`** — main entry: frontier → `Wave`; accepts `Opts.Active` (in-flight footprints keyed by bead id) for rolling-mode in-flight gating

**Dispatch shape.** `Eligible` skips any bead whose `issue_type` is
`epic`/`feature`/`decision`/`merge-request`, or that carries a `no-dispatch`,
`refactor-core`, or `gt:*` label — so a bead filed with the wrong type sits in
`bd ready` forever. `FootprintFor` derives conflict tokens with the precedence:

1. `fp:read:<token>` labels → **read** tokens (two readers of the same token co-run);
2. `fp:<token>` labels (any other suffix) → **write** tokens (existing grammar, unchanged);
3. `area:*` labels resolved through the project's `area_map` → write tokens;
4. else `domain:unknown` (a write token) — conflicts with every other unlabeled bead, serializing them 1-per-wave.

`BuildWave` also accepts `Opts.Active` (in-flight footprint map for rolling mode): a candidate
whose footprint `Conflicts` with any entry here is deferred *before* intra-batch greedy coloring
even runs, so a rolling refill can never clash with already-running beads. Label implementable
beads (`task`/`bug`/`chore`) with one `area:*` per area they touch: over-broad labeling only costs
parallelism, under-broad labeling risks a false-parallel merge conflict.

## rules

Installs the koryph enforcement rules into a project: the hook scripts
(`hooks/*.sh`) and their wiring in `.claude/settings.json`. Hook scripts install
like agents (whole-file, hash-idempotent). `settings.json` is **merged
additively** — koryph's hooks and permission allow/deny entries are added,
every other key is preserved, and only an unparseable file blocks the merge.

- **`Install(root, force)`** — install hooks + merge settings
- **`MergeSettings(root, force)`** — additive settings merge → `created`/`merged`/`unchanged`/`skipped`

## scaffold

Shared, hash-aware installer for binary-embedded assets (personas, commands,
hooks) into a project. Identical content is a no-op (`unchanged`); differing
content is `skipped` (warned) unless `force`, then `overwritten`.

- **`Result`** / action constants (`installed`/`overwritten`/`unchanged`/`skipped`)
- **`CopyEmbed(fsys, destDir, force, perm)`** — copy every embedded file with perm
- **`Conflicts(results)`** / **`Count(results, action)`** — reporting helpers

## schemaver

Single source of truth for the on-disk schema version of every persisted state
surface, and the forward-compatibility guard that stops an older binary from
silently corrupting state a newer koryph wrote. When the on-disk version is
newer than this build understands, load/save is refused with an "upgrade
koryph" error rather than misreading it (or field-stripping it on the next
read-modify-write save). Write paths stamp `Current(surface)`; read paths guard
with `CheckRead` — so the number lives in exactly one place. Design:
`docs/designs/2026-07-state-versioning.md`.

- **`Surface`** — a versioned state surface (`Registry`, `Quota`,
  `SigningVault`, `Project`, `LedgerRun`, `LedgerManifest`)
- **`Current(s)`** — the schema version this binary writes/understands for `s`
- **`CheckRead(s, onDisk)`** / **`CheckWrite(s, onDisk)`** — refuse newer-than-supported
- **`TooNewError`** — the version-skew refusal (errors.As to distinguish from IO errors)

State files: reads/writes no files of its own — it gates the load/save paths in
registry, quota, signing, project, and ledger.

## signing

SSH commit signing and koryph's secret-vault layer. Configures a repo for
signed commits, moves the signing key from a vault into an SSH agent (memory
only — a fetched key is piped to `ssh-add -t 3600 -`, never written to disk),
and provides the **scoped agent**: a koryph-managed ssh-agent holding only the
commit-signing key, which is what dispatched agents receive instead of the
operator's ambient `SSH_AUTH_SOCK`. Vault providers (Proton Pass, 1Password,
KeePassXC, macOS Keychain, age-encrypted file, generic command) are argv
templates in `~/.koryph/vault.json`, so CLI drift is a config edit, not a code
change. `FetchSecret` is the generic secret path other packages (bot keys,
GitLab PATs) reuse; cosign key handling and signing-posture checks also live
here.

- **`ConfigureRepo(ctx, repoRoot, cfg)`** — write the repo's signing git config
- **`EnsureAgent`** / **`EnsureScopedAgent`** — key into the system / scoped agent
- **`FetchSecret(ctx, provider, ref)`** — resolve any secret through the vault seam
- **`VaultConfig`** / **`ProviderTemplates`** — the `vault.json` schema
- State: `~/.koryph/vault.json`, `~/.koryph/signing/config.json`

## stage

Runs one post-implement pipeline stage: a write-capable persona agent executed
synchronously in the implementer's worktree (before review/merge) under the same
account/billing/identity guarantees as a dispatch.

- **`Opts`** — worktree, branch, resolved persona + model, per-stage prompt, profile/billing
- **`Result`** — `Ran` / `OK` / `CostUSD` / `Note`
- **`Run(ctx, o)`** — verify identity, run the `dontAsk` claude one-shot, persist the envelope, report cost

## sysdeps

Platform detection and consented install planning for koryph's external tool
dependencies (`bd`, `claude`, `gh`), plus the consumer-flake edit route
(design: docs/designs/2026-07-adopt.md §4). Planning is pure — this package
never executes an install; the adopt wizard shows the exact argv (sudo called
out) and runs it only on consent.

- **`Detect()`** — `Platform` (GOOS, `/etc/os-release` distro, package managers on PATH in preference order)
- **`Plan(p, tool)`** — `InstallPlan{Route, Argv, Manual, Verify, NeedsSudo}`; data-driven tool spec table, manual fallback always available
- **`PlanFlakeBeads(root)`** / **`ApplyFlakeEdit(ctx, root, e)`** — minimal structural flake.nix edit adding the beads input + devShell package, diff-first, `nix flake lock` after

## sysmem

Reports coarse system memory availability with no external dependencies and
no cgo, so the scheduler can refuse to admit another agent when the host is
under memory pressure (koryph-930). `AvailableBytes` is a deliberately
conservative estimate (Linux: `/proc/meminfo` `MemAvailable`; macOS:
reclaimable page classes from `vm_stat`) used as a soft admission floor, never
a hard accounting number. Callers MUST fail open on `ErrUnsupported` — the
gate is a safety rail, not a correctness dependency.

- **`Available()`** — current `Stat` (`TotalBytes`, `AvailableBytes`)
- **`DefaultFloorMB(totalMB)`** — auto-floor sizing, clamped for small/large hosts
- **`ErrUnsupported`** — platform has no probe; fail open

## tui

The koryph terminal cockpit (`koryph tui`), built on Bubble Tea. `App` is the
root model — tab framework, project switcher, help overlay, status bar, and
refresh loop; each tab is a `TabModel` registered once via `tabRegistry`
(adding a tab = one file with `init()`), and only the active tab receives
`Update` calls. Data comes from `cockpit.Provider`, polled every 100 ms while
agents run (1 s when idle). Minimum terminal floor: 80×24. User docs:
[user-guide/tui.md](../user-guide/tui.md).

- **`NewApp`** — construct the root model
- **`DefaultKeyMap`** / **`DefaultTheme`** — keybindings and styling defaults

## version

Holds the engine's semantic version (set at build time via `-ldflags`) and
version-requirement checking. Single entry point: **`Satisfied(have, want)`**
reports whether `have` meets the `want` semver constraint.

## worktree

Creates and manages per-bead git worktrees (one branch per active issue).

- **`Info`** — metadata (path, branch, bead ID, dirty flag)
- **`EnsureOpts`** / **`RefreshOpts`** / **`RefreshResult`** — lifecycle options and results
- **`BranchFor(beadID)`** — returns `"agent/<beadID>"`
- **`List(ctx, repoRoot)`** — all worktrees for the repo
- **`Ensure(ctx, o)`** — create-or-reuse worktree for a bead
- **`Bootstrap(ctx, path, cmds, env)`** — run bootstrap commands inside worktree
- **`Refresh(ctx, o)`** — rebase or snapshot an existing worktree
- **`Remove(ctx, path, force)`** — delete worktree and prune branch
- **`PatchSnapshot`** / **`DeleteBranch`** — export diff patch, remove remote-tracked branch
