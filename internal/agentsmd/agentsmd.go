// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package agentsmd installs the koryph operating contract as AGENTS.md at a
// managed project's root. AGENTS.md is the canonical, runtime-neutral
// instruction file read natively by Codex, Cursor, Grok, Copilot, opencode,
// amp, and (as a fallback) Claude Code — it is the cross-runtime counterpart
// of CLAUDE.md and wins whenever a runtime reads only one file.
//
// koryph installs AGENTS.md unconditionally during `project add`, regardless
// of the configured runtime, because every runtime benefits from it. The
// file contains the koryph operating contract: capability tiers, beads task
// tracking, the green gate, commit requirements, protected paths, the
// containment model (hooks vs. worktree-isolation + merge-gate), and the
// non-interactive-shell rule.
//
// Overwrite policy: an existing AGENTS.md is compared by content hash against
// the embedded template. If they are identical the install is a silent no-op
// (Unchanged). If the content differs, the file is left untouched unless
// force is set (Skipped vs. Overwritten) — the same hash-aware policy that
// scaffold.CopyEmbed uses for personas and commands.
package agentsmd

import (
	"crypto/sha256"
	_ "embed"
	"os"
	"path/filepath"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/scaffold"
)

//go:embed template.md
var template []byte

// Install writes the koryph operating contract to <root>/AGENTS.md and
// returns one of the scaffold.Action* constants describing what happened.
//
//   - scaffold.ActionInstalled   — written into an empty slot
//   - scaffold.ActionOverwritten — existed with differing content, replaced (force)
//   - scaffold.ActionSkipped     — existed with differing content, left untouched (no force)
//   - scaffold.ActionUnchanged   — existed with identical content — no-op
func Install(root string, force bool) (string, error) {
	dest := filepath.Join(root, "AGENTS.md")

	if onDisk, err := os.ReadFile(dest); err == nil {
		// File exists: check content.
		if sha256.Sum256(onDisk) == sha256.Sum256(template) {
			return scaffold.ActionUnchanged, nil
		}
		if !force {
			return scaffold.ActionSkipped, nil
		}
		if err := fsx.WriteAtomic(dest, template, 0o644); err != nil {
			return "", err
		}
		return scaffold.ActionOverwritten, nil
	}

	// File absent: create it (fsx.WriteAtomic creates missing parent dirs).
	if err := fsx.WriteAtomic(dest, template, 0o644); err != nil {
		return "", err
	}
	return scaffold.ActionInstalled, nil
}

// Template returns the raw embedded AGENTS.md template bytes. It is exported
// so callers can test the template content or diff it against an on-disk file
// without calling Install.
func Template() []byte {
	return append([]byte(nil), template...)
}
