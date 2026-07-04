<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Observability

koryph emits structured telemetry — logs, traces, and metrics — so you can
understand what is happening in a running wave without reading raw log files.
All signals are written locally under `~/.koryph/telemetry/` by default; no
data leaves the machine unless you configure an OTLP endpoint.

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
```

## Configuration file

`~/.koryph/observability.json` controls all behaviour. Live loops re-read it
at every scheduler tick — no restart is needed. Example:

```json
{
  "default_level": "info",
  "format": "text",
  "components": {
    "engine": "debug",
    "govern": "trace"
  }
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `default_level` | `info` | Global minimum level for components with no override |
| `format` | `text` | Log format: `text` (human-readable) or `json` |
| `otel_endpoint` | _(not set)_ | gRPC OTLP endpoint (e.g. `localhost:4317`) for remote export |
| `file` | _(not set)_ | Additional JSON log file path (alongside the console) |
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

Telemetry files live under `~/.koryph/telemetry/` as size-capped, rotated
JSONL files. `koryph doctor` checks that the directory is writable and that
no files are oversized or older than 30 days.

## Troubleshooting a running loop

The recommended pattern when a loop behaves unexpectedly:

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

## doctor checks

`koryph doctor` includes three observability checks under the `obs` category:

| Check | Condition | Level |
|-------|-----------|-------|
| `obs config` | `observability.json` is valid JSON with recognised levels | error |
| `obs telemetry` | `~/.koryph/telemetry/` exists and is writable | warn (missing dir) |
| `obs rotation` | No JSONL file exceeds 50 MB or 30 days old | warn |

```sh
koryph doctor
```

## Telemetry privacy

No telemetry leaves the machine unless you explicitly configure `otel_endpoint`.
The local JSONL files are readable only by you (`0600` permissions). The
redaction layer strips tokens, PEM blocks, Authorization headers, and any field
with a secret-suggestive name before writing any record — secrets never appear
in telemetry even at `trace` level.
