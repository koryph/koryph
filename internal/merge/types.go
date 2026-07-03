// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package merge lands a finished agent branch on the default branch:
// merge-slot acquire (bd mutex) → protected-path check → sync local <def> to
// origin (fetch + ff) → rebase the candidate onto that same local <def> →
// green gate AFTER rebase → ff-only merge → optional push → cleanup (worktree
// + branch). Rebase base and ff-merge target are deliberately the SAME ref so
// a locally-diverged default branch cannot make the fast-forward impossible.
//
// Implementation contract (merge.go, protected.go, gate.go):
//   - Merge(ctx, Opts) (Result, error). On gate failure: reset the worktree
//     to HEAD (pre-commit auto-fixers leave it dirty), release the slot,
//     return the gate output tail. On rebase conflict: abort, write
//     CONFLICT.md into the run dir, status "conflict", never auto-resolve.
//   - Protected(diffPaths, extra) []string — engine defaults (below) plus
//     project extras; any hit rejects the merge outright.
//   - Gate commands run in the worktree via `direnv exec` when available,
//     sequentially, all must exit 0.
//   - Cleanup removes worktree + branch ONLY after a successful merge and
//     never when the tree is dirty (Result records what was kept).
//   - Close-on-merge belongs to the ENGINE (direct mode), not this package;
//     Merge only reports success.
package merge

// DefaultProtected are path prefixes that may never be merged from a
// worktree in any managed project (they control what agents may do).
var DefaultProtected = []string{
	"CLAUDE.md",
	"MEMORY.md",
	"CLAUDE-ACCOUNTS.md",
	"koryph.project.json",
	".claude/",
	".beads/",
	"scripts/lib/",
	".pre-commit-config.yaml",
	".gitignore",
	".github/CODEOWNERS",
	".envrc",
}

// Opts configures one merge.
type Opts struct {
	RepoRoot      string
	Branch        string
	DefaultBranch string   // e.g. main
	Gate          []string // project green-gate commands
	Extra         []string // project extra protected paths
	Squash        bool
	KeepWorktree  bool
	SkipGate      bool // validate-only paths; never set by the loop
	Push          bool // git push origin <default> after merge
	SlotOwner     string
	SlotRetries   int // bd merge-slot acquire retries (default 3)

	// RequireSigned verifies every commit on <default>..<branch> carries a
	// good signature (git %G? == 'G') BEFORE any mutation. Verification must
	// precede the rebase: a rebase rewrites commits and — with
	// commit.gpgsign set — would re-sign them with the merge runner's key,
	// laundering unsigned agent work. Failures return
	// Result{Status: "unsigned"} listing the offending SHAs; the tree is
	// untouched and the merge slot is released.
	RequireSigned bool

	// Slot serializes concurrent merges (bd mutex in production). When nil,
	// no cross-process locking is performed. Acquired at the start of Merge
	// and released on every exit path.
	Slot SlotLocker
}

// Result reports a merge attempt.
type Result struct {
	Status     string   `json:"status"` // merged|conflict|gate-failed|protected|unsigned|error
	MergedSHA  string   `json:"merged_sha,omitempty"`
	GateOutput string   `json:"gate_output,omitempty"`
	Protected  []string `json:"protected_paths,omitempty"`
	ConflictMD string   `json:"conflict_md,omitempty"`
	Pushed     bool     `json:"pushed"`
}
