#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# ensure-repo-settings.sh — apply/verify repository administrative settings
# from the desired-state file .github/repo-settings.json (IaC: the web UI is
# never the source of truth). Companion to ensure-rulesets.sh; both run via
# `make repo-check` / `make repo-apply` (koryph-0vf.7).
#
#   ensure-repo-settings.sh --check [--repo owner/name]  # exit 1 on drift
#   ensure-repo-settings.sh --apply [--repo owner/name]  # PATCH/PUT to match
#
# Manages: repo merge/branch flags (PATCH /repos), security_and_analysis
# (secret scanning, push protection, dependabot security updates),
# vulnerability alerts, and Actions workflow token permissions. Settings
# GitHub exposes no API for are listed under "unmanaged" in the JSON and
# reported informationally. Requires: gh (admin on the repo), jq.
# shellcheck disable=SC2329,SC2317  # apply_* functions are invoked indirectly via section()
set -euo pipefail

MODE=""
REPO=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check ;;
    --apply) MODE=apply ;;
    --repo) REPO="$2"; shift ;;
    *) echo "usage: $0 --check|--apply [--repo owner/name]" >&2; exit 2 ;;
  esac
  shift
done
[ -n "$MODE" ] || { echo "usage: $0 --check|--apply [--repo owner/name]" >&2; exit 2; }
[ -n "$REPO" ] || REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"

FILE="$(cd "$(dirname "$0")/.." && pwd)/.github/repo-settings.json"
[ -f "$FILE" ] || { echo "no desired-state file: $FILE" >&2; exit 2; }

drift=0

# section <label> <live-json> <want-json> <apply-fn-name>
section() {
  local label="$1" live="$2" want="$3" apply_fn="$4"
  if [ "$(jq -S . <<<"$live")" = "$(jq -S . <<<"$want")" ]; then
    echo "OK       $label"
    return 0
  fi
  if [ "$MODE" = check ]; then
    echo "DRIFT    $label:"
    diff <(jq -S . <<<"$live") <(jq -S . <<<"$want") | sed 's/^/         /' || true
    drift=1
  else
    "$apply_fn"
    echo "UPDATED  $label"
  fi
}

# --- repo flags + security_and_analysis (one PATCH endpoint) ----------------
want_repo="$(jq -S .repo "$FILE")"
live_repo="$(gh api "repos/$REPO" | jq -S '{allow_merge_commit, allow_squash_merge,
  allow_rebase_merge, allow_auto_merge, delete_branch_on_merge,
  allow_update_branch, web_commit_signoff_required}')"
apply_repo() { gh api -X PATCH "repos/$REPO" --input <(jq '.repo' "$FILE") >/dev/null; }
section "repo flags" "$live_repo" "$want_repo" apply_repo

want_sec="$(jq -S .security_and_analysis "$FILE")"
live_sec="$(gh api "repos/$REPO" | jq -S '{
  secret_scanning: .security_and_analysis.secret_scanning.status,
  secret_scanning_push_protection: .security_and_analysis.secret_scanning_push_protection.status,
  dependabot_security_updates: .security_and_analysis.dependabot_security_updates.status}')"
apply_sec() {
  gh api -X PATCH "repos/$REPO" --input <(jq '{security_and_analysis: {
    secret_scanning: {status: .security_and_analysis.secret_scanning},
    secret_scanning_push_protection: {status: .security_and_analysis.secret_scanning_push_protection},
    dependabot_security_updates: {status: .security_and_analysis.dependabot_security_updates}}}' "$FILE") >/dev/null
}
section "security & analysis" "$live_sec" "$want_sec" apply_sec

# --- vulnerability alerts (204 = enabled, 404 = disabled) -------------------
want_vuln="$(jq -r .vulnerability_alerts "$FILE")"
if gh api "repos/$REPO/vulnerability-alerts" >/dev/null 2>&1; then live_vuln=true; else live_vuln=false; fi
apply_vuln() {
  if [ "$want_vuln" = true ]; then gh api -X PUT "repos/$REPO/vulnerability-alerts" >/dev/null
  else gh api -X DELETE "repos/$REPO/vulnerability-alerts" >/dev/null; fi
}
section "vulnerability alerts" "{\"enabled\": $live_vuln}" "{\"enabled\": $want_vuln}" apply_vuln

# --- Actions workflow token permissions --------------------------------------
want_actions="$(jq -S .actions_workflow "$FILE")"
live_actions="$(gh api "repos/$REPO/actions/permissions/workflow" | jq -S .)"
apply_actions() {
  gh api -X PUT "repos/$REPO/actions/permissions/workflow" \
    --input <(jq '.actions_workflow' "$FILE") >/dev/null
}
section "actions workflow permissions" "$live_actions" "$want_actions" apply_actions

# --- unmanaged (informational, never drift) ----------------------------------
jq -r '.unmanaged[]? | "INFO     unmanaged: \(.)"' "$FILE"

exit "$drift"
