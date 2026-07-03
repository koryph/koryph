#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# PreToolUse hook: defense-in-depth for autonomous / bypassPermissions dispatch.
# Enforces three invariants the permission globs can't fully express:
#   1. Edit/Write targets must resolve inside the project root (or a temp dir).
#   2. Bash commands can't `cd` to paths outside the project tree.
#   3. Bash commands are screened for prompt-injection / instruction-override
#      patterns that could subvert a dispatched agent.
# Exit 0 = allow; a JSON deny body + exit 2 = block the tool call.
#
# Register in .claude/settings.json (PreToolUse, matcher "Bash|Edit|Write"):
#   {"type":"command","command":"${KORYPH_HOME}/hooks/worktree-guard.sh"}
#
# Project tree root: CLAUDE_PROJECT_DIR, set by Claude Code for every hook
# invocation to the project's root — not this repo's root. This hook is
# shipped centrally but always evaluates boundaries against the *dispatched
# project*, never against the koryph checkout itself.

set -euo pipefail

input="$(cat)"

# Enforce only for koryph-dispatched (bypassPermissions) agents — they carry
# KORYPH_PHASE_ID. The main interactive session and normally-permissioned chat
# subagents are already governed by the allow/ask/deny matrix, and may legitimately
# touch sibling repos / additional working dirs. Set KORYPH_GUARD_ALL=1 to enforce
# everywhere.
[[ -n "${KORYPH_PHASE_ID:-}" || "${KORYPH_GUARD_ALL:-0}" == "1" ]] || exit 0

tool_name="$(jq -r '.tool_name // empty' <<<"${input}")"
command="$(jq -r '.tool_input.command // empty' <<<"${input}")"
file_path="$(jq -r '.tool_input.file_path // empty' <<<"${input}")"
project_dir="${CLAUDE_PROJECT_DIR:-$(pwd)}"

deny() {
  jq -n --arg r "$1" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
  exit 2
}

abspath() { case "$1" in /*) printf '%s' "$1" ;; *) printf '%s/%s' "${project_dir}" "$1" ;; esac }

inside_project() {
  local p
  p="$(abspath "$1")"
  case "${p}" in
    "${project_dir}" | "${project_dir}"/*) return 0 ;;
    # Sibling worktree tree + temp dirs are legitimate dispatch targets.
    "$(dirname "${project_dir}")"/*-worktrees/*) return 0 ;;
    /tmp/* | /private/tmp/* | /var/folders/*) return 0 ;;
    *) return 1 ;;
  esac
}

# --- Edit/Write: keep file writes inside the project / worktrees / temp --------
if [[ "${tool_name}" == "Edit" || "${tool_name}" == "Write" ]]; then
  if [[ -n "${file_path}" ]]; then
    [[ "${file_path}" == *".."* ]] && deny "file_path contains '..' traversal: ${file_path}"
    inside_project "${file_path}" || deny "file_path outside project tree: ${file_path}"
  fi
  exit 0
fi

[[ "${tool_name}" == "Bash" && -n "${command}" ]] || exit 0

# --- Bash: prompt-injection / instruction-override screen ---------------------
lower="$(printf '%s' "${command}" | tr '[:upper:]' '[:lower:]')"
injection_patterns=(
  'ignore[[:space:]]+(all[[:space:]]+)?previous[[:space:]]+instruction'
  'disregard[[:space:]]+(all[[:space:]]+)?(previous|prior)[[:space:]]+instruction'
  'forget[[:space:]]+(all[[:space:]]+)?(previous|prior)[[:space:]]+instruction'
  'you[[:space:]]+are[[:space:]]+now[[:space:]]+(a[[:space:]]+)?(new|different)'
  'new[[:space:]]+system[[:space:]]+prompt'
  'system[[:space:]]*:[[:space:]]*override'
  'jailbreak'
)
for pat in "${injection_patterns[@]}"; do
  [[ "${lower}" =~ ${pat} ]] && deny "command contains prompt-injection pattern (${pat})"
done

# --- Bash: block cd to a path outside the project tree -----------------------
if [[ "${command}" =~ (^|[[:space:]\;\&\|])cd[[:space:]]+(/[^[:space:]\"\';\&\|]+) ]]; then
  target="${BASH_REMATCH[2]}"
  inside_project "${target}" || deny "cd to path outside project tree: ${target}"
fi

# --- Bash: block redirection that writes to system paths ---------------------
if [[ "${command}" =~ \>[[:space:]]*(/etc/|/usr/|/bin/|/sbin/|/System/|/Library/|/var/) ]]; then
  deny "redirection writes to a system path: ${command}"
fi

exit 0
