<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Billing and Quota

Koryph is subscription-first: every dispatched agent runs against your
Claude subscription by default. Per-token (API-key) spend is an opt-in
exception that requires three explicit gates to open.

---

## Subscription-first dispatch

The engine never imports `internal/anthro` and therefore cannot incur
per-token spend by accident. Billing mode is stamped on every slot
(`billing_mode: subscription | api-key`) and written into the manifest and
audit log so you always have a per-dispatch record.

---

## The 80 / 90 / 95 governor

The governor measures two usage windows per account and maps
fraction-of-ceiling to one of four levels:

| Fraction | Level   | Effect in loop mode                                              |
|----------|---------|------------------------------------------------------------------|
| < 80 %   | `ok`    | Full concurrency                                                 |
| ≥ 80 %   | `warn`  | Log warning; concurrency linearly scaled toward 1 as 90 % nears |
| ≥ 90 %   | `drain` | No new dispatch; active agents finish their current turn         |
| ≥ 95 %   | `stop`  | No new dispatch; run paused (`paused-quota`) or switches to API-key billing if explicitly configured |

**Concurrency scaling.** Between 80 % and 90 %, `ScaleSlots` linearly
interpolates wave width from `max` down to 1. At 90 % the slot count is 0
and no new agents are launched. Manual dispatch (`koryph run --manual`) is
exempt from all governor levels.

**Preflight gate.** Before each wave the engine projects the estimated cost
of the candidate items against the 5 h ceiling. A wave that would push the
window to or past 90 % is refused before any agent is launched.

**Fail-closed windows.** A window whose source is `unavailable` reports
fraction 1.0 and immediately triggers drain. An uncalibrated account (both
ceilings zero) is always `ok` — baseline establishment is never blocked.

---

## Calibration — `koryph quota calibrate`

Usage windows are denominated in USD, but the Claude subscription plan
exposes percentages. Calibrate by reading `/usage` in the Claude app and
recording the corresponding ccusage spend:

```sh
# Example: /usage shows "42%"; ccusage reports $8.40 spent this block.
koryph quota calibrate \
  --account personal \
  --window 5h \
  --observed-usd 8.40 \
  --observed-pct 42 \
  --plan-tier max20x      # optional label stored for reference
```

Koryph derives: `ceiling = observed_usd / (observed_pct / 100)` —
**$20.00** in this example. Calibration is persisted to
`~/.koryph/quota/<account>.json`. Repeat with `--window weekly` for the
rolling 7-day window.

**Check current state:**

```sh
koryph quota            # tabular: ACCOUNT  LEVEL  CALIBRATED  5H  WEEKLY
koryph quota --json     # machine-readable snapshot
```

After real dispatches the estimator self-calibrates via an EWMA
(`0.7 × old + 0.3 × actual`) per model-tier × size bucket (S / M / L),
so preflight estimates improve over time without further manual updates.

---

## The billing guard

The billing guard controls whether the governor's throttling constraints
(preflight, drain/stop blocking, concurrency scaling) are **enforced** or
**advisory**:

| Mode | How activated | Effect |
|------|---------------|--------|
| **Enforced** (default) | `billing_guard` unset or `"enforce"` in the registry record | Governor blocks dispatch and scales concurrency |
| **Advisory (registry)** | `billing_guard: advisory` in the registry record | Measure + log + warn; never block |
| **Advisory (run flag)** | `--no-billing-guard` passed to `koryph run` | Advisory for that run only |
| **Automatic baseline** | Account not yet calibrated | Always advisory until a ceiling is set |

In any advisory mode, billing stays on subscription regardless of governor
level — the API-key stop-fallback never fires.

> Spend-authorization gates (explicit API key, batch confirmation) are
> independent of the billing guard and are never bypassed by advisory mode.

---

## Explicit API-key fallback

To continue dispatching after a governor `stop` at subscription-plan
capacity, all three of the following must be satisfied simultaneously:

1. **Run flag** — `koryph run --allow-api-spend`
2. **Registry policy** — `api_fallback: explicit` on the project's registry record
3. **Named env var** — `api_key_env_var` set in the registry record, and
   that variable present in the environment. The variable must not be
   `ANTHROPIC_API_KEY`; a purpose-specific name such as `KORYPH_API_KEY`
   is required.

When all three conditions are met at a `stop` event, the engine logs a
prominent warning and switches the current wave's `billing_mode` to
`api-key`. A per-agent budget cap (`per_agent_max_usd`, default **$25**) is
forwarded as `--max-budget-usd` to limit runaway spend.

---

## Batch mode

`internal/anthro` exposes Message Batches for bulk workloads (planning,
scoring, backfill) at the Anthropic 50 % batch discount. Batch access is
governed separately from loop dispatch:

- **Registry** — `batch_policy: explicit` (default is `deny`)
- **Explicit confirmation** — every `BatchSubmit` call requires a populated
  `Confirm{Confirmed: true}` value; an absent or false confirmation is
  refused and the estimated cost is printed instead

Batch submissions are never initiated by the loop engine. They are available
only to CLI subcommands that explicitly build a `[]MsgReq` slice, surface
the cost estimate via `EstimateUSD`, and pass the user's confirmation to
`BatchSubmit`.

---

## Usage measurement sources

The governor prefers live data and falls back gracefully:

1. **ccusage CLI** — `ccusage blocks --json --active` for the 5 h window;
   `ccusage daily --json` summed over the last 7 days for the weekly window.
   Set `KORYPH_NO_NPX=1` to prevent `npx ccusage@latest` auto-install.
2. **Local transcript scan** — `~/.claude/projects/*/*.jsonl` (or the
   profile's `CLAUDE_CONFIG_DIR`). Approximate; flagged `approx: true`. The
   5 h window uses a fixed UTC epoch-aligned grid; the weekly window is a
   rolling 7-day span.
3. **Unavailable** — reported as fraction 1.0; the governor drains
   (fail-closed).
