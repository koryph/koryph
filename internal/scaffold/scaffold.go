// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package scaffold copies binary-embedded *.md assets (fallback personas,
// Claude slash commands) into a project's .claude tree. It is the shared,
// force-aware installer behind `koryph agents install`, `koryph commands
// install`, and the `project add` onboarding step.
//
// Overwrite policy: an existing destination file is compared by content hash
// against the embedded asset first. If they are identical the copy is a silent
// no-op (Unchanged) — re-installing is idempotent and never warns. If the
// content differs, the file is NEVER replaced unless the caller passes force:
// without force it is Skipped so the CLI can warn and tell the operator to
// re-run with --force; with force it is Overwritten.
package scaffold

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Install actions.
const (
	ActionInstalled   = "installed"   // written into an empty slot
	ActionOverwritten = "overwritten" // existed with differing content, replaced under force
	ActionSkipped     = "skipped"     // existed with differing content, left untouched (no force)
	ActionUnchanged   = "unchanged"   // existed with identical content — no-op, no warning
)

// Result summarises what happened to one embedded file.
type Result struct {
	Name   string // base name without .md
	Action string // one of the Action constants
}

// CopyEmbed copies every file at the root of fsys into destDir with the given
// perm, creating destDir as needed. See the package doc for the overwrite
// policy. The embed pattern (e.g. *.md, *.sh) is the source of truth for what
// ships — every embedded file is copied.
func CopyEmbed(fsys fs.FS, destDir string, force bool, perm os.FileMode) ([]Result, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("scaffold: create %s: %w", destDir, err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("scaffold: read embedded fs: %w", err)
	}

	var results []Result
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		dest := filepath.Join(destDir, e.Name())

		data, rerr := fs.ReadFile(fsys, e.Name())
		if rerr != nil {
			return nil, fmt.Errorf("scaffold: read embedded %s: %w", e.Name(), rerr)
		}

		exists := false
		if onDisk, derr := os.ReadFile(dest); derr == nil {
			exists = true
			// Identical content → idempotent no-op, no warning regardless of force.
			if sha256.Sum256(onDisk) == sha256.Sum256(data) {
				results = append(results, Result{Name: name, Action: ActionUnchanged})
				continue
			}
			// Differing content is only replaced under force.
			if !force {
				results = append(results, Result{Name: name, Action: ActionSkipped})
				continue
			}
		}

		if werr := os.WriteFile(dest, data, perm); werr != nil {
			return nil, fmt.Errorf("scaffold: write %s: %w", dest, werr)
		}
		action := ActionInstalled
		if exists {
			action = ActionOverwritten
		}
		results = append(results, Result{Name: name, Action: action})
	}
	return results, nil
}

// Conflicts returns the names of results that were Skipped because the file
// already existed and force was not set. A non-empty slice is the caller's cue
// to warn and suggest --force.
func Conflicts(results []Result) []string {
	var names []string
	for _, r := range results {
		if r.Action == ActionSkipped {
			names = append(names, r.Name)
		}
	}
	return names
}

// Count returns how many results carry the given action.
func Count(results []Result, action string) int {
	n := 0
	for _, r := range results {
		if r.Action == action {
			n++
		}
	}
	return n
}
