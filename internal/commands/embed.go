// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package commands exposes the Claude Code slash commands shipped with the
// koryph binary and installs them into a project's .claude/commands
// directory. Each koryph-*.md file is a slash-command prompt that drives the
// `koryph`/`bd` CLIs, so a managed project enforces koryph semantics
// whether `koryph` is run explicitly or implied by an in-session prompt.
package commands

import (
	"embed"
	"path/filepath"

	"github.com/koryph/koryph/internal/scaffold"
)

// FS holds every slash-command template bundled at compile time.
//
//go:embed koryph-*.md
var FS embed.FS

// Install copies the embedded slash commands into <root>/.claude/commands.
// Existing files are skipped unless force is set (see scaffold.CopyEmbed).
func Install(root string, force bool) ([]scaffold.Result, error) {
	return scaffold.CopyEmbed(FS, filepath.Join(root, ".claude", "commands"), force, 0o644)
}
