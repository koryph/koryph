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
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/worktree"
)

// LandOpts configures Land.
type LandOpts struct {
	Bead   string    // bead whose engine-opened PR to land (required)
	Method string    // "" → project merge_method (default ff)
	Reason string    // close reason for the bead
	Out    io.Writer // human-readable notices; nil = silent

	// AllowProtected lifts merge.LiftableProtected (.github/, Makefile) for
	// this one landing (koryph-dcn). Set exclusively by the `koryph land
	// --allow-protected` CLI flag — Land is only ever invoked by cmdLand, an
	// explicit operator action; the engine's auto-merge poll loop neither
	// calls Land nor may it ever set merge.Opts.AllowProtected.
	AllowProtected bool
}

// LandResult reports a landing attempt.
type LandResult struct {
	Status string `json:"status"` // merged|conflict|gate-failed|protected|unsigned|commit-style|<other>
	SHA    string `json:"sha,omitempty"`
	Branch string `json:"branch"`
}

// Land fast-forward-merges an engine-opened PR (a pr-opened bead) onto the
// default branch, preserving the gate-checked, SSH-signed commit SHAs, then
// closes the bead. It reuses merge.Merge's sync + rebase + gate + ff + push +
// cleanup primitives (mechanism (a): local ff + push by a bypass-authorized
// identity — the only method that keeps signatures intact).
//
// A moved base is rebased and re-verified inside merge.Merge; a genuine
// conflict is reported (status "conflict") and never rewrite-merged. A
// signature-breaking method (squash) is refused up front while signing is
// required.
func Land(ctx context.Context, rec *registry.Record, cfg *project.Config, o LandOpts) (LandResult, error) {
	if o.Bead == "" {
		return LandResult{}, fmt.Errorf("land: bead is required")
	}
	method := o.Method
	if method == "" {
		method = cfg.LandMethod()
	}
	if err := cfg.LandMethodError(method); err != nil {
		return LandResult{}, err
	}

	// Resolve the branch: prefer the recorded slot (land exactly what the
	// pr-opened run parked), else the canonical agent/<bead> name.
	branch := worktree.BranchFor(o.Bead)
	store := ledger.NewStore(rec.Root)
	run, runErr := store.LoadLatest()
	if runErr == nil && run != nil {
		if sl := run.Slots[o.Bead]; sl != nil && sl.Branch != "" {
			branch = sl.Branch
		}
	}

	res, err := merge.Merge(ctx, merge.Opts{
		RepoRoot:            rec.Root,
		Branch:              branch,
		DefaultBranch:       rec.DefaultBranch,
		Gate:                cfg.Gate,
		Extra:               cfg.ProtectedPaths,
		Squash:              method == "squash",
		Push:                true,
		SlotOwner:           "land:" + o.Bead,
		SlotRetries:         3,
		RequireSigned:       cfg.Signing != nil && cfg.Signing.Required,
		RequireConventional: cfg.EnforceConventional(),
		AllowProtected:      o.AllowProtected,
	})
	if err != nil {
		return LandResult{Status: string(res.Status), Branch: branch}, err
	}
	if res.Status != merge.StatusMerged {
		// Base moved with a real conflict, a gate regression, etc. Never fall
		// back to a rewrite merge — report so the caller can rebase and rerun.
		return LandResult{Status: string(res.Status), Branch: branch}, nil
	}

	// Landed: mark the parked slot merged (best-effort) and close the bead.
	if runErr == nil && run != nil {
		_ = store.UpdateSlot(run, o.Bead, func(s *ledger.Slot) {
			s.Status = ledger.SlotMerged
			s.MergedAt = time.Now().UTC().Format(time.RFC3339)
		})
	}
	adapter := beads.New(rec.Root)
	if v := os.Getenv(envBDBin); v != "" {
		adapter.Bin = v
	}
	if cerr := adapter.Close(ctx, o.Bead, o.Reason); cerr != nil && o.Out != nil {
		fmt.Fprintln(o.Out, "land: warning: close bead failed:", cerr)
	}
	return LandResult{Status: string(merge.StatusMerged), SHA: res.MergedSHA, Branch: branch}, nil
}
