#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# ensure-rulesets.sh — apply/verify the repo's branch-protection rulesets
# from the desired-state files in .github/rulesets/*.json (IaC: the web UI
# is never the source of truth; these files are).
#
#   ensure-rulesets.sh --check [--repo owner/name]   # diff live vs desired, exit 1 on drift
#   ensure-rulesets.sh --apply [--repo owner/name]   # create-or-update each ruleset by name
#
# Matching is by ruleset NAME. --apply PUTs an existing ruleset (preserving
# its id) or POSTs a new one. Requires: gh (authenticated, admin on the
# repo), jq, python3. Never deletes rulesets it does not know about.
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

DIR="$(cd "$(dirname "$0")/.." && pwd)/.github/rulesets"
[ -d "$DIR" ] || { echo "no desired-state dir: $DIR" >&2; exit 2; }

# normalize <json> — strip server-assigned/volatile fields so live and
# desired states compare structurally.
normalize() {
  jq -S 'del(.id, .source, .source_type, .created_at, .updated_at,
             .node_id, ._links, .current_user_can_bypass)
         | .bypass_actors //= []'
}

drift=0
for f in "$DIR"/*.json; do
  name="$(jq -r .name "$f")"
  live_id="$(gh api "repos/$REPO/rulesets" --jq ".[] | select(.name==\"$name\") | .id" || true)"

  if [ -z "$live_id" ]; then
    if [ "$MODE" = check ]; then
      echo "MISSING  $name (no live ruleset)"
      drift=1
    else
      gh api -X POST "repos/$REPO/rulesets" --input "$f" >/dev/null
      echo "CREATED  $name"
    fi
    continue
  fi

  live="$(gh api "repos/$REPO/rulesets/$live_id" | normalize)"
  want="$(normalize <"$f")"
  if [ "$live" = "$want" ]; then
    echo "OK       $name"
    continue
  fi

  if [ "$MODE" = check ]; then
    echo "DRIFT    $name (live differs from $f):"
    diff <(echo "$live") <(echo "$want") | sed 's/^/         /' | head -40 || true
    drift=1
  else
    gh api -X PUT "repos/$REPO/rulesets/$live_id" --input "$f" >/dev/null
    echo "UPDATED  $name"
  fi
done

exit "$drift"
