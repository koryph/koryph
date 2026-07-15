// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import "testing"

// TestAllLiftable pins koryph-zfg (F2): only touches confined to the liftable
// subset (.github/, Makefile) report liftable; any governance default drags the
// whole touch into manual-review territory.
func TestAllLiftable(t *testing.T) {
	cases := []struct {
		name string
		hits []string
		want bool
	}{
		{"empty is not liftable", nil, false},
		{"only Makefile", []string{"Makefile"}, true},
		{"only .github file", []string{".github/workflows/ci.yml"}, true},
		{"github and makefile", []string{".github/workflows/ci.yml", "Makefile"}, true},
		{"includes CLAUDE.md", []string{"Makefile", "CLAUDE.md"}, false},
		{"only a governance default", []string{".claude/settings.json"}, false},
	}
	for _, tc := range cases {
		if got := AllLiftable(tc.hits); got != tc.want {
			t.Errorf("%s: AllLiftable(%v) = %v, want %v", tc.name, tc.hits, got, tc.want)
		}
	}
}

// TestConventionalAcceptsRevert pins koryph-aw9 (F3): a structural revert(scope):
// subject passes the commit-style gate, while git's default capitalized
// `Revert "..."` (no conventional type) is still rejected.
func TestConventionalAcceptsRevert(t *testing.T) {
	if bad := nonConventionalSubjects([]string{
		"revert(gamedef): drop bad tuning",
		"revert: undo the migration",
	}); len(bad) != 0 {
		t.Errorf("revert subjects wrongly rejected: %v", bad)
	}
	if bad := nonConventionalSubjects([]string{`Revert "feat: a"`}); len(bad) != 1 {
		t.Errorf("git default `Revert \"...\"` should still be non-conventional, got %v", bad)
	}
}
