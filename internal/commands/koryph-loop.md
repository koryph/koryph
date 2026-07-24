---
name: koryph-loop
description: Start the koryph loop in the background with a zero-token watcher that flags errors and stalls for intervention
---
<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Start the koryph loop for this project **in the background**, then arm a
**zero-token watcher**: the session consumes no tokens while the loop is
healthy and is woken only by an error/stall event line, at which point you
intervene using the playbook below. The loop joins the shared cross-project
governor — it does **not** get a private thread budget.

Optional arguments: $ARGUMENTS
Recognized: `max=<N>` (parallel cap for THIS project), `budget=<USD>`,
`auto-merge=<on|off>` (default `on`), `once` (single wave), `foreground`
(legacy: run inline and stream progress, no watcher).

Do not raise per-project parallelism casually — it is governed across all
projects on purpose. If `max=` is given, WARN first: a per-project override
bypasses the global concurrency governor and can breach shared rate/memory
budgets; the default exists for that reason.

## 1. Start the loop (background wrapper)

Resolve the project id from `koryph.project.json`. Write this wrapper to your
session scratchpad as `koryph-loop.sh` (fill `<id>`, `<repo-root>`, flags from
arguments), make it executable, and launch it with your background-task
mechanism (NOT nohup — keep it harness-tracked so you can stop it later):

```bash
#!/bin/bash
# koryph loop wrapper: one wave per iteration; --resume reconciles whatever
# the previous iteration left in flight (built-in crash/stall recovery path).
# Exit-code contract (internal/engine/types.go): OK=0 FATAL=1 USAGE=2 DRAINED=4.
# DRAINED means "no ready work right now" — expected steady state, NOT an error.
set -u
cd <repo-root> || exit 1
LOG="${1:?usage: koryph-loop.sh <logfile>}"
iter=0
while true; do
  # Stop sentinel: shutdown touches $LOG.stop instead of killing this script,
  # so a live `koryph run` child is never signalled by task teardown.
  if [ -f "$LOG.stop" ]; then
    echo "LOOP-STOPPED sentinel seen; exiting cleanly" >>"$LOG"
    exit 0
  fi
  iter=$((iter + 1))
  {
    echo "=== $(date -u +%Y-%m-%dT%H:%M:%SZ) iteration $iter start ==="
    koryph run --project <id> --once --auto-merge --review --resume
    status=$?
    echo "=== $(date -u +%Y-%m-%dT%H:%M:%SZ) iteration $iter exit=$status ==="
    case "$status" in
    0) : ;;
    4) echo "LOOP-DRAINED iteration=$iter" ;;
    *) echo "LOOP-ERROR iteration=$iter exit=$status" ;;
    esac
  } >>"$LOG" 2>&1
  case "$status" in
  4) sleep 60 ;;   # drained — back off
  0) sleep 10 ;;   # made progress — keep cycling
  *) sleep 30 ;;   # real error — avoid a crash loop; --resume recovers next pass
  esac
done
```

## 2. Arm the zero-token watcher

Write this as `koryph-loop-watch.sh` beside it (fill `<id>`, `LOG`), then arm
it with a persistent background monitor that notifies you per output line.
Every line it emits is something you should act on; it stays silent otherwise.
`LOOP-DRAINED` is deliberately NOT matched — that is the benign idle case.

```bash
#!/bin/bash
set -u
LOG=<logfile-from-step-1>
TELEM_DIR="$HOME/.koryph/telemetry"
REPO=<repo-root>

# A: crashes / real errors / gate failures in the loop's own output.
tail -F -n0 "$LOG" 2>/dev/null |
  grep -E --line-buffered 'panic:|FATAL|LOOP-ERROR|gate-failed|gate failed' &

# B: WARN/ERROR telemetry for this project (engine.slot.blocked/.conflict/
# .budget_killed/.model_fallback, patrol WARNs, hard ERRORs). Byte-offset
# tail; recomputes the filename each pass so midnight rollover is seamless.
(
  offset=0; last_file=""
  while true; do
    f="$TELEM_DIR/koryph-$(date +%Y%m%d).jsonl"
    [ "$f" != "$last_file" ] && { offset=0; last_file="$f"; }
    if [ -f "$f" ]; then
      size=$(wc -c <"$f" | tr -d ' ')
      if [ "$size" -gt "$offset" ]; then
        tail -c +"$((offset + 1))" "$f" |
          jq -c 'select(.project == "<id>" and (.level == "WARN" or .level == "ERROR"))' 2>/dev/null
        offset=$size
      fi
    fi
    sleep 5
  done
) &

# C: stall probe — a "running" slot whose agent pid is dead, or whose
# stream.jsonl (the ground-truth heartbeat; status.json can idle during long
# tool calls) has been silent >15 min. A live child means a gate/test may be
# running, so it is deliberately not called inert. One line per newly-stalled
# slot. The engine separately SIGTERMs a stale, childless agent and resumes it
# on the same tier; the WARN telemetry in B makes that recovery visible.
(
  while true; do
    koryph cockpit --project <id> --json 2>/dev/null |
      jq -c '.slots[]? | select(.stage=="running") | {bead_id, pid}' 2>/dev/null |
      while read -r s; do
        pid=$(echo "$s" | jq -r .pid); bead=$(echo "$s" | jq -r .bead_id)
        if [ -n "$pid" ] && [ "$pid" != "null" ] && ! kill -0 "$pid" 2>/dev/null; then
          echo "STALL dead-pid bead=$bead pid=$pid"
        elif [ -n "$pid" ] && [ "$pid" != "null" ] && ! pgrep -P "$pid" >/dev/null 2>&1; then
          st=$(find "$REPO/.plan-logs/koryph" -maxdepth 3 -path "*/$bead/stream.jsonl" -mmin +15 2>/dev/null | head -1)
          [ -n "$st" ] && echo "STALL stale-heartbeat bead=$bead pid=$pid (>15m quiet, no child)"
        fi
      done | sort -u
    sleep 60
  done
) &
wait
```

Smoke-test the watcher first (`timeout 7 bash koryph-loop-watch.sh` — expect
exit 124, no output), then arm it persistently. Do NOT poll the log yourself
afterward — that defeats the zero-token design.

## 3. Intervention playbook (when the watcher fires)

Diagnose from ground truth, then use the narrowest recovery. Full CLI
vocabulary: `/koryph-ops`.

- **`LOOP-ERROR exit=1`** — read the last iteration in the log. The wrapper
  already retries with `--resume`; intervene only if it repeats 3+ times.
  `koryph doctor --project <id>` for structural causes. A stale lock from a
  dead pid self-heals — koryph reclaims it on the next run.
- **`STALL dead-pid`** — the engine's dead-agent patrol usually reclassifies
  and re-dispatches on its own; give it one tick. If the bead's bd claim
  strands `in_progress` with no live slot: capture any dirty-worktree WIP
  first (`git -C <worktree> diff HEAD > <run-dir>/<bead>/wip-operator.patch`),
  then `bd update <bead> --status open` — the loop re-dispatches it.
- **`STALL stale-heartbeat`** — the engine automatically sends a graceful
  SIGTERM only after its process snapshot also finds no child process, then
  requeues the frozen same-tier session without charging an attempt. Check the
  matching `engine.slot.stale_heartbeat_recovery` WARN; intervene only if it
  repeats or the engine is not running. A live child running a long gate/test
  is slow, not stuck, and is deliberately left alone.
- **`engine.slot.blocked` (WARN)** — read the reason. `operator-stopped` →
  inject to re-arm (above). Drain-parked → expected during drain.
- **Ready work not dispatching / width starved** — the frontier verdict is
  computed at wave build; labels/beads changed mid-wave are invisible until
  the next refill (refill is event-driven on slot completion). Re-arm
  immediately with `koryph inject <bead> --project <id>` (operator-override
  sidecar; no lock needed against a live run).
- **Engine truly silent** (no log lines, no engine children, injections
  unapplied across multiple ticks) — SIGTERM the `koryph run` pid (plain
  kill, no --force). The wrapper relaunches with `--resume`, which cleanly
  reclassifies every slot; recovery is typically under two minutes.
- **`gate-failed`** — inspect the gate log in the bead's run dir. A flaky
  gate is retried once automatically; repeated deterministic failure means
  the bead's change is wrong — let escalation handle it or stop + fix.

## 4. Shutdown — order matters

**NEVER TaskStop/kill the wrapper task while a `koryph run` child is alive:**
task teardown signals the whole process group, which kills the engine AND its
agents abruptly — no ledger finalization, stranded bd claims, and status/TUI
showing a phantom running thread (learned 2026-07-22: the drain sentinel was
written but the engine died before ever reading it; there was no
`engine.run.end` in telemetry). The safe order:

1. `touch <logfile>.stop` — the wrapper exits at its next loop top instead of
   relaunching; no signal touches the engine.
2. `koryph drain --project <id>` — the live engine finishes active slots and
   exits on its own.
3. Wait for the engine to exit and **verify it drained rather than died**:
   the loop log must show an `engine.run.end` line for the final run, and
   `koryph status --project <id>` must show no `running` slots. A "running"
   slot with a dead engine means the exit was NOT graceful — recover per the
   playbook (capture WIP, `bd update --status open`, and reconcile/repair the
   ledger; `koryph ops reconcile` where available).
4. Only now stop the watcher monitor and (if still sleeping) the wrapper task.
5. Summarize what merged/failed during the run from the loop log.
