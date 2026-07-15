<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Context economy

Koryph's throughput is bounded by subscription quota windows. Every token an
agent spends on Bash dumps, gate logs, or bead-workflow tutorials is a token
it cannot spend on the actual work — and one fewer token of wave capacity
available for the next dispatch. This chapter describes the native levers
koryph uses to keep context lean.

> **Note:** All features on this page operate inside the session and never
> rewrite Anthropic API request bodies. They are safe for subscription billing.

---

## Token telemetry — `koryph metrics tokens`

Before optimizing, measure. `koryph metrics tokens` reads the ledger and
renders per-bead and per-tier token composition, cache-hit ratio, and a
tokens-per-bead trend.

```sh
koryph metrics tokens                        # table for all projects
koryph metrics tokens --project koryph       # one project
koryph metrics tokens --json                 # machine-readable
```

**Reading the table.** Three sections are shown:

1. **Project summary** — total beads with token data, mean tokens/bead,
   cache-hit ratio (`cache_read / (cache_read + input)`).
2. **Per-tier breakdown** — same columns split by model tier (haiku / sonnet /
   opus / fable). Useful for auditing whether secondary spawns are landing on
   the right tier.
3. **Per-bead detail** — one row per closed bead, newest first, showing
   input / cache_creation / cache_read / output token counts and the cache-hit
   ratio for that bead.

**The cache-hit ratio** is the key health metric. A healthy koryph fleet
typically runs at ≥ 90 % cache reads. A sudden drop in the ratio usually
indicates a context-bust: something caused the cached prompt prefix to be
rewritten, turning cheap cache reads into expensive cache writes. The engine's
[I7 cache-ratio tripwire](#the-cache-ratio-tripwire) logs a warning when this
happens on any session with material token volume.

**Older ledger entries** without token fields (dispatched before koryph-77r.1
landed) appear as zero and are excluded from the breakdown. Run the command
periodically after a few waves; the data accumulates automatically.

### The cache-ratio tripwire

The engine evaluates a separate ratio for each attempt:
`cache_read / (input + cache_read + cache_creation)`. When this falls below
the hard-coded floor of **0.50** on a session with at least **20 000** total
tokens (`input + cache_read + cache_creation`), the engine emits one
`slog.Warn` log record. This is **observability only** — it never changes
dispatch behavior, does not write to the ledger, and is not surfaced in
`koryph metrics tokens`. Check your structured log output (e.g.
`koryph run --log-level warn`) to catch these events.

The volume gate (20 000 tokens) prevents false positives on the first turn of
any dispatch, which has no established cache prefix yet. The per-attempt
evaluation (not accumulated totals) means a healthy early turn cannot mask a
later attempt's prefix collapse.

> **Note:** The tripwire ratio (`cache_read / (input + cache_read +
> cache_creation)`) differs from the `CacheHitRatio` metric in
> `koryph metrics tokens` (`cache_read / (cache_read + input)`), which
> omits `cache_creation` from the denominator. Both capture the same
> qualitative signal; the difference is small in practice.

---

## Trimming the fixed prompt prefix — `agent_mcp`

Every dispatched agent opens with a fixed context prefix — system prompt, tool
schemas, and any **ambient MCP-server instructions** the machine has
configured — that is re-read on *every* turn of the session. koryph implementer
personas use only file and Bash tools, so a full MCP suite in that prefix is
dead weight: it inflates the per-turn cache-read bill (see
[Token telemetry](#token-telemetry-koryph-metrics-tokens)) without ever being
called.

Set the registry field `agent_mcp` to `"strict"` (in
`~/.koryph/registry.d/<project-id>.json`) to pass `--strict-mcp-config` on the
implementer dispatch, so the agent loads **no** ambient MCP servers:

```json
{ "agent_mcp": "strict" }
```

The default (`""`/`"inherit"`) is unchanged behavior — dispatch argv is
byte-identical — so turning this on is an explicit, per-project opt-in. Leave it
unset for any project whose agents genuinely need an MCP server. See the
[registry record fields](projects-and-accounts.md#the-registry-record).

## Quiet gate — `make gate-agent`

`make gate` is the human-facing green gate. For agents, use `make gate-agent`
instead.

`gate-agent` runs the identical checks in the identical order with the
identical fail-fast semantics. The difference is its output:

- Each stage's **full raw output** (stdout + stderr, untruncated) is teed to
  `$GATE_LOG_DIR/gate-<stage>.log`.
- **stdout** emits one `PASS` or `FAIL` line per stage. On `FAIL`, a short
  tail (≤ 40 lines) of that stage's log is printed inline, plus a path to the
  full log.
- The script exits 0 iff all stages pass — the same zero/non-zero contract as
  `make gate`.

```sh
make gate-agent               # runs checks; full output at $GATE_LOG_DIR
GATE_LOG_DIR=/tmp/g make gate-agent   # override log dir
```

**Inside a koryph dispatch,** `$GATE_LOG_DIR` resolves to `$KORYPH_PHASE_DIR`
(the phase directory for the current bead run). Full stage logs are available
there via `Read` if you need to diagnose a failure.

**Lint output** is additionally quieted in `gate-agent` (via `make
lint-agent`) by passing `--output.text.print-issued-lines=false` to
golangci-lint. Findings are identical; the inline source-snippet bytes are
suppressed. Pass verdict equivalence — same issues, same exit code — is tested
in `scripts/gate_agent_test.go`.

> **Agents: prefer `make gate-agent` over `make gate`.** The summarizer
> pattern means failures are still surfaced inline; full context is one `Read`
> away via the log path.

---

## Wrappers and file-spill

The principle: run the command, write its complete untruncated output to a
file under the phase directory, emit a brief summary plus a `full output:
<path>` line to stdout.

This is koryph's native equivalent of headroom-ai's context-compressive
reversibility (CCR). No proxy, no injected tools, no TTL store, no API
interception — just a file-spill recoverable via the agent's own `Read` tool.

`make gate-agent` is the primary shipped wrapper. The pattern it follows:

```
==> fmt-check: PASS
==> build: PASS
==> vet: PASS
==> test: FAIL (exit 1)
----- tail: /path/to/phase/gate-test.log -----
--- FAIL: TestFoo (0.12s)
    foo_test.go:42: got bar, want baz
----- end tail -----
full output: /path/to/phase
```

### Recovering the full output

If the summary is insufficient, the full untruncated log is at the path shown:

```sh
# In an agent — read the spilled log directly:
# Read /path/shown/in/full-output/line/gate-test.log
```

The phase-directory logs are retained by `internal/gc` for the same duration
as the rest of the bead run artifacts (default: 7 days after bead close).

### Design invariant: summarizers never hide failures

Error lines are reproduced verbatim. A failing stage always results in a
non-zero exit. The wrapper cannot turn a failing gate into a reported PASS.
This is tested by `scripts/gate_agent_test.go`'s seeded-failure test, which
verifies that a gofmt violation surfaces identically in `make gate` and
`make gate-agent`.

---

## Output caps

Claude Code exposes two tool-output size knobs. Koryph injects conservative
defaults through `account.ChildEnvSpec` — a single point applied uniformly
to all four spawn sites (main dispatch, reviewer, stage, epic reviewer):

| Env var                  | Koryph default | Effect |
|--------------------------|----------------|--------|
| `BASH_MAX_OUTPUT_LENGTH` | 400 000 chars (~400 KB) | When a Bash `tool_result` exceeds this, Claude Code itself spills the full output to a temp file and hands the agent a path + short preview — Claude Code's own native file-spill (CCR). |
| `MAX_MCP_OUTPUT_TOKENS`  | 50 000 tokens  | MCP tool results are truncated at this token count. |

The defaults are deliberately conservative: large enough that ordinary command
output — including `make gate-agent`'s summary and 40-line failure tail — is
never touched; low enough to bound a pathological unbounded dump (e.g. an
agent inadvertently `cat`-ing a multi-GB file).

### Overriding for a specific project

Set `bash_max_output_length` and `max_mcp_output_tokens` under the project's
registry record (or leave unset to inherit the package defaults):

```json
{
  "project": "my-project",
  "bash_max_output_length": 200000,
  "max_mcp_output_tokens": 25000
}
```

A negative value in `ChildEnvSpec` omits the env var entirely, reverting to
Claude Code's own (effectively unbounded) behaviour. This is an explicit
opt-out escape hatch for projects with unusual tooling.

### Verbose-command guard

The engine's `PreToolUse` Bash guard nudges (deny-with-message) the worst
patterns for token inflation:

| Pattern | Guidance issued |
|---------|----------------|
| `go test ./... -v` | Use `make gate-agent`; full `-v` output is spilled to the phase log |
| `golangci-lint run` (without quiet flags) | Use `make lint-agent` (same findings, no inline source snippets) |

The guard points at the quiet target rather than silently blocking; agents can
always re-run the raw command when they need the full output.

---

## bd-prime slimming

`bd prime --hook-json` injects the full bead-workflow context into every
Claude Code session via the `SessionStart` hook. Full context (~20 KB) is
appropriate for main-dispatch agents working the bead queue. It is unnecessary
for secondary spawns (reviewers, stage workers, epic reviewers), which have no
bead-workflow responsibilities.

The `hooks/koryph-prime.sh` wrapper (shipped as part of `koryph rules
install`) implements dispatch-aware profile selection keyed on
`$KORYPH_SPAWN_KIND`:

| `$KORYPH_SPAWN_KIND`                         | Profile injected |
|----------------------------------------------|-----------------|
| unset (interactive / operator session)       | Full `bd prime --hook-json` output, byte-identical |
| `main` (primary dispatch agent)              | Full `bd prime --hook-json` output, byte-identical |
| `review`, `stage`, `epicreview`              | Slim profile (< 500 bytes): spawn kind + phase dir pointer; no bead-workflow tutorial |

The slim profile is a small JSON-shaped hook payload that identifies the
spawn kind and points at the phase directory. It omits all bead-workflow
tutorial content, reducing per-session prefix bytes for secondary spawns.

**Byte accounting.** The wrapper logs the injected byte count to
`$KORYPH_DIR/prime-size.log` (never to stdout, so it does not pollute the
session context). Each line records the spawn kind, the profile mode used, and
the byte size. Use this to audit how much prefix each spawn type is consuming.

### Recovering bead context from a secondary spawn

Secondary spawns that need bead context can still retrieve it:

```sh
bd show $KORYPH_BEAD_ID          # bead detail, including the plan
bd prime                         # full workflow context (without --hook-json)
```

`$KORYPH_BEAD_ID` is set by the engine on every dispatch.

---

## Agent guidance summary

| Practice | Why |
|----------|-----|
| Run `make gate-agent`, not `make gate` | One PASS/FAIL per stage; full logs in phase dir |
| Read spilled log paths, don't re-run with `-v` | The full output is already there |
| Use `make lint-agent` instead of `golangci-lint run` | Same findings; no inline source-snippet bytes |
| Check `koryph metrics tokens` after a wave | Catch cache-ratio collapses early |
| Avoid `go test ./... -v` in agent Bash calls | The PreToolUse guard will nudge you to the quiet target |
