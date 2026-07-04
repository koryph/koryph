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
  default. Per-token API billing is never used unless you explicitly opt in.
  There is no accidental path from "run a wave" to "rack up an API bill."
- **Per-provider governors.** Each provider gets its own concurrency cap that
  *adapts*: a rate-limit response halves it immediately (with settle windows and
  a circuit breaker to stop thrashing), and sustained success probes it back up.
  The fleet finds the edge of what the provider allows and rides it.
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
requires an explicit opt-in — an `api_fallback: explicit` setting plus a named
env var (never the ambient `ANTHROPIC_API_KEY`) — and every dispatched slot is
stamped with its `billing_mode` so the ledger records exactly how each agent was
paid for. Fail-closed is the rule throughout: if a usage source is unreadable,
it reports "full" and the governor drains rather than gambling.

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
