# Capability recovery bridge

## Status

Proposed.

## Context

Recent autonomous runs exposed two capability gaps that model escalation cannot
solve:

1. A dispatched worker can determine that its bead needs an additional
   `area:*`, `fp:*`, or `res:*` label, but its sandbox cannot safely mutate the
   shared Beads database. The prompt currently tells the worker to use `bd`
   directly, so a correct recovery attempt ends as a blocked bead.
2. A worker running under one runtime cannot validate another runtime's
   authenticated profile without either losing the profile projection or
   exposing credentials across the containment boundary. This prevented a
   Codex-dispatched worker from completing a real Claude canary.

Both conditions were retried as bead faults. The final retry was promoted to the
frontier tier even though stronger reasoning could not grant the missing
capability. The run therefore consumed retries and frontier capacity before
surfacing a block that the orchestrator could have resolved immediately.

The repair must remain runtime-neutral. It must not grant workers general access
to the Beads database, runtime credentials, arbitrary host commands, or another
bead's state.

## Goals

- Let a current phase request narrowly scoped metadata additions for its own
  bead.
- Let a current phase request a fixed authenticated canary for a registered
  target runtime.
- Execute privileged operations in the orchestrator, using the registered
  account profile for the target runtime.
- Classify capability blocks separately from implementation failures.
- Wake the autonomous watcher immediately when an unsupported or failed
  capability request needs operator intervention.
- Preserve the branch and worktree without consuming a model retry or promoting
  the bead to the frontier tier.
- Keep every product capability in the `koryph` binary and make the protocol
  testable without a live provider.

## Non-goals

- General-purpose Beads access from a worker.
- Arbitrary label, dependency, status, close, or cross-bead mutations.
- Exporting a target runtime's config directory or credentials into another
  runtime's sandbox.
- A general remote-command or arbitrary-prompt proxy.
- Changing model-tier routing for genuine reasoning or implementation failures.
- Repairing unrelated scheduler telemetry or admission-reporting issues.

## Design

### Phase-scoped request protocol

Add an `internal/phasecontrol` package and a binary-owned phase command:

```text
koryph phase request label-add --label area:docs
koryph phase request runtime-canary --runtime claude
koryph phase block --capability <name> --detail <text>
```

The command derives the active phase and bead from the phase manifest and
environment installed by the orchestrator. It does not accept a bead ID. It
writes an atomic, versioned request record under the phase directory and waits
for a matching response. Request IDs are unique and idempotent; replaying a
completed request returns the recorded result.

The engine polls requests only for the slot that owns the phase directory.
Responses are written atomically to the same phase-owned control directory.
Each request and response is recorded in run telemetry without credential
material. The request schema includes:

- protocol version;
- request ID;
- phase and attempt identity;
- operation and typed arguments;
- creation time;
- response state, safe result detail, and completion time.

Malformed records, phase mismatches, unsupported operations, and duplicate IDs
with different content are rejected and emitted as capability-block events.

The command surface is intentionally declarative. Workers cannot supply shell
commands, prompts, environment variables, account paths, or arbitrary payloads.

### Controlled Beads metadata bridge

The first metadata operation is `label-add` for the current bead. The
orchestrator executes it through `WorkSource.AddLabel`; the worker never sees
`BEADS_DIR` and never invokes shared `bd`.

Allowed labels are limited to the scheduler's declarative resource and footprint
families:

- `area:<value>`
- `fp:<value>`
- `res:<value>`

Values must pass the same canonical label validation used by project planning.
An already-present label is a successful idempotent response.

The bridge rejects:

- another bead ID;
- label removal;
- dependency, status, close, reopen, or claim operations;
- routing and policy labels such as `model:*`, `equiv:*`, `runtime:*`,
  `merge:*`, `gt:*`, `no-dispatch`, and `refactor-core`;
- labels outside the allowlisted families.

This keeps authority with the orchestrator while allowing a worker to correct
an incomplete scheduling footprint discovered during implementation.

The compiled worker prompt is updated to use `koryph phase request` for supported
metadata changes. It must no longer recommend direct shared-database `bd`
mutations. Unsupported changes are reported with `koryph phase block`, which
produces a structured capability block rather than a prose-only failure.

### Authenticated cross-runtime canary

`runtime-canary` accepts only a registered runtime name. The orchestrator:

1. resolves the current project registration;
2. selects `Record.AccountFor(targetRuntime)`;
3. runs the target adapter's authentication and expected-identity checks;
4. launches a fixed canary contract through that target runtime with the
   registered child environment and profile;
5. returns a sanitized pass/fail result to the requesting phase.

The target profile is projected only into the target runtime child process.
Profile paths, tokens, cookies, API keys, and authentication output are never
returned to the requesting worker.

The canary prompt and expected result are hard-coded by protocol version. The
requester cannot provide prompt text or tool arguments. The canary performs one
harmless local tool action under the target runtime's configured headless
permission mode and returns a fixed structured token. This validates all of:

- the intended registered account;
- target-runtime process startup;
- authenticated provider access;
- the installed project containment/permission wiring needed for headless work.

The canary uses the standard tier by default. A canary is a capability probe,
not a frontier reasoning task. Provider rate limits and authentication failures
are returned as typed capability results so the engine can distinguish
transient provider availability from invalid configuration.

Tests use fake runtime adapters and synthetic account profiles. Live provider
access is not required by the gate.

### Capability-block classification

Extend portable phase completion with optional structured block fields:

```json
{
  "state": "blocked",
  "block_kind": "capability",
  "capability": "runtime-canary",
  "detail": "sanitized explanation"
}
```

`koryph phase block` writes this shape atomically. Existing completion files
without the new fields remain compatible.

Candidate assessment gains a capability-block outcome distinct from a bead
fault. When the engine observes one, it:

1. processes any known pending phase request;
2. allows the worker to continue if the request succeeds;
3. otherwise preserves the branch and worktree;
4. parks the bead without incrementing its implementation-attempt budget;
5. emits `engine.slot.capability_blocked` at wake-worthy severity with project,
   bead, phase, capability, and sanitized reason.

Capability blocks never enter the ordinary `requeueSlot` model-escalation path.
They therefore cannot promote a task from standard to frontier merely because a
host capability was unavailable.

Known transient results may use an orchestrator-owned bounded retry policy, such
as a provider rate-limit backoff. Those retries do not launch another coding
agent and do not consume the bead's model attempt count. Unknown capabilities,
invalid profiles, denied operations, and exhausted transient retries wake the
outer watcher immediately.

Reason-string parsing is not used for classification. Structured fields are the
portable contract; prose remains explanatory only.

### Recovery and continuity

The control directory is part of the phase's durable run artifacts. On engine
restart, completed responses are replayable and unfinished requests can be
resumed or rejected deterministically. The branch is never discarded because a
control request failed.

If a manually corrected capability block becomes runnable—for example, an
operator repairs a runtime profile or adds a label—the orchestrator can nudge or
resume the existing bead without losing its commits. Normal merge safety,
footprint checks, and protected-path refusal still apply.

## Security model

The protocol separates request authority from execution authority:

```text
worker sandbox
  -> typed phase request in owned phase directory
  -> engine validates phase, operation, and arguments
  -> orchestrator-owned Beads/runtime adapter action
  -> sanitized typed response in owned phase directory
```

The worker gains no shared database path, target profile path, raw environment,
arbitrary command execution, or cross-bead selector. The engine treats all
phase files as untrusted input and validates them before acting.

Audit events record the operation and outcome but redact provider output and
account material. Error strings returned to the worker pass through the same
sanitization boundary.

## Compatibility

- Existing runtimes require no hook support; the bridge uses phase files and the
  orchestrator process.
- Existing `status.json` records remain valid because the block fields are
  optional.
- Existing workers that report prose-only blocks retain the current retry
  behavior.
- The new prompt instructions take effect after rebuilding and reinstalling the
  binary.
- This work does not restore the former `refactor-core` scheduling protection;
  dependency order and precise footprints make the resulting beads schedulable
  once their prerequisites resolve, per operator direction.

## Implementation plan

### 1. Phase-control protocol and metadata bridge

Add the versioned request/response store, phase CLI, label validation, engine
polling, current-bead `AddLabel` handler, prompt updates, and unit/integration
tests.

Expected footprint:

- `internal/phasecontrol/**`
- `cmd/koryph/phase*.go`
- `internal/engine/phase_control*.go`
- `internal/promptc/**`
- command/help documentation generated from or paired with the command registry

Suggested labels:

- `area:cli`
- `area:engine`
- `fp:go:phase-control`
- `fp:go:prompt-contract`

### 2. Authenticated runtime canary

Build the fixed canary executor on the target runtime adapter and registered
account profile, add the phase request handler, sanitize results, and cover
identity success/failure, authentication failure, headless tool failure, and
protocol rejection with fake adapters.

Expected footprint:

- `internal/runtimecanary/**`
- `internal/engine/phase_canary*.go`
- runtime-neutral adapter test fixtures

Suggested labels:

- `area:runtime`
- `area:engine`
- `fp:go:runtime-canary`

Dependency: implementation unit 1.

### 3. Capability-block recovery policy

Extend portable completion, candidate assessment, polling/recovery telemetry,
watcher wake behavior, restart recovery, and tests proving that capability
blocks do not consume bead retries or frontier promotion.

Expected footprint:

- `internal/engine/candidate.go`
- `internal/engine/poll.go`
- `internal/engine/requeue_test.go`
- engine observation/recovery tests and documentation

Suggested labels:

- `area:engine`
- `fp:go:capability-recovery`
- `fp:docs:recovery`

Dependency: implementation units 1 and 2.

The units are intentionally serial at their engine integration seams. Each
becomes independently schedulable when its dependencies resolve; none requires
the broad `refactor-core` exclusion.

## Acceptance criteria

- A sandboxed worker can add an allowlisted scheduling label to its own bead
  without access to the shared Beads database.
- Attempts to mutate another bead or add a routing/control label are rejected
  and audited.
- A worker can request a Claude canary while running under Codex (and vice
  versa); the target process receives its registered profile while the
  requester receives no credential or profile path.
- The canary verifies expected identity and performs the fixed harmless
  headless tool action.
- A structured capability block causes no coding-agent retry and no
  standard-to-frontier promotion.
- Unsupported or terminal capability blocks emit an immediate watcher wake
  event and preserve the branch/worktree.
- Restarting the engine does not duplicate a completed metadata mutation or
  canary response.
- Legacy phase completion remains compatible.
- `make gate-agent` passes.

## Alternatives considered

### Grant workers direct `BEADS_DIR` access

Rejected. It turns a narrow metadata need into authority over the shared task
database and bypasses the orchestrator's validation and audit boundary.

### Mount the target runtime profile into the requesting sandbox

Rejected. It leaks credential-bearing configuration across runtime and
containment boundaries and still does not guarantee the target adapter's child
environment is reproduced correctly.

### Retry every block with a stronger model

Rejected. Model capability does not repair missing filesystem, authentication,
or host authority. The observed frontier retries preserved useful commits but
could not complete the blocked operations.

### Parse prose for capability keywords

Rejected. Provider-specific wording is unstable and would silently
misclassify failures. A versioned structured status is deterministic and
runtime-neutral.
