<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Hard-block recovery for runtime-neutral autonomous waves

## Problem

A Codex-driven autonomous wave left five implementation beads blocked even
though every preserved branch contained useful committed work. Two failures
were genuine security-review findings, one was a trivial lint defect, and two
were environment- or credential-gated self-reports. Koryph did not move past
them because final-attempt escalation only recognizes Claude model aliases,
Codex maps every capability tier to the same model, self-reported blocked
candidates terminate before the requeue path, and the Codex child environment
does not redirect every mutable build cache into its writable phase directory.

The zero-token watcher correctly ran `doctor --fix`, but doctor repairs
orchestration state rather than source branches, review findings, credentials,
or execution sandboxes. Restarting the installed binary would therefore
reproduce the same terminal states.

## Goals

- Escalate the final bead-fault attempt to the selected runtime's concrete
  frontier model, independent of provider model names.
- Make Codex's frontier tier use the strongest supported local model while
  retaining economical standard and light defaults.
- Give sandboxed Codex agents phase-local mutable caches and disabled Go
  telemetry so normal gates do not fail on host-owned cache paths.
- Boundedly retry a clean, committed candidate that reports `blocked`, allowing
  the final retry to use the recovery model.
- Rebuild and install the repaired binary before recovering the preserved
  blocked branches.
- Correct genuine findings on the DNS, docs deployment, and container release
  branches; freshly validate the migration and permission-allowlist branches.
- Restart the autonomous auto-merge loop with recovery and a zero-token watcher.

## Non-goals

- Weakening fail-closed security review or merging review-blocking candidates.
- Giving agents the operator's ambient credentials or unrestricted filesystem.
- Retrying malformed, dirty, or commitless candidates.
- Making `doctor --fix` edit implementation branches.
- Automatically rewriting arbitrary agent git history.

## Current state

- `internal/modelroute/route.go:EscalationTier` accepts only `haiku` and
  `sonnet` and always targets `opus`.
- `internal/runtime/models.go:CodexModelMap` maps frontier, standard, and light
  to `gpt-5.6-terra`.
- `internal/engine/poll.go:requeueSlot` passes only the frozen concrete model
  to the Claude-specific escalation helper.
- `internal/engine/candidate.go:parkIncompleteCandidate` terminally blocks every
  candidate that reports `blocked`, including clean branches with commits.
- `internal/runtime/codex/codex.go:sandboxCacheEnv` redirects only GOCACHE and
  XDG cache, only under the signing profile; it does not set TMPDIR,
  GOMODCACHE, or GOTELEMETRY.
- The installed binary is built from `04e0a55`; current main is `a9b7661`.
  The two newer commits do not change recovery behavior.
- Preserved branch heads exist for `koryph-bbr.2`, `koryph-r0l.1`,
  `koryph-rdc.1`, `koryph-2ge.1`, and `koryph-3vp.8`.

## Design

### Runtime-neutral recovery model

Replace the engine's dependency on Claude alias ordering with a model-route
operation that takes the current concrete model, runtime name, project model
map, and allowlist. It resolves that runtime's effective frontier target and
returns it only when the current model is a known lower/different selection
and the target is allowed by the same fail-closed policy used for initial
routing. Claude `fable` remains non-downgradable.

Codex's default map becomes frontier=`gpt-5.6-sol`,
standard/light=`gpt-5.6-terra`. Portable xhigh/max/ultra effort values remain
distinct because the installed runtime supports them.

Alternative considered: persist and escalate only a portable tier on each
slot. That is useful provenance but insufficient for existing slots whose
frontier tier previously resolved to Terra; comparing the frozen concrete
model to the current concrete recovery target upgrades those slots correctly.

### Writable sandbox envelope

Make phase-local cache environment construction independent of whether SSH
signing is active. When a scratch directory exists, set GOCACHE, GOMODCACHE,
XDG_CACHE_HOME, TMPDIR, and GOTELEMETRY=off there. Keep PRE_COMMIT_HOME on the
existing vetted cache only for the signing permission profile that explicitly
grants it. No credential namespaces are added.

Alternative considered: grant write access to the host's existing Go, Xcode,
and telemetry cache paths. Phase-local state is narrower, deterministic, and
does not expand the sandbox's secret surface.

### Bounded self-block recovery

Classify candidate validation instead of flattening it to one terminal reason.
A candidate is retryable only when it:

1. reports a blocked/failed/error/cancelled completion state,
2. has at least one commit beyond its dispatch base,
3. is clean, and
4. has remaining attempt budget.

That candidate re-enters the normal `requeueSlot` path with a bead-fault
reason, preserving worktree refresh, backoff, accounting, and final-attempt
model escalation. Malformed status, dirty state, missing commits, or exhausted
attempts still fail closed.

### Preserved branch recovery

- `koryph-bbr.2`: lowercase the Cloudflare error string and run the full gate.
- `koryph-r0l.1`: rebase, run a host-level gate, and obtain a fresh review.
- `koryph-rdc.1`: prevent manual non-default refs from deploying Pages and
  test the rendered workflow.
- `koryph-2ge.1`: make publication depend on the successful release gate and
  scope write/OIDC permissions to the publishing job.
- `koryph-3vp.8`: run the required real Claude canary with projected
  authentication, then gate and freshly review.

The orchestrator reopens these existing beads after the new binary is
installed. No duplicate recovery beads are created for their implementation
scope.

## Implementation outline

1. **Recovery routing foundation**
   - Files: `internal/runtime/models.go`, `internal/modelroute/route.go`,
     related tests, runtime user guide.
   - Resources: none.
   - Dependencies: none.

2. **Sandbox and candidate recovery**
   - Files: `internal/runtime/codex/codex.go`,
     `internal/runtime/codex/codex_test.go`, `internal/engine/candidate.go`,
     `internal/engine/poll.go`, related engine tests.
   - Resources: none.
   - Dependencies: unit 1 for the final-attempt recovery assertion.

3. **Build and install repaired Koryph**
   - Files: no source changes; `make gate-agent`, `make build`, `make install`.
   - Resources: local build and signing environment.
   - Dependencies: units 1 and 2.

4. **Recover the five preserved branches**
   - Files: the existing bead worktrees and their declared footprints.
   - Resources: Claude authentication for `koryph-3vp.8`.
   - Dependencies: unit 3.

5. **Restart and observe the autonomous loop**
   - Files: runtime ledger/state only.
   - Resources: Codex and Claude authenticated runtimes.
   - Dependencies: unit 4.

## Acceptance criteria

- Codex final-attempt escalation selects `gpt-5.6-sol`; Claude continues to
  escalate eligible lower models to `opus`; unknown and disallowed targets
  fail closed.
- Fresh Codex frontier work selects `gpt-5.6-sol`; standard/light work remains
  on Terra.
- Codex dispatch and JSON spawns with a scratch directory receive phase-local
  Go/XDG/TMP caches and `GOTELEMETRY=off`.
- A clean committed self-block retries within budget and reaches recovery
  escalation; dirty, commitless, malformed, and exhausted cases remain
  terminal.
- `make gate-agent` passes before installation.
- `koryph version` reports the newly built source commit after installation.
- Each preserved bead is reopened only after its branch is ready to resume;
  genuine review findings are fixed rather than overridden.
- The restarted loop uses auto-merge and recovery behavior and has a
  zero-token watcher that wakes only on an error/terminal block.

## Open questions / assumptions

- The locally installed Codex runtime accepts `gpt-5.6-sol` and the portable
  xhigh/max/ultra effort values. A direct runtime canary will verify this
  before restarting the wave.
- Claude authentication visible on the host can be projected into the
  `koryph-3vp.8` canary without exposing unrelated ambient credentials.
- Existing worktree branches remain available until recovery completes.
