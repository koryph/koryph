#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
# pre-commit hook: every source file starts with the Apache-2.0 SPDX header.
set -euo pipefail
status=0
for f in "$@"; do
  # REUSE-IgnoreStart
  if ! head -3 "$f" | grep -q 'SPDX-License-Identifier: Apache-2.0'; then
    echo "$f: missing 'SPDX-License-Identifier: Apache-2.0' header" >&2
    status=1
  fi
  # REUSE-IgnoreEnd
done
exit $status
