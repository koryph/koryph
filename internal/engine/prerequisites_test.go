// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/registry"
)

func prerequisiteRunner(t *testing.T) (*runner, string, string) {
	t.Helper()
	f := newFixture(t, fixOpts{})
	landed := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main"))
	runGit(t, f.repo, "checkout", "-b", "unlanded")
	writeFile(t, filepath.Join(f.repo, "unlanded.txt"), "work\n", 0o644)
	runGit(t, f.repo, "add", "unlanded.txt")
	runGit(t, f.repo, "commit", "--no-verify", "-m", "feat(dep): unlanded")
	unlanded := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "HEAD"))
	runGit(t, f.repo, "checkout", "main")
	return &runner{rec: &registry.Record{Root: f.repo, DefaultBranch: "main"}}, landed, unlanded
}

func TestMissingPrerequisiteRequiresCommitOnBase(t *testing.T) {
	r, landed, unlanded := prerequisiteRunner(t)
	base := beads.Issue{ID: "work", Dependencies: []beads.Issue{{
		ID: "dep", Status: "closed", DependencyType: "blocks",
	}}}

	cases := []struct {
		name   string
		reason string
		labels []string
		want   string
	}{
		{"landed", "merged: " + landed, nil, ""},
		{"unlanded", "implemented commit " + unlanded, nil, "not present in dispatch base"},
		{"legacy missing provenance", "implemented manually", nil, "no landed-commit provenance"},
		{"explicit non-code", "decision recorded", []string{"provenance:non-code"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := base
			issue.Dependencies = append([]beads.Issue(nil), base.Dependencies...)
			issue.Dependencies[0].CloseReason = tc.reason
			issue.Dependencies[0].Labels = tc.labels
			got := r.missingPrerequisite(context.Background(), issue)
			if tc.want == "" && got != "" {
				t.Errorf("unexpected deferral: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("reason=%q, want substring %q", got, tc.want)
			}
		})
	}
}

func TestMissingPrerequisiteIgnoresParentChildRelationship(t *testing.T) {
	r, _, _ := prerequisiteRunner(t)
	issue := beads.Issue{ID: "work", Dependencies: []beads.Issue{{
		ID: "epic", Status: "open", DependencyType: "parent-child",
	}}}
	if got := r.missingPrerequisite(context.Background(), issue); got != "" {
		t.Errorf("parent-child relationship deferred dispatch: %s", got)
	}
}
