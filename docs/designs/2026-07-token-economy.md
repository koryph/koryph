<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Token economy: native context reduction, a proxy seam, and evidence-gated interception (2026-07-06)

Status: approved for implementation; epics + children filed from §7.
Origin: operator direction — "Analyze https://pypi.org/project/headroom-ai/ and
determine how to either integrate or natively implement the capabilities that
it provides to reduce token usage."

Research base: headroom-ai v0.30.0 (Apache-2.0; Python core + Rust
`headroom-core` reference spec), its docs/source, and a measured token profile
of this machine's own koryph fleet. Design reviewed by a six-lens adversarial
panel (cache economics, security/supply-chain, ops burden, integrate-only
advocate, native-minimal advocate, OAuth-proxy feasibility); their corrections
are folded in throughout and drove the evidence-gating in §4.

## 1. Problem, with measurements

koryph's throughput is bounded by subscription quota windows: the governor
ladder throttles at .94 and stops at .97 of the calibrated ceiling, so every
wasted token is wave capacity lost. Yet koryph has no token-denominated
telemetry (all accounting is USD roll-ups), performs no context reduction
beyond prompt-cache-stable compilation (`internal/promptc`), and injects a
~20 KB `bd prime --hook-json` block into **every** agent session — main
dispatches, reviewers, stages, and epic validators alike.

Measured fleet profile (this machine: 99 slot dirs, 268 sessions, 1.255 B
tokens, ≈$1,891 quota-notional priced with `internal/quota/usage.go`'s table):

| class | raw tokens | USD share |
|---|---|---|
| cache_read | 94.7% | 38.8% |
| cache_creation | 3.8% | 28.1% |
| output | 1.0% | 28.5% |
| fresh input | 0.5% | 4.7% |

Content bytes: tool_result 67% — of which **Read 70.2%, Bash 27.6%**;
tool_use input (the code agents write) 23.2%; assistant prose only 2.1%.

What headroom-ai teaches, and what transfers:

- Its heuristic catalog (SmartCrusher for JSON arrays, LogCompressor,
  CCR reversible elision, prefix-freeze cache protection, verbosity/effort
  shaping) is sound engineering, Apache-2.0, and explicitly usable without
  any ML model. Its fleet telemetry is honest: **median savings 4.8%**;
  40–80% only on sessions dominated by giant JSON tool payloads.
- That 40–80% band does **not** transfer to koryph: 70% of our tool bytes
  are Read output — source code, which headroom's own defaults refuse to
  compress (correctly), and which its default tool exclusions
  (Read/Glob/Grep/Write/Edit) never touch. Our honestly compressible class
  is Bash output (~18% of content bytes): gate logs, `go test`, lint spew.
  Best-case interception value is **~10–15% of quota burn**.
- The single largest risk is not engineering but **enforcement**: Anthropic
  actively fingerprints Claude Code traffic body-content-side on
  subscription accounts (system-field classification; unwarned bans;
  policy tightened repeatedly in H1 2026). Rewriting request bodies on a
  subscription account is unadjudicated territory. koryph is
  subscription-first by architecture — this risk class is existential, so
  interception must be evidence-gated and minimal, never the default.

Conclusion: capture the cheap, zero-risk savings **natively first** (they are
mostly pre-transcript: quieter tools, slimmer prefixes, capped outputs), build
the backend-agnostic seam + measurement to make any interception attributable
and reversible, and gate native interception on evidence.

## 2. Invariants (the correctness contract)

- **I1 — fail-open means bypass.** Any proxy failure routes dispatches
  *directly to Anthropic* — never a half-working rewriter. Exception: a
  request that only fits the context window *because* of compression must
  never be passed through raw (compress-or-fail-closed). Credential/
  authorization errors (401/403) from upstream durably disable the proxy
  for that account and alert; they never retry-loop.
- **I2 — first-write-only compression.** Content is compressed only on its
  first upstream appearance, before it enters the cached prefix. A
  previously sent prefix is never rewritten: at Anthropic rates
  (read 0.1×, write 1.25×) busting a P-token prefix breaks even in one
  turn only if savings exceed 0.92·P — effectively never. Any bust must be
  justified by an explicit net-expected-USD model; the default is never.
- **I3 — lossy requires reversibility; some classes are untouchable.**
  Every lossy reduction persists the full original under the agent's phase
  dir and cites the path (the agent recovers it with Read). Source code,
  error output, sub-threshold content, and secret-shaped values (per
  `internal/obs` redaction shapes) are never compressed or stored.
- **I4 — transport fidelity.** Loopback-only listeners; auth headers and
  the `system` field pass through byte-identical; interception is limited
  to allowlisted, fully-parsed `tool_result` content on `POST
  /v1/messages`; every other path/method/block type forwards verbatim.
- **I5 — measure first, attribute always.** Token telemetry lands before
  any optimization. Optimization experiments run with a holdout fraction
  and quality tripwires (requeue rate, blocking-review findings, gate
  failures). Estimator/calibration state is segmented by proxy identity so
  populations never pollute each other; flipping a project's proxy config
  marks its account calibration stale.
- **I6 — operator sovereignty.** Everything here is opt-in per project,
  env-configured (no tool ever mutates `.claude/settings*.json` besides
  `koryph rules install`), and removable with zero residue.
- **I7 — determinism.** Same input bytes → same compressed bytes, across
  turns and across process restarts, with the transform version pinned per
  session. Claude Code resends full history every turn; a nondeterministic
  transform converts 90%-discounted cache reads into 1.25× writes and
  becomes a quota *multiplier*. A per-session cache-hit-ratio tripwire in
  telemetry detects this class of failure at runtime.

## 3. Design

### L1 — Token telemetry foundation

Persist per-slot token composition (input, output, cache_read,
cache_creation) alongside the existing `CostUSD`: parse the stream-json
`result` line's usage block where present (extend
`internal/runtime/claude/events.go`), fall back to the transcript-scan
plumbing that `internal/quota/usage.go` already has. New ledger fields; a
`koryph metrics tokens` view (per bead, per tier:size, cache-hit ratio,
tokens-per-bead trend); the I7 cache-ratio tripwire (WARN when a session's
cache_read share collapses mid-run). Add the estimator-segmentation seam
now: `quota.Record`'s calibration key gains the slot's proxy identity
(empty today) so §L5 experiments can't pollute the bias/MAPE EWMAs — a
one-line key change now, an ugly state migration later.

### L2 — Prefix hygiene (the fixed cost every turn re-reads)

- **bd-prime slimming**: the SessionStart hook currently injects
  `bd prime --hook-json` (~20 KB) into every session. Replace the direct
  call with a koryph-owned wrapper that (a) measures and logs the injected
  size, (b) emits a slim profile for secondary spawns (reviewers, stages,
  epic validators need no bead-workflow tutorial), (c) caches within a
  run. Dispatch-type awareness comes from the koryph contract env vars
  already exported in launch.sh.
- **Byte-budget audit**: CLAUDE.md (5.8 KB), AGENTS.md (3.7 KB), persona
  files — measure, then trim to contract-only where the user guide already
  carries the prose. CLAUDE.md's own header states the policy ("keep this
  file small and stable").
- **PromptCachePolicy**: the registry field is stamped into manifests but
  read by nothing. Wire it to mean "opt out of L2/L4 shaping for this
  project" or delete it; a dead policy knob is worse than none.

### L3 — Bash output economy (the measured biggest native lever)

Bash tool results are ~18% of content bytes and the only class that is
both large and safely reducible — and koryph controls both ends of the
pipe *inside* the session, no API interception required:

- **Quiet gate**: an agent-facing `make gate-agent` (or `GATE_AGENT=1`)
  target that passes quiet flags through the pipeline (`go test` without
  `-v`, summarized lint output, full logs tee'd to `$KORYPH_PHASE_DIR`),
  plus an output-budget audit of the existing gate. The engine preamble
  and AGENTS.md teach agents to prefer it.
- **Summarizer wrappers with file-spill (native CCR)**: koryph-shipped
  wrappers for the known offenders (gate, `go test`, lint) that print the
  summary + tail, write the **full untruncated output** to the phase dir,
  and end with `full output: <path>` — reversible via the agent's own
  Read tool. No proxy, no injected tools, no TTL store, no interception.
  This is strictly simpler than headroom's CCR because koryph agents have
  a filesystem; API-side elision exists for clients that don't.
- **Verbose-command policy**: extend the existing PreToolUse Bash guard to
  nudge (deny-with-message pointing at the quiet target) the worst
  patterns (`go test ./... -v`, unfiltered `golangci-lint run`).
- **Output caps as first-class env**: thread the Claude Code output-cap
  env knobs (`BASH_MAX_OUTPUT_LENGTH`-class; the bead verifies current
  names against CLI docs) through `account.ChildEnvSpec` as typed fields —
  not `env_passthrough` entries — so all four spawn sites get them
  uniformly and the allowlist discipline stays single-point.

### L4 — Output-side economy (subscription-safe by construction)

Assistant prose is only 2.1% of content bytes, so expectations are modest,
but the levers are free: a byte-stable terse-output contract block in the
**koryph-authored prompt** (engine preamble §1 — user-turn content; the
`system` field is never touched, so this is enforcement-safe), and an
effort/model routing audit for secondary spawns (stages and validators at
`--effort low` where quality signals permit; the persona `tier:`/`effort:`
plumbing already exists).

### L5 — Agent proxy seam (backend-agnostic, ships dark)

A per-project registry block:

```json
"agent_proxy": { "base_url": "http://127.0.0.1:8787", "health": "/health", "pin": "headroom-ai==0.30.0" }
```

- Injection happens as a **first-class `ChildEnvSpec` field** (like
  `APIKey`/`SSHAuthSock`) inside `account.ChildEnv` — the single sanctioned
  `ANTHROPIC_BASE_URL` source — applied uniformly to all four spawn sites
  (main dispatch, review, stage, epicreview). This also fixes the existing
  gap where `env_passthrough` reaches main dispatches only.
- Every slot's ledger row stamps the proxy identity+pin (empty = direct);
  L1's estimator segmentation keys on it.
- Doctor checks: health endpoint, loopback bind verification, positive
  routing verification (compare upstream-seen vs direct request counts),
  pin match — refuse-to-route on mismatch.
- Flipping `agent_proxy` marks the account's quota calibration stale
  (`koryph quota calibrate` re-run prompted): the ccusage$↔/usage% slope
  is not proven invariant under compression.

### L6 — Measurement harness (the go/no-go machine)

A `--proxy-holdout <fraction>` dispatch mode: the fraction bypasses the
proxy; the report (`koryph metrics tokens --experiment`) compares
tokens-per-bead, cache-hit ratio, requeue rate, blocking-review-finding
rate, and gate failures between arms, plus the calibration-slope check.
The holdout stays on permanently while any proxy is configured — it is a
standing canary, not a one-time experiment.

### L7 — Experimental headroom integration (docs-only, risk-flagged)

A user-guide chapter, not code: exact-version pipx install matching the
registry pin; **env-only** configuration through L5 (never
`headroom wrap`, which would fight `koryph rules install` for
`.claude/settings.local.json`); heuristics-only extras (no `[ml]`);
`HEADROOM_MODE=cache`; output shaper **off** (it appends system blocks —
the exact surface Anthropic's subscription enforcement classifies);
telemetry off, verified. Documented hazards: the enforcement risk itself
(recommend a disposable/canary account first), the 1M-context beta header
regression behind custom base URLs, the oversize/auto-compaction
interaction (a compression-dependent context overflow dies on breaker
passthrough), and version churn (~2-day release cadence — never unpinned).
This chapter is the experiment vehicle that produces L6's evidence.

## 4. What we deliberately do NOT build now

A native Go interception proxy (`koryph proxy`) is **not** scheduled. The
measured ceiling (~10–15%), the enforcement risk class, the I7 perpetual
byte-stability tax, and the Anthropic-API-tracking burden all argue it
must earn its way in. A `decision` bead records the tripwires:

- **Build** if L6 shows ≥15% sustained per-bead quota reduction with no
  quality regression across ≥4 weeks of holdout data, AND the enforcement
  climate is clarified (Anthropic guidance, or sustained incident-free
  community precedent for body-rewriting proxies on subscriptions).
- **Pre-agreed minimal scope** (from the ops review): transparent reverse
  proxy, `POST /v1/messages` interception only, suffix-only (post-last-
  cache-breakpoint) LogCompressor for allowlisted Bash `tool_result`
  blocks, file-spill reversibility, deterministic per I7, strict
  unknown-shape passthrough. **Explicitly cut**: SmartCrusher parity port
  (our JSON class is immaterial; a 50-line lossless compaction suffices if
  that changes), CCR store/tool interception (file-spill supersedes it),
  output shaping (system-field risk), engine-managed proxy lifecycle
  (operator-managed via L5 wins; at most `koryph proxy serve` standalone).
- **Abandon** headroom integration if it is abandoned upstream or an
  enforcement incident is credibly attributed to body rewriting; L3/L4
  native levers are unaffected either way.

## 5. Compatibility

| surface | behavior |
|---|---|
| default (no config) | zero change: no proxy env, hooks unchanged except measured bd-prime wrapper, gate output unchanged for humans (`make gate` intact; agents get the quiet variant) |
| work profiles (`CLAUDE_CONFIG_DIR`) | L5 injection composes; ChildEnvSpec remains the single env authority |
| api-key billing mode | L5 co-exists (base URL + key are orthogonal); noted in docs as the enforcement-safe arm for interception experiments |
| `--resume --fork-session` | resume replays local uncompressed history; safe under I7 determinism; file-spill paths remain valid within the phase dir lifetime (`internal/gc` retention applies) |
| runtime contract (koryph-v8u) | `agent_proxy` + env knobs live in the runtime-agnostic spec; only the claude adapter maps them today |
| protected paths | koryph.project.json / Makefile / hooks edits routed refactor-core or orchestrator-applied per CLAUDE.md |

## 6. Testing

- L1: golden stream.jsonl fixtures (result-line usage variants, absent
  usage → transcript fallback); ledger round-trip; tripwire unit tests.
- L2: hook wrapper emits byte-identical bd-prime for main dispatches by
  default (golden), slim profile goldens for secondary spawns; size log.
- L3: wrapper goldens (summary + spill path; spill file byte-equals raw
  output); PreToolUse policy table tests; gate-agent produces identical
  pass/fail verdicts to gate on seeded failures (the summarizer must never
  eat a failure — error lines are verbatim per I3).
- L5: env construction table tests across all four spawn sites × billing
  modes × work profiles; doctor check units; fail-closed on pin mismatch.
- L6: experiment report on fixture ledgers; segmentation keys proven
  disjoint in estimator tests.
- e2e: one self-build canary bead run with quiet-gate + wrappers enabled,
  comparing L1 metrics against the pre-change baseline.

## 7. Sequencing (the epics' children)

**Epic T — token telemetry + native context economy** (all zero
enforcement risk; T1 first per the seam-first law, the rest parallel):

T1 telemetry foundation (ledger fields, events parse, metrics CLI,
estimator segmentation seam, tripwire — refactor-core: engine/quota loop)
→ T2 TUI/cockpit token dashboards ∥ T3 bd-prime wrapper + prefix budget
audit ∥ T4 quiet gate + verbose-command policy (Makefile edits
orchestrator-applied) ∥ T5 summarizer wrappers + file-spill contract +
ChildEnvSpec output caps (refactor-core: account/dispatch) ∥ T6 terse
contract + secondary-spawn effort audit → T7 user-guide chapter
("context economy") + governor-docs drift fix.

**Epic P — proxy seam + evidence harness** (blocked by T1):

P1 agent_proxy registry schema + ChildEnvSpec injection across four spawn
sites + ledger stamp (refactor-core) → P2 doctor checks + calibration-
stale wiring → P3 holdout harness + experiment report → P4 headroom
integration chapter (risk-flagged; blocked by P1–P3).

**D1** — `decision` bead: native `koryph proxy` tripwires + pre-agreed
minimal scope (§4). Never dispatched; revisited when L6 data exists.

## 8. Risks

- **Enforcement (existential class)**: mitigated by ordering — native
  levers carry none; interception is opt-in, documented as experimental,
  system-field-untouched, canary-account-first. Residual risk is the
  operator's informed choice.
- **Savings disappoint**: the measured ceiling already says interception
  is marginal; the design's center of gravity (L2/L3) does not depend on
  headroom at all. If L6 shows <5%, D1 closes as won't-do and L7's chapter
  gains a "not recommended" verdict — the seam remains as generic plumbing.
- **Summarizers eat a signal an agent needed**: file-spill + verbatim
  error preservation + the gate-verdict-equivalence test bound this; the
  wrapper is also trivially bypassable by the agent (it can re-run the raw
  command), which is the correct failure direction.
- **bd prime drift**: the wrapper shells out to `bd prime` and filters —
  it must fail-open to the full output on any parse surprise (I1 spirit).
- **Quota-slope assumption**: calibration-stale-on-flip plus the L6 slope
  check convert a silent governor bias into a prompted recalibration.

## Beads (filed 2026-07-06)

| Epic | ID | Children |
|---|---|---|
| Token economy: telemetry + native context reduction | koryph-77r | .1 telemetry capture (refactor-core), .2 metrics tokens CLI, .3 TUI/cockpit dashboards, .4 bd-prime wrapper (refactor-core), .5 quiet gate (refactor-core), .6 wrappers+file-spill+env caps (refactor-core), .7 teaching pass, .8 effort/model audit (refactor-core), .9 docs chapter + governor drift fix, .10 budget-kill classification + warm-resume requeue (refactor-core; follow-up 2026-07-06, usage-credits impact review) |
| Agent proxy seam + interception evidence harness | koryph-3l1 | .1 seam+ChildEnvSpec+ledger stamp (refactor-core), .2 doctor+calibration-stale, .3 holdout harness (refactor-core), .4 headroom chapter |
| (standalone) | koryph-bvz | `decision` — native-proxy tripwires + pre-agreed minimal scope (§4); blocked by koryph-3l1.3 |

Plan scored 84/100 (koryph-plan-scorer, one iteration applied: 77r.6→3l1.1
ChildEnvSpec ordering edge; 3l1.1 footprint reconciled to single-point
`fp:go:account`). 7 of 13 children are refactor-core — orchestrator-authored
on main; peak loop-dispatched width ≈3 ({.2, .3, .7} after .1/.5/.6 land).
