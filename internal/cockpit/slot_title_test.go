// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"testing"

	"github.com/koryph/koryph/internal/beads"
)

// TestEnrichSlotTitles verifies slot display titles are upgraded from the queue
// cache's real bd titles, keyed by bead id, and that a bead absent from the
// queue keeps its id-based fallback so the Threads tab can blank the redundant
// description.
func TestEnrichSlotTitles(t *testing.T) {
	qs := QueueSnapshot{
		Roots: []QueueNode{
			{
				Issue: beads.Issue{ID: "e1", Title: "Cockpit epic"},
				Children: []QueueNode{
					{Issue: beads.Issue{ID: "koryph-2im.9", Title: "footprint persistence"}},
				},
			},
			{Issue: beads.Issue{ID: "t3", Title: "standalone task"}},
		},
	}
	slots := []SlotSnapshot{
		{BeadID: "koryph-2im.9", Title: "koryph-2im.9"}, // id fallback → real title
		{BeadID: "t3", Title: "t3"},                     // id fallback → real title
		{BeadID: "unknown-1", Title: "unknown-1"},       // not in queue → stays id
		{PhaseID: "md-phase", Title: "md-phase"},        // markdown phase, no bead id
	}
	enrichSlotTitles(slots, qs)

	if slots[0].Title != "footprint persistence" {
		t.Errorf("nested child: Title = %q, want real title", slots[0].Title)
	}
	if slots[1].Title != "standalone task" {
		t.Errorf("root task: Title = %q, want real title", slots[1].Title)
	}
	if slots[2].Title != "unknown-1" {
		t.Errorf("absent bead: Title = %q, want unchanged id fallback", slots[2].Title)
	}
	if slots[3].Title != "md-phase" {
		t.Errorf("markdown phase: Title = %q, want unchanged", slots[3].Title)
	}
}

// TestEnrichSlotTitles_EmptyQueueNoop verifies no panic / no change when the
// queue cache is empty (before the first derived refresh).
func TestEnrichSlotTitles_EmptyQueueNoop(t *testing.T) {
	slots := []SlotSnapshot{{BeadID: "x", Title: "x"}}
	enrichSlotTitles(slots, QueueSnapshot{})
	if slots[0].Title != "x" {
		t.Errorf("Title = %q, want unchanged", slots[0].Title)
	}
}
