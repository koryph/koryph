#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
# Shim: ruleset logic has moved into 'koryph repo check|apply'. See internal/posture.
mode=check; repo_args=()
while [ $# -gt 0 ]; do case "$1" in --check) mode=check;; --apply) mode=apply;; --repo) repo_args=(--repo "$2"); shift;; esac; shift; done
exec koryph repo "$mode" "${repo_args[@]+"${repo_args[@]}"}"
