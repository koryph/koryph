// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package commands seeds a project's canonical commands/*.md workflows and
// creates runtime-native links to that same source. Claude Code reads its
// .claude/commands links as slash commands; Codex reads .agents/skills links
// as repository-scoped skills. No runtime gets a separately editable copy.
package commands

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/runtime"
	_ "github.com/koryph/koryph/internal/runtime/codex"
	"github.com/koryph/koryph/internal/scaffold"
)

// FS holds every slash-command template bundled at compile time.
//
//go:embed koryph-*.md
var FS embed.FS

// Install seeds canonical commands and installs Claude Code slash-command
// links. It is equivalent to InstallForRuntime(root, force, "claude").
func Install(root string, force bool) ([]scaffold.Result, error) {
	return InstallForRuntime(root, force, "claude")
}

// InstallForRuntime seeds root/commands from the bundled workflows, then
// creates links in the selected runtime's native workflow location. A
// differing existing projection is left untouched unless --force is passed:
// that protects a project-local customization during migration while making
// the canonical commands/ directory the only editable source going forward.
func InstallForRuntime(root string, force bool, runtimeName string) ([]scaffold.Result, error) {
	if runtimeName == "" {
		runtimeName = "claude"
	}
	sourceDir := filepath.Join(root, "commands")
	if _, err := scaffold.CopyEmbed(FS, sourceDir, force, 0o644); err != nil {
		return nil, err
	}

	if runtimeName == "claude" {
		return linkWorkflows(root, sourceDir, func(name string) string {
			return filepath.Join(".claude", "commands", name+".md")
		}, force)
	}
	rt, ok := runtime.Default.Get(runtimeName)
	if !ok {
		return nil, fmt.Errorf("commands: unknown runtime %q (not registered in runtime.Default)", runtimeName)
	}
	projector, ok := rt.(runtime.WorkflowProjector)
	if !ok {
		return nil, fmt.Errorf("commands: runtime %q has no reusable-workflow projection", runtimeName)
	}
	return linkWorkflows(root, sourceDir, projector.WorkflowPath, force)
}

func linkWorkflows(root, sourceDir string, destination func(string) string, force bool) ([]scaffold.Result, error) {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, err
	}
	results := make([]scaffold.Result, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		source := filepath.Join(sourceDir, entry.Name())
		dest := filepath.Join(root, destination(name))
		action, err := linkWorkflow(source, dest, force)
		if err != nil {
			return nil, err
		}
		results = append(results, scaffold.Result{Name: name, Action: action})
	}
	return results, nil
}

func linkWorkflow(source, dest string, force bool) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(filepath.Dir(dest), source)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(dest)
	if err == nil {
		if info.Mode()&fs.ModeSymlink != 0 {
			if target, rerr := os.Readlink(dest); rerr == nil && target == rel {
				return scaffold.ActionUnchanged, nil
			}
		}
		if !force {
			return scaffold.ActionSkipped, nil
		}
		if err := os.Remove(dest); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Symlink(rel, dest); err != nil {
		return "", fmt.Errorf("commands: link %s -> %s: %w", dest, rel, err)
	}
	return scaffold.ActionInstalled, nil
}
