// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

// PromptPreparer is an optional runtime capability for CLIs that do not have
// a command-line equivalent of Claude's --agent flag. It turns the canonical
// persona source into an immutable prompt prefix before untrusted bead text is
// appended. The worktree is supplied so the same source of truth travels with
// every managed project.
type PromptPreparer interface {
	PreparePrompt(worktree, persona, prompt string) (string, error)
}

// PersonaRenderer is an optional installer capability. It lets a runtime
// project its canonical Markdown persona source into its native on-disk format
// without creating a second editable corpus.
type PersonaRenderer interface {
	PersonaDir() string
	RenderPersona(name string, source []byte) ([]byte, error)
}

// WorkflowProjector identifies the runtime-native location of a reusable
// Markdown workflow. The command installer keeps the Markdown file in the
// project's canonical commands/ directory and links this projection to that
// exact file, so changing a workflow cannot make Claude and Codex drift.
type WorkflowProjector interface {
	WorkflowPath(name string) string
}
