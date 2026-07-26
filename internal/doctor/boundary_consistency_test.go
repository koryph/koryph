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
	"github.com/koryph/koryph/internal/promptc"
)

// The deterministic boundary guard is the sole enforcement owner for
// orchestrator-only commands. Persona files own role behavior and must not
// duplicate that policy. This test holds both sides of that ownership split:
// every boundary operation remains enforced by the hook, while the
// implementer remains a valid role-only contract.

// forbiddenOp is one orchestrator-only operation the boundary forbids.
type forbiddenOp struct {
	command string
	// hookRe is the literal regex fragment the guard matches it with (the guard
	// is a bash script; we assert its source contains this fragment).
	hookRe string
}

var boundaryOps = []forbiddenOp{
	{"git checkout main", "git[[:space:]]+checkout[[:space:]]+(main|master)"},
	{"git switch main", "git[[:space:]]+switch[[:space:]]+(main|master)"},
	{"git merge", "git[[:space:]]+merge"},
	{"git push", "git[[:space:]]+push"},
	{"bd", "([^[:space:]]*/)?bd([[:space:]]|$)"},
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

	if err := promptc.ValidateRoleContract(prose); err != nil {
		t.Fatalf("implementer is not a role-only contract: %v", err)
	}
	for _, op := range boundaryOps {
		if strings.Contains(prose, "`"+op.command+"`") {
			t.Errorf("implementer duplicates engine-owned boundary operation `%s`", op.command)
		}
		if !strings.Contains(guard, op.hookRe) {
			t.Errorf("boundary guard has no matcher %q for forbidden operation `%s`", op.hookRe, op.command)
		}
	}

	// Reverse direction: the guard must not enforce a boundary op that
	// boundaryOps does not know about. Count line-start
	// `deny "` calls — this excludes the `emit_deny "` helper (not line-start
	// once trimmed of its two-space indent? it starts with emit_) and the
	// `deny_nudge "` verbose-command steers (a different function).
	denyLines := regexp.MustCompile(`(?m)^[ \t]*deny "`).FindAllString(guard, -1)
	if want := len(boundaryOps) + guardNonBoundaryDenies; len(denyLines) != want {
		t.Errorf("guard has %d line-start deny calls, want %d (len(boundaryOps)=%d + %d non-boundary config denies); a new boundary deny must be reflected in boundaryOps",
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
