# AGENTS.md — koryph operating contract

The **canonical, runtime-neutral operating contract** every agent follows in this
repository — Claude Code, Codex, Cursor, Grok, or any other runtime. It states the
*rules*; the deep *how* lives in `docs/`, linked inline (every fact stated twice is a
future inconsistency). Claude-specific wiring lives in [CLAUDE.md](CLAUDE.md); nothing
here assumes you are Claude.

## Capability tiers (not model names)

koryph sizes work by runtime-agnostic **tier**, mapped to your runtime's models by its
adapter — see [agents/README.md](agents/README.md):

- **frontier** — strongest reasoning tier; required where an error poisons downstream
  automation (decomposition, footprint/dependency assignment, plan scoring, security
  review, recovery analysis).
- **standard** — capable coding tier; implementation against a precise spec, tests, docs.
- **light** — fast/cheap tier; exploration, summarization, log triage.

## Task tracking: beads only

All work lives in **beads** (`bd`) — never TodoWrite or markdown TODO lists. Loop:
`bd ready` → `bd show <id>` → `bd update <id> --claim` → `bd close <id>`. Persist durable
insight with `bd remember` (no MEMORY.md files). Run `bd prime` once per session for the
full reference; the managed block below is the short form.

## Dispatch-shaped beads

The wave loop **silently skips** beads that are not dispatch-shaped. To be dispatched:

- **Type `task`, `bug`, or `chore`.** `feature`/`epic`/`decision`/`merge-request` are
  containers and never dispatch; a `gt:*` gate label also blocks it.
- **Footprint-labeled**, so the scheduler can batch conflict-free work.

### Footprint grammar — `internal/sched/footprint.go`

Tokens split into **read** and **write** sets with RWMutex semantics: two beads sharing a
token conflict only when **at least one writes** it. Labels **compose** — a bead may carry
several and the token sets union:

- `area:<name>` → **write** tokens via `area_map` in `koryph.project.json`. Prefer the
  **narrowest honest area** (per-package: `area:sched`, `area:quota`, `area:dispatch`,
  `area:ledger`, `area:govern`, `area:merge`, `area:review`, `area:worktree`,
  `area:beads`, `area:registry`, `area:docs`; `area:engine` = the wave-loop package).
- `fp:<token>` → a raw **write** token.
- `fp:read:<token>` → a **read** token; read-only touches co-run with any other reader and
  exclude only a *writer* of that token (e.g. a docs bead that merely reads engine code).
- `area:*` and `fp:*` labels **union** — an `fp:*` label never suppresses the area write
  tokens; to narrow an over-broad area, drop the `area:*` label. A token declared both
  read and write collapses to **write**.
- **No footprint label** → the catch-all write token `domain:unknown`, colliding with
  every other unlabeled bead — unknowns serialize one-per-wave.

Over-broad areas cost only parallelism; under-broad risks a false-parallel merge conflict.
Audit a corpus with `koryph plan audit`. Full model:
[docs/user-guide/running-waves.md](docs/user-guide/running-waves.md).

### Never-dispatched labels

- `refactor-core` — authored and landed by the orchestrating session **on main**, never
  loop-dispatched (self-hosting safety rule).
- `no-dispatch` — manually deferred; skipped until the label is removed.

## The green gate

One command validates everything: `make gate` (format, build, vet, tests, lint — identical
to CI). It must be green before any work is called done; `make help` lists all targets.
Details: [CONTRIBUTING.md](CONTRIBUTING.md).

## Commits

- **Conventional Commits**: `type(scope): imperative subject` — `feat`, `fix`, `docs`,
  `chore`, `refactor`, `test`, `ci`, `build`, `perf`, `style`.
- **DCO sign-off** on every commit: `git commit -s`.
- **SSH-signed** commits are required, enforced by local hooks, CI, and GitHub rulesets;
  enable signing first — [docs/user-guide/signing.md](docs/user-guide/signing.md).

Commit early and often — commits are the only durable checkpoint; uncommitted work is lost
if a run is interrupted.

## Protected paths and boundary guards

Worktree merges are **refused** if the branch touches any of: `.claude/`, `.beads/`,
`hooks/`, `agents/`, `.github/`, `koryph.project.json`, `Makefile`,
`.pre-commit-config.yaml`, `.envrc`, `LICENSE`. Headless agents additionally run behind
boundary guards (installed outside any writable worktree) that **deterministically block**
`git checkout main`, `git merge`, `git push`, `bd close`, touching another worktree, or
writing koryph's own enforcement surface. A guard denial means you drifted — those actions
belong to the orchestrator, not the agent; do not route around it.

## The merge pipeline

A finished branch (`agent/<bead-id>`) lands through one pipeline regardless of policy:
**sync default branch → rebase onto it → run the green gate → fast-forward-only merge**.
ff-only is deliberate — it keeps the gate-checked, reviewed, SSH-signed commits
byte-for-byte (merge/squash/rebase-merge would break the signatures). Merge policy (`auto`
/ `manual` / `pr`) is set per project or per epic; `pr` opens a GitHub PR instead of
fast-forwarding, landed later with `koryph land`. Full flow:
[docs/user-guide/running-waves.md](docs/user-guide/running-waves.md).

## Dispatch modes

- **rolling** (default) — refill continuously; each poll tick tops off freed slots without
  waiting for the batch.
- **wave** — dispatch a conflict-free batch, wait for every slot to land, rescan.
  Footprint conflicts are honored identically in both modes.

Set via `dispatch_mode` in `koryph.project.json` or `--dispatch-mode`.

## Operator verbs

Pick the narrowest lever — all in
[docs/user-guide/running-waves.md](docs/user-guide/running-waves.md):

- `koryph drain` — stop new dispatch, let running agents finish, then exit.
- `koryph resize --max N` — change a live loop's width (re-read at each boundary).
- `koryph stop <id>` — SIGTERM one agent (it commits and exits; loop requeues or merges).
- `koryph stop <id> --force` / `koryph stop --all --force` — SIGKILL; **uncommitted work
  is lost**.
- `koryph nudge <id> "…"` — append a course-correction to the agent's `INBOX.md`.
- `koryph tail <id>` — inspect a running agent's output without attaching.

## Governors

Two orthogonal gates guard every dispatch; work proceeds only when **both** allow it. The
**cost governor** (`internal/quota`) tracks each account's rolling 5-hour and 7-day spend
against a calibrated ceiling and steps OK → Warn → Drain → Stop, failing closed when usage
is unmeasurable. The **machine-global concurrency governor** (`internal/govern`) caps the
agents running across *all* projects so parallel `koryph run` invocations cannot
collectively trip Claude rate limits; an optional **AIMD adaptive overlay** (`koryph
governor set --adaptive`) turns the static cap into a congestion controller — additive
increase after quiet, multiplicative decrease on a rate-limit signal, hardened by settle
windows, a circuit breaker, and dispatch smoothing. See
[docs/user-guide/billing-and-quota.md](docs/user-guide/billing-and-quota.md).

## Non-interactive shell

Always pass non-interactive flags so a `-i`-aliased tool cannot hang on a prompt:
`rm -f`, `rm -rf`, `cp -f`, `mv -f`; `ssh`/`scp -o BatchMode=yes`; `apt-get -y`.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking — commands and rules are in
"Task tracking: beads only" above; run `bd prime` for the full workflow.

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions. It is subordinate to the operating contract above and to any explicit user, repository, or orchestrator instruction. Do not commit, push, or sync unless the active instructions grant it; at handoff, report changed files, validation, issue status, and any blocked commit/push step.
<!-- END BEADS INTEGRATION -->
