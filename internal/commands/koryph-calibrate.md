---
description: Walk through calibrating the quota governor for an account — reads current state, gathers observed usage, sets the ceiling, and confirms calibrated=yes
---

<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Calibrate the quota governor for an account so the koryph can police spend
against a real ceiling.

> **Honesty note — calibration ≠ more threads.**
> Calibration sets a *ceiling* that the governor enforces.  It cannot increase
> your Claude plan's concurrent-session limit; that is set by your subscription.
> If you are trying to run more agents in parallel, increase `--max` on
> `koryph run` (and read the warning in `/koryph-loop`).

Optional argument (an account profile name): $ARGUMENTS

---

## Step 1 — Read current governor state

```
koryph quota show
```

This prints every registered account with its calibration status, 5h window
spend/ceiling, and weekly spend/ceiling.

- If no projects are registered yet, run `koryph quota show --account <name>`
  where `<name>` is the name of the Claude account profile you want to calibrate
  (check `~/.koryph/projects/*.json` for the `account_profile` field, or
  pass `default` for the personal account).
- Note which windows show `calibrated=no` — those need calibration.

---

## Step 2 — Gather observed usage from Claude

You need two numbers from Claude's `/usage` page (or from ccusage):

**Option A — Claude /usage page**
Open [claude.ai/usage](https://claude.ai/usage) and note:
- The percentage consumed in the **current 5-hour window** (e.g. `38%`)
- The USD amount spent in that same window (shown below the bar, e.g. `$5.20`)
- Repeat for the **weekly** view

**Option B — ccusage CLI**
```
ccusage blocks --active        # 5h window spend
ccusage daily --days 7         # weekly spend (sum the 7 entries)
```

For the percentage you still need the `/usage` page — ccusage gives you
absolute USD but not the plan-ceiling percentage.

---

## Step 3 — Calibrate each uncalibrated window

Run one of the following for each window you need to calibrate.  Replace:
- `<account>` with the account profile name from Step 1
- `<observed-usd>` with the dollar amount you saw (e.g. `5.20`)
- `<observed-pct>` with the percentage (e.g. `38`)
- `<plan-tier>` with the tier label on your plan (e.g. `max20x`, `teams`, `pro`)

**5-hour window:**
```
koryph quota calibrate \
  --account <account> \
  --window 5h \
  --observed-usd <observed-usd> \
  --observed-pct <observed-pct> \
  --plan-tier <plan-tier>
```

**Weekly window:**
```
koryph quota calibrate \
  --account <account> \
  --window weekly \
  --observed-usd <observed-usd> \
  --observed-pct <observed-pct>
```

The ceiling is derived as: `ceiling = observed_usd / (observed_pct / 100)`.
Both windows are independent — calibrate the 5h window from a 5h reading
and the weekly from a 7-day reading.

---

## Step 4 — Confirm calibrated=yes

```
koryph quota show
```

The account row should now show `calibrated=yes` and the ceiling columns should
reflect the computed values.  If `calibrated=no` still appears, at least one
window ceiling is still 0 — repeat Step 3 for the remaining window.

---

## What to do if the readings seem off

- **Stale ccusage cache**: run `ccusage blocks --json --active` — if it shows
  `{}` or an empty blocks array, your 5h window just reset and spend is $0;
  try again mid-window.
- **No ccusage installed**: `koryph quota show` falls back to a local
  JSONL transcript scan (marked `approx` in the source column); calibration
  still works — just use the `/usage` page percentages, which are authoritative.
- **Multiple accounts**: run Steps 1–4 once per account, passing the correct
  `--account` flag each time.

---

## Reference

- Governor thresholds: warn 80%, drain (stop new dispatches) 90%, hard stop 95%
- Calibration persists to `~/.koryph/quota/<account>.json`
- Manual one-shot: `koryph quota calibrate --account <a> --window 5h --observed-usd <$> --observed-pct <%>`
- Show raw config: `cat ~/.koryph/quota/<account>.json`
