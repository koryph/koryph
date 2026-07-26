<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Production kernel release

Status: approved by the operator on 2026-07-26 and tracked by
`koryph-x553.12`.

This document replaces the open-ended release acceptance portion of
`2026-07-autonomous-loop-reliability.md`. The earlier implementation and its
useful safety fixes remain; its model-dependent SLO canary is no longer the
definition of engine correctness.

## Release question

Koryph is ready when its control plane can repeatedly turn eligible project
work into isolated, validated, reviewed, safely merged results, while recovering
from interruption and consuming no model tokens when idle. A worker producing a
bad candidate is an expected task outcome, not proof that the engine is broken.

The release therefore separates three concerns:

1. deterministic kernel correctness;
2. explicit release-canary observation;
3. the quality of an individual Bead or model response.

Only the first is a hard prerequisite for installing Koryph. The canary verifies
the installed integration without redefining the kernel during its run.

## Production kernel

The supported kernel owns:

- discover eligible Beads and honor dependency, footprint, and resource limits;
- select a runtime-neutral capability tier and dispatch into an isolated
  worktree;
- bind completion evidence to the attempt, base, and candidate commit;
- run one authoritative project gate before semantic review;
- review the exact gated candidate against explicit acceptance criteria and
  risk;
- serialize and verify default-branch mutation;
- route bounded retries from typed evidence;
- park candidate-local failures without stopping unrelated work;
- stop the run on an actual safety or engine invariant;
- recover authenticated in-progress state after interruption;
- supervise child process groups without signaling an unverified process;
- account for normalized tokens and bounded artifacts;
- wait for new work without dispatching a model or creating empty run ledgers.

The default implementation tier is `standard`. `frontier` is required for
decomposition, dependency/footprint/resource assignment, plan scoring, security
review, and recovery analysis where an incorrect decision poisons downstream
automation. It is not a generic last-attempt implementation tier. Mechanical
environment, capability, validation, and metadata failures never justify a
stronger implementation model.

## Control-plane boundary

Koryph owns Bead claiming and closure, dependency state, worktree creation,
process lifecycle, retries, merge, push policy, and loop state. Dispatched
workers receive:

- the selected Bead's implementation contract;
- the applicable repository instructions;
- a focused validation scope;
- exact immutable gate or review evidence when repairing a candidate;
- role-specific completion instructions.

Workers do not receive responsibility to run `bd prime`, claim or close Beads,
change dependencies, merge, push, or operate Koryph. They may read the files
needed to implement the Bead and run focused tests. Broad validation is an
engine responsibility.

## One recovery authority

Every unsuccessful attempt is classified before another dispatch:

| Class | Scope | Default action |
|---|---|---|
| code or acceptance defect | slot | one evidence-carrying standard repair |
| review defect | slot | one evidence-carrying standard repair |
| runtime transient | slot | bounded same-tier retry |
| capability or environment block | project/slot | park until evidence changes |
| unchanged or exhausted candidate | slot | preserve and park |
| unsafe merge, containment escape, data loss | run | contain workers and stop |
| authenticated-state or engine invariant | run | contain workers and stop |

The typed outcome and budget are authoritative. Historical counters may remain
as derived telemetry for compatible ledgers, but they cannot independently
admit or deny a retry. A retry always identifies the evidence it must change.

## Ordinary loop and release tooling

An initialized project with valid repository, runtime, account, signing, and
gate configuration can enter the ordinary loop. Canary promotion, autonomy SLO
reports, cohort history, and migration/readiness terminology do not gate normal
scheduling.

Canary mode remains an explicit operator command. Its fixed cohort, report, and
generation metadata are release evidence, not project eligibility state.
Existing registry fields remain readable during this release; removing or
renaming them is a later compatibility migration.

## Deterministic release acceptance

One documented command must run network-free fixtures and fail if any required
scenario is absent:

1. only eligible dependency-ready work is scheduled;
2. footprint and resource conflicts do not overlap;
3. dispatch occurs in the expected isolated worktree and runtime;
4. nonterminal or stale completion evidence cannot finalize;
5. the authoritative gate precedes semantic review;
6. gate/review repairs receive exact immutable evidence and bounded budgets;
7. an unchanged or exhausted candidate parks only its slot;
8. containment, unsafe merge, data-loss, and engine invariants stop and reap
   the run;
9. a valid candidate merges once and closes through the control plane;
10. restart recovery resumes authenticated state and idle supervision consumes
    no model tokens or empty ledgers.

This target complements `make gate-agent`; it does not duplicate the full
repository gate in every worker or scenario.

## Frozen canary

After the deterministic target and `make gate-agent` pass, build and install the
exact signed commit. Select ten non-kernel Beads whose dependencies are resolved,
contracts are self-contained, and footprints/resources are declared. Record the
cohort before starting.

During the canary:

- do not edit engine, loop, runtime, merge, recovery, prompt, or gate code;
- do not add a safeguard in response to an ordinary candidate failure;
- preserve every terminal result and diagnostic;
- allow independent eligible slots to continue after a candidate-local failure;
- stop only for data loss, unsafe merge, containment/process escape, or a
  reproducible kernel invariant.

A canary Bead may end merged/closed or parked with an explicit terminal
diagnosis. Parked candidate work measures task/model quality; it does not fail
the kernel release. A safety stop fails the release and produces exactly one
release-blocker Bead from preserved evidence.

## Stop-loss and done criteria

The stabilization release permits at most two reproducible kernel blockers
after this contract is frozen. A third observation is recorded for a later
release rather than expanding this one. No engine edits occur during the
canary, and no model-dependent percentage or latency threshold can reopen
architecture.

The release is done when:

- the production contract and implementation Beads are closed with signed,
  DCO-signed commits;
- the deterministic production target and `make gate-agent` pass;
- the installed binary identifies that exact clean commit;
- one frozen ten-Bead canary completes with no safety stop and with terminal
  evidence for every selected Bead;
- the ordinary zero-token-idle loop is started from that binary and survives
  one observed idle-to-work-or-idle checkpoint without creating an empty run.

Anything else is follow-up product work, not a reason to keep this release open.
