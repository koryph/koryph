<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Model-routing audit: is opus spent where it earns it, and does a haiku tier pay off? (2026-07-22)

Status: audit complete; one telemetry-correctness fix landed (see §5),
remaining items filed as follow-ups.
Origin: bead **koryph-cyq** — "In the latest stampede-games run, opus beads
averaged \$22.80 vs \$6.90 for sonnet (3.3×). Confirm opus is only spent where
it earns it (recovery/review) and not on routine implementers, and evaluate a
haiku tier for purely mechanical chore-type beads."

Method: this is a **code-grounded** audit. Every routing claim below is traced
to the resolution/dispatch source, not inferred from behavior. The cost figures
(\$22.80 opus / \$6.90 sonnet) are the operator-supplied per-bead means from the
stampede-games run; the mechanism for reproducing them from telemetry is
`koryph metrics` (§4).

## 1. The routing map, as actually implemented

Every dispatched bead resolves its model through `modelroute.Resolve`
(`internal/modelroute/route.go`), driven from `engine.runner.resolveModel`
(`internal/engine/wave.go`). The precedence (highest first):

1. explicit `--model` on a single dispatch;
2. stage-scoped label `model:<stage>:<tier>`;
3. plain bead label `model:<tier>`;
4. run default `--default-model`;
5. persona `tier`/`model` frontmatter (via the runtime's model map);
6. **stage default** — the hardcoded `claudeStageDefaults` table.

The stage-default table is the floor that applies when a bead carries no
`model:*` label and no persona pin:

| Stage | Default tier | Persona |
|---|---|---|
| plan, design, score, **review** | **opus** | architect / scorer / reviewer |
| implement, docs, test | **sonnet** | implementer / docs-author / test-engineer |
| explore, debug | **haiku** | explorer / debugger |

Crucially, **every wave-dispatched bead (`task`/`bug`/`chore`) enters at the
`implement` stage** (`resolveModel`: `stage := StageImplement`, switching to
`docs` only for auto-filed `validation:docs` beads). So the *routine
implementer floor is sonnet* — a chore is routed identically to a feature at
the model layer. Opus reaches an implementer only by an explicit `model:opus`
label (as on this very bead) or a persona pin — never by default.

Opus is otherwise reached on exactly two engine-internal paths, both
hardcoded to `TierOpus` and both off the implement path:

- **Review** — `poll.go:1045` and `reviewpr.go:324` dispatch the reviewer
  persona at `modelroute.TierOpus` unconditionally.
- **Recovery / final-attempt escalation** — `poll.go:1654` calls
  `modelroute.EscalationTier(sl.Model, allowed)`, which upgrades a
  sub-opus tier to opus (`RecoveryUpgrade` → opus for every tier;
  fable structurally excluded) on the last attempt of a bead-fault requeue.

**Verdict (opus escalation audit): the split is sound.** Opus is confined to
review, recovery-escalation, and explicit per-bead opt-in. Routine
implementers default to sonnet. There is no code path that silently routes a
plain implementer bead to opus.

## 2. Finding: the registry model-policy knobs are vestigial

`registry.Record` declares three model-policy fields —
`PlannerModel` ("opus"), `ImplModel` ("sonnet"), `RecoveryModelPolicy`
("upgrade-opus") — set by onboarding (`internal/onboard/register.go`). A
whole-tree grep shows they are **read nowhere** outside their own type
definition, the onboarding defaults, and tests:

```
$ grep -rn --include='*.go' -e '\.PlannerModel' -e '\.ImplModel' \
    -e '\.RecoveryModelPolicy' internal/ | grep -v _test
(no matches)
```

The actual policy lives in `modelroute`'s hardcoded `claudeStageDefaults` and
the two `TierOpus` literals in the review/recovery paths. The registry fields
happen to *describe* the same policy the code hardcodes, so there is no
behavioral bug today — but they are documentation masquerading as
configuration. An operator who edits `impl_model` in the registry expecting to
change the implementer floor gets **no effect**, silently.

This is a latent foot-gun, not a live defect. Wiring the knobs (or deleting
them) is a behavior change that does not belong on an audit bead; it is filed
as a follow-up (§6).

## 3. Finding: the cost telemetry could not, before this bead, attribute opus spend

The bead's premise — "opus averaged \$22.80 vs sonnet \$6.90" — is exactly the
kind of number `koryph metrics` produces (`internal/metrics`, `ByModel`
breakdown: slots / cost / mean / merged / failed / retries per model). Two
gaps limited its usefulness for *this* audit:

1. **Wrong key (fixed in this bead).** `metrics.aggregateRun` bucketed cost by
   `sl.Model` — the model dispatch *requested* — not `sl.ModelActual`, the
   model that *actually served*. When the CLI's hardcoded `--fallback-model`
   downgrades a session mid-flight, a bead requested on opus but served by
   sonnet was charged to the **opus** row it never spent, inflating exactly
   the number this audit turns on. The sibling token rollup in
   `internal/cockpit/efficiency.go` already keyed on `ModelActual`; the cost
   rollup did not. §5 aligns them.

2. **No stage/persona dimension (follow-up).** `ByModel` answers "how much did
   opus cost" but not "*why* was opus spent" — review vs recovery vs explicit
   opt-in. Confirming the §1 verdict *from telemetry* (rather than from code,
   as done here) needs a per-persona or per-stage cost slice. The `Slot.Agent`
   (persona) field already carries the signal; the rollup just does not
   project it. Filed as a follow-up (§6) — the code-level audit in §1 stands
   on its own in the meantime.

### 3.1 How to reproduce the per-model report

```
koryph metrics --json            # all projects; by_model[*].{slots,cost_usd,mean_usd,merged,failed,retries}
koryph metrics --project koryph  # human table, per-model breakdown section
```

Post-fix, `by_model` is keyed on the served tier. A 3.3× opus/sonnet mean-cost
ratio is expected and healthy *given the split in §1*: opus runs the
hardest work (review of every merged bead, plus the recovery tail of the
beads that failed sonnet), so its population is selected for difficulty. The
ratio is a red flag only if opus slots show up under implementer personas with
attempt==1 — which §1 shows the code cannot produce, and which the §6
per-persona slice will let an operator verify continuously.

## 4. Haiku tier evaluation

**Question:** should purely mechanical `chore`-type beads route to haiku by
default?

**Decision: reject haiku-as-a-default-for-chores; adopt haiku as an explicit,
already-supported per-bead opt-in.** Rationale:

- **The mechanism already exists.** A bead can carry `model:haiku` today
  (`plainModelLabel`), a persona can pin `tier: haiku`, and the explore/debug
  stages already default to haiku. Nothing needs to be built to *use* haiku on
  a bead an operator judges trivial. The only thing on the table is making it a
  **default** keyed on bead *type*.

- **A koryph chore is not "purely mechanical" from the agent's seat.** Even a
  one-line diff must pass the full green gate (`gofmt`, `go build`, `go vet`,
  `go test ./...`, lint, reuse), resolve any rebase conflict, write a
  conforming SUMMARY, honor footprints, and produce a DCO-signed Conventional
  Commit. The *diff* is mechanical; the *dispatch envelope* is not. Haiku's
  higher rate of gate/format/commit-hygiene misses on this envelope is the
  risk, and it is not proportional to diff size.

- **The failure tail is cost-asymmetric.** A haiku bead that fails and requeues
  escalates to **opus** on its final attempt (`EscalationTier`, §1). So the
  expected cost of routing a chore to haiku is
  `p(success)·haiku + p(fail)·(haiku + … + opus)`. Because the escalation
  target is opus, not sonnet, even a modest haiku failure rate can make the
  expected cost *exceed* simply running sonnet once. Haiku only pays off when
  its first-attempt success rate on the koryph envelope is high — which we
  have no telemetry to assert, precisely because no chore has run on haiku yet.

- **The safe path is evidence-gated.** Adopting haiku-by-default blind would be
  a bet against an asymmetric downside with no data. Instead: keep haiku an
  opt-in, let operators tag genuinely-trivial beads `model:haiku`, and let the
  existing `internal/modellearn` escalation-feedback loop accumulate evidence.
  If per-persona/per-tier telemetry (§6) later shows haiku chores succeeding
  first-attempt at a rate that beats sonnet's expected cost including the
  opus tail, *then* promote it to a default behind a registry knob — with
  numbers, not a guess.

**Net:** no routing change for haiku in this bead. The decision is recorded;
the opt-in is documented in the user guide (see §5).

## 5. What changed in this bead

- `internal/metrics/rollup.go` — `aggregateRun` now keys the per-model
  cost/outcome breakdown on `ModelActual` (fallback `Model`), so a mid-flight
  downgrade is charged to the tier that ran, matching
  `internal/cockpit/efficiency.go`. Regression test
  `TestCollectByModelActualAttribution` locks it in.
- `internal/metrics/types.go` — package contract comment updated.
- `docs/user-guide/` — haiku opt-in guidance (see the model-routing chapter).
- This design doc.

No change to routing behavior: implementers stay sonnet, review/recovery stay
opus, haiku stays opt-in.

## 6. Follow-ups

1. **Wire or retire the registry model-policy knobs** (`PlannerModel`,
   `ImplModel`, `RecoveryModelPolicy`). Either plumb them into
   `modelroute.Resolve` (project-overridable stage floors) or delete them so
   the registry stops advertising configuration it does not honor. Behavior
   change — author on main / dedicated bead, not an audit.
2. **Per-persona (stage) cost slice in `metrics`.** Project `Slot.Agent` into a
   `ByPersona` (or stage-bucketed) cost/outcome map so "opus spent on review vs
   recovery vs implement" is answerable from `koryph metrics` directly,
   turning the §1 code audit into a standing telemetry check.
3. **Haiku promotion, evidence-gated.** Once (2) lands and chores have run on
   `model:haiku`, revisit whether a `chore → haiku` default (behind a registry
   knob) beats sonnet's expected cost including the opus escalation tail.
