<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Experimental: headroom-ai integration

> **EXPERIMENTAL. Off by default. Read the hazards section before enabling.**
> This chapter documents how to point koryph's `agent_proxy` seam at
> [headroom-ai](https://pypi.org/project/headroom-ai/), a third-party
> context-compression proxy. It is not koryph code, not koryph-maintained,
> and not recommended for production accounts. See
> [Hazards](#hazards) before you install anything.

For the native, zero-risk levers koryph ships and enables by default, see
[Context economy](context-economy.md). This chapter only covers the opt-in
interception path — routing agent traffic through an external proxy — which
carries a materially different risk profile.

## What headroom-ai is, and the honest expected value

headroom-ai is a Python proxy that sits between an Anthropic API client and
`api.anthropic.com`, rewriting request bodies to reduce token usage: JSON
compaction, log/tool-output compression, and prefix-aware caching heuristics.
Its own fleet telemetry is honest about the range: **median savings 4.8%**,
with the widely-cited 40–80% figures only on workloads dominated by giant
JSON tool payloads.

That headline band does not transfer to koryph. Measurement of this
project's own fleet (docs/designs/2026-07-token-economy.md §1) found 70% of
tool-result bytes are `Read` output — source code, which headroom's default
tool exclusions never touch, and which should never be lossily compressed
regardless. The honestly compressible class for koryph workloads is Bash
output (gate logs, `go test`, lint spew), which koryph already handles
natively via `make gate-agent` and file-spill wrappers (see
[Context economy](context-economy.md#wrappers-and-file-spill)) — no proxy
required.

**Expected value for a koryph fleet: roughly 10–15% of quota burn, best
case** (design §1's measured ceiling) — not the 40–80% headline. Koryph's
[holdout workflow](#holdout-workflow-measure-before-you-believe-it) exists
precisely so you measure your own fleet's number instead of trusting either
figure.

## Setup

### 1. Install an exact pinned version

Never install unpinned — see
[Upstream release cadence](#upstream-release-cadence) below. Install with
`pipx`, matching the version you will set as the registry's
`agent_proxy.pin`, and do **not** install the `[ml]` extra:

```sh
pipx install "headroom-ai==0.30.0" --python python3.12   # no [ml]
```

Heuristics-only (SmartCrusher, LogCompressor, prefix-freeze) is Apache-2.0,
deterministic, and sufficient for the Bash-output class koryph cares about;
the ML extra adds a model dependency this chapter does not evaluate and
koryph's I7 determinism expectations do not cover.

### 2. Configure env-only — never `headroom wrap`

**Never run `headroom wrap`.** It mutates `.claude/settings.local.json` to
inject its own hooks and base-URL override. `koryph rules install` owns
`.claude/settings*.json` (design I6: no tool but `koryph rules install`
mutates it); `headroom wrap` fighting that file is not a hypothetical
conflict, it is the exact failure mode this design step exists to prevent.

Instead, run headroom-ai as a standalone proxy process and point koryph's
`agent_proxy` at it via the registry — the single sanctioned
`ANTHROPIC_BASE_URL` source, injected as a typed `ChildEnvSpec` field at all
four spawn sites (main dispatch, review, stage, epicreview):

```sh
HEADROOM_MODE=cache \
HEADROOM_OUTPUT_SHAPING=off \
HEADROOM_TELEMETRY=off \
headroom serve --host 127.0.0.1 --port 8787
```

- **`HEADROOM_MODE=cache`** — cache-safe compression mode. Other modes trade
  more aggressively against prompt-cache stability than koryph's I7
  determinism invariant tolerates.
- **Output shaper OFF.** headroom's output shaper appends blocks to the
  `system` field — the exact surface Anthropic's subscription enforcement
  fingerprints (see [Hazards](#hazards)). This is not optional; leaving it on
  defeats the point of routing through `agent_proxy` instead of `headroom
  wrap` in the first place.
- **Telemetry off, and verify it.** Confirm no outbound telemetry connection
  is opened (e.g. `lsof -i -P | grep headroom` while idle, or your platform's
  equivalent) before trusting it with real traffic. Do not take the flag's
  presence as proof; verify.

Then register the proxy for the project:

```json
{
  "project": "my-project",
  "agent_proxy": {
    "base_url": "http://127.0.0.1:8787",
    "health": "/health",
    "pin": "headroom-ai==0.30.0",
    "holdout": 0.1
  }
}
```

- `base_url` must be an `http://` loopback address (`127.0.0.0/8`,
  `localhost`, or `[::1]`) — the registry refuses to load a non-loopback
  value at read time (design I4).
- `pin` should match the exact installed version string; `koryph doctor`
  compares it against the proxy's self-reported health-endpoint pin and
  errors on mismatch (refuse-to-route).
- `holdout` defaults to `0.1` (10%) if omitted — see
  [Holdout workflow](#holdout-workflow-measure-before-you-believe-it).

Run `koryph doctor --project my-project` after configuring: it verifies the
health endpoint is reachable, the base URL is loopback, and the reported pin
matches the registry's configured pin. Flipping `agent_proxy` also marks the
account's quota calibration stale — `koryph doctor` will prompt a
`koryph quota calibrate --account <account>` re-run, because the
ccusage-USD↔`/usage`-percent slope is not proven invariant under
compression.

## Hazards

Each of these is a reason this chapter opens with "experimental" and "off by
default." Read all four before enabling.

### Subscription enforcement risk

Anthropic actively fingerprints Claude Code traffic on subscription
(non-API-key) accounts using body-content signals, including the `system`
field. Enforcement has tightened repeatedly and bans have occurred without
warning. Rewriting request bodies on a subscription account through any
third-party proxy is unadjudicated territory — this is not a
koryph-specific risk, it is inherent to running any body-rewriting proxy in
front of Claude Code on a subscription plan. koryph is subscription-first
by architecture, which makes this risk class existential rather than
incidental. **Use a disposable or canary account for any headroom-ai
experiment, never your primary subscription.** Do not enable this on an
account whose loss you cannot absorb.

### 1M-context beta-header regression

Claude's extended (1M token) context window is gated behind a beta header
that some proxy configurations drop or mishandle when requests are routed
through a custom `ANTHROPIC_BASE_URL`. If your workload depends on the
extended context window, verify it still negotiates correctly through the
proxy before relying on it — a silent fallback to the default context
window is easy to miss until a long session truncates unexpectedly.

### The oversize/auto-compaction interaction

A response whose context usage now fits only because of headroom's
compression creates a hazard if the proxy ever fails open (returns the raw,
uncompressed request on error): a request that only fit the window
*because* of compression is passed through raw, overflows, and dies on
Claude Code's own context-overflow breaker instead of failing at the proxy.
This is precisely why koryph's own design invariant (I1) requires
compress-or-fail-closed for any request that depends on compression to fit
— verify headroom-ai's failure behavior on
oversized requests before trusting it, since koryph does not control what
happens inside a third-party proxy process.

### Upstream release cadence

headroom-ai ships new releases roughly every two days. An unpinned install
(`pip install headroom-ai` with no version) can change compression behavior,
defaults, or the output-shaper default state under you between two koryph
runs with zero warning. **Never run unpinned.** Always pin the exact version
in both the `pipx install` command and the registry's `agent_proxy.pin`
field, and let `koryph doctor`'s pin-match check catch drift.

## Holdout workflow: measure before you believe it

Once `agent_proxy` is configured, a fraction of dispatches (default 10%,
the `holdout` field above) always bypass the proxy — a **permanent standing
canary**, not a one-time experiment, so a claimed compression win is never
confused with "the beads that week happened to be smaller." Arm assignment
is deterministic per bead ID: a requeued or resumed bead always lands in the
same arm it started in.

Run a wave, then read the two-arm comparison:

```sh
koryph metrics tokens --project my-project --experiment
koryph metrics tokens --project my-project --experiment --json   # scripting
```

This reports, per arm (`proxied` vs. `holdout`): beads observed,
tokens/bead, cache-hit ratio, requeue rate, blocking-review-finding rate,
gate-failure count, cost, and the account's estimator bias/MAPE segment for
that arm. It also prints a `calibration slope` line — half of the design's
calibration-slope check (ccusage-USD vs. `/usage`-percent) is ledger-derived
and shown; the other half needs an operator-read `/usage` percentage per arm
(no ledger field carries it), which the report documents as an explicit seam
rather than silently omitting.

### Go/no-go tripwires

The design (§4) pre-agrees the decision criteria so the question isn't
relitigated per project. **Build the native scope** (a minimal, pre-agreed
proxy — see the decision bead) only if, sustained across at least 4 weeks of
holdout data:

- The proxied arm shows **≥ 15% sustained per-bead quota reduction** over
  the holdout arm — not a single wave's number, a trend.
- **No quality regression**: requeue rate, blocking-review-finding rate, and
  gate-failure count in the proxied arm are not worse than the holdout arm.
- **Enforcement clarity**: either explicit Anthropic guidance on
  body-rewriting proxies for subscription traffic, or sustained,
  incident-free community precedent.

If the proxied arm shows less than roughly 5% sustained improvement, the
honest verdict is **not recommended** — the seam remains available as
generic plumbing (it's backend-agnostic; another compression backend could
still use it), but headroom-ai specifically did not earn its keep for this
fleet.

The standing decision record is **koryph-bvz** (`decision` bead, never
loop-dispatched — revisited only when holdout data exists). Read it before
proposing to build the native interception proxy; it also carries the
pre-agreed minimal scope so that build, if it happens, doesn't scope-creep
past what the holdout evidence actually justified.

## See also

- [Context economy](context-economy.md) — the native, zero-risk, on-by-default
  levers (`koryph metrics tokens`, `make gate-agent`, file-spill wrappers,
  bd-prime slimming). Start there; this chapter is the opt-in extension.
- [Doctor](doctor.md) — `koryph doctor --project <id>` and its checks.
- [Billing & quota](billing-and-quota.md) — calibration and the governor
  ladder that `agent_proxy` calibration-stale wiring interacts with.
