#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# SessionStart wrapper around `bd prime --hook-json` (koryph-77r.4, design:
# docs/designs/2026-07-token-economy.md §3 L2). The bare command previously
# registered by internal/rules injected bd prime's full hook-json payload
# (~20-25KB on this repo) into EVERY session's fixed prefix, re-read every
# turn — including reviewer/stage/epic-validator sessions that never touch
# bead workflow at all. This wrapper:
#
#   1. Runs bd prime --hook-json, measures the byte size of what it emitted,
#      and logs it — NEVER onto this hook's own stdout (stdout IS the
#      injected SessionStart context; a log line there would corrupt it).
#      Appended to "$KORYPH_DIR/prime-size.log" when KORYPH_DIR is set
#      (the koryph dispatch contract's phase dir, internal/dispatch/
#      types.go), else written to stderr only.
#   2. Mode selection: any Koryph dispatch (KORYPH_PHASE_ID or
#      KORYPH_SPAWN_KIND set) receives no Beads context and never invokes bd.
#      Koryph owns task state and already injected the selected contract.
#      Interactive/operator sessions carry neither marker and receive the
#      full bd prime output.
#   3. Fails open (design invariant I1): if bd is missing, exits non-zero,
#      or emits something odd, whatever it produced (or nothing) is passed
#      through verbatim with exit 0 — a broken wrapper must never wedge
#      session start. This is unconditional in full mode: we do not attempt
#      to validate bd's output before relaying it, so a bd-side JSON bug
#      degrades no worse than the bare command already did.
#
# Log line format (one per invocation):
#   <ISO-8601 UTC timestamp> bytes=<n> mode=<full|suppressed-<kind>|no-bd|bd-error>
#
# Registered in .claude/settings.json (SessionStart, no matcher) via the
# same central ${KORYPH_HOME:-$HOME/.koryph}/hooks/ path pattern as the
# guard scripts (internal/rules/rules.go) — installs for free via
# scaffold.CopyEmbed alongside them, no install-list change needed.
set -uo pipefail

log_size() {
  # log_size <bytes> <mode>
  local line
  line="$(date -u +%Y-%m-%dT%H:%M:%SZ) bytes=$1 mode=$2"
  if [[ -n "${KORYPH_DIR:-}" ]]; then
    if mkdir -p "${KORYPH_DIR}" 2>/dev/null && printf '%s\n' "${line}" >>"${KORYPH_DIR}/prime-size.log" 2>/dev/null; then
      return 0
    fi
    # KORYPH_DIR set but unwritable — fall back to stderr rather than lose
    # the measurement, still never touching stdout.
  fi
  printf '%s\n' "${line}" >&2
}

# --- Dispatched mode: Beads are control-plane state -----------------------
if [[ -n "${KORYPH_PHASE_ID:-}" || -n "${KORYPH_SPAWN_KIND:-}" ]]; then
  kind="${KORYPH_SPAWN_KIND:-main}"
  log_size 0 "suppressed-${kind}"
  exit 0
fi

# --- Full mode (default) ---------------------------------------------------
# Fail-open: no bd on PATH at all -> nothing to inject, never block start.
if ! command -v bd >/dev/null 2>&1; then
  log_size 0 "no-bd"
  exit 0
fi

# Capture to a temp file (not command substitution) so byte-identity holds
# exactly, including any trailing newline bd prime emits — the same
# technique hooks/koryph-spill.sh uses for its combined-capture contract.
tmp="$(mktemp "${TMPDIR:-/tmp}/koryph-prime.XXXXXX")" || exit 0
trap 'rm -f "${tmp}"' EXIT

bd prime --hook-json >"${tmp}" 2>/dev/null
status=$?
bytes="$(wc -c <"${tmp}" | tr -d ' ')"

mode="full"
[[ ${status} -eq 0 ]] || mode="bd-error"

# Fail-open is unconditional here: whatever bd emitted (full success, a
# partial write before erroring, or nothing) is relayed as-is. A non-zero
# exit from bd is logged for visibility but never turned into a wedged
# session — this hook always exits 0.
cat "${tmp}"
log_size "${bytes}" "${mode}"
exit 0
