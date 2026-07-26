<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Autonomy canary SLOs

Koryph does not treat a green repository gate as proof that autonomous
operation is safe. A bounded canary must also prove completion quality,
retry economy, latency, resource behavior, and safety. The canary report is
the durable release decision.

## Publish a report

The bounded native-loop supervisor derives evidence from its durable state,
engine ledgers, authenticated review and gate artifacts, terminal manifests,
and command events. It publishes the release report directly to
`.plan-logs/koryph/canary/autonomous-loop-reliability.json`; there is no
operator-authored intermediate input in the release path.

For offline fixture evaluation and recovery diagnostics only, the same report
evaluator is available as:

```sh
koryph metrics autonomy \
  --input .plan-logs/koryph/canary/evidence.json \
  --out .plan-logs/koryph/canary/offline-autonomy-diagnostic.json
```

`--json` also prints the report.
A report created by this command cannot satisfy release doctor by itself:
doctor also requires the exact matching identity and publication checkpoint
from durable native-supervisor state.
Koryph writes the report atomically and create-once: an existing report is
never replaced, even by identical bytes. When an account or other
canary-bound registry setting changes, the next fixed-canary invocation moves
the prior report and checkpoint into generation-keyed history before freeing
the fixed path for the new generation. Historical evidence is retained; it
cannot promote the new registry identity or satisfy `doctor
--autonomy-canary`.
Publication and loading walk every path component relative to an open directory
descriptor with no-follow semantics. A symlinked parent, final symlink, FIFO,
device, socket, or other non-regular report target is rejected.

The command publishes failure evidence before returning a nonzero exit status.
The report includes the installed source commit, binary version, fixed cohort
and its digest, contract digest, non-repeating registry validation identity,
every implementation attempt and typed outcome, stage timings, process events,
pressure samples, normalized tokens, artifact bytes, and the final decision.
Its evidence digest authenticates all identity, evidence, derived metrics, and
checks.

## Initial thresholds

The evidence input carries the thresholds applied to that canary. The approved
defaults are:

| Measure | Default |
|---|---:|
| Eligible small/medium Beads | at least 10 |
| Autonomous correct completion | at least 90% |
| First semantic-review pass | at least 75% |
| Retry attempts / implementation dispatches | at most 20% |
| Frontier implementation dispatches | at most 5% |
| Median dispatch to terminal | at most 15 minutes |
| p95 dispatch to terminal | at most 30 minutes |
| Active artifacts, excluding retained failure evidence | at most 2 GiB |
| Hard active-artifact tripwire | 5 GiB |

Gate, semantic-review, and merge queue time and service time are separate
distributions. They are not folded into one opaque “finalization” duration.
Each stage carries an explicit `reached` bit. A reached stage contributes both
queue and service samples even when either duration is zero; an unreached stage
must carry zero durations. Even-sized medians round upward so integer
truncation cannot turn a threshold miss into a pass.
Dispatch-to-terminal begins at the first attempt and ends at the final typed
terminal outcome, so retries remain visible in bead latency.

Normalized token total is exactly:

```text
fresh input + output + cache read + cache creation
```

A provider's inclusive input counter is retained for audit but is never added
to that total.

## Denominators and exclusions

Only a predeclared `external-capability` hold may leave the eligible cohort.
The report retains excluded Beads and their evidence, but removes them from
velocity denominators. An unplanned capability block stays eligible and adds
the `misclassified-capability-block` safety violation; it cannot improve a
rate by shrinking the denominator.

Autonomous completion uses all eligible cohort Beads as its denominator. A
completion counts only when the final outcome is both terminal and correct and
no attempt records operator intervention. First-review pass uses candidates
that reached semantic review. Retry rate is retry implementation dispatches
divided by all eligible implementation dispatches. Attempts must be numbered
contiguously per Bead, and every retry must carry a typed reason plus prior and
current evidence digests.

Model tiers are the closed vocabulary `light`, `standard`, and `frontier`.
Outcomes are also closed and their truth fields are canonical: successful
terminal outcomes are correct, failure outcomes are not, and intermediate
outcomes are neither terminal nor correct. An external capability exclusion
must end in the final canonical `capability-hold` outcome with
`block_kind: external-capability`; free-form or intermediate capability states
cannot remove a Bead from a denominator.

All attempt dispatches, terminal times, process events, and pressure samples
must be ordered and fall inside the declared canary interval. Authenticated
candidate completion must follow dispatch; gate completion must follow the
candidate; general review must follow the gate; required security review must
follow general review; and the typed terminal outcome must follow the completed
review lanes and precede the canary end. Review completion timestamps and
queue/service reach evidence must agree. `generated_at` is publication time and
must not precede the interval end. Token counters use checked arithmetic:
negative provider totals and any per-attempt or aggregate overflow reject the
report rather than wrapping.

## Fail-closed posture

The canary fails on any configured threshold miss. It also fails immediately
for missing or failed required safety evidence, an unchanged retry, an idle
ledger creation, a duplicate broad command, inconsistent token semantics,
unjustified frontier implementation, a misclassified capability hold,
persistent critical pressure, or the artifact hard limit.

Admission durably fixes a 30-minute inactivity limit, aligned with the approved
p95 dispatch-to-terminal ceiling. The progress timestamp starts at admission
and advances only when the supervisor records new terminal evidence for a
cohort member. Observation, dispatch, duplicate terminal events, process
restart, and idle polling cannot refresh or loosen it. Expiry drains the engine,
opens the circuit, and publishes the available cohort evidence as a failed
partial report. The same failed-publication path is used when observation,
engine, or non-progress restart budgets are exhausted.

The project doctor consumes the same immutable report and independently
recomputes its metrics and decision. It also requires a complete live release
expectation supplied from trusted supervisor/release state. The expectation
pins the exact project, full installed commit, binary version, contract digest,
cohort and cohort digest, thresholds, canary start, evidence digest, generation
time, and publication checkpoint. A maximum publication delay and explicit
future clock-skew bound prevent a replayed, stale, or future-dated report from
passing. Calling the doctor check without that expectation fails closed;
generic strict loading is only for internal inspection and recovery
comparison.

A missing, malformed, duplicate-keyed, tampered, foreign, stale, unsafe, or
below-threshold report is an error posture, not a warning. The native loop must
remain drained until the report is valid and every check passes.
