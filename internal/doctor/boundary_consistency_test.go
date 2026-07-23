// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"embed"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/koryph/koryph/agents"
	"github.com/koryph/koryph/hooks"
)

// The dispatched-agent orchestrator boundary is expressed in two otherwise
// uncoordinated places (koryph-fiv finding #7):
//   - the human-facing prose contract in agents/koryph-implementer.md ("Never
//     run `git checkout main`, ...")
//   - the deterministic PreToolUse regex in hooks/agent-boundary-guard.sh
//
// Nothing but this cross-test tied them together, so a forbidden op could be
// added to one and silently forgotten in the other — the prose could promise a
// guardrail the hook does not enforce, or the hook could block a command the
// contract never warned the agent about. boundaryOps is the single shared
// source: every op here must be named in the prose AND matched by the guard,
// and the guard must carry no boundary deny that is not listed here.
//
// Both files live under protected paths (agents/, hooks/) that a worktree
// merge refuses, so this test reads them through their Go embeds
// (agents.FS/hooks.FS) rather than editing or generating either file.

// forbiddenOp is one orchestrator-only operation the boundary forbids.
type forbiddenOp struct {
	// prose is the exact command as backticked in the agent contract.
	prose string
	// hookRe is the literal regex fragment the guard matches it with (the guard
	// is a bash script; we assert its source contains this fragment).
	hookRe string
}

var boundaryOps = []forbiddenOp{
	{"git checkout main", "git[[:space:]]+checkout[[:space:]]+(main|master)"},
	{"git switch main", "git[[:space:]]+switch[[:space:]]+(main|master)"},
	{"git merge", "git[[:space:]]+merge"},
	{"git push", "git[[:space:]]+push"},
	{"bd close", "bd[[:space:]]+close"},
	{"gh pr merge", "gh[[:space:]]+pr[[:space:]]+merge"},
}

// guardNonBoundaryDenies is the count of line-start `deny "..."` calls in the
// guard that are NOT one of boundaryOps — today the two git-config
// persistence/signing-trust vector denies. If the guard grows a new boundary
// command deny, the total count changes and this test fails until boundaryOps
// (and the prose) are updated in lockstep.
const guardNonBoundaryDenies = 2

func TestAgentBoundaryProseMatchesHook(t *testing.T) {
	prose := mustReadEmbed(t, agents.FS, "koryph-implementer.md")
	guard := mustReadEmbed(t, hooks.FS, "agent-boundary-guard.sh")

	for _, op := range boundaryOps {
		if !strings.Contains(prose, "`"+op.prose+"`") {
			t.Errorf("agents/koryph-implementer.md does not name forbidden op `%s` — prose/hook drift; update the contract or boundaryOps", op.prose)
		}
		if !strings.Contains(guard, op.hookRe) {
			t.Errorf("hooks/agent-boundary-guard.sh has no matcher %q for forbidden op `%s` — prose/hook drift; update the guard or boundaryOps", op.hookRe, op.prose)
		}
	}

	// Reverse direction: the guard must not enforce a boundary op that
	// boundaryOps (hence the prose) does not know about. Count line-start
	// `deny "` calls — this excludes the `emit_deny "` helper (not line-start
	// once trimmed of its two-space indent? it starts with emit_) and the
	// `deny_nudge "` verbose-command steers (a different function).
	denyLines := regexp.MustCompile(`(?m)^[ \t]*deny "`).FindAllString(guard, -1)
	if want := len(boundaryOps) + guardNonBoundaryDenies; len(denyLines) != want {
		t.Errorf("guard has %d line-start deny calls, want %d (len(boundaryOps)=%d + %d non-boundary config denies); a new boundary deny must be reflected in boundaryOps and the agent prose",
			len(denyLines), want, len(boundaryOps), guardNonBoundaryDenies)
	}
}

func mustReadEmbed(t *testing.T, fsys embed.FS, name string) string {
	t.Helper()
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatalf("read embedded %q: %v", name, err)
	}
	return string(data)
}
