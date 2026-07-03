// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package personas installs fallback Claude sub-agent persona files into a
// project's .claude/agents directory. It uses the FS embedded in the
// koryph binary (github.com/koryph/koryph/agents) so no network
// access is required at onboard time, and the shared scaffold installer for
// the hash-aware, force-guarded copy policy.
package personas

import (
	"path/filepath"

	"github.com/koryph/koryph/agents"
	"github.com/koryph/koryph/internal/scaffold"
)

// Install copies each embedded fallback persona into <root>/.claude/agents.
// A file that already exists with identical content is an idempotent no-op; a
// file with differing content is skipped (warned by the caller) unless force
// is set, in which case it is overwritten. See scaffold.CopyEmbed.
func Install(root string, force bool) ([]scaffold.Result, error) {
	return scaffold.CopyEmbed(agents.FS, filepath.Join(root, ".claude", "agents"), force, 0o644)
}
