// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package worktree manages agent worktrees: one active bead per worktree,
// sibling directory layout <parent>/<repo>-worktrees/<name>.
//
// Implementation contract (worktree.go):
//   - Ensure(ctx, EnsureOpts) (Info, error) — attach if the worktree already
//     exists for the branch (fixes the recover-vs-existing-worktree bug:
//     `git worktree add` on an existing path/branch must ATTACH, not fail);
//     otherwise create branch from Base and add the worktree. Never reuse a
//     dirty worktree silently: attaching to a dirty tree is allowed (that IS
//     resume), but creating fresh over a dirty tree is an error.
//   - List(ctx, repoRoot) ([]Info, error) — `git worktree list --porcelain`.
//   - IsDirty(ctx, path) (bool, error).
//   - Bootstrap(ctx, path, cmds, env) error — run project bootstrap commands
//     (via `direnv exec` when available) in a fresh/attached tree.
//   - Refresh(ctx, RefreshOpts) (RefreshResult, error) — rebase a RUNNING
//     clean worktree onto an advanced base when behind >= Threshold AND the
//     base delta overlaps the branch footprint; dirty tree → "deferred-dirty";
//     conflict → abort rebase, write CONFLICT.md, return "conflict".
//   - Remove(ctx, path, force) error — REFUSES dirty worktrees unless force;
//     force is only ever set after explicit human approval upstream.
//   - PatchSnapshot(ctx, path, outDir) (patchPath, error) — `git diff` (+
//     untracked via `git add -N` trick) for WIP preservation.
package worktree

// Info describes one worktree.
type Info struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
	Dirty  bool   `json:"dirty"`
	// Created reports whether Ensure created the worktree (true) or attached
	// to a pre-existing one (false). Zero value on List/Refresh results.
	Created bool `json:"created,omitempty"`
}

// EnsureOpts requests a worktree for a bead.
type EnsureOpts struct {
	RepoRoot     string
	WorktreeRoot string // default <parent>/<repo>-worktrees
	Branch       string // e.g. agent/<bead-id>
	Base         string // e.g. main
	Name         string // dir name; default = branch with '/'→'-'
}

// RefreshOpts controls drift refresh.
type RefreshOpts struct {
	RepoRoot  string
	Path      string
	Branch    string
	Base      string
	Threshold int // commits behind before refresh considered (default 5)
	CheckOnly bool
	Force     bool
}

// RefreshResult reports what happened.
type RefreshResult struct {
	Action  string `json:"action"` // none|advised|refreshed|deferred-dirty|conflict
	Behind  int    `json:"behind"`
	Overlap bool   `json:"overlap"`
}

// BranchFor returns the canonical branch name for a bead.
func BranchFor(beadID string) string { return "agent/" + beadID }
