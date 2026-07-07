#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# docs-lint: catch designs/-relative links in docs/ pages outside docs/designs/.
#
# CI removes docs/designs/ before zensical build --strict (see
# .github/workflows/docs.yml), so any user-guide page that links
# ../designs/<doc>.md passes a local build (designs/ present) but breaks the
# published-site build. Regression shape: tui.md via 9b41aee and
# vscode-extension.md via ew2.8 both caused post-merge red main.
#
# Usage (all from repo root):
#   bash scripts/docs-lint.sh              # check real docs/
#   bash scripts/docs-lint.sh <docs-root>  # explicit path (used by tests)
#
# Exit 0 — no violations.
# Exit 1 — one or more ../designs/ links found outside docs/designs/.

set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
docs_dir="${1:-$repo_root/docs}"

violations=()
while IFS= read -r -d '' f; do
  if grep -qF '../designs/' "$f" 2>/dev/null; then
    violations+=("$f")
  fi
done < <(find "$docs_dir" -name "*.md" -not -path "*/designs/*" -print0 | sort -z)

if [[ ${#violations[@]} -eq 0 ]]; then
  echo "docs-lint: OK — no designs/-relative links outside docs/designs/"
  exit 0
fi

echo "docs-lint: FAIL — ../designs/ link(s) found outside docs/designs/ (CI strips docs/designs/ before build):"
for f in "${violations[@]}"; do
  echo "  $f"
  grep -nF '../designs/' "$f" | sed 's/^/    /'
done
exit 1
