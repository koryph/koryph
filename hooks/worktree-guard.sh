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
# Project tree root: Claude Code provides CLAUDE_PROJECT_DIR; Codex provides
# cwd in its hook JSON. This hook is shipped centrally but always evaluates
# boundaries against the *dispatched project*, never against the koryph
# checkout itself.

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
project_dir="${CLAUDE_PROJECT_DIR:-$(jq -r '.cwd // empty' <<<"${input}")}"
project_dir="${project_dir:-$(pwd)}"

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

# Self-protection: a dispatched agent must never write koryph's own enforcement
# surface. These are protected paths (agents never legitimately edit them —
# refactor-core work is orchestrator-authored), and blocking writes here is
# defense-in-depth even though the ACTIVE guards now live in ${KORYPH_HOME}
# outside the worktree.
selfprotected() {
  local p
  p="$(abspath "$1")"
  case "${p}" in
    "${project_dir}"/hooks | "${project_dir}"/hooks/* | \
    "${project_dir}"/.claude | "${project_dir}"/.claude/* | \
    "${project_dir}"/.codex | "${project_dir}"/.codex/* | \
    "${project_dir}"/agents | "${project_dir}"/agents/*) return 0 ;;
    *) return 1 ;;
  esac
}

# --- Edit/Write: keep file writes inside the project / worktrees / temp --------
if [[ "${tool_name}" == "Edit" || "${tool_name}" == "Write" ]]; then
  if [[ -n "${file_path}" ]]; then
    [[ "${file_path}" == *".."* ]] && deny "file_path contains '..' traversal: ${file_path}"
    inside_project "${file_path}" || deny "file_path outside project tree: ${file_path}"
    selfprotected "${file_path}" && deny "koryph enforcement path is read-only for agents: ${file_path}"
  fi
  exit 0
fi

# Codex exposes file changes through apply_patch. The patch body is a command
# string, so reject any target that resolves outside the project or lands on
# koryph's protected enforcement surface. This is intentionally conservative:
# a malformed patch is allowed through for Codex to reject itself, but an
# explicit absolute/traversal target is never delegated to the tool.
if [[ "${tool_name}" == "apply_patch" && -n "${command}" ]]; then
  while IFS= read -r target; do
    [[ -n "${target}" ]] || continue
    [[ "${target}" == *".."* ]] && deny "apply_patch path contains '..' traversal: ${target}"
    inside_project "${target}" || deny "apply_patch path outside project tree: ${target}"
    selfprotected "${target}" && deny "koryph enforcement path is read-only for agents: ${target}"
  done < <(printf '%s\n' "${command}" | sed -n -E 's#^\*\*\* (Add|Update|Delete) File: (.*)$#\2#p')
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

# --- Bash: block redirection into koryph's enforcement surface ----------------
# Catches `> hooks/x`, `>> .claude/y`, `> .codex/y`, `> ./agents/z`, and absolute/nested forms
# `> /path/hooks/x`, so an agent cannot rewrite a guard via shell redirect.
if [[ "${command}" =~ \>\>?[[:space:]]*(\./)?(hooks|\.claude|\.codex|agents)/ ]] ||
  [[ "${command}" =~ \>\>?[[:space:]]*[^[:space:]]*/(hooks|\.claude|\.codex|agents)/ ]]; then
  deny "redirection writes to a koryph enforcement path (hooks/.claude/.codex/agents): ${command}"
fi

# --- Bash: block any reference to a credential / koryph-state path -------------
# The .claude/settings.json Read-tool denies (.env, *.pem, *.key) are bypassable
# via Bash(*): `cat`/`base64`/`cp`/`dd`/`python -c open(...)` all read the same
# files the Read tool refuses, and there is no egress restriction. Rather than
# enumerate every read/write verb, screen for the PATH: a dispatched agent
# working inside its worktree has no legitimate reason to touch the operator's
# credential stores or koryph's own machine state, whether reading (exfiltrate
# vault keys, bot PEMs, ~/.claude.json OAuth token, ssh keys) or writing (poison
# ~/.gitconfig hooksPath, governor caps, ~/.ssh/authorized_keys). Every such
# command must *name* the path, so a token scan catches the overwhelming
# majority of direct attempts.
#
# This is defense-in-depth, NOT a sandbox: a variable-indirected path
# (`H=$HOME/.ssh; cat "$H/id_rsa"`) evades a static scan. True same-uid
# isolation for dispatched agents requires an OS sandbox (seatbelt/bubblewrap/
# container) — tracked separately. The screen raises the bar sharply against the
# direct-command exfiltration the Read denies were meant to stop.
secret_ref_patterns=(
  '(^|[^a-z0-9._-])\.koryph(/|[[:space:]]|$)'
  '(^|[^a-z0-9._-])\.ssh/'
  'authorized_keys'
  'id_rsa' 'id_ed25519' 'id_ecdsa' 'id_dsa'
  '\.claude\.json'
  '\.gitconfig'
  '\.aws/' '\.config/gcloud' '\.config/gh/' '\.netrc' '\.npmrc' '\.pypirc' '\.docker/config'
  'vault\.json'
  '\.pem([[:space:]"/]|$)'
  '\.key([[:space:]"/]|$)'
  '\.env([[:space:]"/.]|$)'
)
for pat in "${secret_ref_patterns[@]}"; do
  if [[ "${lower}" =~ ${pat} ]]; then
    deny "command references a credential / koryph-state path (matched ${pat}); reading or writing secrets and machine state is orchestrator-only, not an agent operation"
  fi
done

# --- Bash: block writes landing outside the project in $HOME ------------------
# Redirect (> >>), tee/cp/mv/install/ln/rsync, or dd of= whose target is a
# home-anchored dotfile escapes the worktree: shell rc files (~/.zshrc,
# ~/.bashrc — RCE persistence on the operator's next shell), ~/.config, and any
# other $HOME dotfile the credential screen above doesn't enumerate. Reads are
# not blocked here (the credential screen covers the sensitive ones); this is
# the general home-WRITE ceiling.
if [[ "${command}" =~ (\>\>?[[:space:]]*|of=|(^|[[:space:]])(tee|cp|mv|install|ln|rsync|dd)[[:space:]])[^|\;\&]*(~|\$HOME|\$\{HOME\})/\. ]]; then
  deny "command writes to a \$HOME dotfile outside the project tree (persistence / tamper vector): ${command}"
fi

exit 0
