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

import "context"

// DefaultProtected are path prefixes that may never be merged from a
// worktree in any managed project (they control what agents may do). These are
// hardcoded rather than left to per-project opt-in: a project that forgot to
// list .github/ or Makefile would otherwise let an agent land a CI workflow or
// gate Makefile that runs on the next CI/build.
var DefaultProtected = []string{
	"CLAUDE.md",
	"MEMORY.md",
	"AGENTS.md",
	"CLAUDE-ACCOUNTS.md",
	"koryph.project.json",
	".claude/",
	".beads/",
	"hooks/",
	"agents/",
	".github/",
	"Makefile",
	"scripts/lib/",
	".pre-commit-config.yaml",
	".gitignore",
	".envrc",
}

// LiftableProtected is the subset of DefaultProtected an operator may
// consciously lift with `koryph merge --allow-protected` / `koryph land
// --allow-protected` (koryph-dcn): routine CI/build surfaces whose changes
// are a legitimate bead outcome. Everything else in DefaultProtected —
// agent-governance files (CLAUDE.md, agents/, hooks/, .claude/),
// project/tracker state (koryph.project.json, .beads/), and gate plumbing —
// stays refused even under the flag, as do the project's extra protected
// paths. The engine's auto-merge path never sets AllowProtected at all: the
// protected gate is the agent sandbox, and only an explicit operator CLI
// invocation may lift its liftable part.
var LiftableProtected = []string{
	".github/",
	"Makefile",
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

	// AllowProtected lifts ONLY the LiftableProtected subset of the
	// protected-path preflight (koryph-dcn) — routine CI/build paths
	// (.github/, Makefile) an operator explicitly chooses to land. Diffs
	// touching any other DefaultProtected entry or a project Extra path are
	// still refused. Set exclusively by the `koryph merge`/`koryph land`
	// CLI flags; the engine's auto-merge path MUST never set it — dispatched
	// agents do not get to lift their own sandbox.
	AllowProtected bool

	// RequireConventional rejects the merge when any commit subject in
	// <default>..<branch> fails the Conventional Commits grammar
	// (type(scope): subject). Read-only and BEFORE any mutation, like the
	// protected-path and signature checks. Failures return
	// Result{Status: "commit-style"} listing the offending subjects; the
	// engine bounces the bead back to the implementer. Enforcement is a
	// project-config default (commit_style), disabled only by opt-out.
	RequireConventional bool

	// Slot serializes concurrent merges (bd mutex in production). When nil,
	// no cross-process locking is performed. Acquired at the start of Merge
	// and released on every exit path.
	Slot SlotLocker

	// OpenPR diverges after the shared preflight (slot, protected-path,
	// signing, sync, rebase, gate): instead of fast-forward merging into the
	// default branch, it pushes o.Branch to origin and opens a pull request
	// against the default branch. The worktree and branch are always kept
	// alive so a later fast-forward landing step can resume them. Used for
	// protected default branches (merge_policy=pr).
	OpenPR  bool
	PRTitle string // conventional-commit-shaped PR title
	PRBody  string // PR body (bead id, title, acceptance criteria)

	// PR opens the pull request on the OpenPR path. A nil PR defaults to the
	// gh CLI (GhCLI); tests inject a fake to avoid a real GitHub round-trip.
	PR PROpener
}

// PROpener publishes a pull request for a pushed branch. The gh-CLI default
// (GhCLI) shells out to `gh`; the interface exists so tests can substitute a
// fake without a live GitHub remote.
type PROpener interface {
	// Ready reports whether pull requests can be opened from dir (the PR host
	// CLI is installed and authenticated).
	Ready(ctx context.Context, dir string) bool
	// Open returns the existing OPEN pull request for branch, or creates one
	// against base, and reports its URL and number.
	Open(ctx context.Context, dir, branch, base, title, body string) (url string, number int, err error)
}

// Result reports a merge attempt.
// Status is the outcome of a merge attempt. The constants below are the whole
// vocabulary — producers and the engine's consumer switches use them so a typo
// is a compile error instead of a silent fall-through to "merge failed".
type Status string

const (
	StatusMerged      Status = "merged"       // landed on the default branch
	StatusPROpened    Status = "pr-opened"    // PR opened (merge_policy pr)
	StatusConflict    Status = "conflict"     // rebase conflict; CONFLICT.md written
	StatusGateFailed  Status = "gate-failed"  // green gate failed after rebase
	StatusProtected   Status = "protected"    // diff touched a protected path
	StatusUnsigned    Status = "unsigned"     // required signature missing
	StatusCommitStyle Status = "commit-style" // non-conventional commit subject
	StatusPRNoRemote  Status = "pr-no-remote" // merge_policy pr but no git remote
	StatusPRNoGH      Status = "pr-no-gh"     // merge_policy pr but gh unavailable
	StatusError       Status = "error"        // infrastructure failure (see error)
)

type Result struct {
	Status     Status   `json:"status"`
	MergedSHA  string   `json:"merged_sha,omitempty"`
	GateOutput string   `json:"gate_output,omitempty"`
	Protected  []string `json:"protected_paths,omitempty"`
	ConflictMD string   `json:"conflict_md,omitempty"`
	Pushed     bool     `json:"pushed"`
	PRURL      string   `json:"pr_url,omitempty"`
	PRNumber   int      `json:"pr_number,omitempty"`
}
