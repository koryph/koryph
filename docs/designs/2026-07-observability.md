<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Observability: OTEL-instrumented koryph with dynamic verbosity (2026-07-04)

Status: approved for implementation; epic + children filed from §7.
Origin: operator direction — instrument the entire tool with OpenTelemetry
at multiple verbosity levels so users can study long-term how it works and
how to improve it; structured, fast, easy to slice; dynamically
configurable so agents can toggle it to troubleshoot a loop or the tool.

The self-hosting case study proved the thesis at grep-scale: koryph's
operational record answered "why isn't my fleet parallel?" in minutes.
This epic upgrades that record from log-lines-plus-grep to first-class
telemetry — same philosophy, industrial tooling.

## 1. Logger choice: slog API, pluggable speed

**Recommendation: `log/slog` as the ONLY logging API, with swappable
handlers.** Rationale:

- slog is the stdlib structured-logging interface (Go 1.21+); the Go
  ecosystem has converged on it. Choosing the API matters more than the
  backend, because handlers swap without touching call sites.
- **zap** remains the fastest mainstream backend and ships an official
  slog handler (`go.uber.org/zap/exp/zapslog`), so "slog frontend + zap
  backend" is the canonical high-performance pairing. **zerolog** edges
  zap in some microbenchmarks but has no first-party slog handler and a
  less idiomatic API — not worth the divergence.
- Honest sizing: koryph's hot path is *agent processes*, not log calls —
  the engine emits thousands of events per hour, not millions per second.
  slog's native `JSONHandler` is more than sufficient at that volume.
  **Default: slog + native handlers; `zapslog` wired behind the handler
  seam as the documented escape hatch** if profiling ever shows log cost.
  This is the "better option" answer: the seam, not a bet on one backend.

Levels: `ERROR / WARN / INFO / DEBUG / TRACE` (TRACE as a custom slog
level below Debug), settable **per component** (`engine`, `sched`,
`govern`, `quota`, `dispatch`, `merge`, `forge`, `bot`, `signing`, …).

## 2. The three signals

- **Logs** — every current progress/diagnostic line becomes a structured
  slog event. The console handler preserves today's human-readable output
  byte-compatibly at INFO (golden-tested) — nobody's terminal experience
  changes; the structure rides underneath.
- **Traces** — spans mirror the engine's real shape: `run` → `refill` →
  per-bead `slot` (child spans: `dispatch`, `poll`, `review`, `rebase`,
  `gate`, `merge`), plus `forge.api` and `vault.resolve` client spans.
  Requeues and cap changes are span events. One glance at a trace answers
  "where did this bead's hour go?"
- **Metrics** — the catalog the grep sessions kept re-deriving:
  dispatched-per-refill, deferrals **by blocking token**, requeues by
  reason, gate/review/merge duration histograms, per-bead cost,
  estimator bias/MAPE (koryph-6bl), governor cap + probe/breaker events,
  quota window burn, forge API latency/error counts.

**Canonical attributes on everything**: `run_id`, `project`, `bead_id`,
`attempt`, `provider`, `model`, `persona`. Cross-signal correlation is the
point — a WARN log links to its slot span which links to its cost metric.

## 3. Local-first export (ejectability applies to telemetry too)

Default: **OTLP-file JSONL** under `~/.koryph/telemetry/` (size-capped,
rotated, retention-pruned) — zero infrastructure, works offline, and
standard enough that `duckdb`/`jq` slice it directly. Optional:
`otlp_endpoint` config exports to any collector (Jaeger, Grafana, Datadog
— user's choice, koryph bundles nothing). `koryph obs export --run <id>`
bundles one run's telemetry into a shareable archive. No telemetry ever
leaves the machine unless the user configures an endpoint — restating the
no-SaaS invariant for observability explicitly.

## 4. Dynamic configuration (agents included)

- `~/.koryph/observability.json`: global + per-component levels, handler
  selection, sampling, endpoint. **Watched by live loops** (re-read at
  each scheduler tick — no restart needed).
- `koryph obs status | level <component> <level> | enable | disable |
  tail [--component X]` — the whole surface is CLI, so **agents can
  toggle it mid-troubleshoot** (an agent debugging a stuck loop runs
  `koryph obs level govern trace`, reads `koryph obs tail`, then restores).
  The ops skill teaches exactly that flow.
- Env overrides for one-shots: `KORYPH_LOG_LEVEL`, `KORYPH_LOG_FORMAT`,
  `KORYPH_OTEL_ENDPOINT`.

## 5. Redaction is a feature, not a hope

A central redaction layer scrubs known-secret shapes (tokens, PEM blocks,
Authorization headers, vault material) from every signal, with unit tests
that FAIL if a new field with a secret-suggestive name flows unredacted.
The vault/bot/forge components log references (`key_ref`, provider name),
never values. This inherits the dispatch allowlist philosophy: safe by
omission.

## 6. Relationship to existing records

`audit.jsonl` (append-only account/dispatch audit) and the run ledger stay
authoritative and unchanged — they are *records*, telemetry is *signal*.
The estimator work (koryph-6bl) publishes its bias/MAPE as metrics. The
self-hosting chapter gains a follow-up section once the deferral-by-token
metric replaces the grep.

## 7. Sequencing (the epic's children)

O1 foundation (internal/obs: slog seam, levels, dynamic config, redaction)
→ O2 engine/sched instrumentation (console goldens; spans; scheduler
metrics) ∥ O3 govern/quota/dispatch ∥ O4 forge/bot/vault (redaction-heavy)
→ O5 `koryph obs` CLI + ops-skill verbs + doctor check → O6 export/
rotation/retention + observability docs chapter. O2–O4 are disjoint by
the per-package footprints and can co-dispatch.
