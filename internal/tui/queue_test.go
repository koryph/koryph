// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

// Tests for the Queue tab's resource-deferred rendering (koryph-4ql.10,
// design docs/designs/2026-07-resource-governor.md L7/L4). Mirrors the
// existing footprint-deferred treatment: badge, row style, and filter
// membership.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/cockpit"
)

// resourceDeferredSnap returns a queue snapshot with one resource-deferred
// leaf node carrying a kind and holder, for render assertions.
func resourceDeferredSnap() cockpit.Snapshot {
	node := cockpit.QueueNode{
		Issue:          beads.Issue{ID: "t2", Title: "Needs kind-cluster", IssueType: "task", Status: "open"},
		State:          cockpit.QueueStateResourceDeferred,
		Reason:         "resource kind-cluster at capacity (held by t1)",
		ResourceKind:   "kind-cluster",
		ResourceHolder: "t1",
	}
	return cockpit.Snapshot{
		Queue: cockpit.QueueSnapshot{
			Roots:      []cockpit.QueueNode{node},
			NodeCount:  1,
			ComputedAt: time.Now(),
		},
	}
}

// TestQueueTab_ResourceDeferredBadge verifies the list view renders the
// "res-deferred" badge and the kind/holder reason text, mirroring
// footprint-deferred's "fp-deferred" treatment.
func TestQueueTab_ResourceDeferredBadge(t *testing.T) {
	m := newQueueModel(DefaultTheme())
	m.width = 100
	m.height = 24
	m.SetSnapshot(resourceDeferredSnap())

	out := stripANSI(m.View())
	if !strings.Contains(out, "res-deferred") {
		t.Errorf("expected 'res-deferred' badge in list view; got: %q", out)
	}
	// The list view's Reason column is width-truncated; check the (untruncated)
	// prefix rather than the full reason string.
	if !strings.Contains(out, "resource kind-cluster a") {
		t.Errorf("expected resource deferral reason in list view; got: %q", out)
	}
}

// TestQueueTab_ResourceDeferredDetail verifies the inline detail panel shows
// the structured Resource/Held-by fields.
func TestQueueTab_ResourceDeferredDetail(t *testing.T) {
	m := newQueueModel(DefaultTheme())
	m.width = 100
	m.height = 24
	m.SetSnapshot(resourceDeferredSnap())

	// Open the detail panel on the (only) row.
	m.updateList(tea.KeyMsg{Type: tea.KeyEnter})

	out := stripANSI(m.View())
	if !strings.Contains(out, "Resource:") || !strings.Contains(out, "kind-cluster") {
		t.Errorf("expected Resource: kind-cluster in detail view; got: %q", out)
	}
	if !strings.Contains(out, "Held by:") || !strings.Contains(out, "t1") {
		t.Errorf("expected Held by: t1 in detail view; got: %q", out)
	}
}

// TestQueueTab_ResourceDeferredFilter verifies the "blocked" and "deferred"
// filters include resource-deferred rows, mirroring footprint-deferred's
// membership in both.
func TestQueueTab_ResourceDeferredFilter(t *testing.T) {
	m := newQueueModel(DefaultTheme())
	m.width = 100
	m.height = 24
	m.SetSnapshot(resourceDeferredSnap())

	if !m.stateVisible(cockpit.QueueStateResourceDeferred) {
		t.Error("resource-deferred should be visible under the default 'all' filter")
	}

	m.filter = queueFilterBlocked
	if !m.stateVisible(cockpit.QueueStateResourceDeferred) {
		t.Error("resource-deferred should be visible under the 'blocked' filter (mirrors footprint-deferred)")
	}

	m.filter = queueFilterDeferred
	if !m.stateVisible(cockpit.QueueStateResourceDeferred) {
		t.Error("resource-deferred should be visible under the 'deferred' filter (mirrors footprint-deferred)")
	}

	m.filter = queueFilterRunning
	if m.stateVisible(cockpit.QueueStateResourceDeferred) {
		t.Error("resource-deferred should NOT be visible under the 'running' filter")
	}
}
