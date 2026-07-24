// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

type statefulBlockedSource struct {
	fakeSource
	issues map[string]beads.Issue
}

func (s *statefulBlockedSource) Show(_ context.Context, id string) (beads.Issue, error) {
	return s.issues[id], nil
}

func (s *statefulBlockedSource) SetStatus(_ context.Context, id, status string) error {
	issue := s.issues[id]
	issue.ID = id
	issue.Status = status
	s.issues[id] = issue
	s.setStatus = append(s.setStatus, [2]string{id, status})
	return nil
}

func TestReconcileBlockedBeadIsIdempotent(t *testing.T) {
	source := &statefulBlockedSource{issues: map[string]beads.Issue{
		"b1": {ID: "b1", Status: "in_progress"},
	}}
	r := &runner{
		adapter: source,
		run:     &ledger.Run{RunID: "r1", Slots: map[string]*ledger.Slot{}},
	}
	sl := &ledger.Slot{PhaseID: "b1", Attempts: 2, Model: "standard"}

	r.reconcileBlockedBead(t.Context(), sl, "protected path")
	r.reconcileBlockedBead(t.Context(), sl, "protected path")

	if len(source.setStatus) != 1 {
		t.Errorf("SetStatus calls=%v, want exactly one", source.setStatus)
	}
	if len(source.comments) != 1 {
		t.Errorf("Comment calls=%v, want exactly one", source.comments)
	}
}

func TestPatrolReportsCleanTerminalBlockedWorktreeAndMismatch(t *testing.T) {
	f := newFixture(t, fixOpts{})
	wt := filepath.Join(f.wtRoot, "blocked")
	runGit(t, f.repo, "worktree", "add", "-b", "agent/blocked", wt, "main")
	source := &statefulBlockedSource{issues: map[string]beads.Issue{
		"b1": {ID: "b1", Status: "in_progress"},
	}}
	r := &runner{
		rec:     &registry.Record{Root: f.repo, DefaultBranch: "main"},
		adapter: source,
		run: &ledger.Run{Slots: map[string]*ledger.Slot{
			"b1": {PhaseID: "b1", Branch: "agent/blocked", Worktree: wt, Status: ledger.SlotBlocked},
		}},
	}

	findings := r.patrolCheckStaleWorktrees(t.Context())
	if len(findings) != 1 || findings[0].level != "warn" {
		t.Fatalf("findings=%+v, want one warning", findings)
	}
	msg := findings[0].message
	for _, want := range []string{"ledger/Bead mismatch", "b1", "agent/blocked", wt, "git -C", "bd update b1 --status open", "dirty=false"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "remove --force") {
		t.Errorf("blocked-worktree recovery recommended destructive removal: %s", msg)
	}
}
