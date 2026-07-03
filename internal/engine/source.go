// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"

	"github.com/koryph/koryph/internal/beads"
)

// WorkSource is the task-graph the wave loop reads, claims, closes, and
// serializes merges against. *beads.Adapter (the bd CLI) is the only
// implementation today, but holding the loop against this interface — rather
// than the concrete adapter — is what lets the engine be unit-tested without a
// real `bd` binary and keeps a future non-bd tracker a drop-in. The bd-specific
// merge mutex (MergeSlotAcquire/Release, modeled as a bd claim) is part of the
// contract because the loop depends on it for single-writer merges.
type WorkSource interface {
	Ready(ctx context.Context, opts beads.ReadyOpts) ([]beads.Issue, error)
	Show(ctx context.Context, id string) (beads.Issue, error)
	ListChildren(ctx context.Context, id string) ([]beads.Issue, error)
	Claim(ctx context.Context, id string) error
	Close(ctx context.Context, id, reason string) error
	Comment(ctx context.Context, id, text string) error
	SetStatus(ctx context.Context, id, status string) error
	MergeSlotAcquire(ctx context.Context, slotID, owner string, retries int) error
	MergeSlotRelease(ctx context.Context, slotID string) error
}

// Compile-time assertion that the bd adapter satisfies the interface.
var _ WorkSource = (*beads.Adapter)(nil)
