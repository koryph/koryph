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

## The governor ladder

The governor measures two usage windows per account and maps
fraction-of-ceiling to one of five levels (defaults; configurable per-account
via `quota.Ladder`):

| Fraction | Level      | Effect in loop mode                                                 |
|----------|------------|---------------------------------------------------------------------|
| < 90 %   | `ok`       | Full concurrency                                                    |
| ≥ 90 %   | `warn`     | Log warning; full concurrency continues                             |
| ≥ 94 %   | `throttle` | Concurrency linearly scaled toward 1 as 97 % nears                 |
| ≥ 97 %   | `drain`    | No new dispatch; active agents finish their current turn            |
| ≥ 99 %   | `stop`     | In-flight agents interrupted (SIGTERM); worktrees preserved for resume |

**Concurrency scaling.** Between 94 % and 97 %, `ScaleSlots` linearly
interpolates wave width from `max` down to 1. At 97 % the slot count is 0
and no new agents are launched. Manual dispatch (`koryph run --manual`) is
exempt from all governor levels.

**Preflight gate.** Before each wave the engine projects the estimated cost
of the candidate items against the 5 h ceiling. A wave that would push the
window to or past 97 % (the graceful-stop threshold) is refused before any
agent is launched.

**Configuring thresholds.** All four thresholds are configurable per-account
in `~/.koryph/quota/<account>.json` under the `ladder` key:

```json
{
  "account": "personal",
  "ladder": {
    "warn": 0.90,
    "throttle": 0.94,
    "graceful_stop": 0.97,
    "hard_stop": 0.99
  }
}
```

Zero fields use the package defaults shown in the table above. Fields must be
strictly ascending.

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
koryph quota --json     # machine-readable snapshot (ccusage probe may take up to 40 s)
```

`--json` emits an array of per-account snapshots.  Each element has this shape:

```json
[
  {
    "account": "personal",
    "level": "ok",
    "calibrated": true,
    "usage": {
      "account": "personal",
      "at": "2026-07-03T21:26:00Z",
      "window_5h": {
        "hours": 5,
        "spent_usd": 8.40,
        "ceiling_usd": 20.00,
        "source": "ccusage",
        "approx": false
      },
      "weekly": {
        "hours": 168,
        "spent_usd": 42.00,
        "ceiling_usd": 140.00,
        "source": "ccusage",
        "approx": false
      }
    }
  }
]
```

Field notes:

| Field | Values | Meaning |
|-------|--------|---------|
| `level` | `ok` / `warn` / `drain` / `stop` | Current governor verdict |
| `calibrated` | bool | False until at least one `quota calibrate` run |
| `usage.at` | RFC 3339 | Snapshot timestamp |
| `*.source` | `ccusage` / `jsonl-scan` / `unavailable` | Where the number came from |
| `*.approx` | bool | True for the local transcript scan (less precise) |

After real dispatches the estimator self-calibrates via an EWMA
(`0.7 × old + 0.3 × actual`) per model-tier × size bucket (S / M / L),
so preflight estimates improve over time without further manual updates.

### Estimator error loop (koryph-6bl)

The dispatch-time estimate is now persisted beside the eventual actual cost
on every ledger slot (`estimate_usd`). After each agent completes, koryph
computes two accuracy metrics per `(tier, size)` bucket and persists them in
the quota config alongside `calibration`:

| Metric | Meaning |
|--------|---------|
| **Bias** | EWMA of `actual / estimate` — 1.0 is perfect; > 1 = under-estimating |
| **MAPE** | EWMA of `|actual − estimate| / estimate × 100` — percentage error |

Once a bucket accumulates **5 observations**, the bias factor is applied to
future estimates automatically (`corrected = base × bias`), so systematic
under- or over-estimation self-corrects without any manual intervention.
The refill log line gains a confidence hint when MAPE data is available:

```
wave 3: 12 ready, dispatching 2 (est $3.20 +/-35% / window 18%)
```

**Inspect estimator accuracy:**

```sh
koryph metrics estimator              # tabular: account / key / n / base / bias / MAPE / correction active
koryph metrics estimator --json       # machine-readable
koryph metrics estimator --account personal   # single account
```

The table marks rows with `|bias − 1| > 0.5` as **WARN** — these buckets
have large systematic error and are candidates for a manual `quota calibrate`
pass to reset the ceiling.

**The feedback loop:**

1. `waveEstimate` computes a per-item estimate at dispatch and persists it on
   the slot as `estimate_usd`.
2. When the agent completes, `completeSlot` records `actual / estimate` in
   `ErrorStats["tier:size"]` via the same lock-guarded EWMA as the base
   calibration.
3. The next wave picks up the corrected estimate automatically — no restart,
   no CLI command.

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
| **Automatic baseline** | Account not yet calibrated | Advisory until a ceiling is set — but **warned loudly every run** (see below) |

In any advisory mode, billing stays on subscription regardless of governor
level — the API-key stop-fallback never fires.

### Uncalibrated governor (koryph-grz)

An uncalibrated account (both ceilings `0`) cannot enforce the 5h/weekly ladder,
so it runs **advisory** — but this is no longer *silent*. Every run emits a
prominent warning that spend limits are **not** being enforced, naming the
account and the `koryph quota calibrate` fix. If you want spend safety to be a
hard guarantee rather than a nudge, opt into **fail-closed**:

- **Per run:** `koryph run … --require-calibration` — the run refuses to
  dispatch (reason `governor-uncalibrated`) until a ceiling is calibrated.
- **Per project:** `"require_calibration": true` in `koryph.project.json` —
  same block for every run of that project.

Calibrating (below) clears both the warning and the block.

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

### Per-agent budget caps and the turn-boundary nuance

`--max-budget-usd` **is enforced under subscription OAuth**, not just
API-key billing — confirmed empirically with a live canary. But the
enforcement point matters for capacity planning:

> **The Claude CLI checks the budget cap at turn boundaries, not mid-generation.**
> A turn already in flight when the cap is reached is allowed to finish before
> the session is killed. On a thinking-heavy turn this can **overshoot the
> configured cap substantially** — the pinning canary observed a $0.001 cap
> overshoot to $0.43 (~428x) because the in-flight turn was allowed to
> complete. Treat `per_agent_max_usd` / `--max-budget-usd` as a
> **per-turn-bounded** ceiling, not a hard mid-generation interrupt: actual
> spend on a killed agent can exceed the configured cap by the cost of one
> turn.

When an agent is killed this way, the engine classifies it distinctly from a
crash or rate-limit death (`DeathReason: budget-killed`,
`error_max_budget_usd` in the stream) and applies a warm-resume-then-park
policy so the cap isn't silently re-paid on every retry — see [Budget-killed
agents](running-waves.md#budget-killed-agents) in Running Waves for the full
requeue/park semantics.

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
