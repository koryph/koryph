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
run's spend, pass `--budget <USD>`: once the run's cumulative agent cost reaches
the ceiling, no new agents are dispatched (active ones finish) and the run pauses
with a `budget-cap` reason. This is a per-run ceiling, separate from the account
cost governor and the global concurrency governor.

## The bd ready frontier

Every wave starts with `bd ready`, which returns all issues whose dependencies are
closed. Container beads (epics, features, decisions, merge-requests) and beads with a
`gt:*` gate label are silently dropped. Beads labeled `no-dispatch` or `refactor-core`
are deferred with a logged reason. The remainder are sorted by priority (P0 first) and
passed to the conflict filter.

Scope a run to a single epic:

```sh
koryph run --project myproject --parent beads-001
```

## Footprint labels: fp:\* and area:\*

The scheduler prevents two agents from touching the same code at once via _footprint
tokens_. Two beads conflict when they share any token; the lower-priority bead is
deferred to the next wave.

**Explicit `fp:` labels** — the values after the prefix become conflict tokens directly:

```
fp:auth fp:billing   →  tokens: ["auth", "billing"]
```

**`area:` labels** — resolved through `koryph.project.json`'s `area_map`:

```json
"area_map": { "api": ["auth", "billing", "routes"] }
```

A bead with `area:api` gets tokens `["auth", "billing", "routes"]` and conflicts with
any bead carrying any of those tokens (whether via `fp:*` or another `area:*` mapping).

**No footprint label** — the bead receives the catch-all token `domain:unknown`, which
conflicts with every other unknown bead. Unknowns serialize: only one runs per wave.

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
next `review-pr` because state is read live from GitHub.

> More of the review workbench — reviewing the whole open-PR queue, inline line comments, an
> IDE handoff loop, and detecting PRs closed by any means — is tracked under its epic and
> lands incrementally.

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

**tail** — inspect a running or recently finished agent's output without attaching:

```sh
koryph tail --project myproject beads-042          # last 40 lines
koryph tail --project myproject beads-042 --n 100  # last 100 lines
```

Output includes `session.log` (human-readable progress), `stderr.log`, and the path to
`stream.jsonl` (the raw Claude event stream, useful for cost and token breakdowns).
