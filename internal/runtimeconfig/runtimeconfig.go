// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package runtimeconfig resolves registered runtimes with process-local binary
// overrides. Keeping this adapter construction in one place prevents command
// and engine call sites from drifting back to runtime-specific launch logic.
package runtimeconfig

import (
	"os"

	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/claude"
	"github.com/koryph/koryph/internal/runtime/codex"
)

const (
	EnvClaudeBin = "KORYPH_CLAUDE_BIN"
	EnvCodexBin  = "KORYPH_CODEX_BIN"
)

// Get returns the configured adapter for name. Built-in runtimes honor their
// test/operator binary override; third-party runtimes come from the registry.
func Get(name string) (runtime.Runtime, bool) {
	switch name {
	case "claude":
		return claude.New(os.Getenv(EnvClaudeBin)), true
	case "codex":
		return codex.New(os.Getenv(EnvCodexBin)), true
	default:
		return runtime.Default.Get(name)
	}
}
