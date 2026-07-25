<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Retry containment and autonomous throughput

## Problem

Recent autonomous waves produced useful commits but consumed many agent turns
before reaching a terminal result. A slot-level capability block currently
terminates the whole engine run, and an external watcher responds by reopening
the bead without evidence that the failed capability changed. The wrapper then
starts another run. This turns one host or sandbox defect into repeated model
work and pauses unrelated ready beads.

The same waves exposed two amplifiers. Codex receives phase-local cold module
and build caches, so a transient network problem is paid again by every retry,
while Darwin's `xcrun` writes outside the configured `TMPDIR`. Planning checks
graph shape but do not reject an implementation unit that spans several
independent products or subsystems. Routine standard-tier implementation also
defaults to high reasoning effort even when the specification is precise.

## Goals

- Contain a capability block to the affected slot while unrelated work
  continues.
- Retry only after a bounded, observable change in capability, candidate, or
  recovery state.
- Prevent a zero-token watcher from manufacturing model retries.
- Keep reusable dependency caches warm without granting agents ambient
  credential or source-write access.
- Support the Darwin toolchain's real temporary-write location narrowly.
- Reject or warn on oversized, weakly cohesive implementation beads before
  dispatch.
- Use medium effort for routine standard-tier implementation while preserving
  frontier escalation for advanced design, scoring, security, and final
  recovery.
- Rebuild, install, and observe a controlled canary before restoring the
  autonomous loop.

## Non-goals

- Retrying indefinitely or weakening attempt ceilings.
- Treating transient network recovery as proof that engine defects are fixed.
- Giving sandboxed agents unrestricted host caches or credentials.
- Replacing semantic frontier design and plan review with line-count
  heuristics.
- Using the unstable autonomous loop to implement its own repair.

## Current state

- `parkCapabilityBlock` records a run-wide capability flag after parking and
  releasing the affected slot.
- The run loop converts that flag into a fatal capability handoff, checkpoints
  unrelated workers, and exits.
- The shell watcher runs diagnostics and reopens the same bead up to a fixed
  count, but does not compare a capability fingerprint or successful probe.
- Codex places `GOCACHE`, `GOMODCACHE`, `XDG_CACHE_HOME`, and `TMPDIR` below
  each phase directory. Module downloads and compilation therefore start cold
  on every phase.
- Darwin developer tools use the per-user Darwin temporary directory for
  `xcrun_db`, which is not covered by a generic phase `TMPDIR` grant.
- The strict plan audit validates required fields, labels, traceability,
  footprints, and graph conflicts, but not unit cohesion or scope size.
- The implementation persona assigns high effort to all standard work.

## Design

### Slot-local capability parking

A structured capability block is terminal for the current slot attempt, not
for the run. The engine persists the blocked reason, releases all leases held
by that slot, emits a capability event containing a stable fingerprint, and
continues polling other running slots and dispatching unrelated ready work.
The normal run completion condition decides when the iteration ends.

The fingerprint consists only of non-secret recovery inputs: runtime,
capability class, relevant executable/version identity, policy/config digest,
and probe result. The event and bead note may contain the fingerprint hash,
never environment values or credentials.

The engine may reopen or resume a capability-blocked bead only when one of
these facts is true:

1. its candidate branch or dispatch base changed;
2. a named capability probe changed from failing to passing;
3. the relevant runtime, policy, configuration, or installed engine
   fingerprint changed; or
4. an operator explicitly requested another attempt.

Attempt ceilings and backoff still apply. An unchanged fingerprint is a
deterministic no-op, not another model turn.

### Watcher as notifier, not scheduler

The external watcher remains zero-token and wakes on error transitions. It may
run read-only health checks and an explicit deterministic capability probe,
but it does not change Beads state or restart a failed candidate merely
because time passed. Repeated dead-PID and capability messages are deduplicated
by durable event key. Scheduling and retry ownership stays in the engine,
where attempt and fingerprint state are transactional.

### Secure warm caches and terminal gate evidence

Keep mutable workspace, test output, telemetry, and generic temporary state
phase-local. Normal repository dispatches share only a project-scoped Go module
cache under `.git/koryph-cache`; JSON/repo-less spawns remain fully
scratch-local. The existing sandbox already grants the repository's exact
`.git` directory for worktree commits, so this adds no ambient host-cache or
credential access. Go's module cache provides concurrent-writer safety and
content verification; different repositories never share it.

Observed Darwin `xcrun_db` denial text is a warning emitted during an otherwise
successful `make gate-agent`, not a blocked capability. Granting the whole
per-user Darwin temporary root would expose unrelated user data and is
unnecessary. The worker contract therefore requires the terminal command exit
and gate `PASS`/`FAIL` verdict before classifying a warning as a capability
failure. A zero exit wins over warning text.

Network readiness remains a named capability probe that may update the durable
capability evidence record. It is intentionally not a mandatory pre-dispatch
probe: not every bead requires network, and a global probe would suppress
offline-ready work. When a network-dependent command produces a structured
block, a later passing probe changes the fingerprint and restores eligibility
without resetting unrelated work.

### Planning cohesion and execution budgets

The planner records a machine-readable `koryph.unit/v1` contract in each
child's design field: `kind`, exactly one `provides` capability slug, exact
owned path prefixes, consumed capability slugs, and an optional cohesion
rationale. Broad roots and wildcards are invalid. The strict audit reports:

- an error when a child provides multiple independent outcomes, declares a
  broad root/glob, or consumes a capability without depending on its provider;
- an error when its declared write footprint crosses multiple unrelated
  subsystems without an integration-only rationale;
- a warning/error when declared write-area or owned-package counts cross
  ordinary-unit thresholds; and
- an error when docs and production ownership are mixed outside a justified
  integration unit; and
- a warning when a child has no explicit progress checkpoints for a long
  integration task.

Thresholds count declared capabilities, packages, and areas rather than
predicted lines of code. They are diagnostics, not a substitute for semantic
scoring. The frontier scorer decides cohesion from the design and canonical
snapshot. The planner performs one correction pass, decomposing along
observable seams and preserving dependencies and integration ownership.

Routine standard-tier implementation uses medium effort by default. High
effort requires a bead-local rationale; frontier remains mandatory for design,
decomposition, plan scoring, security review, and recovery analysis. Final
recovery escalation may use frontier only after deterministic recovery and
standard retry paths are exhausted.

Routing vocabulary follows the engine contract: `equiv:<tier>:<effort>` is
portable and `model:<id>` is an exact runtime-native override. Routine
standard work carries neither. Any explicit routing or effort override
requires a unit-contract rationale.

### Completion responsiveness

Completion, review, merge-gate, and slot reaping work must not prevent the
engine from observing other live slots or releasing finished resources.
Existing bead `koryph-lv07.14` owns this requirement and is retained rather
than duplicated. The implementation must preserve deterministic merge order
and protected-path checks.

Recovery also distinguishes coding from finalization. A dead slot with
committed, completion-ready candidate state resumes in a durable finalizing
state and re-enters candidate assessment/review without launching another
coding agent or incrementing its implementation attempt. A dead incomplete or
commitless slot continues through the existing bounded requeue path. If the
engine dies during synchronous review, the next run may repeat the idempotent
finalization checks, but it does not repeat implementation.

## Implementation units

1. **Slot-local capability state**
   - Files: engine candidate, poll, run, rolling, and wave control.
   - Dependencies: none.
   - Acceptance: one capability-blocked slot parks without ending the run and
     unrelated work completes.

2. **Durable capability evidence hold**
   - Files: capability ledger, engine frontier filter, operator nudge, tests.
   - Dependencies: none.
   - Acceptance: unchanged fingerprints consume zero backend dispatches; an
     observed branch/note/config/build/probe change makes the bead eligible
     once within budget; concurrent writers lose no update.

3. **Reusable module cache envelope**
   - Files: Codex runtime sandbox/environment code and focused tests.
   - Dependencies: none.
   - Acceptance: dependency modules survive phase changes within one project;
     secrets, ambient caches, other repositories, and mutable workspace state
     are not shared.

4. **Terminal gate evidence**
   - Files: prompt compiler and gate-wrapper regression tests.
   - Dependencies: none.
   - Acceptance: a successful gate that emits Darwin warning text remains
     successful and workers wait for a terminal verdict before blocking.

5. **Resource-loop test isolation**
   - Files: focused rolling resource fixture.
   - Dependencies: none.
   - Acceptance: the fixture awaits merge and engine exit and leaks no detached
     process across 20 repeated runs.

6. **Planning unit quality gate**
   - Files: unit-contract transport/audit, CLI, canonical planning/scoring
     workflow assets, and focused tests.
   - Dependencies: none.
   - Acceptance: oversized and incoherent fixtures produce actionable strict
     findings and coherent integration fixtures pass.

7. **Routine effort policy**
   - Files: implementer/test personas, installed projections, engine policy
     test.
   - Dependencies: none.
   - Acceptance: standard implementation/test work defaults to medium effort;
     frontier quality roles remain unchanged.

8. **Responsive completion and restart finalization**
   - Owner: existing `koryph-lv07.14`.
   - Dependencies: unit 1.
   - Acceptance: completed slots are reaped and other slots continue while
     review and merge gates execute, without weakening merge ordering; dead
     completion-ready slots resume finalization with zero coding dispatches.

9. **Release and controlled restart**
   - Actions: full quiet gate, signed commits, build, install, version check,
     operational watcher update, one controlled canary, then autonomous
     restart.
   - Dependencies: units 1–4.
   - Acceptance: canary shows no blind reopen or whole-run exit from a
     slot-local block; the restarted loop advances unrelated ready work and
     reports bounded retry metrics.

## Acceptance criteria

- A two-slot test proves a capability block parks only its owner.
- Replaying the same block fingerprint consumes zero additional agent turns.
- Capability recovery is triggered by a passing probe or explicit operator
  action and remains within the original retry budget.
- A committed completion-ready slot survives engine restart and proceeds to
  review/merge without another coding attempt.
- The watcher never calls `bd update` to manufacture eligibility and emits at
  most one alert for an unchanged event key.
- Codex cache-isolation tests cover same-project reuse, cross-project denial,
  repo-less spawns, and ambient-cache exclusion; a synthetic successful gate
  pins the Darwin warning behavior.
- Strict planning fixtures cover oversized, incoherent, and justified
  integration units.
- Standard implementation effort is medium in every canonical and projected
  persona asset.
- `make gate-agent` passes before `make build` and `make install`.
- The installed version matches the gated source commit.
- A controlled canary is observed through at least one terminal transition
  before the autonomous loop is restored.

## Alternatives considered

### Keep exiting the run and improve the wrapper

Rejected. The wrapper cannot transactionally see slot attempts, leases,
fingerprints, and scheduler state. It also serializes recovery by stopping
unrelated work.

### Blind bounded reopen

Rejected. A retry count bounds cost but does not create new evidence, so it
repeats deterministic failures and obscures the real block.

### Fully shared writable host caches

Rejected. They expand the sandbox boundary, admit cross-phase mutation, and
make failures dependent on ambient machine state.

### Route slow implementation to frontier immediately

Rejected. A stronger model does not repair unavailable networks, denied
paths, stale binaries, oversized scopes, or engine-wide handoff behavior.

## Assumptions and release posture

The operator's instruction to drain, repair, rebuild, install, and retry
constitutes approval for this implementation and release sequence. The
currently active drained slot may finish and preserve its commits, but the
autonomous loop is not used to implement these repairs. Directly supervised
agents own disjoint source areas, and the loop remains stopped until the
controlled canary succeeds.
