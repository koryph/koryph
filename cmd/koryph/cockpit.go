// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// cmdCockpit implements `koryph cockpit --project ID [--json]`.
//
// This command is the SOLE entry point through which the VS Code extension
// reads agent and project state. It calls cockpit.NewLedgerProvider.Refresh()
// and serialises the result, so both the terminal TUI (internal/tui) and the
// VS Code panel consume the SAME view-model with IDENTICAL caching semantics.
//
// Design constraint: the extension MUST NOT add new direct
// ledger.Store / govern.Store / beads.Adapter / quota.LoadConfig calls to
// its data layer.  All data assembly lives here, via cockpit.LedgerProvider.
// See docs/developer-guide/ide-setup.md §"Data layer" for the rationale.
package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/koryph/koryph/internal/cockpit"
)

func init() {
	registerCmd(command{
		name:    "cockpit",
		summary: "emit a cockpit snapshot for the VS Code extension",
		run:     cmdCockpit,
		DocLinks: []string{
			"developer-guide/ide-setup.md",
		},
	})
}

// ---------------------------------------------------------------------------
// Wire types — JSON-serialisable snapshot emitted to the extension.
// These mirror cockpit.Snapshot fields but carry json tags so the extension
// can parse them without knowing Go's reflect conventions.
// ---------------------------------------------------------------------------

// CockpitSnapshot is the top-level JSON document returned by `koryph cockpit
// --json`.  The extension MUST consume all agent/project state from this
// document; it MUST NOT read ledger/govern/quota files directly.
type CockpitSnapshot struct {
	ProjectID  string        `json:"project_id"`
	RunID      string        `json:"run_id,omitempty"`
	RunStatus  string        `json:"run_status,omitempty"`
	Wave       int           `json:"wave"`
	Slots      []CockpitSlot `json:"slots"`
	Governor   CockpitGov    `json:"governor"`
	CapturedAt time.Time     `json:"captured_at"`
}

// CockpitSlot is one slot's view-model as emitted by the cockpit command.
type CockpitSlot struct {
	PhaseID      string  `json:"phase_id"`
	BeadID       string  `json:"bead_id,omitempty"`
	Stage        string  `json:"stage"`
	Model        string  `json:"model,omitempty"`
	Attempt      int     `json:"attempt"`
	PID          int     `json:"pid,omitempty"`
	Branch       string  `json:"branch,omitempty"`
	Worktree     string  `json:"worktree,omitempty"`
	CostUSD      float64 `json:"cost_usd"`
	EstimateUSD  float64 `json:"estimate_usd,omitempty"`
	StatusLine   string  `json:"status_line,omitempty"`
	StatusJSON   string  `json:"status_json,omitempty"`
	DispatchedAt string  `json:"dispatched_at,omitempty"` // RFC3339 or ""
	ElapsedSec   float64 `json:"elapsed_sec,omitempty"`
}

// CockpitGov is the machine-wide governor state as emitted by the cockpit
// command.
type CockpitGov struct {
	Pools map[string]CockpitPool `json:"pools"`
}

// CockpitPool is one governor pool's observable state.
type CockpitPool struct {
	Provider     string `json:"provider"`
	Cap          int    `json:"cap"`
	Dynamic      int    `json:"dynamic"`
	Adaptive     bool   `json:"adaptive"`
	Leases       int    `json:"leases"`
	BreakerState string `json:"breaker_state,omitempty"`
}

// ---------------------------------------------------------------------------
// Command
// ---------------------------------------------------------------------------

// cmdCockpit is the `koryph cockpit` implementation.
func cmdCockpit(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("cockpit", stderr)
	projectID := fs.String("project", "", "project id (required)")
	asJSON := fs.Bool("json", false, "emit snapshot as JSON (used by the VS Code extension)")
	setUsage(fs, stdout,
		"emit a cockpit snapshot for one project — use --json for the VS Code extension",
		"--project ID [--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *projectID == "" {
		return usageErr(stderr, "cockpit: --project is required")
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, err := store.Get(*projectID)
	if err != nil {
		return fail(stderr, fmt.Errorf("cockpit: project %q not found: %w", *projectID, err))
	}

	provider := cockpit.NewLedgerProvider(rec.ProjectID, rec.Root, rec.AccountProfile)
	snap, err := provider.Refresh()
	if err != nil {
		return fail(stderr, fmt.Errorf("cockpit: refresh failed: %w", err))
	}

	out := snapshotToWire(snap)

	if *asJSON {
		if err := printJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}

	// Human-readable summary.
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "PROJECT\t%s\n", out.ProjectID)
	if out.RunID != "" {
		fmt.Fprintf(tw, "RUN\t%s  %s  wave %d\n", out.RunID, out.RunStatus, out.Wave)
	} else {
		fmt.Fprintf(tw, "RUN\t(no active run)\n")
	}
	fmt.Fprintf(tw, "SLOTS\t%d total\n", len(out.Slots))
	for _, s := range out.Slots {
		cost := ""
		if s.CostUSD > 0 {
			cost = fmt.Sprintf("  $%.2f", s.CostUSD)
		}
		fmt.Fprintf(tw, "  %-20s\t%s  %s%s\n", s.PhaseID, s.Stage, s.Model, cost)
	}
	poolCount := 0
	poolLeases := 0
	for _, p := range out.Governor.Pools {
		poolCount++
		poolLeases += p.Leases
	}
	fmt.Fprintf(tw, "GOVERNOR\t%d pool(s)  %d lease(s)\n", poolCount, poolLeases)
	fmt.Fprintf(tw, "CAPTURED\t%s\n", out.CapturedAt.Format(time.RFC3339))
	tw.Flush()
	return 0
}

// snapshotToWire maps a cockpit.Snapshot to the JSON wire type.
func snapshotToWire(snap cockpit.Snapshot) CockpitSnapshot {
	slots := make([]CockpitSlot, 0, len(snap.Slots))
	for _, s := range snap.Slots {
		cs := CockpitSlot{
			PhaseID:     s.PhaseID,
			BeadID:      s.BeadID,
			Stage:       s.Stage,
			Model:       s.Model,
			Attempt:     s.Attempt,
			PID:         s.PID,
			Branch:      s.Branch,
			Worktree:    s.Worktree,
			CostUSD:     s.CostUSD,
			EstimateUSD: s.EstimateUSD,
			StatusLine:  s.StatusLine,
			StatusJSON:  s.StatusJSON,
		}
		if !s.DispatchedAt.IsZero() {
			cs.DispatchedAt = s.DispatchedAt.UTC().Format(time.RFC3339)
			cs.ElapsedSec = s.Elapsed.Seconds()
		}
		slots = append(slots, cs)
	}

	pools := make(map[string]CockpitPool, len(snap.Governor.Pools))
	for k, p := range snap.Governor.Pools {
		pools[k] = CockpitPool{
			Provider:     p.Provider,
			Cap:          p.Cap,
			Dynamic:      p.Dynamic,
			Adaptive:     p.Adaptive,
			Leases:       p.Leases,
			BreakerState: p.BreakerState,
		}
	}

	return CockpitSnapshot{
		ProjectID:  snap.ProjectID,
		RunID:      snap.RunID,
		RunStatus:  snap.RunStatus,
		Wave:       snap.Wave,
		Slots:      slots,
		Governor:   CockpitGov{Pools: pools},
		CapturedAt: snap.CapturedAt,
	}
}
