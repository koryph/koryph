// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/sched"
)

var commitSHAInReason = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

// applyPrerequisitePolicy prevents a tracker-only "closed" state from
// substituting for code that is absent from the branch selected as the
// dispatch base. It is runtime-independent: every adapter receives the same
// checked git base after this filter.
func (r *runner) applyPrerequisitePolicy(ctx context.Context, issues []beads.Issue) ([]beads.Issue, []sched.Reason) {
	eligible := make([]beads.Issue, 0, len(issues))
	var deferred []sched.Reason
	for _, issue := range issues {
		full := issue
		if len(full.Dependencies) == 0 && full.DependencyCount > 0 && r.adapter != nil {
			if shown, err := r.adapter.Show(ctx, full.ID); err == nil {
				full = shown
			}
		}
		if reason := r.missingPrerequisite(ctx, full); reason != "" {
			deferred = append(deferred, sched.Reason{ID: issue.ID, Title: issue.Title, Reason: reason})
			continue
		}
		// Keep the authoritative Show payload: it includes acceptance criteria
		// and dependency provenance that bd ready may omit.
		eligible = append(eligible, full)
	}
	return eligible, deferred
}

func (r *runner) missingPrerequisite(ctx context.Context, issue beads.Issue) string {
	base := r.rec.DefaultBranch
	if base == "" {
		base = "main"
	}
	for _, dep := range issue.Dependencies {
		if dep.DependencyType != "blocks" {
			continue
		}
		if dep.Status != "closed" {
			return fmt.Sprintf("prerequisite %s is %s, not closed", dep.ID, dep.Status)
		}
		if dep.HasLabel("provenance:non-code") || dep.HasLabel("dependency:non-code") {
			continue
		}
		shas := commitSHAInReason.FindAllString(dep.CloseReason, -1)
		if len(shas) == 0 {
			return fmt.Sprintf(
				"closed prerequisite %s has no landed-commit provenance; close code work as `merged: <sha>` or label a non-code prerequisite `provenance:non-code`",
				dep.ID)
		}
		for _, sha := range shas {
			res, err := execx.Run(ctx, execx.Cmd{
				Dir: r.rec.Root, Name: "git",
				Args: []string{"merge-base", "--is-ancestor", sha, base},
			})
			if err == nil && res.ExitCode == 0 {
				goto landed
			}
		}
		return fmt.Sprintf(
			"closed prerequisite %s commit %s is not present in dispatch base %s; land/reconcile the prerequisite before retrying",
			dep.ID, strings.Join(shas, ","), base)
	landed:
	}
	return ""
}
