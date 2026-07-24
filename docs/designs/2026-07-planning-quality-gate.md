<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Planning quality gate: trustworthy designs and dispatch-ready bead graphs

## Problem

Koryph's design and planning workflows describe the right planning discipline,
but the filed corpus can violate that discipline without failing. Recent
examples include epics without success criteria, child acceptance criteria
that contradict refreshed descriptions, labels outside the project's
`area_map`, and a supposedly parallel family serialized by a shared write
token. `bd create --validate` checks field presence, while `koryph plan`
currently checks scheduling conflicts; neither proves that an epic is a
coherent, traceable, dispatch-ready implementation graph.

This makes review and implementation compensate for planning defects. The
result can still be correct, but only after expensive retries and semantic
review.

## Goals

- Make the designer resolve architectural decisions before decomposition.
- Make every epic and child independently understandable and testable.
- Add a deterministic, read-only post-filing gate in the Koryph binary.
- Give the frontier plan scorer a canonical snapshot of the graph it reviews.
- Fail before dispatch on invalid labels, missing traceability, incomplete
  criteria, or unintended serialization.
- Repair the currently active epics that exposed these defects.

## Non-goals

- Replace semantic frontier review with heuristics.
- Infer exact source-code footprints without repository inspection.
- Automatically rewrite bead text or dependency edges.
- Require explicit model labels for routine standard-tier implementation;
  the project/runtime default remains the normal route.
- Reopen or rewrite completed historical beads.

## Current state

- `commands/koryph-design.md` and `commands/koryph-plan.md` are installed
  workflow projections of the embedded sources in `internal/commands/`.
- `agents/koryph-architect.md` and `agents/koryph-plan-scorer.md` define the
  frontier designer and scorer personas.
- `cmd/koryph/plan.go` loads the open Beads corpus and renders
  `internal/plan.Audit`.
- `internal/plan/audit.go` detects unknown footprints, non-dispatchable
  issues, unordered footprint conflicts, derived-artifact risks, and width.
- `bd --validate` does not compare description against acceptance criteria,
  validate `area_map`/resource vocabulary, require design traceability, or
  validate epic success criteria.
- The scorer's allowed tools do not include a Beads-capable shell, so it
  cannot independently inspect the graph after filing.

## Design

### 1. Designer contract: decisions before units

Strengthen the designer and architect instructions around a compact decision
ledger. Every design records:

- stable decisions that implementation must not reopen;
- rejected alternatives and the reason;
- invariants and failure posture;
- an extension seam that keeps sibling write sets disjoint;
- a unit table containing exact paths, dependencies, resources, and
  observable acceptance.

An unresolved choice that changes architecture, security posture, persistence,
or compatibility stays in the design's open questions and blocks
decomposition. Implementation beads may make local coding choices, but they
must not be asked to decide product architecture.

### 2. Canonical pre-file plan snapshot

Before filing, the planner writes a transient canonical JSON snapshot under
`.plan-logs/` containing the epic, children, labels, dependencies, design
reference, and predicted width. The frontier scorer reviews that snapshot
before any bead becomes visible. This avoids partially filing a bad graph and
lets the scorer review exactly what the mechanical filing step will create.

The snapshot is scratch evidence, not task state; Beads remains the durable
source of truth after filing.

### 3. Deterministic post-filing gate

Extend `koryph plan` with:

```text
koryph plan --epic <id> --strict [--json]
```

The scoped report retains the existing conflict/width analysis and adds
deterministic quality findings:

- target is an epic and has non-empty success/acceptance criteria;
- every child is a dispatchable implementation type unless explicitly
  `no-dispatch`;
- every child has non-empty description and acceptance criteria;
- every child names an existing `docs/designs/*.md` source;
- every `area:*` label exists in `koryph.project.json.area_map`;
- every `res:*` label is syntactically valid and exists in the configured
  resource vocabulary;
- every child resolves to a non-unknown footprint;
- dependency-unordered children are write-disjoint;
- the dependency graph does not reference another child in the wrong
  direction through a parent edge;
- deprecated concrete/equivalence routing labels are warned; normal
  implementation inherits the standard default, while only non-default
  routing needs an explicit portable tier and rationale.

`--strict` exits non-zero for error findings or conflict/derived-artifact
risks. Human output names exact remediation; JSON is stable input for the
frontier scorer.

Deterministic checks deliberately do not claim to detect semantic
contradictions. The scorer receives the post-file JSON plus the design and
must compare description, acceptance, and stable decisions.

### 4. Two-key release to the loop

A newly planned epic is dispatch-ready only after:

1. `koryph plan --epic <id> --strict --json` passes; and
2. the frontier scorer returns `SHIP` after reading the design and canonical
   report.

The planner applies at most one scorer correction iteration. If the second
gate still fails, it leaves the epic blocked for operator/design revision
rather than allowing the loop to discover the defect.

### 5. Existing-corpus remediation

Repair open children—not closed history—under the audited epics:

- add explicit epic success criteria;
- reconcile refreshed descriptions and stale acceptance criteria;
- replace invalid `area:*` labels with mapped areas or exact `fp:*` tokens;
- restore design references;
- separate unresolved design decisions into a frontier design bead or resolve
  them in the parent design;
- redesign shared registration/golden seams or explicitly order the work when
  write-disjoint parallelism is impossible;
- keep routine implementation on the standard default.

## Alternatives considered

### Prompt-only validation

Rejected. The current prompt already states most rules and still emitted
invalid graphs. Mechanical invariants need executable checks.

### Teach `bd lint` Koryph-specific policy

Rejected as the primary path. `bd` is a general task tracker and does not know
Koryph's project configuration, scheduler footprints, model routing, or
design-doc conventions. Koryph should own its orchestration gate.

### Fully automatic bead repair

Rejected. Adding labels or dependencies can silently change concurrency and
scope. The gate should diagnose deterministically; a frontier planner applies
the repair with repository context.

## Implementation outline

1. **Quality analysis and CLI gate**
   - Files: `internal/plan/audit.go`, `internal/plan/audit_test.go`,
     `cmd/koryph/plan.go`, `cmd/koryph/plan_test.go`, CLI reference.
   - Dependencies: none.
   - Resources: none.
   - Acceptance: scoped epic audit and strict exit behavior are covered by
     unit/command tests; JSON is stable; existing unscoped behavior remains
     compatible.

2. **Designer, planner, and scorer contracts**
   - Files: `internal/commands/koryph-design.md`,
     `internal/commands/koryph-plan.md`, canonical personas under `agents/`,
     projection/install tests and documentation.
   - Dependencies: unit 1 so instructions name a real command.
   - Resources: none.
   - Acceptance: pre-file snapshot, decision ledger, two-key gate, valid
     default routing, and scorer access are explicit and tested as embedded
     assets.

3. **Corpus remediation**
   - State: Beads records for `koryph-lv07`, `koryph-r0l`, `koryph-bbr`,
     `koryph-rdc`, and `koryph-2ge`.
   - Dependencies: units 1 and 2.
   - Resources: none.
   - Acceptance: every active target epic passes the deterministic strict
     gate, with any unavoidable serialization documented and dependency
     ordered.

4. **Release and resume**
   - Actions: `make gate-agent`, signed commits, `make build`, `make install`,
     verify installed commit, remove the runner stop sentinel, and restart the
     autonomous auto-merge loop plus zero-token watcher.
   - Dependencies: units 1–3.
   - Resources: local build/signing environment.
   - Acceptance: installed `koryph version` names the landed commit; the new
     loop dispatches only quality-gated ready work and the watcher is healthy.

## Acceptance criteria

- A malformed fixture epic produces deterministic findings for missing epic
  criteria, missing child criteria/design reference, invalid labels, and
  unordered write conflicts.
- A well-formed fixture epic passes `--strict`.
- `--strict --json` returns the full report and a non-zero status on errors.
- The design/plan workflows require a decision ledger, pre-file scorer pass,
  post-file binary gate, and final semantic scorer pass.
- The scorer can consume the canonical snapshot/report rather than trusting a
  prose summary.
- The audited active epics are reconciled and pass the new gate.
- `make gate-agent`, build, installation, version verification, and autonomous
  restart all succeed.

## Open questions / assumptions

- The operator's instruction to complete and restart autonomously constitutes
  approval to proceed after this design is written; no separate design-review
  pause is required for this run.
- Closed children remain immutable historical evidence and are excluded from
  remediation, though their labels may still appear in an all-history audit.
- Design-reference enforcement applies to planned epic children. Incident
  follow-up bugs may reference a concrete run/commit instead when they are
  filed through the issue workflow rather than decomposition.
