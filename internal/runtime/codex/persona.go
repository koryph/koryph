// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PreparePrompt projects the canonical Markdown persona into the dispatch
// prompt. Codex has no `exec --agent` selector, so this is the portable
// equivalent of Claude's native named-agent invocation.
func (c Codex) PreparePrompt(worktree, persona, prompt string) (string, error) {
	if persona == "" {
		return prompt, nil
	}
	path := filepath.Join(worktree, "agents", persona+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("codex persona %q: read canonical source %s: %w", persona, path, err)
	}
	return "# Koryph persona\n\nFollow this repository-owned persona before processing the task.\n\n" + string(b) + "\n\n# Assigned task\n\n" + prompt, nil
}

func (c Codex) PersonaDir() string { return filepath.Join(".codex", "agents") }

// WorkflowPath follows Codex's repository-scope skill discovery convention.
// Commands are represented as Skills because Codex custom prompts are
// user-local and deprecated; a project skill is both shareable and discoverable.
func (c Codex) WorkflowPath(name string) string {
	return filepath.Join(".agents", "skills", name, "SKILL.md")
}

// RenderPersona produces Codex's native custom-agent TOML from the same
// Markdown corpus Claude receives. The dispatch path still injects the source
// directly (above), while this projection supports subagents created by Codex.
func (c Codex) RenderPersona(name string, source []byte) ([]byte, error) {
	body := stripFrontmatter(string(source))
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("codex persona %q has no instructions", name)
	}
	description := "Koryph persona " + name
	return []byte("name = " + tomlString(name) + "\n" +
		"description = " + tomlString(description) + "\n" +
		"developer_instructions = " + tomlMultiline(body) + "\n"), nil
}

func stripFrontmatter(s string) string {
	trimmed := strings.TrimLeft(s, "\n")
	if !strings.HasPrefix(trimmed, "---\n") {
		return s
	}
	if i := strings.Index(trimmed[4:], "\n---"); i >= 0 {
		return strings.TrimLeft(trimmed[4+i+4:], "\n")
	}
	return s
}

func tomlMultiline(s string) string {
	return `"""` + strings.ReplaceAll(s, `"""`, `\"""`) + `"""`
}
