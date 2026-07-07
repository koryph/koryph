#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# Generic file-spill summarizer wrapper (koryph-77r.6, design: docs/designs/
# 2026-07-token-economy.md §3 L3, invariant I3). Wraps ANY command: runs it,
# captures its full combined stdout+stderr byte-for-byte to a spill file
# under the koryph phase dir, and prints only a head+tail summary to the
# caller's own stdout — koryph's native answer to headroom-ai's CCR (no
# proxy, no injected tools, no TTL store; the agent recovers the full output
# with its own Read tool against the printed path).
#
# Usage: koryph-spill.sh <label> -- <command...>
#
#   <label>    a short slug identifying this invocation (e.g. "go-test",
#              "lint"); used only to name the spill file.
#   <command>  the command + args to run, exec'd directly (NOT through a
#              shell) — no quoting/injection surprises. Everything after the
#              literal "--" is passed through unchanged.
#
# Contract (I3 — lossy requires reversibility; see hooks/spill_test.go):
#   (a) the spill file is BYTE-IDENTICAL to the command's combined
#       stdout+stderr (redirected together into one file, the same
#       technique scripts/gate-agent.sh uses per stage).
#   (b) the wrapped command's exit code is preserved exactly.
#   (c) every line that looks like an error (case-insensitive "error",
#       "fail", "panic", "fatal" — mirroring I3's "error output ... never
#       compressed") is NEVER dropped from the printed summary, even when it
#       falls in the elided middle — such lines print verbatim under an
#       "errors:" section. The summarizer must never eat a failure signal.
#   (d) output already smaller than the head+tail budget prints UNMODIFIED,
#       with no spill note — nothing was elided, so there is nothing to
#       recover.
#
# Log dir resolution mirrors scripts/gate-agent.sh's GATE_LOG_DIR: koryph's
# dispatch phase dir (KORYPH_PHASE_DIR, the design doc's name, or KORYPH_DIR,
# the actual dispatch-contract env var — internal/dispatch/types.go) if set,
# else a repo-local scratch dir under the real git dir. Note the
# `git rev-parse --git-dir` fallback: in a koryph worktree, .git is a FILE
# (not a directory) pointing at the real gitdir, so a naive ".git" path
# would be wrong there. The spill filename increments (spill-<label>-<n>.log)
# so repeat invocations with the same label never clobber each other.
#
# Overrides (test/tuning seams):
#   KORYPH_SPILL_HEAD_LINES  (default 20) — lines printed from the start
#   KORYPH_SPILL_TAIL_LINES  (default 40) — lines printed from the end
set -uo pipefail

label="${1:?usage: koryph-spill.sh <label> -- <command...>}"
shift
if [[ "${1:-}" != "--" ]]; then
  echo "koryph-spill.sh: expected '--' before the command, got: ${1:-<none>}" >&2
  exit 64
fi
shift
if [[ $# -eq 0 ]]; then
  echo "koryph-spill.sh: no command given after '--'" >&2
  exit 64
fi

head_lines="${KORYPH_SPILL_HEAD_LINES:-20}"
tail_lines="${KORYPH_SPILL_TAIL_LINES:-40}"

log_dir="${KORYPH_PHASE_DIR:-${KORYPH_DIR:-}}"
if [[ -z "$log_dir" ]]; then
  git_dir="$(git rev-parse --git-dir 2>/dev/null || echo .git)"
  log_dir="$git_dir/koryph-spill"
fi
mkdir -p "$log_dir"

n=1
while [[ -e "$log_dir/spill-$label-$n.log" ]]; do
  n=$((n + 1))
done
spill_path="$log_dir/spill-$label-$n.log"

# Combined capture: both streams redirected to the same fd before exec, so
# the file holds byte-identical merged output (contract (a)). Not a live
# tee — matches gate-agent.sh's per-stage capture, which is likewise
# after-the-fact rather than streamed to the terminal.
"$@" >"$spill_path" 2>&1
exit_code=$?

total_lines=$(wc -l <"$spill_path" | tr -d ' ')
budget=$((head_lines + tail_lines))

if [[ "$total_lines" -le "$budget" ]]; then
  # Contract (d): nothing elided, so print as-is with no spill note.
  cat "$spill_path"
  exit "$exit_code"
fi

mid_start=$((head_lines + 1))
mid_end=$((total_lines - tail_lines))

head -n "$head_lines" "$spill_path"
echo "... [$((mid_end - mid_start + 1)) lines elided] ..."

# Contract (c): error-shaped lines in the elided middle still print verbatim.
errors="$(sed -n "${mid_start},${mid_end}p" "$spill_path" | grep -iE 'error|fail|panic|fatal' || true)"
if [[ -n "$errors" ]]; then
  echo "errors (from elided middle, verbatim):"
  printf '%s\n' "$errors"
fi

tail -n "$tail_lines" "$spill_path"
echo "full output: $spill_path"
exit "$exit_code"
