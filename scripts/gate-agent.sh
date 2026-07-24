#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# Agent-facing quiet gate (koryph-77r.5, design: docs/designs/
# 2026-07-token-economy.md §3 L3). Bash tool output is ~28% of agent
# transcript bytes; `make gate` itself (fmt-check, build, vet, test, lint,
# reuse) is the single biggest repeat offender. This script runs the exact
# same stage commands, in the exact same order, with the exact same
# fail-fast semantics as `make gate` — only the amount of *stdout* differs:
#
#   - Each stage's full raw output (stdout+stderr, untruncated) is teed
#     verbatim to "$log_dir/gate-<stage>.log".
#   - stdout gets one PASS/FAIL line per stage; on FAIL, a short tail of
#     that stage's log (so the actionable error still reaches the agent's
#     transcript) plus a pointer to the full log.
#   - Stops at the first failing stage, exactly like `make gate` stops at
#     the first failing prerequisite (verified: GNU Make does not run
#     later prerequisites once one fails).
#
# Verdict parity with `make gate`: this script exits 0 iff every stage
# passed, non-zero iff any stage failed — the same "pass vs fail" contract
# `make gate` provides. Note `make` itself always exits 2 on a failing
# prerequisite (never the child's exact code, verified empirically) so
# "verdict parity" is deliberately the zero/non-zero contract, not exact
# numeric exit-code equality; see scripts/gate_agent_test.go for the
# machine-checked property and the bead report for a real seeded-failure
# comparison (gofmt violation) run by hand against `make fmt-check` /
# `make gate` / `make gate-agent`.
#
# The summarizer must never eat a failure (design I3 spirit, §6 L3
# testing note): FAIL always shows real output (the tail) and always
# forwards a non-zero exit; nothing here can turn a failing stage into a
# reported PASS.
#
# Usage: scripts/gate-agent.sh <log-dir>
# Called by `make gate-agent` (see Makefile); GATE_LOG_DIR there resolves
# $KORYPH_PHASE_DIR / $KORYPH_DIR (the koryph dispatch contract's phase
# dir — internal/dispatch/types.go) or a repo-local scratch dir.  The
# identical bash-side resolution is in scripts/koryph-phase-dir.sh
# (koryph_resolve_log_dir), used directly by hooks/koryph-spill.sh.
set -uo pipefail

# Run from the repo root regardless of the caller's cwd (make normally
# invokes recipes from the Makefile's directory already, but this keeps the
# script correct if ever invoked directly from elsewhere).
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
cd "$repo_root" || exit 1

# Source the shared phase-dir/citation helper (koryph-qta.9).
# koryph_resolve_log_dir and koryph_cite_full_output are defined there.
# shellcheck source=scripts/koryph-phase-dir.sh
. "$script_dir/koryph-phase-dir.sh"

# The log dir is provided by the Makefile's GATE_LOG_DIR variable, which
# performs the KORYPH_PHASE_DIR → KORYPH_DIR → git-dir resolution in Make
# syntax (see Makefile line ~125).  The same resolution in bash is defined in
# koryph_resolve_log_dir above, used directly by hooks/koryph-spill.sh so
# both scripts share one definition.
log_dir="${1:?usage: gate-agent.sh <log-dir>}"
mkdir -p "$log_dir"

# Stage list: "<name>|<command>" pairs, run via `make <target>` so the real
# check logic (gofmt flags, vet, golangci config, reuse) lives in exactly
# one place — the Makefile — and never drifts from `make gate`. `lint` is
# replaced with `lint-agent`, the only stage where the *command itself*
# differs from `make gate` (it drops golangci-lint's inline source-snippet
# per issue via --output.text.print-issued-lines=false; same issues, same
# verdict, fewer bytes on a lint failure).
#
# KORYPH_GATE_AGENT_STAGES overrides this list (newline-separated
# "name|command" pairs) — a test seam for scripts/gate_agent_test.go, which
# seeds synthetic pass/fail commands instead of paying for two real go
# toolchain runs per test invocation. Unset in production use.
stages=()
if [[ -n "${KORYPH_GATE_AGENT_STAGES:-}" ]]; then
  while IFS= read -r line; do
    [[ -n "$line" ]] && stages+=("$line")
  done <<<"${KORYPH_GATE_AGENT_STAGES}"
else
  stages=(
    "fmt-check|make fmt-check"
    "build|make build"
    "vet|make vet"
    "test|make test"
    "lint|make lint-agent"
    "reuse|make reuse"
  )
fi

overall=0
test_fixture_dir=""
trap 'rm -rf "$test_fixture_dir"' EXIT

# run_test_stage executes the gate's package tests without the dispatch
# contract inherited by this wrapper.  Hook and Beads tests intentionally
# distinguish interactive work from an active dispatch, so passing the
# worker's phase metadata through would make them test the wrong mode (and
# can direct a leaked runner at the live Beads database).
#
# KORYPH_PHASE_DIR remains available to this parent process for gate logs;
# only the go test subprocess receives the neutral environment.  KORYPH_HOME
# and KORYPH_BD_BIN point at disposable fixtures so an unisolated test fails
# safely instead of observing an operator's real koryph home or Beads DB.
run_test_stage() {
  test_fixture_dir="$(mktemp -d "$log_dir/gate-test-env.XXXXXX")"
  mkdir -p "$test_fixture_dir/home"
  cat >"$test_fixture_dir/bd" <<'EOF'
#!/bin/sh
echo "gate-agent test fixture refuses bd invocation: $*" >&2
exit 97
EOF
  chmod 755 "$test_fixture_dir/bd"

  env \
    -u KORYPH_RUN_ID \
    -u KORYPH_SESSION_ID \
    -u KORYPH_PHASE_ID \
    -u KORYPH_SPAWN_KIND \
    -u KORYPH_PHASE_DIR \
    -u KORYPH_STATUS_PATH \
    -u KORYPH_SUMMARY_PATH \
    -u KORYPH_LOG_PATH \
    -u KORYPH_DIR \
    KORYPH_HOME="$test_fixture_dir/home" \
    KORYPH_BD_BIN="$test_fixture_dir/bd" \
    PATH="$test_fixture_dir:$PATH" \
    bash -c "$1"
}

for stage in "${stages[@]}"; do
  name="${stage%%|*}"
  cmd="${stage#*|}"
  log="$log_dir/gate-$name.log"
  stage_exit=0
  if [[ "$name" == "test" ]]; then
    run_test_stage "$cmd" >"$log" 2>&1 || stage_exit=$?
  else
    bash -c "$cmd" >"$log" 2>&1 || stage_exit=$?
  fi
  if [[ "$stage_exit" -eq 0 ]]; then
    echo "==> $name: PASS"
  else
    ec=$stage_exit
    echo "==> $name: FAIL (exit $ec)"
    echo "----- tail: $log -----"
    tail -n 40 "$log"
    echo "----- end tail -----"
    overall=$ec
    break
  fi
done

koryph_cite_full_output "$log_dir"
exit "$overall"
