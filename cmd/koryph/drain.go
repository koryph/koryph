// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// cmdDrain writes a one-shot operator drain request for a project (or, with
// --all, every registered project). The engine's shared governorGate check
// (koryph-57v.1, internal/engine/wave.go) re-reads the sentinel at every
// scheduling boundary in BOTH the wave and rolling loops: it stops new
// dispatch, lets whatever is already running finish untouched, and — once
// the last slot lands — exits through the normal drained finalize path,
// consuming the sentinel itself so the NEXT run starts clean.
//
// This is the graceful "finish in-flight, then stop" lever, distinct from
// the process-signaling commands: `koryph stop` (SIGTERM one live agent, or
// SIGKILL with --force) and `koryph stop --all --force` (SIGKILL every
// agent) act on running PROCESSES; drain acts on the LOOP's own dispatch
// decision and never touches a process directly.
func cmdDrain(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("drain", stderr)
	projectID := fs.String("project", "", "project id (required unless --all)")
	all := fs.Bool("all", false, "request a drain for every registered project")
	setUsage(fs, stdout, "request a graceful wind-down: stop new dispatch, finish active slots, exit drained",
		"--project ID | --all")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) > 0 {
		return usageErr(stderr, "drain: takes no positional arguments")
	}
	if *all && *projectID != "" {
		return usageErr(stderr, "drain --all takes no --project")
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}

	if *all {
		records, lerr := store.List()
		if lerr != nil {
			return fail(stderr, lerr)
		}
		n := 0
		for _, rec := range records {
			if derr := requestDrain(store, rec); derr != nil {
				fmt.Fprintf(stderr, "drain %s: %v\n", rec.ProjectID, derr)
				continue
			}
			fmt.Fprintf(stdout, "drain requested for %s\n", rec.ProjectID)
			n++
		}
		fmt.Fprintf(stdout, "drain --all: requested for %d project(s)\n", n)
		return 0
	}

	if *projectID == "" {
		return usageErr(stderr, "drain: --project is required (or use --all)")
	}
	rec, err := store.Get(*projectID)
	if err != nil {
		return fail(stderr, err)
	}
	if err := requestDrain(store, rec); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "drain requested for %s: no new dispatch; active slots will finish, then the run exits (operator-drain)\n", rec.ProjectID)
	return 0
}

// requestDrain writes the sentinel next to the project's run ledger and
// audits the operator action.
func requestDrain(store *registry.Store, rec *registry.Record) error {
	if err := ledger.NewStore(rec.Root).RequestDrain(); err != nil {
		return err
	}
	return store.Audit(registry.Event{
		Kind:      "drain",
		ProjectID: rec.ProjectID,
		Actor:     cliActor(),
	})
}

// cmdResize writes (or clears) a live wave-width override for a project (or,
// with --all, every registered project). Like the drain sentinel, it is
// re-read by the engine's shared governorGate at every boundary
// (koryph-57v.1): a running loop picks up a new width on its very next
// scheduling boundary — no restart needed.
func cmdResize(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("resize", stderr)
	projectID := fs.String("project", "", "project id (required unless --all)")
	all := fs.Bool("all", false, "apply to every registered project")
	max := fs.Int("max", 0, "new width cap (must be > 0; use --clear to remove an override)")
	force := fs.Bool("force", false, "allow --max to exceed the project's max_concurrent_slots")
	clear := fs.Bool("clear", false, "remove the width override (revert to project config)")
	setUsage(fs, stdout, "live wave-width override: re-read by the loop at every boundary, no restart needed",
		"(--project ID | --all) (--max N [--force] | --clear)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) > 0 {
		return usageErr(stderr, "resize: takes no positional arguments")
	}
	maxPassed := flagPassed(fs, "max")
	if *clear && maxPassed {
		return usageErr(stderr, "resize: --max and --clear are mutually exclusive")
	}
	if !*clear && !maxPassed {
		return usageErr(stderr, "resize: --max N or --clear is required")
	}
	if !*clear && *max <= 0 {
		return usageErr(stderr, "resize: --max must be > 0 (0 is invalid — use `koryph drain` to wind a run down)")
	}
	if *all && *projectID != "" {
		return usageErr(stderr, "resize --all takes no --project")
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}

	var records []*registry.Record
	if *all {
		if records, err = store.List(); err != nil {
			return fail(stderr, err)
		}
	} else {
		if *projectID == "" {
			return usageErr(stderr, "resize: --project is required (or use --all)")
		}
		rec, gerr := store.Get(*projectID)
		if gerr != nil {
			return fail(stderr, gerr)
		}
		records = []*registry.Record{rec}
	}

	n := 0
	for _, rec := range records {
		if *clear {
			if cerr := clearResize(store, rec); cerr != nil {
				fmt.Fprintf(stderr, "resize %s: %v\n", rec.ProjectID, cerr)
				continue
			}
			fmt.Fprintf(stdout, "%s: width override cleared\n", rec.ProjectID)
			n++
			continue
		}
		effective, clamped, serr := setResize(store, rec, *max, *force)
		if serr != nil {
			fmt.Fprintf(stderr, "resize %s: %v\n", rec.ProjectID, serr)
			continue
		}
		if clamped {
			fmt.Fprintf(stdout, "%s: width override set to %d (clamped from %d; pass --force to exceed max_concurrent_slots)\n",
				rec.ProjectID, effective, *max)
		} else {
			fmt.Fprintf(stdout, "%s: width override set to %d\n", rec.ProjectID, effective)
		}
		n++
	}
	if *all {
		verb := "set"
		if *clear {
			verb = "cleared"
		}
		fmt.Fprintf(stdout, "resize --all: %s for %d project(s)\n", verb, n)
	}
	return 0
}

// setResize clamps max to [1, project max_concurrent_slots] unless force,
// writes the override next to the project's run ledger, and audits the
// operator action. It reports the clamped effective width so the caller can
// tell the operator what actually took effect.
func setResize(store *registry.Store, rec *registry.Record, max int, force bool) (effective int, clamped bool, err error) {
	cfg, err := project.Load(rec.Root)
	if err != nil {
		return 0, false, err
	}
	effective = max
	if !force && cfg.MaxConcurrentSlots > 0 && effective > cfg.MaxConcurrentSlots {
		effective = cfg.MaxConcurrentSlots
		clamped = true
	}
	if effective < 1 {
		effective = 1
		clamped = true
	}
	if err := ledger.NewStore(rec.Root).SetResize(ledger.ResizeOverride{Max: effective, Force: force}); err != nil {
		return 0, false, err
	}
	_ = store.Audit(registry.Event{
		Kind:      "resize",
		ProjectID: rec.ProjectID,
		Actor:     cliActor(),
		Detail: map[string]any{
			"requested": max,
			"effective": effective,
			"force":     force,
			"clamped":   clamped,
		},
	})
	return effective, clamped, nil
}

// clearResize removes the width override and audits the operator action.
func clearResize(store *registry.Store, rec *registry.Record) error {
	if err := ledger.NewStore(rec.Root).ClearResize(); err != nil {
		return err
	}
	return store.Audit(registry.Event{
		Kind:      "resize",
		ProjectID: rec.ProjectID,
		Actor:     cliActor(),
		Detail:    map[string]any{"cleared": true},
	})
}
