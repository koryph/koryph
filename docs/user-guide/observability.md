<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Observability

koryph emits structured telemetry — logs, traces, and metrics — so you can
understand what is happening in a running wave without reading raw log files.
All signals are written locally under `~/.koryph/telemetry/` by default; **no
data leaves the machine** unless you explicitly configure an OTLP endpoint.

## Quick start

```sh
# See current configuration
koryph obs status

# Raise the govern component to trace for a troubleshoot session
koryph obs level govern trace

# Watch the telemetry stream live (Ctrl-C to stop)
koryph obs tail --component govern --follow

# Restore the level when done
koryph obs level govern info

# Bundle one run's telemetry for offline analysis
koryph obs export --run <run-id> --output /tmp/run.jsonl
```

## Signals

koryph emits three signal types, all sharing the same JSONL schema:

| Signal | Description |
|--------|-------------|
| **Logs** | Structured records from every subsystem (engine, scheduler, governor, …) |
| **Spans** | Sub-operation timing: API calls, vault fetches, stage durations |
| **Events** | Discrete lifecycle transitions: dispatch, requeue, merge, block |

Every record includes `time`, `level`, `msg`, and `component` fields.
Run-scoped records also carry `run_id` and `project`. Bead-scoped records
carry `bead_id`.  These standard fields make all three signal types
filterable by the same jq/duckdb queries.

## Telemetry files

Records are written as JSONL under `~/.koryph/telemetry/`.  Each day starts
a new file named `koryph-YYYYMMDD.jsonl`.  When the active file reaches the
size cap (default 50 MiB) it is renamed to `koryph-YYYYMMDD-HHmmss.jsonl`
and a fresh daily file is started.

```
~/.koryph/telemetry/
  koryph-20260704.jsonl          ← today's active file
  koryph-20260703.jsonl          ← yesterday
  koryph-20260703-183005.jsonl   ← mid-day rotation from 18:30:05 UTC
```

Files are written with `0600` permissions (readable only by you).

### Retention

Files older than the retention window (default 30 days) can be removed with:

```sh
koryph obs prune
koryph obs prune --dry-run    # show what would be removed
```

`koryph doctor` warns when files are oversized or stale so you are notified
before disk space becomes an issue.

## Configuration file

`~/.koryph/observability.json` controls all behaviour. Live loops re-read it
at every scheduler tick — no restart is needed. Example:

```json
{
  "default_level": "info",
  "format": "text",
  "telemetry_max_size_mb": 100,
  "telemetry_retention_days": 60,
  "components": {
    "engine": "debug",
    "govern": "trace"
  }
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `default_level` | `info` | Global minimum level for components with no override |
| `format` | `text` | Log format for the console: `text` (human-readable) or `json` |
| `otel_endpoint` | _(not set)_ | OTLP/HTTP endpoint for remote forwarding (e.g. `localhost:4318`) |
| `file` | _(not set)_ | Additional JSON log file path (alongside the console) |
| `telemetry_max_size_mb` | `50` | Maximum size in MiB before a JSONL file is rotated |
| `telemetry_retention_days` | `30` | Days to keep rotated JSONL files (`koryph obs prune` applies this) |
| `components` | `{}` | Per-component level overrides (see Components below) |

**Levels** (coarsest → finest): `error`, `warn`, `info`, `debug`, `trace`

## Environment overrides

Environment variables take precedence over the config file and are useful for
one-off runs without editing the file:

| Variable | Overrides |
|----------|-----------|
| `KORYPH_LOG_LEVEL` | `default_level` |
| `KORYPH_LOG_FORMAT` | `format` |
| `KORYPH_OTEL_ENDPOINT` | `otel_endpoint` |

## Components

Each koryph subsystem has an independent log level. Setting a component to
`trace` or `debug` without raising the global level keeps other subsystems quiet.

| Component | Description |
|-----------|-------------|
| `engine` | Wave loop orchestration |
| `sched` | Scheduler / wave planner |
| `govern` | Concurrency governor (slots, leases, AIMD) |
| `quota` | Quota governor (billing, window tracking) |
| `dispatch` | Agent dispatch |
| `merge` | Branch merge and land |
| `forge` | LLM API client |
| `bot` | Release-bot lifecycle |
| `signing` | Commit-signing / vault interactions |

## `koryph obs` commands

### `koryph obs status`

Print the current configuration and telemetry directory state.

```
koryph obs status [--json]
```

### `koryph obs level <component> <level>`

Set the log level for a named component, or change the global default:

```sh
koryph obs level engine debug
koryph obs level govern trace
koryph obs level default warn
```

The config file is written atomically; live loops pick up the change at the
next tick (within one scheduler cycle, usually under a second).

### `koryph obs enable`

Restore observability after a `disable`. If the default level was set to
`error`, it is raised back to `info`. If it was already at a permissive level,
this is a no-op on the level (it always re-saves the config).

```sh
koryph obs enable
```

### `koryph obs disable`

Silence all observability output by setting every level to `error`. Useful when
debugging a performance-sensitive loop where log volume is the issue.

```sh
koryph obs disable
```

### `koryph obs tail`

Render the JSONL telemetry stream in human-readable form. By default shows the
last 40 records; `--follow` streams new records as they arrive.

```sh
koryph obs tail                          # last 40 records
koryph obs tail -n 100                   # last 100 records
koryph obs tail --component govern       # filter to govern component
koryph obs tail --level warn             # only warn and above
koryph obs tail --component govern --follow   # live stream; Ctrl-C to stop
```

| Flag | Default | Description |
|------|---------|-------------|
| `--component C` | _(all)_ | Show only records from this component |
| `-n N` | `40` | Number of trailing records (0 = all) |
| `--follow` | off | Stream new records in real time |
| `--level L` | _(all)_ | Minimum level to display |

### `koryph obs export`

Bundle one run's telemetry as redaction-verified JSONL.  The export re-applies
the full redaction layer to every record before writing — suitable for sharing
with a colleague or feeding into an external tool.

```sh
koryph obs export --run <run-id>                         # write to stdout
koryph obs export --run <run-id> --output /tmp/run.jsonl # write to a file
```

| Flag | Required | Description |
|------|----------|-------------|
| `--run ID` | yes | The run_id to filter for |
| `--output FILE` | no | Write JSONL to this path instead of stdout |

### `koryph obs prune`

Remove telemetry files older than the retention window configured in
`observability.json` (default 30 days).

```sh
koryph obs prune            # remove stale files
koryph obs prune --dry-run  # show what would be removed
```

## Troubleshooting a running loop

### Liveness heartbeat

Every run emits a single `engine` INFO line roughly once a minute, whether or
not anything else happened that tick:

```
engine alive: 2 active, 5 ready, wave 3, last action dispatched koryph-abc 12s ago
```

This is deliberately quiet-hours-friendly — one line per interval, no matter
how idle the loop is — so a wedged loop is distinguishable from a genuinely
quiet one from logs alone: a healthy loop's `last action ... ago` stays small
relative to your workload's cadence, while a wedged loop's keeps growing
without bound even though the heartbeat itself keeps ticking. The interval
defaults to 60s and is overridable for troubleshooting via
`KORYPH_HEARTBEAT_SEC` (seconds).

Two known silent-wait sites also self-report if they run unusually long:

- A subprocess (`bd`, `git`, `gh`, `make gate`, a dispatched agent's CLI)
  still running past ~30s logs `execx: still waiting on <cmd> <args> ...`.
- A blocked ledger reclaim-guard lock wait past ~30s logs
  `ledger: still waiting on lock guard <path> ...`.

Both are one-shot: a normal-latency call never logs anything.

### Standard diagnostic flow

```sh
# 1. Raise verbosity on the suspect component
koryph obs level govern trace

# 2. Watch the telemetry stream
koryph obs tail --component govern --follow

# 3. Diagnose from the trace output

# 4. Restore to avoid log volume accumulation
koryph obs level govern info
```

This entire flow is available as a CLI sequence that agents can run
autonomously — see the `/koryph-ops` skill's TROUBLESHOOT section.

### Diagnosing parallelism ceilings

When the fleet seems slower than expected, check whether scheduling or
throttling is the limit:

```sh
# All deferrals, grouped by blocking token
koryph obs export --run <run-id> | \
  jq -r 'select(.msg == "engine.bead.deferred") | .deferral_token' | \
  sort | uniq -c | sort -rn | head -10

# Co-dispatch gauge over time (how many slots ran simultaneously)
koryph obs export --run <run-id> | \
  jq 'select(.msg == "engine.co_dispatch") | {time, co_dispatch, width}'
```

See [Self-hosting: how koryph builds koryph](../developer-guide/self-hosting.md)
for a worked example of this query pattern.

## Slice-and-dice with jq

The JSONL format is designed for jq and duckdb.  Some useful starting points:

```sh
# All ERROR records from the last run
koryph obs tail -n 0 | jq 'select(.level == "ERROR")'

# Dispatch timeline for a specific bead
cat ~/.koryph/telemetry/koryph-$(date +%Y%m%d).jsonl | \
  jq 'select(.bead_id == "koryph-jr8.6")'

# Slot merge latency histogram (ms)
koryph obs export --run <run-id> | \
  jq -r 'select(.msg == "engine.slot.merged") | .latency_ms' | \
  sort -n
```

## Slice-and-dice with DuckDB

DuckDB can query the JSONL files directly without any schema setup:

```sql
-- Load all telemetry from the last 7 days
SELECT time, level, msg, component, run_id
FROM read_ndjson_auto('~/.koryph/telemetry/koryph-2026070*.jsonl')
WHERE level IN ('WARN', 'ERROR')
ORDER BY time;

-- Deferrals by token (parallelism ceiling report)
SELECT deferral_token, COUNT(*) AS n
FROM read_ndjson_auto('~/.koryph/telemetry/*.jsonl')
WHERE msg = 'engine.bead.deferred'
GROUP BY deferral_token
ORDER BY n DESC;

-- Average slot merge latency per day
SELECT DATE_TRUNC('day', CAST(time AS TIMESTAMP)) AS day,
       AVG(latency_ms) AS avg_ms,
       COUNT(*) AS merges
FROM read_ndjson_auto('~/.koryph/telemetry/*.jsonl')
WHERE msg = 'engine.slot.merged'
GROUP BY 1 ORDER BY 1;
```

## OTLP collector setup (optional)

When `otel_endpoint` is set, koryph forwards records in OTLP/HTTP JSON
format to `{endpoint}/v1/logs` in addition to writing local JSONL files.
This enables integration with any OTLP-compatible backend (Grafana, Jaeger,
OpenObserve, …) without removing the local copy.

The endpoint should be an OTLP/HTTP receiver (default port **4318**, not the
gRPC port 4317):

```json
{
  "otel_endpoint": "http://localhost:4318"
}
```

Or with the env override for one-off runs:

```sh
KORYPH_OTEL_ENDPOINT=https://collector.internal:4318 koryph run --project myproject
```

**`https://` is required for any non-`localhost` endpoint.** A bare host
(no scheme) defaults to `http://` for `localhost`/`127.0.0.1`/`::1` and to
`https://` for everything else; an *explicit* `http://` for a non-local host
is rejected outright at startup (with a `koryph: WARN: obs: OTLP export
disabled: …` line) rather than silently sending log records — which can
carry account identity, bead IDs, and proxy diagnostics — in the clear.

A typical local collector stack (OTel Collector → Grafana Tempo):

```yaml
# otel-collector-config.yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: "0.0.0.0:4318"
exporters:
  otlphttp:
    endpoint: "http://grafana-tempo:4318"
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [otlphttp]
```

Records are batched in memory (up to 100 records or 5 s, whichever comes
first) and delivered in a background goroutine.  Delivery failures (marshal
errors, a non-2xx response, or a transport failure) do not stop the local
JSONL write — it remains the authoritative record — but each dropped batch
increments a process-wide counter and writes a `koryph: WARN: obs: OTLP
export to … dropped N record(s): …` line directly to stderr, so a failing or
misconfigured collector is no longer silent.

## doctor checks

`koryph doctor` includes three observability checks under the `obs` category:

| Check | Condition | Level |
|-------|-----------|-------|
| `obs config` | `observability.json` is valid JSON with recognised levels | error |
| `obs telemetry` | `~/.koryph/telemetry/` exists and is writable | warn (missing dir) |
| `obs rotation` | No JSONL file exceeds 50 MiB or 30 days old | warn |

```sh
koryph doctor
```

When `obs rotation` fires, run `koryph obs prune` to clean up stale files.

## Telemetry privacy

**No telemetry leaves the machine unless you explicitly configure
`otel_endpoint`.**  The local JSONL files are readable only by you (`0600`
permissions).  The redaction layer strips tokens, PEM blocks, Authorization
headers, and any field with a secret-suggestive name before writing any
record — secrets never appear in telemetry even at `trace` level.

`koryph obs export` re-applies the redaction layer to every record at export
time, so even records written before a redaction rule was added are safe to
share.
