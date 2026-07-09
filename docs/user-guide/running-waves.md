<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Running Waves

This chapter explains the wave loop from an operator's perspective: how koryph selects
work each cycle, dispatches agents, and lands the results.

## The wave loop

`koryph run --project <id>` executes wave cycles until the frontier is empty:

```
scan (bd ready) → build wave → preflight → dispatch → poll → stages → review → merge → repeat
```

Each iteration is one _wave_: a conflict-free batch of at most `max_concurrent_slots`
(or `--max`) beads dispatched in parallel. Between waves the engine checks the quota
governor and re-scans the frontier.

To run exactly one wave, pass `--once`. To see what _would_ be dispatched without
actually sending anything, pass `--dry-run`.

To build **one specific bead** instead of the whole frontier, pass `--only <bead-id>`:
the wave is narrowed to that bead, and the run drains once it closes. To cap the
run's spend, pass `--budget <USD>`: once the run's **projected** cost reaches the
ceiling, no new agents are dispatched (active ones finish) and the run pauses
with a `budget-cap` reason. Projected cost is settled spend **plus each in-flight
agent's dispatch-time estimate** — so a wide wave or a retry cannot slip past the
cap and only settle over it afterward. The check is re-evaluated per bead within
a wave (dispatch stops mid-wave the moment the projection crosses the cap), and a
requeue is refused once the budget is exhausted (the slot parks needs-attention
rather than spending another attempt). This is a per-run ceiling, separate from
the account cost governor and the global concurrency governor.

## Dispatch mode: wave vs rolling

Two dispatch loops share every scan/preflight/dispatch/poll primitive above; they
differ only in when the next scan happens:

- **`wave`** — the loop described above: dispatch a batch, then wait for
  **every** slot in it to land before scanning again. Simple and predictable, at the
  cost of idling a slot that frees early while its wave-mates are still running.
- **`rolling`** (default) — continuously refills: every poll tick it re-checks the governor,
  recomputes free capacity from the currently-running count, and tops off any slot
  that has freed up — without waiting for the rest of the batch. A slot that lands
  early is refilled on the next tick instead of sitting idle.

Rolling is the engine default (it became so after the 2026-07-03 self-build
burn-in). Select a mode explicitly with `dispatch_mode` in `koryph.project.json`
(`"wave"` or `"rolling"`) or per run with `--dispatch-mode wave|rolling` (the
flag wins over the config; an unrecognized value is a usage error). `--once` runs the exact same
single-pass semantics — one dispatch pass, poll to idle, exit — in **both** modes,
so a validation/canary invocation behaves identically either way. Every other flag
(`--only`, `--budget`, `--dry-run`, `--resume`, quota governor levels, footprint
gating) applies identically in rolling mode; footprint conflicts against a
currently in-flight bead are still deferred (and re-checked on the next tick) so
two conflicting beads never run at once, whichever mode is active.

## The bd ready frontier

Every wave starts with `bd ready`, which returns all issues whose dependencies are
closed. Container beads (epics, features, decisions, merge-requests) and beads with a
`gt:*` gate label are **structurally skipped** — they will never dispatch as-is, so the
engine reports each one **once per run** with a fix hint (`skipped <id>: … — file as
task/bug/chore; area:* label; drop gt:*`). Beads labeled `no-dispatch` or
`refactor-core`, already-active beads, container beads with open children, footprint
collisions, [resource](../concepts/resources.md) capacity collisions, and the width
cap are **deferred**: the engine prints a per-wave `deferred N bead(s): …` summary.
Under `--dry-run` every deferral is listed in full
alongside the would-dispatch set, so you can see exactly why a ready bead is not running
before committing to a wave. The remainder are sorted by priority (P0 first) and passed
to the conflict filter.

Scope a run to a single epic:

```sh
koryph run --project myproject --parent beads-001
```

## Footprint labels: fp:\* and area:\*

The scheduler prevents two agents from touching the same code at once via _footprint
tokens_. Footprints are split into **read** and **write** token sets and follow
RWMutex semantics: two beads sharing a token only conflict when **at least one** holds
it as a write. Two readers of the same token co-run freely — a docs bead that only
reads engine code no longer excludes an unrelated engine writer.

**`fp:read:<token>` labels (new)** — produce **read** tokens; beads that only read a
surface run alongside any other reader:

```
fp:read:engine fp:read:docs   →  reads: ["docs", "engine"], writes: []
```

A bead carrying `fp:read:engine` does **not** conflict with another bead that merely
reads `engine`; it **does** conflict with a bead that writes `engine`.

**`fp:<token>` labels (plain suffix)** — produce **write** tokens (existing grammar,
unchanged); a token declared as both read and write collapses to write-only:

```
fp:auth fp:billing   →  reads: [], writes: ["auth", "billing"]
```

**`fp:*` and `area:*` labels compose** — they do not override each other. Every
`area:*` label contributes its mapped write tokens, every `fp:read:<token>` label
contributes a read token, and every other `fp:<token>` label contributes a write
token; the bead's footprint is the union of all of it (a token present in both the
read and write sets collapses to write-only, since a write already excludes
readers). Only when a bead carries **none** of the above — no `area:*` and no
`fp:*` label at all — does the catch-all `domain:unknown` apply.

**`area:` labels** — resolved through `koryph.project.json`'s `area_map` as **write** tokens:

```json
"area_map": { "api": ["auth", "billing", "routes"] }
```

A bead with `area:api` gets write tokens `["auth", "billing", "routes"]` and conflicts
with any bead carrying any of those tokens (whether via `fp:*` or another `area:*`
mapping). A bead labeled `area:api fp:read:go:signing` gets write tokens
`["auth", "billing", "routes"]` **and** the read token `go:signing` — both areas'
worth of protection stack, they don't compete. (Before koryph-2im, `fp:*` used to
suppress `area:*` outright; that behavior silently dropped write tokens on mixed
`fp:read:` + `area:` beads and was fixed — see `internal/sched/footprint.go`'s
`FootprintFor` doc comment for the full history. If you were narrowing an
over-broad `area:*` with an `fp:*` label under the old precedence rule, drop the
`area:*` label instead — that is the one authoring pattern the fix costs.)

**No footprint label** — the bead receives the catch-all write token `domain:unknown`,
which conflicts with every other unknown bead. Unknowns serialize: only one runs per wave.

## Resource labels: res:\<kind\>

Footprints prevent two agents from touching the same *code*. They don't know
anything about what an agent starts *running* on the host — a kind/k8s dev
cluster, a docker compose stack, a long-lived dev server — and those live
outside the agent's process tree and worktree. [Resources](../concepts/resources.md)
are a second, additive admission dimension for exactly that: a bead labels
`res:<kind>` per external resource kind it will provision, and the scheduler
and governor treat each kind as a **counted capacity** (default 1, i.e.
exclusive, unless the machine is configured otherwise) rather than a
read/write lock. A bead with no `res:*` labels — the common case — is
unaffected; declaring nothing never serializes it against anything.

A capacity-exhausted kind produces a deferral at one of two points:

- **Wave packing** (`sched.BuildWave`, project-local): `resource <kind> at
  capacity (held by <id>)` — the candidate is skipped and packing continues
  past it, so a lower-priority resource-free bead behind it still dispatches.
- **Global admission** (`govern.Store.Acquire`, cross-project, under the
  flock): `bead <id>: deferred — resource <kind> at capacity (N/N, held by
  <project>/<bead>)` — the authoritative, cross-engine check; a bead that
  cleared wave packing can still be denied here by another engine's holdings.

Both are per-bead skips, not batch-wide breaks: a resource-heavy bead
deferring never stalls the lightweight beads behind it in the same wave.
See [Machine: resources](../concepts/resources.md) for the label grammar,
capacity/reservation semantics, and the agent contract, and
[Global governor](../developer-guide/global-governor.md) for the
`governor.json` schema and admission clauses.

## Model labels

The model resolved at dispatch time controls which Claude tier runs the bead.
Precedence (highest first):

| Label | Scope |
|---|---|
| `model:implement:<tier>` | this bead, implement stage only |
| `model:<tier>` | this bead, all stages |
| `--default-model <tier>` | all label-less beads in this run |
| _(none)_ | stage default (configured in `koryph.project.json`) |

`<tier>` is a model ID such as `sonnet`, `opus`, or a full model string. Stage-scoped
labels (`model:implement:*`) take precedence over bare `model:*` labels so a bead can
pin the implement tier without affecting review.

## Merge policies

After an agent finishes, the engine applies a _merge policy_:

| Policy | Behaviour |
|---|---|
| `auto` | Merge automatically when `--auto-merge` is passed and review is clean |
| `manual` | Leave slot in `merge-pending`; operator runs `koryph merge` |
| `pr` | Push the agent branch and open a GitHub PR; slot ends `pr-opened` |

**Epic label wins over project config.** Add `merge:auto`, `merge:manual`, or `merge:pr`
to an epic bead and every child bead under that epic inherits that policy, overriding
`merge_policy` in `koryph.project.json`.

When an issue has no epic or the epic carries no merge label, the project config
`merge_policy` applies.

Auto-merge never fires without `--auto-merge` on the command line, even when the policy
says `auto`. This keeps CI-only runs safe by default.

**Owner override — `--direct`.** `koryph run --direct` is the escape hatch for an
owner/select-maintainer who wants to skip PRs entirely: it forces the effective policy to
`auto` (direct ff-merge + push to the default branch) **even on a `merge:pr` epic**. koryph
does not gate on org role — the push to a protected default branch still succeeds only if the
pushing identity is on the branch-protection **bypass allowlist**. A blocking `--review`
verdict still downgrades to manual, so the safety path is not bypassed.

### `pr` — pull-request merges for protected branches

`merge_policy: pr` is the path for a default branch you never push to directly (branch
protection, required reviews). It runs the same preflight as an auto-merge — protected-path
check, signature verification, sync of the local default branch to origin, rebase onto it,
and the green gate — but then **pushes the agent branch (`agent/<bead-id>`) and opens a PR**
against the default branch instead of fast-forwarding it. The PR title is
conventional-commit-shaped and the body carries the bead id, title, and acceptance criteria.

- The slot ends in **`pr-opened`** (terminal for the run); the worktree and branch are kept
  so a later fast-forward landing step can resume them. Nothing is pushed to the default branch.
- Opening a PR does **not** require `--auto-merge` — it is the safe alternative to a direct
  merge, so setting the policy is the opt-in.
- Requires a git remote and an authenticated [`gh`](https://cli.github.com/) CLI. Without
  either, the bead is **blocked** with a clear reason (never crashed or silently dropped),
  and the branch is kept so a `--resume` retries once the remote or `gh` is available.
- A re-run reuses an already-open PR for the branch rather than opening a duplicate.

### Landing an opened PR (fast-forward only)

Once a PR is opened, a maintainer lands it with:

```sh
koryph land --project myproject <bead-id>
```

This re-verifies the branch against the (possibly advanced) default branch, runs the gate,
and lands it **fast-forward-only** — then flips the slot to `merged` and closes the bead.

**Why not the GitHub merge button?** GitHub has no true fast-forward merge. Every native
method breaks the koryph merge contract of preserving the exact gate-checked, reviewed,
SSH-signed commits: a *merge commit* adds an unsigned commit, and *squash* / *rebase* merges
rewrite SHAs and the committer identity, destroying the signatures `signing.required`
mandates. koryph therefore lands with **mechanism (a): a local `git merge --ff-only` + push**
by the engine's signing identity — the only method that keeps the signed SHAs byte-for-byte.

- **Base moved.** If the base advanced, `koryph land` rebases the branch onto it and
  re-verifies; a genuine conflict is reported (the bead is rebased/re-run, **never**
  rewrite-merged). A clean rebase re-signs the rewritten commits with the engine's signing
  key, so signatures still verify.
- **Override.** `merge_method` in `koryph.project.json` (or `--method` per run) selects the
  landing method: `ff` (default) or `squash`. A non-`ff` method is **refused with a clear
  error while `signing.required`** is set, because it rewrites the signed commits.
- **Required branch-protection ruleset shape.** Protect the default branch (require pull
  requests / disallow direct pushes for everyone) **and add the engine's signing identity to
  the ruleset bypass allowlist** ("Allow specified actors to bypass required pull requests").
  The engine runs the same green gate locally before it pushes, so required status checks stay
  satisfied; GitHub marks the PR merged automatically once its commits land on the base.

## Reviewing other people's PRs

`koryph review-pr` is a **human-in-the-loop** tool for reviewing pull requests authored by
someone else (including contributors who used koryph). koryph *analyzes* — it never approves
on its own:

```sh
# 1. Analyze: koryph runs its reviewer over the PR head and prints its findings.
koryph review-pr --project myproject 42

# 2. You read the analysis, examine the flagged code, and decide (you may override koryph).

# 3. Instruct approval — this registers YOUR approving review on the PR.
koryph review-pr --project myproject 42 --approve --body "Looks good, thanks"
```

- **Analysis** checks out the PR head into an ephemeral worktree, runs the reviewer over its
  diff, and prints a verdict plus findings (severity · file · summary). It records **no**
  approval — the decision is yours.
- **Approval** is a separate, explicit instruction. The approving review is registered under
  *your* identity, so it works for others' PRs regardless of who authored them. You can
  approve even when the analysis flagged issues (you own the call).
- Approving your **own** PR is refused with a clear error — GitHub rejects self-approval; land
  your own work directly instead (`koryph land`, or `merge_policy: auto` / `--direct` with a
  branch-protection bypass).

**Clearing the queue.** `koryph review-pr --project myproject --all` analyzes every open PR in
turn, **skipping drafts and PRs you authored** (each skip is logged with its reason). It only
analyzes — approve each PR individually afterwards. `Ctrl-C` stops the loop cleanly after the
current PR.

**Inline comments.** `koryph review-pr --project myproject 42 --comment` posts koryph's
line-anchored findings as inline review comments on the PR (findings without a line fold into
the review body). Add your own with a repeatable `--comment-on path:line:message`:

```sh
koryph review-pr --project myproject 42 --comment \
  --comment-on "internal/foo.go:88:this needs a nil check" \
  --comment-on "cmd/bar.go:12:rename for clarity"
```

Comments post as a single **`COMMENT`** review anchored to the PR head commit — no approval.
Approve separately with `--approve` once you're satisfied.

**IDE handoff loop.** The analysis is persisted, so you can review in koryph, switch to your
IDE to examine the flagged files (and add manual comments there or via `--comment-on`), then
come back:

```sh
koryph review-pr --project myproject 42            # analyze (saves state)
# ...open the flagged files in your IDE, think it over...
koryph review-pr --project myproject 42 --resume   # replay the saved analysis (no re-run)
koryph review-pr --project myproject 42 --approve   # or --close --body "superseded"
```

`--resume` replays the saved findings without re-running the (costly) reviewer, and warns if
the PR head moved since the analysis. `--close [--body "..."]` closes the PR from koryph. A PR
closed or merged **by any means** (koryph, the GitHub UI, or another tool) is reflected on the
next `review-pr` because state is read live from GitHub — an action on a terminal PR is a
no-op that reports the state and clears the stale saved analysis.

**Reconciling engine-opened PRs.** For PRs koryph itself opened (`merge_policy: pr`, parked in
the `pr-opened` slot), `koryph pr-sync --project myproject` checks each one's live state and
reconciles the ledger: a PR that **merged** (landed by anyone) marks the slot merged and
closes the bead; one **closed without merging** marks the slot blocked. Nothing is left
stranded in `pr-opened` when a PR ends outside koryph.

## Post-implement stages

If the project declares a [`pipeline`](projects-and-accounts.md#post-implement-pipeline-stages),
each stage runs sequentially **in the same worktree** once the implementer finishes and
before review/merge — a persona agent (docs, tests, changelog, …) that may add its own
commits on the branch. A failed non-`optional` stage blocks the bead rather than merging
incomplete work; an `optional` stage logs and continues. Stage cost counts toward the
quota governor, and a review bounce re-runs the whole pipeline on the updated code.

## Review bounces

Pass `--review` to insert a reviewer pass (Opus) between implementation and merge:

```sh
koryph run --project myproject --review --auto-merge
```

If the reviewer reports blocking findings, the bead is _bounced_ back to a fresh
implementer dispatch that receives the review report. Up to two bounces are allowed; on
the third blocking result the engine forces `manual` policy regardless of the epic or
project setting, and the slot lands in `merge-pending` for operator inspection.

Degraded reviewer output (model error, empty report) is treated as non-blocking and
does not delay the merge.

## Recovery and resume

Interrupt a run (Ctrl-C, host sleep, etc.) and resume where it left off:

```sh
koryph run --project myproject --resume
```

On resume the engine classifies the latest run's slots:

| Slot state | Action |
|---|---|
| Agent still alive | Reattach — poll resumes from live PID |
| Dead, has commits or SUMMARY.md | Re-dispatch with the branch HEAD as resume SHA; if a Claude session ID was recorded, the agent resumes that session natively |
| Dead, no commits, attempts < 3 | Re-dispatch fresh with exponential backoff |
| Dead, attempts ≥ 3 | Blocked — requires operator intervention |

Nothing is lost: committed checkpoints survive the interruption and are replayed
into the new dispatch context.

Every requeue also refreshes the worktree onto the current default branch first, so a
retried agent never runs against a checkout that predates a main-side fix: a bead with
no commits is rebuilt from a fresh checkout, and one carrying commits is rebased onto the
advanced base before re-dispatch.

### Budget-killed agents

An agent stopped by `--max-budget-usd` (see [Billing and
quota](billing-and-quota.md#per-agent-budget-caps-and-the-turn-boundary-nuance))
is classified distinctly from a crash or rate-limit death and gets its own
warm-resume policy:

| Situation | Action |
|---|---|
| Budget-killed, first time on this bead | Warm-resume requeue: worktree and branch are **preserved** (not rebuilt), so `--resume --fork-session` reattaches to the live Claude session and any uncommitted WIP snapshot is cited in the resume prompt |
| Budget-killed a second consecutive time | Parked `blocked` with a `needs-attention` note instead of spending a third cap — raise the account's per-agent budget or split the bead |
| Budget-killed with zero commits and pathological token volume (thrash guard) | Parked immediately, skipping even the first warm resume — the attempt is judged unrecoverable rather than retried |

Parked budget-kill slots surface the same way as any other `blocked` slot —
via `koryph board`, the TUI, and the health-patrol channels — with the note
prefixed `needs-attention:` and the accumulated `CostUSD` so far.

## Poll interval

The engine polls each running slot's `status.json` heartbeat every **10 seconds**
by default. To tune this per project, set `poll_seconds` in `koryph.project.json`:

```json
{ "poll_seconds": 20 }
```

A lower value increases poll frequency (more responsive to fast agents; slightly
higher filesystem load). A higher value is useful for long-running models where
frequent polling adds noise. The environment variable `KORYPH_POLL_SEC` and the
programmatic `Options.PollSec` field take precedence over the project config,
in that order.

## Exit code 4 — drained

```
exit 4  →  Outcome.Drained = true
```

The engine exits 4 when `bd ready` returns no eligible beads, no agents are running,
and the scheduler cannot form a wave. This is the normal end-of-work signal. Outer
loops (systemd timers, CI jobs, shell `while`) should treat exit 4 as success and stop
re-invoking until new work is pushed.

Exit 0 is also success but indicates the run stopped for another reason (quota pause,
`--once`, interrupted) with work potentially remaining.

## Nudge, stop, and tail

**nudge** — append an operator message to a running agent's `INBOX.md`. The agent polls
the inbox between steps and adjusts course:

```sh
koryph nudge --project myproject beads-042 "prefer the interface approach from issue 38"
```

**stop** — send SIGTERM to an agent's process group. The agent commits any open work
and exits cleanly; the engine detects the exit on the next poll tick and either requeues
(no commits) or proceeds to review/merge:

```sh
koryph stop --project myproject beads-042
```

Add `--force` to send SIGKILL instead — the agent is killed immediately and any
uncommitted in-progress work is lost:

```sh
koryph stop --project myproject beads-042 --force
```

To stop every live agent at once, use `--all` (combine with `--force` for SIGKILL).
On its own `--all` sweeps every managed project; add `--project` to scope the sweep
to one project:

```sh
koryph stop --all                              # every agent, every project
koryph stop --all --project myproject          # every agent in one project
koryph stop --all --force
```

**tail** — inspect a running or recently finished agent's output without attaching:

```sh
koryph tail --project myproject beads-042          # last 40 lines
koryph tail --project myproject beads-042 -n 100   # last 100 lines
```

Output includes `session.log` (human-readable progress), `stderr.log`, and the path to
`stream.jsonl` (the raw Claude event stream, useful for cost and token breakdowns).

## Drain and resize

koryph gives you three levers for slowing or stopping a loop, at three different scopes.
Pick the narrowest one that does what you need:

| Command | Scope | Effect |
|---|---|---|
| `koryph drain` | the whole loop | stop new dispatch; let whatever is running **finish**; then exit |
| `koryph stop <phase-id>` | one agent | SIGTERM that agent; the loop requeues or proceeds to merge as usual |
| `koryph stop <phase-id> --force` (or `--all --force`) | one agent, or every agent | SIGKILL immediately; **uncommitted work is lost** |

**drain** — request a graceful wind-down of the loop itself, without touching any running
process:

```sh
koryph drain --project myproject
```

The engine checks for a drain request at every scheduling boundary (every wave in `wave`
mode, every refill tick in `rolling` mode — see [Dispatch mode](#dispatch-mode-wave-vs-rolling)
above): once seen, no new bead is dispatched, but any agent already running is left
completely alone to finish its current attempt. The moment the last active slot lands, the
run exits through the normal drained path with reason `operator-drain` (distinct from the
ordinary `drained` reason, which means the frontier itself was empty) — even if more work is
still ready, it is left for the next invocation. The request is one-shot: it consumes itself
on that exit, so the next `koryph run` starts clean. Use `--all` to drain every registered
project at once:

```sh
koryph drain --all
```

If nothing is currently running when the drain fires, the run exits immediately — there is
nothing to wait for. A drain request left behind by a run that never got back around to a
boundary (e.g. the host died) is treated as stale and cleared **at the start of the next
run**, with a log line noting it — a leftover request can never silently prevent a fresh,
intentional run from doing any work.

**resize** — change a running loop's dispatch width without restarting it:

```sh
koryph resize --project myproject --max 5
```

Like the drain request, the override is re-read at every scheduling boundary, so it takes
effect on the very next wave or refill tick. It is clamped to `[1, max_concurrent_slots]`
unless you pass `--force` (useful for a deliberate short-lived burst above the project's
normal cap). `0` is not a valid width — that is what `drain` is for. Remove the override
and revert to the project's configured width with:

```sh
koryph resize --project myproject --clear
```

`--all` applies the same `--max`/`--clear` to every registered project. Both `drain` and
`resize` are recorded in the central audit log (`~/.koryph/audit.jsonl`), same as other
operator actions.

### Memory admission floor

Every dispatched agent is a separate `claude` subprocess plus a git worktree, so a wide
wave — especially with the [adaptive concurrency overlay](../developer-guide/global-governor.md)
probing the cap upward — can exhaust host RAM and OOM the machine. The **memory admission
floor** is a machine-wide guard: when the host's available memory drops below the floor, the
scheduler defers new dispatches to a later wave (running agents are never touched), exactly
like a concurrency-cap denial. It is a soft safety rail — a missing or unreadable memory
signal always fails open (dispatch proceeds).

A bead that declares [`res:<kind>` labels](#resource-labels-reskind) sharpens this check
from reactive to demand-aware: the floor comparison also subtracts every other live
lease's outstanding memory reservation for its declared kinds, so a wave of
cluster-provisioning beads reserves memory for the ones already admitted instead of
waiting for the host to actually feel the pressure. See
[Machine: resources](../concepts/resources.md#reservation-aware-memory-admission-and-the-ramp-window)
for the mechanics.

The floor is a machine property (like the global concurrency cap), so it lives in
`~/.koryph/governor.json`, per provider pool. It is **on by default**, sized to physical
memory (~1/8 of total RAM, clamped to a 1–8 GB band) — e.g. ~3 GB on a 24 GB host. Override
or turn it off with:

```sh
koryph governor set --min-free-memory-mb 4096          # explicit: defer while < 4 GB free
koryph governor set --min-free-memory-mb 0             # reset to the auto (sized) floor
koryph governor set --min-free-memory-mb -1            # disable the gate entirely
```

`koryph governor show` reports the active floor (auto, explicit, or disabled). For a one-off
run without editing `governor.json`, set `KORYPH_MIN_FREE_MEMORY_MB` in the environment
(same values: a positive floor, `0` for auto, negative to disable) — it overrides the
configured floor for that run. The available-memory signal is read from `/proc/meminfo`
(Linux) or `sysctl` + `vm_stat` (macOS); a platform with no probe fails open (gate off).

## Corpus audit: koryph plan audit

Before running the loop — or after changing `area_map` in `koryph.project.json` — run the
corpus audit to see how well your bead corpus parallelizes under the current scheduler rules:

```sh
koryph plan audit --project myproject
```

The audit is **read-only** (no bd mutations, no loop-behavior change). It reports:

| Section | What it means |
|---|---|
| **UNLABELED** | Beads whose footprint resolves to `domain:unknown` — they serialize one-per-wave. Add `area:*` or `fp:*` labels to unlock concurrency. |
| **NON-DISPATCHABLE** | Beads that will never dispatch as-is: wrong `issue_type` (epic/feature/decision/merge-request), `gt:*` gate label, `no-dispatch`, or `refactor-core`. |
| **CONFLICTING PAIRS** | Every pair of open, dependency-unordered beads whose footprints conflict under the scheduler's rules. A dependency-unordered pair could in principle run simultaneously but their footprints prevent it. The shared conflict tokens and the mode (`write-write`, `write-read`, or `mixed`) are named for each pair. |
| **PARALLEL WIDTH** | Current: maximum beads that can run simultaneously with current labels (greedy, no concurrency cap). Potential: same metric after virtually re-labeling every `domain:unknown` bead — shows the concurrency recoverable by labeling. |
| **CORPUS STATS** | Counts of `refactor-core` (orchestrator-authored on main; never loop-dispatched) and `no-dispatch` (manually deferred) beads. |

**Machine-readable output.** Pass `--json` to get a structured JSON report for agent consumption
(e.g., for a `koryph-replan` skill that automatically files label-fix beads):

```sh
koryph plan audit --project myproject --json | jq .parallel_width
```

**Typical workflow after changing `area_map`.** Refinining the area map changes which tokens
each `area:*` label resolves to, which can reveal new conflicts or unlock new parallelism:

```sh
# 1. Edit koryph.project.json: add/modify area_map entries.
# 2. Audit the corpus to see the impact:
koryph plan audit --project myproject
# 3. File labeling tasks for beads with domain:unknown, or split conflicting beads.
# 4. Re-run the audit to confirm the improvement.
```
