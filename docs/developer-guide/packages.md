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

## engine

The wave loop: scan → batch → preflight → dispatch → poll → stages → review →
merge. Main orchestration entry point called by `cmd/`.

- **`Options`** — full run config (project ID, wave width, auto-merge, review flags, …)
- **`Outcome`** — summary counts (dispatched, merged, failed, blocked)
- **`Run(ctx, opts)`** — blocking entry point; returns when the run terminates

Internal files: `poll.go` (heartbeat/completion), `recover.go` (resume
logic), `wave.go` (engine-side wave assembly), `pipeline.go` (post-implement
stage execution). Requeues refresh the worktree onto current main first (rebuild
when no commits, rebase when there are) so a retry never runs a stale checkout.

## execx

Runs external commands with an explicit working directory and environment.
All engine subprocess calls go through here.

- **`Cmd`** — command spec (bin, args, dir, env, timeout); **`Result`** — exit code + output
- **`Run(ctx, c)`** / **`MustSucceed(ctx, c)`** — execute; latter errors on non-zero exit
- **`BaseEnv(remove...)`** — current environment with named vars stripped
- **`LookPath(name)`** — reports whether `name` is on `$PATH`

## fsx

Small filesystem helpers shared across the engine. All writes are atomic
(write-to-temp + rename) to prevent torn files on crash.

- **`WriteAtomic`** / **`WriteJSONAtomic`** — atomic byte-slice and JSON writes
- **`ReadJSON(path, v)`** — unmarshal file into `v`
- **`AppendLine(path, line)`** — append one newline-terminated record; **`Exists(path)`** — stat check

## govern

Machine-global concurrency governor: a cross-process cap on concurrently running
agents (across all projects) so independent `koryph run` invocations cannot
collectively breach the Claude API rate limits. Coordinates via lease + demand
files under `~/.koryph/slots` guarded by a flock — no daemon. See
[global-governor.md](global-governor.md).

- **`Store`** — `Acquire` (cap-checked reserve) / `Hold` (post-launch attach) /
  `Release` / `Prune` / `RefreshDemand` / `DropDemand` / `Snapshot` / `Cap` / `SetCap`
- **`Lease`** / **`Demand`** — on-disk records
- Fair-share: `floor(cap/n)` per demander, rotating remainder (no starvation)

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
- **`Merge(ctx, o)`** — main entry: run gate → squash-merge
- **`RunGate(ctx, dir, cmds)`** — execute green-gate commands; returns `ok`, output

## metrics

Rolls up burn-rate and reliability baselines from the run ledger for
reporting and quota decisions.

- **`ModelStat`** / **`ProjectStat`** / **`Report`** — nested stats types
- **`Collect(store, projectID)`** — read ledger, compute `*Report`
- **`Render(r, w)`** — pretty-print report to an `io.Writer`

## modelroute

Resolves a (stage, bead-labels, run-defaults, project-config) tuple to a
`(model, effort)` pair, with persona-file overrides and recovery upgrades.

- **`Req`** / **`Resolution`** — request and resolved `(model, effort)`
- **`Resolve(r)`** — main entry; consults label rules, then defaults
- **`PersonaFor(stage, stages)`** — picks persona name from stage map
- **`RecoveryUpgrade(current)`** — escalates model for a recovery re-dispatch
- **`PersonaMeta(repoRoot, persona)`** — reads persona file → `(model, effort)`

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

## project

Loads the per-project adapter configuration (`koryph.project.json`).

- **`Config`** — wave width, green-gate commands, footprint rules
- **`FootprintRule`** — path-pattern → conflict-scope mapping
- **`Default(projectID)`** / **`Load(repoRoot)`** — sensible defaults or parse from disk

## promptc

Compiles the dispatch prompt in a cache-stable, deterministic order so
prompt-cache hits are maximised across re-dispatches.

- **`Input`** — all variable sections (task, plan, project context, …)
- **`Compile(in)`** — assemble final prompt string
- **`Preamble(engineVersion)`** — static koryph-protocol preamble

## quota

Per-account usage governor. Estimates wave cost, tracks rolling-window spend,
and gates or scales dispatch.

- **`Config`** — daily/monthly caps and thresholds per account
- **`Usage`** / **`Window`** / **`Level`** — spend snapshot, rolling measurement, `"ok"` | `"warn"` | `"stop"`
- **`State(u, cfg)`** — derive `Level`; **`ScaleSlots(u, max)`** — reduce wave width under pressure
- **`Preflight(u, estimateUSD, cfg)`** — gate a wave before dispatch
- **`EstimateItem`** / **`EstimateWave`** — pre-flight USD estimates
- **`Record`** / **`LoadConfig`** / **`SaveConfig`** — persist actual spend and config

## registry

Central multi-project registry stored under `paths.RegistryDir()`.
One JSON file per project.

- **`Store`** — registry root; `NewStore()` / `NewStoreAt(home)`
- **`Record`** — full project registration (ID, root, account, billing, hooks, …)
- **`Event`** — audit-log entry written on every mutation

Key `Store` methods: `Get`, `Put`, `Delete`, `List`, `All`.

## review

Runs a read-only post-implementation review pass before a branch is merged.

- **`Opts`** — branch, project root, model to use
- **`Finding`** — one review comment (file, line, severity, message)
- **`Verdict`** — pass/fail + `[]Finding`
- **`Review(ctx, o)`** — launch reviewer agent, collect `Verdict`

## sched

Builds conflict-free waves from the beads ready frontier, respecting
footprint rules and the wave-width cap.

- **`Item`** / **`Wave`** — selected issues + deferred/blocked explanations
- **`Footprint`** — file-path scope claimed by an issue; **`Reason`** — why deferred/blocked
- **`FootprintFor(issue, cfg)`** — derive `Footprint` from labels + project rules
- **`Conflicts(a, b)`** — true if two footprints overlap
- **`Eligible(issue, activeIDs)`** — can this issue be dispatched now?
- **`BuildWave(...)`** — main entry: frontier → `Wave`

**Dispatch shape.** `Eligible` skips any bead whose `issue_type` is
`epic`/`feature`/`decision`/`merge-request`, or that carries a `no-dispatch`,
`refactor-core`, or `gt:*` label — so a bead filed with the wrong type sits in
`bd ready` forever. `FootprintFor` derives conflict tokens with the precedence
`fp:*` labels → `area:*` resolved through the project's `area_map` → else the
catch-all `domain:unknown`, which conflicts with **every other unlabeled bead**
and serializes them 1-per-wave regardless of the width cap. Label implementable
beads (`task`/`bug`/`chore`) with one `area:*` per area they touch: over-broad
labeling only costs parallelism, under-broad labeling risks a false-parallel
merge conflict.

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

## stage

Runs one post-implement pipeline stage: a write-capable persona agent executed
synchronously in the implementer's worktree (before review/merge) under the same
account/billing/identity guarantees as a dispatch.

- **`Opts`** — worktree, branch, resolved persona + model, per-stage prompt, profile/billing
- **`Result`** — `Ran` / `OK` / `CostUSD` / `Note`
- **`Run(ctx, o)`** — verify identity, run the `dontAsk` claude one-shot, persist the envelope, report cost

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
