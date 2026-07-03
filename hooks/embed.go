// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package hooks exposes the koryph enforcement hook scripts shipped with the
// binary (agent-boundary-guard, worktree-guard). They are installed into a
// managed project's hooks/ directory and wired via .claude/settings.json so the
// agent boundary and worktree containment rules hold whether `koryph` runs
// explicitly or a prompt drives it.
package hooks

import "embed"

// FS holds every hook script bundled at compile time.
//
//go:embed *.sh
var FS embed.FS
