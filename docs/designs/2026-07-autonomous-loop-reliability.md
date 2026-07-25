<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Autonomous loop reliability: simplify control, preserve safety, restore quality

Status: approved by the operator on 2026-07-25; pre-file plan scoring in
progress before Beads decomposition.

Origin: a 36-hour Koryph self-hosting incident in which the autonomous loop
produced useful, reviewed commits but failed the product objective: cost-effective,
high-quality autonomous completion.

## Problem

Koryph has accumulated individually reasonable protections around every observed
failure. The protections do not have clear single owners. Worker instructions,
the engine, the merge path, an external shell wrapper, a watcher, the governor,
and semantic reviewers all perform overlapping parts of completion, validation,
retry, recovery, and health control.

The result is safe in several important ways but operationally poor:

- useful work is repeatedly tested, reviewed, and re-dispatched;
- environment and orchestration failures are mixed with code defects;
- a heartbeat that never reached `done` can enter review;
- final-attempt implementation escalates too broadly to the frontier model;
- a security persona reviews every ordinary change;
- a single finalization FIFO serializes review and merge-gate work;
- conservative macOS memory accounting starves width even when the kernel
  reports low pressure;
- phase-local build caches and temporary trees consume hundreds of megabytes per
  attempt;
- a shell wrapper creates a new run ledger every idle minute;
- the reported token total double-counts Codex cached input;
- prompt and repository instructions repeat and sometimes contradict one
  another.

This is not primarily a model-strength failure. It is a control-plane design
failure that makes capable models repeat work and makes strong quality controls
expensive.

## Incident evidence

The retained ledgers and review artifacts from the 36-hour window show:

| Signal | Observed |
|---|---:|
| Run ledgers | 270 |
| Empty runs | 233 (86%) |
| Slot records | 62 |
| Implementation attempts | 97 |
| Retry attempts beyond the first | 35 (56% of slot count) |
| Unique Beads attempted | 28 |
| Latest outcomes | 15 merged, 10 blocked, 3 merge-pending |
| Review iterations | 27 |
| Retained semantic reviews | 87 |
| Blocking reviews | 13 |
| Review findings | 28 blocking, 8 major, 47 minor |
| Criteria results | 88 satisfied, 17 unsatisfied, 2 not-applicable |

The current loop log contains 54 implementation dispatches, 12 requeues, 7
merges, 30 capability parks, 3 frontier escalations, 14 watcher wakes, 50
cache-ratio warnings, and 77 governor-cap-or-memory-floor deferrals. The log is
only part of the full 36-hour interval, so it is not the denominator for the
ledger totals above.

The quality signal is mixed:

- The semantic gate is valuable. It caught real release-ordering, deployment,
  identity, permission, process-signalling, model-routing, and path-traversal
  defects. It must remain fail closed.
- Final merged quality is materially better because of that gate.
- First-pass quality is weak: 5 of 15 merged Beads required a second
  implementation attempt, and the stopped DNS Bead reached a third attempt
  after two rounds of new blocking findings.
- Autonomous completion is only 15 of 28 attempted Beads. Many blocks were
  host, sandbox, validation, or orchestration failures rather than unsolved
  implementation problems.

Token telemetry currently exaggerates the scale of input:

- Codex reports `input_tokens` as total input, with
  `cached_input_tokens` as a subset.
- Koryph stores both values as disjoint classes and sums them.
- The retained window therefore reports 303,776,150 input tokens plus
  294,574,848 cache-read tokens even though normalized fresh input is
  9,201,302 and the actual cache share is about 96.97%.
- The cache-ratio tripwire consequently warns on healthy Codex sessions, and
  token totals and any token-based kill logic are biased.

The coarse estimated cost of about $229.50 is not an invoice and is not reliable
enough for product decisions until the accounting semantics are fixed.

The stopped `koryph-bbr.3` attempt is a representative trace:

1. Terra implemented and committed a plausible DNS check.
2. Review found three blocking correctness defects.
3. Terra repaired them; review found two additional defects.
4. The generic final-attempt rule promoted the next implementation to Sol.
5. The worker launched three overlapping full `make gate-agent` processes.
6. Its status remained `testing`, but branch cleanliness and commits made the
   candidate eligible for review because `assessCandidate` rejects explicit
   failure states rather than requiring a terminal success state.
7. The operator stopped the loop. Two committed changes and the Sol worktree
   edits remain preserved for later recovery.

## Goals

- Restore autonomous completion without weakening merge safety.
- Make one component own each lifecycle decision.
- Require an engine-verifiable terminal result before validation or review.
- Run one authoritative full gate for each candidate/base/config tuple.
- Review the exact gated candidate and every atomic acceptance criterion.
- Route retries by typed cause, not by a shared attempt counter or note text.
- Keep frontier implementation rare and evidence-justified.
- Replace the shell wrapper and repair watcher with a binary-native,
  zero-model-token idle supervisor.
- Prevent one phase from starting duplicate broad validation process trees.
- Admit work from real host pressure and observed demand without risking
  another OOM incident.
- Correct token, cost, latency, retry, and storage measurements.
- Reduce prompt and instruction duplication while keeping runtime-neutral
  containment.
- Release only after a bounded canary meets explicit safety, quality, cost, and
  velocity objectives.

## Non-goals

- Removing semantic review, protected-path refusal, DCO/signature checks,
  worktree isolation, or serialized default-branch mutation.
- Making Sol the default implementer or using a stronger model to mask
  environment and engine defects.
- Letting a watcher rewrite Beads, branches, credentials, or policy from log
  text.
- Claiming every external capability failure is autonomously repairable.
  Unavailable credentials, networks, or operator-owned services must park with
  evidence and resume only after that evidence changes.
- Building a speculative multi-candidate merge train in the first repair.
- Reopening completed historical Beads merely to rewrite their records.

## Current control conflicts

### Validation has four owners

The repository contract says work must pass `make gate-agent`; the implementer
persona says to run project lint/test commands; the compiled prompt lists the
six raw project gate commands and recommends `make gate-agent`; and
`merge.Merge` runs the six commands again after rebasing. Optional post-
implementation stages can add another validation pass.

This duplication directly caused concurrent gates inside one phase. It also
spends reviewer/model time before the authoritative merge gate is known to
pass.

### Completion is advisory instead of imperative

`status.json` is both a heartbeat and a completion signal. The engine rejects
explicit `blocked|failed|error|cancelled` states, but it does not require
`done`. A process that exits while the last heartbeat says `testing` can enter
finalization if commits exist and the worktree is clean. `SUMMARY.md` and
status content are not bound to an attempt, candidate SHA, or base SHA, so stale
artifacts can satisfy later attempts.

The closed Bead `koryph-lv07.19` strengthened prompt wording but did not make the
terminal verdict an engine invariant.

### Retry state is an accumulation of special cases

The engine has separate attempt, review, gate, merge, conflict, rate-limit,
budget, turn-exhaustion, generic-block, and capability counters. Decisions are
spread through `poll.go` and are sometimes carried through string reasons and
notes. This makes it easy to lose a counter across a new path and hard to prove
that a failure class cannot reach the wrong model.

The current final-attempt rule escalates most Bead-fault retries, including
review findings and some mechanical failures, to the configured recovery model.
It does not first prove that model capability is the unresolved constraint.

### Review mixes acceptance, correctness, and security

Every normal review defaults to `koryph-security-reviewer`, with a frontier-
quality security posture and scanner expectations. The review prompt itself is
a general acceptance/correctness audit. These are different jobs.

`splitAcceptance` separates only lines. Most current Beads carry several
semicolon-separated outcomes on one line, so the reviewer sees them as a single
criterion. That encourages progressive findings: a later pass notices another
clause that was hidden inside `AC1`.

Review currently runs before the authoritative merge gate. The engine can spend
a long semantic review on a candidate that deterministic validation will
reject.

### Finalization is responsive but serialized too broadly

The new finalization lane keeps the poll goroutine responsive, which is correct,
but one FIFO owns review, gate/merge, and PR finalization. A slow review or full
gate delays every completed sibling. The merge mutation itself needs a lock;
the entire finalization pipeline does not.

### The loop control plane is outside the product

`commands/koryph-loop.md` generates a shell wrapper that calls
`koryph run --once --resume` repeatedly. The wrapper created 233 empty run
ledgers during the incident. A deployed variant also added
`--allow-unvalidated`, contradicting the planning quality gate.

The generated watcher scrapes logs and telemetry, polls cockpit state, and
describes manual or broad repair actions. A deployed watcher ran reconciliation
and doctor fixes from broad text matches. It cannot safely know candidate,
lease, retry, or capability transaction state, and it cannot provide semantic
recovery while the outer agent session is asleep.

### Memory protection observes the wrong macOS signal

The prior OOM fix correctly stopped counting all inactive pages as immediately
available. On a healthy Mac this can leave only free, speculative, and
purgeable pages in the admission value even while `memory_pressure -Q` reports
low pressure and no swap. The current message combines global-cap and memory
denials, hiding which one deferred a Bead.

The configured 256 MB estimated agent footprint is also far below the observed
1.49 GB peak of the DNS phase. Duplicate gates inflated that peak, so simply
raising the estimate before fixing process duplication would trade throughput
for a symptom.

The older resource design fails open on probe/config errors. For a
correctness-neutral advisory this was reasonable; for the safety mechanism
that prevents an unrecoverable desktop OOM, unrestricted fail-open admission is
not acceptable.

### Caches and evidence have incompatible retention

Codex uses a phase-local `GOCACHE`; an active run can retain 400–600 MB per
phase plus interrupted `go-build*` temporary directories. This isolates
writers but defeats Go's content-addressed cross-process cache and repeats
compilation. A prior shared host cache grew to tens of gigabytes because Koryph
did not own a budget.

Terminal cache pruning exists but is tied to explicit GC and leaves the active
or latest run untouched. The current `.plan-logs` corpus contains roughly
140,000 files. Durable evidence, transient compiler state, full transcripts,
and committed planning snapshots need different retention policies.

### Instructions repeat instead of compose

Codex receives the full implementer persona, the full engine preamble, the
project gate block, and the repository's `AGENTS.md`. Boundary, commit,
heartbeat, inbox, summary, and validation rules appear more than once.

Concrete conflicts include:

- persona inbox: repository-root `INBOX.md`; preamble inbox: phase directory;
- persona protected-path examples: not the actual project configuration;
- persona: run project lint/tests; engine should own the full gate;
- project block: six raw commands; output-economy block: run `make gate-agent`;
- old comments and docs name Claude models while the runtime-neutral router
  resolves Codex tiers differently;
- permanent `refactor-core`/`no-dispatch` conventions conflict with the
  operator goal that work become schedulable once dependencies and the stable
  engine substrate exist.

## Design

### 1. One owner per lifecycle responsibility

| Responsibility | Owner |
|---|---|
| Focused implementation checks | worker |
| Terminal result construction | `koryph phase complete` |
| Candidate/result validation | engine candidate lifecycle |
| Full deterministic project gate | Koryph validation service |
| General acceptance/correctness review | general reviewer |
| Security review | risk-triggered security reviewer |
| Retry/escalation decision | typed recovery policy |
| Default-branch mutation | merge lock |
| Idle scheduling and deterministic recovery | binary-native loop supervisor |
| Alerts | zero-token health notifier |

No other layer repeats the owned action. In particular, workers do not run the
full project gate, reviewers do not compensate for missing deterministic gate
evidence, and a watcher does not schedule or mutate recovery state.

### 2. Imperative, SHA-bound result contract

Keep `status.json` as a non-terminal heartbeat only. A worker finishes with:

```text
koryph phase complete --evidence <path>
```

The command derives and atomically writes a versioned result manifest. The
worker does not supply trusted identity fields. The manifest contains:

- run, phase, attempt, and dispatch-generation identifiers;
- dispatch base SHA and current candidate SHA;
- a terminal `done` state;
- worktree-clean and commit-count observations;
- summary path and digest;
- focused-test evidence with command, exit status, and log path;
- an acceptance evidence matrix keyed by stable criterion ID.

The engine accepts a candidate only when the manifest matches the live slot
generation, candidate SHA, and base SHA; the branch has commits; and the
worktree is clean. A heartbeat state, summary file, or process exit alone never
enters finalization.

If the process exits after useful commits but before a valid result, the
candidate enters `completion-repair`, not generic implementation. One
same-tier repair prompt names only the missing manifest/evidence action. Dirty
or commitless work remains preserved and parked after its bounded repair
budget.

Legacy candidates remain preserved. They may be explicitly adopted through a
one-time compatibility path that recomputes identity and requires a fresh
focused check; stale result artifacts are never grandfathered.

### 3. Atomic acceptance and completion evidence

The planning snapshot stores acceptance criteria as an ordered array with
stable IDs. Filing renders one criterion per line:

```text
AC1: observable outcome and boundary
AC2: failure behavior
AC3: exact validation evidence
```

The strict planning gate rejects:

- a compound semicolon-packed acceptance field;
- missing or duplicate IDs;
- criteria that have no observable result;
- a unit that provides more than one independent capability;
- a result matrix that omits an applicable criterion.

The worker's completion matrix maps every criterion to files and focused-test
evidence. The general reviewer receives the same parsed array and the matrix;
it cannot reinterpret an entire paragraph as `AC1`.

This extends the open planning epic `koryph-6an8`, whose four children are
closed while the parent remains open. Parent closure and post-filing
consistency become part of the gate rather than an operator convention.

### 4. Gate once, then review the gated candidate

The finalization stages become:

1. validate the terminal result manifest;
2. rebase the candidate onto a captured default-branch SHA;
3. run the authoritative full project gate through a project-wide
   single-flight gate lane;
4. persist gate evidence keyed by candidate SHA, base SHA, gate-config digest,
   command digest, and binary version;
5. run general acceptance/correctness review against that exact evidence;
6. run security review only when risk policy requires it;
7. acquire the short merge lock, verify the default branch still equals the
   gated base, then fast-forward and push.

If the base moved, Koryph rebases and invalidates only evidence whose key
changed. The full gate reruns for the new base; semantic review can be reused
only when the candidate diff digest and acceptance evidence are unchanged and
the reviewer declared no base-sensitive finding. Otherwise it reruns. This is
safe without holding the merge lock for minutes.

Gate, review, and merge each have their own bounded lane:

- one gate per project by default, because it is host-heavy;
- one or two review slots according to provider capacity;
- one short merge mutation globally per repository.

The lane scheduler preserves candidate age and prevents starvation. It records
queue and service time separately.

### 5. Risk-tiered review

Introduce a general `koryph-reviewer` persona for acceptance, correctness,
regression, scope, and evidence checks. Routine review uses the standard tier
at high effort. It gets one normal attempt and one retry only for a classified
transient runtime failure or malformed envelope.

The frontier security reviewer runs when either:

- the Bead carries an explicit security-risk label;
- the diff touches configured high-risk surfaces such as authentication,
  secret handling, signing, permissions, workflow privilege, boundary hooks,
  merge enforcement, or cryptography; or
- the general reviewer returns `security_review_required` with evidence.

Security review remains fail closed. A non-security change does not pay for
security scanners and frontier reasoning by default.

Prior review findings are cumulative evidence. A repair review must verify all
previous blocking findings in addition to the complete criterion set so that
successive passes do not rediscover the contract one clause at a time.

### 6. Typed outcome and recovery policy

Replace string-reason branching with one pure transition function:

```text
Decision = Policy(current state, typed outcome, budgets, evidence change)
```

Every path emits one outcome class:

| Outcome | Default action | Model consequence |
|---|---|---|
| candidate-ready | enter validation | none |
| completion-contract-missing | one targeted completion repair | same standard tier |
| code-defect | one targeted repair with gate evidence | standard tier |
| semantic-defect | one targeted repair with cumulative review | standard tier |
| persistent-semantic-defect | frontier recovery analysis, then targeted repair | frontier analysis; standard implementation |
| security-defect | targeted repair; re-review | standard implementation; frontier security review |
| capability-unavailable | durable evidence hold | no model |
| runtime-transient/rate-limit | bounded same-tier retry | no fault escalation |
| merge-base-moved | rebase and revalidate | no model |
| commit-style/mechanical | deterministic repair or one targeted repair | never frontier |
| operator-stop/drain | terminal/park | no retry |
| engine-invariant | open circuit and stop dispatch | no model |

Frontier implementation is a separate, disabled-by-default final action. It is
allowed only when a frontier recovery analysis returns a structured
`model-capability` diagnosis, confirms that environment, contract, gate,
scope, and orchestration causes are excluded, and records the reason in the
ledger. It is never selected merely because the attempt number is final.

The first release uses small fixed budgets rather than another large config
surface:

- one completion-contract repair;
- one code-defect repair per distinct gate fingerprint;
- one ordinary semantic repair;
- one frontier recovery analysis after a repeated semantic defect;
- one post-analysis standard repair;
- at most two transient runtime retries with backoff;
- zero unchanged capability retries.

The transition table is exhaustively tested. Counters remain observability
facts, not independent policy owners.

### 7. Phase process safety and duplicate validation control

Extend existing epic `koryph-4rk6.5` rather than creating a second duplicate-
gate effort.

The worker contract permits focused package tests and forbids the full project
gate. Koryph classifies known broad commands (`make gate*`, full-repository
`go test`, full build/vet/lint) and supervises the phase process cohort:

- at most one broad command signature may be live in a phase;
- a second matching command receives the existing command's PID, log path, and
  status instead of starting;
- a worker-started full gate is rejected because the validation service owns it;
- focused commands remain available;
- process identity includes start time and process-group membership to avoid PID
  reuse;
- a redundant group is terminated gracefully and specifically, never by killing
  every descendant;
- command starts, reuse, denial, peak RSS, CPU, and duration are ledger events.

Runtimes with pre-tool hooks reject before execution. Runtimes without hooks
receive phase-local command shims where safe and the process supervisor as a
backstop. The authoritative validation service uses a separate role token and
is not blocked by the worker policy.

### 8. Pressure-aware, fail-safe host admission

On Darwin, kernel memory pressure becomes the primary admission health signal.
The conservative free/speculative/purgeable byte estimate remains useful but
is not treated as a complete picture of reclaimable memory.

Admission combines:

- pressure band: normal, warning, or critical;
- current swap/compressor trend;
- live process-cohort RSS;
- outstanding declared resource reservations;
- an observed per-runtime process estimate derived from recent successful
  attempts after duplicate-command removal;
- the machine-wide count cap.

Normal pressure allows reservation-based admission even when the conservative
page count is low. Warning pressure stops new admission. Persistent critical
pressure opens the circuit and gracefully checkpoints/terminates the newest
recoverable agent cohort if necessary to protect the host; it never SIGKILLs
the fleet.

Probe failure uses the last known good pressure sample for a short TTL. With no
usable sample, admission degrades to one active agent rather than failing open
without limit. Deferral events identify the exact cause instead of combining
cap and memory into one message.

Observed estimates use hysteresis and a conservative percentile plus margin.
They are not recalibrated from failed attempts containing duplicate gates.

### 9. Binary-native autonomous loop

Add a long-lived `koryph loop` supervisor to the binary. It owns:

- ready-work observation with filesystem/event wakes and bounded exponential
  idle backoff;
- engine resume after process restart;
- zero-model-token idle state;
- typed retry and capability evidence;
- bounded crash restart with a circuit breaker;
- drain, stop, status, and inject control;
- deterministic health checks;
- durable supervisor state and structured alerts.

The supervisor does not create a run directory until work is selected. A
drained repository therefore creates no minute-by-minute ledgers and launches
no model process.

The zero-token watcher becomes an internal health notifier, not a repair
engine. It may emit alerts and run read-only probes. Deterministic repairs live
behind typed engine actions; semantic recovery uses the explicitly budgeted
recovery-analysis path. Broad `doctor --fix`, Bead reopening, signing changes,
and branch mutations are not triggered by regexes.

`commands/koryph-loop.md` becomes a thin invocation and operational explanation
of the binary command. `--allow-unvalidated` is break-glass, incompatible with
autonomous mode, and never persisted into loop configuration.

### 10. Correct accounting and bounded runtime artifacts

Normalize runtime usage at the adapter boundary:

- `fresh_input = max(0, input_tokens - cached_input_tokens)` for Codex;
- store provider-total input separately when useful for audit;
- keep cache-read, cache-creation, output, and fresh input disjoint;
- compute totals, cache ratios, cost estimates, and kill thresholds from the
  normalized schema;
- version the ledger semantics so old runs are labeled rather than silently
  mixed.

Use one bounded project-scoped Go build cache keyed by Go version, OS, and
architecture. Go's content-addressed cache is concurrency-safe; project
scoping prevents ambient cross-project trust. Keep the module cache
project-scoped. Put temporary compiler trees in a dedicated phase temp
directory and remove them at terminal transition.

Retention classes:

- live/nonterminal: preserve all evidence and mutable state;
- terminal immediate: remove caches and temporary build trees;
- compact durable evidence: ledger, result manifest, summaries, gate verdict,
  review verdict, audit events, and selected log tails;
- full streams/transcripts: retain by age and failure policy, then compress or
  prune;
- pre-file planning snapshots: prune after the design is committed and the
  filed graph digest is recorded in Beads;
- project budget: warn at a soft limit and reclaim eligible artifacts before a
  hard limit.

GC runs after terminal transitions and at loop idle boundaries. It never
requires the operator to notice disk growth first.

### 11. One canonical dispatch contract

Represent dispatch rules as named semantic clauses with one source owner:

- `AGENTS.md`: repository operating contract and project conventions;
- role persona: only role-specific behavior;
- engine contract: worktree boundary, phase paths, and result command;
- volatile task tail: Bead contract, exact atomic criteria, prior findings,
  and focused-test scope.

The runtime adapter declares whether repository instructions are loaded
natively. Koryph includes only missing clauses. Projection tests assert clause
identity and reject duplicate or contradictory owners.

Specific instruction changes:

- one phase-directory inbox path;
- no stale protected-path examples in personas;
- workers run focused tests, not `make gate-agent`;
- the project gate list is engine evidence, not worker instruction;
- only `koryph phase complete` creates terminal success;
- runtime-neutral tier names replace model-name comments;
- `commands/koryph-loop.md` invokes the native supervisor;
- temporary direct-supervision during this repair is an execution mode, not a
  permanent reason that future dependency-ready Beads cannot be scheduled.

## Decision ledger

| ID | Stable decision | Reason |
|---|---|---|
| D1 | Keep fail-closed merge and semantic quality controls. | They prevented real defects from merging. |
| D2 | A terminal result is a signed-by-context manifest, not a heartbeat or prose summary. | Candidate eligibility must be imperative and stale-proof. |
| D3 | Workers own focused tests; Koryph owns one full gate. | Eliminates duplicate gates and makes evidence authoritative. |
| D4 | Gate precedes semantic review. | Deterministic failures should not consume reviewer time. |
| D5 | General review and security review are separate roles. | Ordinary correctness does not always require frontier security analysis. |
| D6 | Retry policy is a typed transition table. | Attempt count alone does not identify the remedy. |
| D7 | Frontier is primarily design, security, and recovery analysis; frontier implementation requires a recorded diagnosis. | Preserves quality without making hard-block cost unbounded. |
| D8 | Autonomous looping is a binary capability, not generated shell control. | The engine alone has transactional state and can idle without ledger churn. |
| D9 | Watchers notify; they do not infer mutations from text. | Regex-triggered repair is unsafe and races authoritative state. |
| D10 | Host safety may gracefully stop one recoverable cohort under persistent critical pressure. | “Never interrupt” cannot outrank preventing a machine-wide OOM. |
| D11 | Runtime telemetry is normalized at the adapter boundary. | Downstream policy must not know provider-specific token semantics. |
| D12 | Build caches are project-shared and budgeted; temporary state is phase-local and terminally removed. | Reuse and isolation are both required. |
| D13 | Release is SLO-gated, not “gate passed, restart everything.” | Unit correctness does not prove autonomous product behavior. |

## Alternatives rejected

### Keep the wrapper and add more retries

Rejected. The wrapper does not own attempts, leases, candidate generations,
capability fingerprints, or merge evidence. More wrapper logic creates more
split-brain recovery.

### Use Sol for every difficult implementation

Rejected. The observed long tail contains duplicate gates, false memory
deferrals, environment blocks, malformed completion, and merge mechanics.
Stronger implementation reasoning cannot repair those causes.

### Weaken or remove semantic review

Rejected. Review found serious defects that deterministic tests missed. The
fix is to make review targeted, evidence-bound, and correctly ordered.

### Let every worker run the full gate and trust single-flight prompts

Rejected. Prompt wording already failed. Ownership and enforcement must make a
duplicate impossible or observable.

### Restore inactive pages to macOS “available” memory

Rejected. That recreates the OOM failure direction. Pressure-aware admission
uses the kernel's health signal without pretending all inactive pages are free.

### Treat `SUMMARY.md` as the result contract

Rejected. Prose is not attempt- or SHA-bound and cannot be mechanically
validated.

### Preserve every run artifact indefinitely

Rejected. Compiler caches and full transcripts are not equivalent to durable
audit evidence. Unlimited retention already caused material disk exhaustion.

## Implementation outline

Each unit provides exactly one capability. Control-plane units are implemented
under direct supervision while the installed loop is stopped. They are not
permanently unschedulable; after the canary, ordinary dependency-ready work
returns to the loop.

| Unit | Provides | Exact owned paths/prefixes | Depends on | Resources |
|---|---|---|---|---|
| U1 candidate lifecycle | `typed-candidate-lifecycle` | `internal/phasecontrol/result.go`, `internal/phasecontrol/result_test.go`, `cmd/koryph/phase.go`, `cmd/koryph/phase_test.go`, `internal/engine/candidate.go`, `internal/engine/candidate_test.go`, `internal/engine/outcome.go`, `internal/engine/retry_policy.go`, `internal/engine/retry_policy_test.go`, `internal/ledger/types.go`, `internal/ledger/classify.go`, `internal/ledger/classify_test.go` | none | none |
| U2 acceptance evidence | `atomic-acceptance-evidence` | new child under `koryph-6an8`; `internal/plan/criteria.go`, `internal/plan/criteria_test.go`, `internal/plan/audit.go`, `internal/plan/audit_test.go`, `internal/review/criteria.go`, `internal/review/criteria_test.go`, `internal/commands/koryph-design.md`, `internal/commands/koryph-plan.md` | U1 | none |
| U3 quality pipeline | `sha-bound-quality-pipeline` | `internal/engine/finalize.go`, `internal/engine/finalize_test.go`, `internal/engine/poll.go`, `internal/engine/poll_test.go`, `internal/merge/gate.go`, `internal/merge/gate_test.go`, `internal/merge/merge.go`, `internal/merge/merge_test.go`, `internal/review/review.go`, `internal/review/review_test.go`, `internal/review/types.go`, `internal/modelroute/persona.go`, `internal/modelroute/persona_test.go` | U1, U2 | none |
| U4 phase process safety | `phase-command-singleflight` | new child under existing epic `koryph-4rk6.5`; `internal/engine/process_guard.go`, `internal/engine/process_guard_test.go`, `internal/resmon/command.go`, `internal/resmon/command_test.go`, `internal/runtime/codex/codex.go`, `internal/runtime/codex/codex_test.go`, `scripts/gate-agent.sh`, `scripts/gate-agent_test.go` | U1 | `res:codex-fixture` (predeclared in project and host vocabulary) |
| U5 host admission | `pressure-aware-admission` | `internal/sysmem/sysmem.go`, `internal/sysmem/sysmem_darwin.go`, `internal/sysmem/sysmem_darwin_test.go`, `internal/govern/govern.go`, `internal/govern/govern_test.go`, `internal/govern/types.go`, `internal/engine/govern.go`, `internal/engine/memgate_test.go`, `internal/resmon/usage.go`, `internal/resmon/usage_test.go` | U4 for clean calibration data | none |
| U6 native supervisor | `native-autonomous-loop` | `cmd/koryph/loop.go`, `cmd/koryph/loop_test.go`, `internal/loop`, `internal/engine/run.go`, `internal/engine/rolling.go`, `internal/engine/recover.go`, `internal/commands/koryph-loop.md` | U1, U3, U5 | none; enforces fixed-cohort canary admission, two-slot start, five-terminal width gate, and tripwire drain |
| U7 usage semantics | `normalized-token-accounting` | `internal/runtime/events.go`, `internal/runtime/codex/events.go`, `internal/runtime/codex/events_test.go`, `internal/ledger/types.go`, `internal/metrics/tokens.go`, `internal/metrics/tokens_test.go`, `internal/metrics/experiment.go`, `internal/metrics/experiment_test.go`, `internal/quota/thrash.go`, `internal/quota/thrash_test.go` | U1 | none |
| U8 artifact lifecycle | `bounded-runtime-artifacts` | `internal/runtime/codex/codex.go`, `internal/runtime/codex/codex_test.go`, `internal/gc/gc.go`, `internal/gc/gc_test.go`, `internal/gc/policy.go`, `internal/gc/policy_test.go`, `cmd/koryph/gc.go`, `cmd/koryph/gc_test.go` | U4, U6 | none; active/final canary reports are retained |
| U9 dispatch contract | `single-source-dispatch-contract` | `internal/promptc/compile.go`, `internal/promptc/compile_test.go`, `internal/runtime/codex/persona.go`, `internal/runtime/codex/persona_test.go`, `internal/agentsmd/template.md`, `internal/agentsmd/template_test.go` | U1, U2, U3, U6 | none |
| U10 protected projections | `orchestrator-contract-projections` | `agents/koryph-implementer.md`, `agents/koryph-architect.md`, `agents/koryph-plan-scorer.md`, `agents/koryph-reviewer.md`, `agents/koryph-security-reviewer.md`, `AGENTS.md`, `commands/koryph-design.md`, `commands/koryph-plan.md`, `commands/koryph-loop.md` | U2, U3, U6, U9 | none; `no-dispatch` |
| U11 SLO evidence | `autonomy-slo-report` | `internal/metrics/autonomy.go`, `internal/metrics/autonomy_test.go`, `cmd/koryph/metrics.go`, `cmd/koryph/metrics_test.go`, `internal/doctor/autonomy.go`, `internal/doctor/autonomy_test.go`, `docs/user-guide/running-waves.md`, `docs/reference/cli.md` | U3, U5, U6, U7, U8, U10 | none; atomically writes the schema-versioned immutable canary report |
| U12 controlled release | `installed-autonomy-canary` | Beads reconciliation, preserved `koryph-bbr.3` worktree recovery, signed commits, `make gate-agent`, `make build`, `make install`, installed-version verification, bounded canary, loop enablement | U1–U11 | local build/signing environment; `no-dispatch` |

U3 is intentionally an integration unit: result identity, gate evidence,
review evidence, and merge invalidation must share one candidate-generation
contract. Its internal stages remain separate lanes and packages.

U10 is intentionally last among contract changes. It projects the protected
canonical assets only after their replacement commands and invariants exist.

## Existing Beads reconciliation

After approval:

- create one new P0 epic for this design;
- add U4 as a new child of `koryph-4rk6.5` instead of duplicating its scope,
  then require that parent epic to pass strict validation and close;
- add U2 as a new child of the still-open `koryph-6an8`, then require that
  parent epic to pass strict validation and close;
- link closed `koryph-lv07`, `koryph-qta.17`, `koryph-2im.13`,
  `koryph-77r.1`, `koryph-3xs`, and `koryph-lv07.14` as discovered evidence,
  not as reopened work;
- preserve `koryph-bbr.3` commits and uncommitted work until U11;
- relabel implementation units with narrow footprints and atomic criteria;
- use direct supervised execution for U1–U10 while the loop is stopped;
- remove temporary recovery-only scheduling holds after the canary so future
  Koryph Beads are schedulable when dependencies and footprints allow.

No new Beads are filed until this design is approved.

## Acceptance criteria

### Safety

- No candidate reaches gate, review, PR, or merge without a matching terminal
  result manifest, nonzero commits, and a clean worktree.
- Protected-path refusal, signature/DCO validation, and fail-closed review
  remain covered by regression tests.
- One phase cannot run overlapping broad validation process groups.
- Capability failures with unchanged evidence consume zero model dispatches.
- Persistent critical host pressure stops admission and can gracefully preserve
  one recoverable cohort without OOMing the host.
- Autonomous watcher/supervisor code never runs broad mutation from regex text.

### Correctness and quality

- Every planned criterion has a stable ID and independent review result.
- No merged candidate has an unsatisfied or unevaluated applicable criterion.
- General and security review policies are separately tested and observable.
- All prior blocking findings are rechecked on a repair pass.
- The full gate passes on the exact candidate/base/config tuple that is merged;
  a moved base invalidates the right evidence.

### Efficiency and cost

- Workers launch zero full project gates.
- At most one full gate runs per unique candidate/base/config tuple.
- Codex token totals and cache ratio use disjoint normalized fields; fixture
  usage proves cached input is not double-counted.
- Idle loop operation launches zero model processes and creates zero empty run
  directories.
- Terminal phase caches/temp trees are removed automatically.
- Active project runtime artifacts stay below a 2 GB soft target and 5 GB hard
  target during the canary, excluding explicitly retained failure evidence.
- Sol implementation is zero during the canary unless a structured frontier
  analysis records `model-capability`; steady-state target is at most 5% of
  implementation dispatches.

### Velocity and autonomy

- In a representative canary of at least 10 small/medium Beads, excluding
  predeclared human/external capability holds:
  - at least 90% reach a correct terminal outcome without operator action;
  - at least 75% pass the first semantic review;
  - retry attempts are at most 20% of implementation dispatches;
  - no unchanged failure is dispatched twice;
  - median dispatch-to-terminal is at most 15 minutes and p95 at most
    30 minutes for this repository's small/medium class.
- Gate, review, and merge queue time are reported separately.
- A completed sibling can gate while another is reviewed, and merge mutation
  remains serialized.
- An engine crash resumes from durable lifecycle state without another coding
  attempt for a completed candidate.

### Release

- `make gate-agent` passes once as the release gate.
- Every release commit is signed and DCO-signed.
- `make build` and `make install` succeed.
- `koryph version` identifies the gated source commit.
- The preserved DNS work is recovered without losing commits or uncommitted
  changes.
- The native loop is enabled only if the bounded canary meets every safety
  criterion and all quality/cost/velocity tripwires.

## Canary and rollback

The canary cohort is fixed before release. These existing Beads are audited,
made atomic, relabeled, and dependency-reconciled before the canary starts:

| Bead | Class | Expected size | Exclusion |
|---|---|---|---|
| `koryph-bbr.3` | DNS/doctor integration and preserved-candidate recovery | medium | none |
| `koryph-3vp.8` | permission-boundary policy | medium | none |
| `koryph-rdc.2` | CLI/docs publishing integration | medium | none |
| `koryph-2ge.2` | release doctor/docs integration | medium | none |
| `koryph-r0l.1` | state migration/fingerprint foundation | medium | none |
| `koryph-r0l.2` | registry state owner | small | none |
| `koryph-r0l.3` | quota state owner | small | none |
| `koryph-r0l.4` | governor state owner | medium | none |
| `koryph-r0l.5` | signing-vault state owner | small | none |
| `koryph-r0l.6` | ledger state owner | medium | none |

The state-versioning children are refreshed to cover only remaining work; work
already present on main is not turned into a zero-commit dispatch. No selected
Bead has a human or external-capability exclusion. Any capability block in this
cohort therefore fails the canary instead of shrinking its denominator.

Run the new binary in a bounded canary with:

- one project;
- exactly two implementation slots initially;
- auto-merge and review enabled;
- no `--allow-unvalidated`;
- exactly the ten quality-gated Beads above;
- Sol implementation disabled unless the structured exception is produced;
- a hard stop on any safety invariant, duplicate broad command, empty-run
  creation, token-accounting inconsistency, or second unchanged retry.

The supervisor writes immutable evidence to
`.plan-logs/koryph/canary/autonomous-loop-reliability.json`. The report records
the installed source commit, binary version, cohort IDs and contract digests,
start/end times, every attempt and typed outcome, gate/review/merge evidence
keys and timings, process reuse/denial/RSS/CPU/duration events, normalized token
composition, artifact bytes, pressure samples, and the final SLO decision.

Increase width only after five consecutive terminal outcomes with normal memory
pressure, no duplicate process event, and no failed tripwire. The final report
must show median dispatch-to-terminal at most 15 minutes and p95 at most
30 minutes.

The canary drains immediately on any candidate without a valid terminal
manifest, protected-path/signature bypass, gate/merge evidence mismatch,
duplicate broad validation group, unchanged retry, unjustified Sol
implementation, empty idle ledger, token-class inconsistency, or persistent
critical memory pressure.

Rollback means:

1. drain the native loop;
2. leave all candidate worktrees and manifests intact;
3. reinstall the previous known-good binary;
4. keep failed canary Beads parked with evidence;
5. do not restore the shell wrapper.

## Contradiction pass

The cumulative designs were checked against this proposal:

- Retry containment correctly made capability blocks slot-local and evidence-
  gated. This design keeps that invariant and removes wrapper-owned scheduling.
- Token economy correctly introduced quiet gates and file-spill output. This
  design keeps quiet output but moves the full gate from the worker to the
  engine, so the worker instruction is deleted.
- Scheduler throughput correctly made completion polling responsive. This
  design keeps rolling dispatch but separates finalization lanes and creates no
  idle run ledgers.
- Resource governance correctly separates footprints from runtime resources.
  This design keeps reservations and replaces the incomplete Darwin admission
  signal and unsafe unrestricted probe-failure behavior.
- Planning quality correctly requires design decisions, strict analysis, and
  semantic scoring. This design adds atomic acceptance arrays, removes
  autonomous `--allow-unvalidated`, and reconciles the still-open parent epic.
- Hard-block recovery correctly made runtime-neutral frontier routing possible.
  This design narrows when frontier implementation may be selected.
- Acceptance-aware review correctly fails closed. This design preserves that
  result while separating general and security review and putting the gate
  first.
- Async finalization correctly removed blocking work from the poll goroutine.
  This design retains responsiveness while replacing the one-lane bottleneck.

No goal requires weakening a demonstrated safety invariant. The main
behavioral reversal is deliberate: “never interrupt a running agent” becomes
“never kill useful work for ordinary admission changes, but gracefully
checkpoint one recoverable cohort under persistent critical host pressure.”
Host survival is the higher-level safety invariant.

## Open questions and assumptions

- The proposed 90% autonomous completion, 75% first-review pass rate, and
  15/30-minute latency targets are initial self-hosting SLOs. They should be
  recalibrated from normalized canary evidence, not silently relaxed during the
  canary.
- A project-scoped shared Go build cache is assumed safe under Go's documented
  content-addressed cache behavior; tests must still cover concurrent use and
  interrupted cleanup.
- Runtime adapters can state whether repository instructions are natively
  loaded. Until every adapter reports that capability, Koryph includes the
  compact engine boundary clauses fail-safe.
- The current stopped loop and preserved DNS worktree remain untouched until
  U12 performs the approved recovery and release sequence.
