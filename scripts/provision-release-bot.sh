#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# DEPRECATED — use 'koryph bot' instead.
#
# This script is a compatibility shim kept for backwards compatibility.
# All functionality has been superseded by the koryph binary:
#
#   koryph bot create [--name N] [--org ORG] [--public] [--headless]
#   koryph bot install --name N
#   koryph bot attach --name N --repo OWNER/REPO [--org-secrets]
#   koryph bot check --name N [--repo OWNER/REPO]
#   koryph bot list [--check]
#
# See: koryph bot -h
#      docs/user-guide/release-bot.md

set -euo pipefail
echo "provision-release-bot.sh is deprecated. Use 'koryph bot' instead." >&2
echo "" >&2
echo "  koryph bot create --name <name>                    # was --bootstrap" >&2
echo "  koryph bot install --name <name>                   # open install page" >&2
echo "  koryph bot attach --name <name> --repo OWNER/REPO  # was --attach" >&2
echo "  koryph bot check --name <name> [--repo OWNER/REPO] # was --check" >&2
echo "" >&2
echo "Run 'koryph bot -h' for full usage." >&2
exit 1
