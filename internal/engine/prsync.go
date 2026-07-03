// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

// PRSyncOutcome is one reconciled pr-opened bead.
type PRSyncOutcome struct {
	Bead   string
	Number int
	State  string // OPEN|CLOSED|MERGED|unknown
	Action string // merged|blocked|kept|error
}

// SyncPROpened reconciles engine-opened PRs that ended outside koryph. For each
// pr-opened slot in the latest run it reads the PR state live (so it detects a
// PR closed or merged by ANY means — the GitHub UI, another tool, koryph land,
// or koryph review-pr --close) and:
//
//   - MERGED → mark the slot merged and close the bead (it landed elsewhere);
//   - CLOSED (without merge) → mark the slot blocked (abandoned);
//   - OPEN   → leave it pr-opened.
//
// Nothing is left stranded in pr-opened, and any stale saved review-pr analysis
// for a terminal PR is cleared.
func SyncPROpened(ctx context.Context, rec *registry.Record, host PRHost, out io.Writer) ([]PRSyncOutcome, error) {
	if host == nil {
		host = GhHost{}
	}
	store := ledger.NewStore(rec.Root)
	run, err := store.LoadLatest()
	if err != nil {
		// No runs yet → nothing to reconcile; not an error.
		if out != nil {
			fmt.Fprintln(out, "pr-sync: no runs to reconcile")
		}
		return nil, nil
	}

	adapter := beads.New(rec.Root)
	if v := os.Getenv(envBDBin); v != "" {
		adapter.Bin = v
	}

	var outcomes []PRSyncOutcome
	for id, sl := range run.Slots {
		if sl == nil || sl.Status != ledger.SlotPROpened {
			continue
		}
		oc := PRSyncOutcome{Bead: id}
		meta, ierr := host.Info(ctx, rec.Root, sl.Branch)
		if ierr != nil {
			oc.State, oc.Action = "unknown", "error"
			outcomes = append(outcomes, oc)
			if out != nil {
				fmt.Fprintf(out, "bead %s (%s): PR state unavailable: %v\n", id, sl.Branch, ierr)
			}
			continue
		}
		oc.Number, oc.State = meta.Number, meta.State
		switch meta.State {
		case "MERGED":
			_ = store.UpdateSlot(run, id, func(s *ledger.Slot) {
				s.Status = ledger.SlotMerged
				s.MergedAt = time.Now().UTC().Format(time.RFC3339)
				s.Note = "PR merged externally: " + meta.URL
			})
			_ = adapter.Close(ctx, id, "PR merged: "+meta.URL)
			clearPRState(rec, meta.Number)
			oc.Action = "merged"
		case "CLOSED":
			_ = store.UpdateSlot(run, id, func(s *ledger.Slot) {
				s.Status = ledger.SlotBlocked
				s.Note = "PR closed without merge: " + meta.URL
			})
			clearPRState(rec, meta.Number)
			oc.Action = "blocked"
		default:
			oc.Action = "kept"
		}
		outcomes = append(outcomes, oc)
		if out != nil {
			fmt.Fprintf(out, "bead %s (PR #%d): %s -> %s\n", id, meta.Number, meta.State, oc.Action)
		}
	}
	if out != nil {
		fmt.Fprintf(out, "pr-sync: %d pr-opened bead(s) checked\n", len(outcomes))
	}
	return outcomes, nil
}
