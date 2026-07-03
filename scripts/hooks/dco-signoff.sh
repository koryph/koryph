#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
# commit-msg hook: require a DCO sign-off trailer.
set -euo pipefail
msg_file="$1"
if ! grep -qE '^Signed-off-by: .+ <.+@.+>' "$msg_file"; then
  echo "commit message must contain a DCO sign-off trailer (use: git commit -s)" >&2
  exit 1
fi
