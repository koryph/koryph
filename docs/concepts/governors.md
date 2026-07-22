<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Money: governors, quota, and subscription-first

*This page expands the [Concepts overview](index.md). See
[Billing & quota](../user-guide/billing-and-quota.md) for the commands that
operate it.*

## The idea

An autonomous fleet can spend money faster than a human can watch it. Left
unchecked, a loop that dispatches agents in parallel will happily burn through a
budget — or a rate limit — in minutes. koryph treats spend as something the
system must bound *structurally*, not something you remember to check.

Three ideas stack up:

- **Subscription-first.** Dispatch rides your flat-rate Claude subscription by
  default. Per-token API billing is never used unless you explicitly opt in —
  either as a break-glass fallback at governor stop, or by registering an
  account under the explicit `api-key` [auth
  mode](../user-guide/authentication-modes.md). There is no accidental path
  from "run a wave" to "rack up an API bill."
- **Per-account governors.** Each *account* — not just each provider — gets its
  own concurrency cap that *adapts*: a rate-limit response halves it immediately
  (with settle windows and a circuit breaker to stop thrashing), and sustained
  success probes it back up. A larger subscription (a 20x Max seat) and a
  smaller work seat on the same provider get independent pools and independent
  caps — one account's rate limit never throttles another's. The fleet finds
  the edge of what each account's provider allows and rides it.
- **Quota tracking.** On top of raw concurrency, koryph calibrates observed
  spend against your plan's usage windows and throttles or pauses dispatch as a
  window fills.

## In koryph

Quota is inspected and calibrated through `koryph quota`:

```bash
koryph quota                # per-account governor snapshot (tabular)
koryph quota --json         # machine-readable, includes the usage probe
koryph quota calibrate --account personal --window 5h \
  --observed-usd 12.40 --observed-pct 62
```

The governor drives dispatch off the fullest usage window with a staged
response:

- **< 80% — `ok`:** full concurrency.
- **≥ 80% — `warn`:** log a warning and scale width linearly toward 1.
- **≥ 90% — `drain`:** stop dispatching new work; let active agents finish.
- **≥ 95% — `stop`:** pause the run entirely.

Subscription-first is enforced by config, not convention. API-key billing
requires an explicit opt-in — either an `api_fallback: explicit` setting plus a
named env var (never the ambient `ANTHROPIC_API_KEY`) as a break-glass fallback
once a subscription account hits governor stop, or a project registered with
`auth_mode: api-key` up front, which bills per token from wave one instead of
riding the subscription at all. Either way every dispatched slot is stamped
with its `billing_mode` so the ledger records exactly how each agent was paid
for. Fail-closed is the rule throughout: if a usage source is unreadable, it
reports "full" and the governor drains rather than gambling.

An `api-key` account has no subscription usage window to calibrate against,
so the 5h/weekly percentage ladder above does not apply to it. It is governed
by spend instead — a rolling-$ ceiling compared against actual dollars spent
— though today that ladder is advisory only: the enforced caps are the
per-run `--budget` flag and the per-agent cost cap. See [Billing &
quota](../user-guide/billing-and-quota.md#billing-and-quota-by-auth-mode) and
[Authentication modes](../user-guide/authentication-modes.md) for the full
picture.

## The failure mode it prevents

The runaway bill, and its quieter cousin, the rate-limit death spiral. Without a
subscription-first default, a misconfigured loop silently meters API tokens
until someone notices the invoice. Without an adaptive governor, hitting a rate
limit at full concurrency produces a storm of retries that each hit the limit
again, and throughput collapses to zero while spend on failed calls continues.
The staged 80/90/95 response and the halve-on-limit governor keep the fleet
running at the edge of what your plan allows and never past it.

## Operate it

- [Billing & quota](../user-guide/billing-and-quota.md) — the governor, billing
  guard, and calibration in practice.
- The `/koryph-calibrate` skill walks the calibration interactively.
- Governors pace [rolling dispatch](rolling-dispatch.md): they set how many
  worktree slots the loop is allowed to keep full.
- Per-account concurrency caps: a live operator override
  (`koryph governor set --account`) or a persisted default
  (`koryph quota set-threads`) — see [Billing &
  quota](../user-guide/billing-and-quota.md#per-account-concurrency-default-koryph-1o23)
  and the [design doc](../developer-guide/global-governor.md#per-account-governor-pools-koryph-v8u11--koryph-1o21-l5c).
