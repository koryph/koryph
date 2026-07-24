// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package personas maintains a project's canonical agents/*.md corpus and
// renders runtime-native projections from it. It uses the FS embedded in the
// koryph binary (github.com/koryph/koryph/agents) to seed missing canonical
// files without network access, and the shared scaffold installer for the
// hash-aware, force-guarded copy policy.
//
// # Per-runtime rendering (koryph-v8u.12)
//
// A persona file's `model:` frontmatter scalar is a Claude-specific model
// pin (see agents/README.md's frontmatter contract); a Codex project must
// never receive a Claude model name it cannot honor. The canonical source is
// always root/agents/*.md. Claude receives a Markdown projection; a runtime
// with a native renderer receives its own generated format, while another
// registered runtime receives a minimally rewritten Markdown projection.
package personas

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"

	"github.com/koryph/koryph/agents"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/scaffold"
)

// Install seeds <root>/agents and links Claude's native projection into
// <root>/.claude/agents. Equivalent to InstallForRuntime(root, force,
// "claude"). The canonical Markdown files are the source of truth; a
// change is therefore immediately visible to Claude without an install step.
func Install(root string, force bool) ([]scaffold.Result, error) {
	results, _, err := InstallForRuntime(root, force, "claude")
	return results, err
}

// InstallForRuntime is Install generalized to render canonical personas for
// the selected runtime.
//
// runtimeName == "" or "claude" renders a Markdown projection under
// .claude/agents. The canonical corpus is seeded only when files are missing;
// it is never overwritten without --force.
// Any other name must be registered in runtime.Default; an unregistered
// runtime is a hard error (fail closed) rather than a silent claude
// fallback — an installer that guessed wrong here would hand a project a
// model pin its runtime cannot honor.
//
// untiered lists the persona names (without ".md") that were installed
// UNCHANGED because they carry no `tier:` frontmatter scalar, or their tier
// is unmapped by runtimeName's ModelMap — the caller's cue to note that
// these personas still carry claude's legacy `model:` pin verbatim, which
// the target runtime may not honor. It is always nil for the claude path,
// since no rendering (and therefore no such gap) is possible there.
func InstallForRuntime(root string, force bool, runtimeName string) (results []scaffold.Result, untiered []string, err error) {
	if runtimeName == "" {
		runtimeName = "claude"
	}
	source, err := canonicalFS(root, force)
	if err != nil {
		return nil, nil, err
	}
	if runtimeName == "claude" {
		return linkClaudePersonas(root, force)
	}

	rt, ok := runtime.Default.Get(runtimeName)
	if !ok {
		return nil, nil, fmt.Errorf("personas: unknown runtime %q (not registered in runtime.Default)", runtimeName)
	}
	if renderer, ok := rt.(runtime.PersonaRenderer); ok {
		rendered, err := renderNativePersonasFS(source, renderer)
		if err != nil {
			return nil, nil, err
		}
		results, err = scaffold.CopyEmbed(rendered, filepath.Join(root, renderer.PersonaDir()), force, 0o644)
		return results, nil, err
	}

	rendered, untiered, err := renderPersonasFS(source, rt.ModelMap())
	if err != nil {
		return nil, nil, err
	}
	results, err = scaffold.CopyEmbed(rendered, filepath.Join(root, ".claude", "agents"), force, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return results, untiered, nil
}

// linkClaudePersonas gives Claude Code a native named-agent surface while
// preserving agents/*.md as the sole editable copy. A relative link keeps a
// checked-out project relocatable and lets a user edit a persona once for both
// Claude (native agent loading) and Codex (prompt preparation).
func linkClaudePersonas(root string, force bool) ([]scaffold.Result, []string, error) {
	sourceDir := filepath.Join(root, "agents")
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, nil, err
	}
	results := make([]scaffold.Result, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		action, err := linkCanonicalPersona(
			filepath.Join(sourceDir, entry.Name()),
			filepath.Join(root, ".claude", "agents", entry.Name()),
			force,
		)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, scaffold.Result{Name: name, Action: action})
	}
	return results, nil, nil
}

func linkCanonicalPersona(source, dest string, force bool) (string, error) {
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
			if target, readErr := os.Readlink(dest); readErr == nil && target == rel {
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
		return "", fmt.Errorf("personas: link %s -> %s: %w", dest, rel, err)
	}
	if info != nil {
		return scaffold.ActionOverwritten, nil
	}
	return scaffold.ActionInstalled, nil
}

// canonicalFS seeds root/agents from the bundled defaults, then returns the
// on-disk directory as the rendering source. CopyEmbed's no-overwrite policy
// means operator edits to agents/*.md always win unless --force is explicit.
func canonicalFS(root string, force bool) (fs.FS, error) {
	if _, err := scaffold.CopyEmbed(agents.FS, filepath.Join(root, "agents"), force, 0o644); err != nil {
		return nil, err
	}
	return os.DirFS(filepath.Join(root, "agents")), nil
}

// renderNativePersonasFS asks a runtime adapter to project the canonical
// Markdown corpus into its native configuration format. The source remains
// agents/*.md; generated runtime projections are never read back as source.
func renderNativePersonasFS(source fs.FS, renderer runtime.PersonaRenderer) (fstest.MapFS, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, err
	}
	out := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" || entry.Name() == "README.md" {
			continue
		}
		data, err := fs.ReadFile(source, entry.Name())
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		rendered, err := renderer.RenderPersona(name, data)
		if err != nil {
			return nil, err
		}
		out[name+".toml"] = &fstest.MapFile{Data: rendered, Mode: 0o644}
	}
	return out, nil
}
