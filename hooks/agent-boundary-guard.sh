#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# PreToolUse hook: deterministic boundary between a dispatched (headless)
# agent and the orchestrator. Some operations are the Koryph's job alone —
# merging, pushing, closing beads, landing on a protected branch — and must
# never happen from inside an agent's worktree, no matter what the prompt
# says. This hook is defense-in-depth *behind* the prompt contract
# (agents/implementer.md "Koryph protocol"), not a substitute for it.
#
# Enforcement gate: only active when KORYPH_PHASE_ID is set in the
# environment — i.e. only for agents the Koryph itself dispatched. An
# interactive main session (no KORYPH_PHASE_ID) is exempt and may run any
# of these commands; exit 0 immediately in that case.
#
# Denied operations (Bash tool only), matched per &&/;/|-separated segment,
# tolerant of leading whitespace and leading VAR=val env assignments:
#   git push · git merge · git checkout main · git checkout master ·
#   git switch main · git switch master · bd close · gh pr merge
# Explicitly ALLOWED and never matched: git rebase (including onto main —
# agents rebase their branch onto origin/main routinely), git commit.
#
# Decision output:
#   - jq available: emit the PreToolUse JSON deny body on stdout, exit 0.
#     (The JSON `permissionDecision` field itself IS the deny signal; Claude
#     Code does not require a non-zero exit for the structured form.)
#   - jq unavailable: fall back to the simple hook form — reason on stderr,
#     exit 2 (Claude Code's plain "blocked" exit code).
# Allow (no match, or not enforcing): exit 0, no stdout.
#
# Register in .claude/settings.json (PreToolUse, matcher "Bash"):
#   {"type":"command","command":"${KORYPH_HOME}/hooks/agent-boundary-guard.sh"}
#
# --- Inline test examples -----------------------------------------------
#
# 1. Not dispatched (KORYPH_PHASE_ID unset) → always allow, regardless of command.
#      $ echo '{"tool_name":"Bash","tool_input":{"command":"git push origin main"}}' \
#          | ./agent-boundary-guard.sh
#      → exit 0, no output
#
# 2. Dispatched, plain git push → denied.
#      $ echo '{"tool_name":"Bash","tool_input":{"command":"git push origin HEAD"}}' \
#          | KORYPH_PHASE_ID=phase-12a ./agent-boundary-guard.sh
#      → {"hookSpecificOutput":{...,"permissionDecision":"deny",...}}, exit 0
#
# 3. Dispatched, git commit → allowed (never matched).
#      $ echo '{"tool_name":"Bash","tool_input":{"command":"git commit -m \"feat: x\""}}' \
#          | KORYPH_PHASE_ID=phase-12a ./agent-boundary-guard.sh
#      → exit 0, no output
#
# 4. Dispatched, git rebase onto main → allowed (never matched, even onto main).
#      $ echo '{"tool_name":"Bash","tool_input":{"command":"git rebase origin/main"}}' \
#          | KORYPH_PHASE_ID=phase-12a ./agent-boundary-guard.sh
#      → exit 0, no output
#
# 5. Dispatched, chained command with the denied op mid-chain → denied.
#      $ echo '{"tool_input":{"command":"make test && git checkout main && rm -rf x"},"tool_name":"Bash"}' \
#          | KORYPH_PHASE_ID=phase-12a ./agent-boundary-guard.sh
#      → deny (git checkout main), exit 0
#
# 6. Dispatched, env-assignment-prefixed close → denied.
#      $ echo '{"tool_name":"Bash","tool_input":{"command":"GH_TOKEN=x bd close sg-42"}}' \
#          | KORYPH_PHASE_ID=phase-12a ./agent-boundary-guard.sh
#      → deny (bd close), exit 0
# ---------------------------------------------------------------------------

set -euo pipefail

# Enforce only for Koryph-dispatched agents (they carry KORYPH_PHASE_ID).
# Interactive / non-dispatched sessions are governed by the normal
# allow/ask/deny permission matrix and may legitimately push, merge, etc.
[[ -n "${KORYPH_PHASE_ID:-}" ]] || exit 0

input="$(cat)"

have_jq=0
command -v jq >/dev/null 2>&1 && have_jq=1

extract_field() {
  # extract_field <json-key path via jq> <grep field name>
  # $1: jq filter, $2: raw-field name for the grep fallback
  if [[ "${have_jq}" == "1" ]]; then
    jq -r "$1 // empty" <<<"${input}"
  else
    printf '%s' "${input}" \
      | grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" \
      | head -1 \
      | sed -E 's/.*:[[:space:]]*"(.*)"$/\1/'
  fi
}

tool_name="$(extract_field '.tool_name' 'tool_name')"
raw_command="$(extract_field '.tool_input.command' 'command')"

[[ "${tool_name}" == "Bash" && -n "${raw_command}" ]] || exit 0

deny() {
  local op="$1"
  local reason="koryph boundary: ${op} is orchestrator-only — stay on your branch; the koryph merges, pushes, and closes beads"
  if [[ "${have_jq}" == "1" ]]; then
    jq -n --arg r "${reason}" \
      '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
    exit 0
  fi
  printf '%s\n' "${reason}" >&2
  exit 2
}

# --- Split into &&/;/|-separated segments, checked independently -----------
# (bash literal substitution, not a regex — avoids alternation-escaping bugs
# and works on the bash 3.2 shipped with macOS.)
normalized="${raw_command//&&/$'\n'}"
normalized="${normalized//'||'/$'\n'}"
normalized="${normalized//;/$'\n'}"
normalized="${normalized//|/$'\n'}"

check_segment() {
  local seg="$1"

  # Trim leading whitespace.
  seg="$(printf '%s' "${seg}" | sed -E 's/^[[:space:]]+//')"

  # Strip leading `VAR=value ` env assignments, repeatedly (`FOO=1 BAR=2 git push`).
  while [[ "${seg}" =~ ^[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+(.*)$ ]]; do
    seg="${BASH_REMATCH[1]}"
  done

  [[ -n "${seg}" ]] || return 0

  # git rebase / git commit are explicitly allowed — never matched below,
  # regardless of arguments (including `git rebase origin/main`).

  if [[ "${seg}" =~ ^git[[:space:]]+push([[:space:]]|$) ]]; then
    deny "git push"
  fi
  if [[ "${seg}" =~ ^git[[:space:]]+merge([[:space:]]|$) ]]; then
    deny "git merge"
  fi
  if [[ "${seg}" =~ ^git[[:space:]]+checkout[[:space:]]+(main|master)([[:space:]]|$) ]]; then
    deny "git checkout ${BASH_REMATCH[1]}"
  fi
  if [[ "${seg}" =~ ^git[[:space:]]+switch[[:space:]]+(main|master)([[:space:]]|$) ]]; then
    deny "git switch ${BASH_REMATCH[1]}"
  fi
  if [[ "${seg}" =~ ^bd[[:space:]]+close([[:space:]]|$) ]]; then
    deny "bd close"
  fi
  if [[ "${seg}" =~ ^gh[[:space:]]+pr[[:space:]]+merge([[:space:]]|$) ]]; then
    deny "gh pr merge"
  fi
}

while IFS= read -r segment; do
  check_segment "${segment}"
done <<<"${normalized}"

exit 0
