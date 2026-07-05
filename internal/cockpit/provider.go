// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"context"
	"time"
)

// Provider assembles a Snapshot for one project on demand. Implementations
// are expected to be cheap (reading from files already on disk) and are called
// on every refresh tick (default 100 ms).
//
// Refresh is NOT expected to be goroutine-safe; callers must serialize calls.
type Provider interface {
	// Refresh assembles and returns a fresh Snapshot.
	Refresh() (Snapshot, error)

	// ProjectID returns the project this provider is bound to.
	ProjectID() string
}

// DetailProvider is an optional extension of Provider. When a Provider also
// implements DetailProvider, the TUI calls BeadDetail asynchronously when the
// user navigates to the Detail tab so that the panel is populated rather than
// showing a permanent "Loading…" placeholder.
//
// Implementations MUST be safe to call from a goroutine other than the Bubble
// Tea event loop.
type DetailProvider interface {
	BeadDetail(ctx context.Context, beadID string, now time.Time) BeadDetailSnapshot
}
