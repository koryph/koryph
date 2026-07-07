#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# Shared shell library: phase-dir resolution + "full output:" citation helpers
# (koryph-qta.9, design: docs/designs/2026-07-token-economy.md §3 L3).
#
# This file is SOURCED, not executed.  Both scripts/gate-agent.sh and
# hooks/koryph-spill.sh had independently implemented the same two patterns:
#
#   1. Phase-dir resolution — KORYPH_PHASE_DIR → KORYPH_DIR →
#      "$(git rev-parse --git-dir)/<suffix>" (worktree-safe fallback: in a
#      koryph worktree .git is a pointer FILE, not a directory, so a naive
#      ".git/<suffix>" path resolves to the wrong tree; git rev-parse handles
#      it by following the pointer).
#
#   2. "full output: <path>" citation — the standard one-liner both scripts
#      emit so the calling agent can recover the full, untruncated output with
#      its Read tool.
#
# Usage:
#   . "$(dirname "${BASH_SOURCE[0]}")/koryph-phase-dir.sh"   # from scripts/
#   . "$(dirname "${BASH_SOURCE[0]}")/../scripts/koryph-phase-dir.sh"  # from hooks/

# Guard: refuse direct execution (this file defines functions, nothing else).
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  echo "koryph-phase-dir.sh: this file must be sourced, not executed directly" >&2
  exit 1
fi

# koryph_resolve_log_dir <suffix>
#
# Resolves the log/spill directory following the koryph dispatch contract:
#
#   KORYPH_PHASE_DIR  — design-doc name; set by newer engine versions
#   KORYPH_DIR        — actual dispatch-contract env var (internal/dispatch/types.go)
#   fallback          — "$(git rev-parse --git-dir)/<suffix>"
#
# The git rev-parse fallback is worktree-safe: in a koryph worktree .git is a
# pointer file, so git rev-parse --git-dir resolves to the real gitdir path
# (e.g. ../.git/worktrees/agent-foo) rather than the ambiguous literal ".git".
#
# Prints the resolved directory path to stdout.  Callers should mkdir -p the
# result before writing files.
koryph_resolve_log_dir() {
  local suffix="${1:?koryph_resolve_log_dir: suffix argument required}"
  local dir="${KORYPH_PHASE_DIR:-${KORYPH_DIR:-}}"
  if [[ -z "$dir" ]]; then
    local git_dir
    git_dir="$(git rev-parse --git-dir 2>/dev/null || echo .git)"
    dir="$git_dir/$suffix"
  fi
  printf '%s\n' "$dir"
}

# koryph_cite_full_output <path>
#
# Prints "full output: <path>" — the standard citation line that both
# gate-agent.sh and koryph-spill.sh emit at the end of their output so the
# calling agent can recover the full, untruncated log with its Read tool.
#
# gate-agent.sh passes its log dir (the agent recovers any stage log from
# there); koryph-spill.sh passes the specific spill file path.  Same wording,
# same contract, one definition.
koryph_cite_full_output() {
  printf 'full output: %s\n' "${1:?koryph_cite_full_output: path argument required}"
}
