// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package epicreview

import (
	"context"
	"fmt"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/project"
)

// fakeBeadStore is a minimal in-memory BeadStore for exercising Act directly,
// without the full engine runner or a real bd subprocess.
type fakeBeadStore struct {
	children map[string][]beads.Issue
	created  []beads.CreateInput
	closed   []string // "<id>:<reason>"
	labels   []string // "<id>:<label>"
}

func (f *fakeBeadStore) Show(context.Context, string) (beads.Issue, error) {
	return beads.Issue{}, fmt.Errorf("not implemented")
}
func (f *fakeBeadStore) ListChildren(ctx context.Context, id string) ([]beads.Issue, error) {
	return f.ListChildrenAll(ctx, id)
}
func (f *fakeBeadStore) ListChildrenAll(_ context.Context, id string) ([]beads.Issue, error) {
	return f.children[id], nil
}
func (f *fakeBeadStore) Create(_ context.Context, in beads.CreateInput) (string, error) {
	f.created = append(f.created, in)
	id := fmt.Sprintf("created-%d", len(f.created))
	f.children[in.Parent] = append(f.children[in.Parent], beads.Issue{
		ID: id, Title: in.Title, Status: "open", Labels: in.Labels, ParentID: in.Parent,
	})
	return id, nil
}
func (f *fakeBeadStore) Close(_ context.Context, id, reason string) error {
	f.closed = append(f.closed, id+":"+reason)
	return nil
}
func (f *fakeBeadStore) AppendNotes(context.Context, string, string) error { return nil }
func (f *fakeBeadStore) AddLabel(_ context.Context, id, label string) error {
	f.labels = append(f.labels, id+":"+label)
	return nil
}
func (f *fakeBeadStore) RemoveLabel(context.Context, string, string) error { return nil }
func (f *fakeBeadStore) DepAdd(context.Context, string, string) error      { return nil }

func effectiveCfg() project.EpicValidationConfig {
	return (*project.EpicValidationConfig)(nil).Effective()
}

// TestActMetAfterMergedDocsBeadFilesNothing covers koryph-4b50 BUG-2:
// fileDocsBead used to dedup only against OPEN children, so a "met" verdict
// on a later round whose docs bead already merged filed a verbatim
// duplicate every time (koryph-c6j.8 was filed this way). A closed/done
// child matching the canonical title or LabelDocs must now be recognized as
// already satisfying §4b — no new bead.
func TestActMetAfterMergedDocsBeadFilesNothing(t *testing.T) {
	epicID := "ep1"
	store := &fakeBeadStore{
		children: map[string][]beads.Issue{
			epicID: {
				{ID: "docs-1", Title: DocsBeadTitle(epicID), Status: "closed", Labels: []string{LabelDocs}, ParentID: epicID},
			},
		},
	}
	res, err := Act(context.Background(), store, ActOpts{
		EpicID: epicID,
		Round:  2,
		Config: effectiveCfg(),
		Actor:  "test",
	}, Verdict{Met: true, Summary: "nothing new to document"})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(store.created) != 0 {
		t.Errorf("created = %+v, want no new docs bead — the round-1 docs bead already merged", store.created)
	}
	if res.DocsBeadID != "docs-1" {
		t.Errorf("DocsBeadID = %q, want docs-1 (the existing merged bead)", res.DocsBeadID)
	}
	// auto_close defaults true: with nothing pending, the epic should close
	// on this round rather than being left stranded on validation:passed
	// waiting for a `koryph epic validate` re-run to notice.
	if len(store.closed) != 1 || store.closed[0] != epicID+":validated round 2; docs already merged" {
		t.Errorf("closed = %v, want epic closed with a docs-already-merged reason", store.closed)
	}
	if res.Outcome != "met-closed" {
		t.Errorf("Outcome = %q, want met-closed", res.Outcome)
	}
}

// TestActMetOpenDocsBeadStillDefersClose is the control case: an OPEN
// existing docs bead must still defer the close (pre-existing behavior,
// unchanged by the BUG-2 fix) rather than being treated as satisfied.
func TestActMetOpenDocsBeadStillDefersClose(t *testing.T) {
	epicID := "ep1"
	store := &fakeBeadStore{
		children: map[string][]beads.Issue{
			epicID: {
				{ID: "docs-1", Title: DocsBeadTitle(epicID), Status: "open", Labels: []string{LabelDocs}, ParentID: epicID},
			},
		},
	}
	res, err := Act(context.Background(), store, ActOpts{
		EpicID: epicID,
		Round:  1,
		Config: effectiveCfg(),
		Actor:  "test",
	}, Verdict{Met: true, Summary: "all good"})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(store.created) != 0 {
		t.Errorf("created = %+v, want no duplicate docs bead (an open one already exists)", store.created)
	}
	if len(store.closed) != 0 {
		t.Errorf("closed = %v, epic must not close while the docs bead is still open", store.closed)
	}
	if res.Outcome != "met-pending-docs" {
		t.Errorf("Outcome = %q, want met-pending-docs", res.Outcome)
	}
}

// TestActMetNoExistingDocsBeadFilesOne is the baseline case: no matching
// child at all files a fresh docs bead, unchanged by the BUG-2 fix.
func TestActMetNoExistingDocsBeadFilesOne(t *testing.T) {
	epicID := "ep1"
	store := &fakeBeadStore{children: map[string][]beads.Issue{}}
	res, err := Act(context.Background(), store, ActOpts{
		EpicID: epicID,
		Round:  1,
		Config: effectiveCfg(),
		Actor:  "test",
	}, Verdict{Met: true, Summary: "all good"})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("created = %+v, want exactly one docs bead", store.created)
	}
	if res.Outcome != "met-pending-docs" {
		t.Errorf("Outcome = %q, want met-pending-docs", res.Outcome)
	}
}

// --- ClosedAfterDocs -----------------------------------------------------

func TestClosedAfterDocsTrueWhenPassedAndAllChildrenClosed(t *testing.T) {
	epic := beads.Issue{ID: "ep1", Labels: []string{LabelPassed}}
	children := []beads.Issue{
		{ID: "c1", Status: "closed"},
		{ID: "c2", Status: "done"},
	}
	if !ClosedAfterDocs(epic, children) {
		t.Error("want true: passed label + all children terminal")
	}
}

func TestClosedAfterDocsFalseWithoutPassedLabel(t *testing.T) {
	epic := beads.Issue{ID: "ep1"}
	children := []beads.Issue{{ID: "c1", Status: "closed"}}
	if ClosedAfterDocs(epic, children) {
		t.Error("want false: epic never passed validation")
	}
}

func TestClosedAfterDocsFalseWithOpenChild(t *testing.T) {
	epic := beads.Issue{ID: "ep1", Labels: []string{LabelPassed}}
	children := []beads.Issue{{ID: "c1", Status: "closed"}, {ID: "docs-1", Status: "open"}}
	if ClosedAfterDocs(epic, children) {
		t.Error("want false: docs bead still open")
	}
}

func TestClosedAfterDocsFalseWithNoChildren(t *testing.T) {
	epic := beads.Issue{ID: "ep1", Labels: []string{LabelPassed}}
	if ClosedAfterDocs(epic, nil) {
		t.Error("want false: no children at all is not a completion state")
	}
}
