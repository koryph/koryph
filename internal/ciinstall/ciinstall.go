// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package ciinstall provides forge-native CI asset installation logic shared
// by koryph ci setup and future installers (e.g. koryph rdc for docs pipelines).
//
// # Overview
//
// [Install] renders a CI asset kind via a [forge.CIService] and writes it to
// the forge-native path under the project root. [Check] compares the on-disk
// asset against the current Render output and reports drift. Both operations
// are idempotent — re-running over an up-to-date asset is a no-op.
//
// # Forge-native paths
//
// GitHub kinds are installed under .github/workflows/:
//
//	gate    → .github/workflows/koryph-gate.yml
//	scanner → .github/workflows/koryph-scanner.yml
//
// GitLab kinds are written as includable fragments under .koryph/ci/:
//
//	gate    → .koryph/ci/koryph-gate.yml
//	scanner → .koryph/ci/koryph-scanner.yml
//
// The GitLab installer prints guidance to add an `include:` entry to
// .gitlab-ci.yml so the fragment takes effect.
//
// # Reuse
//
// The exported [Install] and [Check] functions are the stable API for
// downstream installers. Do not copy their logic — import the package.
package ciinstall

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/koryph/koryph/internal/forge"
)

// Action constants describe the outcome of one [Install] call.
const (
	// ActionInstalled means the asset file was newly written.
	ActionInstalled = "installed"
	// ActionUnchanged means the file already matches the rendered content.
	ActionUnchanged = "unchanged"
	// ActionUnsupported means the forge provider does not support this kind.
	ActionUnsupported = "unsupported"
)

// AllKinds is the ordered set of CI asset kinds the ci setup verb installs
// when --kind all is specified. Append new kinds here when they are added to
// the forge CIService contract.
var AllKinds = []string{"gate", "scanner"}

// Result is the outcome of one [Install] or [Check] call.
type Result struct {
	// Kind is the CI asset kind (e.g. "gate", "scanner").
	Kind string
	// Path is the forge-native install path relative to the project root.
	// Empty when Action is ActionUnsupported.
	Path string
	// Action is one of the Action* constants.
	Action string
	// HasDrift is set by [Check]: true when the on-disk content differs from
	// the current Render output (or the file is absent). Always false for
	// ActionUnsupported.
	HasDrift bool
}

// KindPath returns the forge-native install path for kind on the named forge,
// relative to the project root. Returns ("", false) for unknown combinations.
func KindPath(forgeName, kind string) (string, bool) {
	switch forgeName {
	case "github":
		switch kind {
		case "gate":
			return filepath.Join(".github", "workflows", "koryph-gate.yml"), true
		case "scanner":
			return filepath.Join(".github", "workflows", "koryph-scanner.yml"), true
		case "caller":
			return filepath.Join(".github", "workflows", "release.yml"), true
		case "docs":
			return filepath.Join(".github", "workflows", "koryph-docs.yml"), true
		}
	case "gitlab":
		switch kind {
		case "gate":
			return filepath.Join(".koryph", "ci", "koryph-gate.yml"), true
		case "scanner":
			return filepath.Join(".koryph", "ci", "koryph-scanner.yml"), true
		case "release", "caller":
			return filepath.Join(".koryph", "ci", "koryph-release.yml"), true
		case "docs":
			return filepath.Join(".koryph", "ci", "koryph-docs.yml"), true
		}
	}
	return "", false
}

// Install renders kind via ci and writes it to the forge-native path under
// root. It is idempotent: if the file already exists with identical content
// the action is ActionUnchanged and no write occurs. If the forge provider does
// not support kind, the action is ActionUnsupported (not an error).
func Install(root, forgeName string, ci forge.CIService, kind string) (Result, error) {
	path, known := KindPath(forgeName, kind)
	if !known {
		// Unknown kind/forge combination — treat as unsupported.
		return Result{Kind: kind, Action: ActionUnsupported}, nil
	}

	content, err := ci.Render(kind)
	if err != nil {
		if errors.Is(err, forge.ErrUnsupported) {
			return Result{Kind: kind, Path: path, Action: ActionUnsupported}, nil
		}
		return Result{}, fmt.Errorf("ci install: render %q: %w", kind, err)
	}

	absPath := filepath.Join(root, path)

	// Idempotence: compare with existing file before writing.
	existing, readErr := os.ReadFile(absPath)
	if readErr == nil && bytes.Equal(existing, content) {
		return Result{Kind: kind, Path: path, Action: ActionUnchanged}, nil
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("ci install: create parent dir for %s: %w", path, err)
	}

	if err := os.WriteFile(absPath, content, 0o644); err != nil {
		return Result{}, fmt.Errorf("ci install: write %s: %w", path, err)
	}

	return Result{Kind: kind, Path: path, Action: ActionInstalled}, nil
}

// Check compares the on-disk asset at path with the current Render output and
// returns drift information. A missing file counts as drift. If the provider
// does not support kind, HasDrift is false and Action is ActionUnsupported.
func Check(root, forgeName string, ci forge.CIService, kind string) (Result, error) {
	path, known := KindPath(forgeName, kind)
	if !known {
		return Result{Kind: kind, Action: ActionUnsupported}, nil
	}

	content, err := ci.Render(kind)
	if err != nil {
		if errors.Is(err, forge.ErrUnsupported) {
			return Result{Kind: kind, Path: path, Action: ActionUnsupported}, nil
		}
		return Result{}, fmt.Errorf("ci check: render %q: %w", kind, err)
	}

	absPath := filepath.Join(root, path)
	existing, readErr := os.ReadFile(absPath)
	if readErr != nil {
		// File absent — definite drift.
		return Result{Kind: kind, Path: path, Action: ActionInstalled, HasDrift: true}, nil
	}

	hasDrift := !bytes.Equal(existing, content)
	return Result{Kind: kind, Path: path, Action: ActionUnchanged, HasDrift: hasDrift}, nil
}
