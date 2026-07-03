// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/intake"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// cmdIntake polls a project's labeled GitHub issues and files one planning
// bead per new issue. When the project's koryph.project.json carries an
// "intake" list, all configured sources are iterated in one run and the
// --label / --limit / --comment flags act as global overrides for that run.
// When no intake list is configured, intake falls back to the project's
// registry remote with the flag values applied directly.
func cmdIntake(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("intake", stderr)
	projectID := fs.String("project", "", "project id (required)")
	label := fs.String("label", "", "trigger label to poll (overrides per-source config; default \"triage\")")
	limit := fs.Int("limit", 0, "max open issues to poll (overrides per-source config; default 20)")
	dryRun := fs.Bool("dry-run", false, "print what would be ingested; mutate nothing")
	comment := fs.Bool("comment", false, "comment the bead id back on each ingested issue")
	if _, err := parseFlags(fs, args); err != nil {
		return engine.ExitUsage
	}
	if *projectID == "" {
		return usageErr(stderr, "intake: --project is required")
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, err := store.Get(*projectID)
	if err != nil {
		return fail(stderr, err)
	}

	// Load the project config to discover configured intake sources.
	// A missing config file is acceptable (project may not be fully onboarded);
	// a present-but-invalid config is a hard error to prevent silent misrouting.
	var cfg *project.Config
	cfgPath := filepath.Join(rec.Root, project.ConfigFileName)
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		c, lerr := project.Load(rec.Root)
		if lerr != nil {
			return fail(stderr, fmt.Errorf("intake: project config invalid: %w", lerr))
		}
		cfg = c
	}

	if cfg != nil && len(cfg.Intake) > 0 {
		return runMultiSourceIntake(ctx, store, rec, cfg, stdout, stderr, *label, *limit, *dryRun, flagPassed(fs, "comment"), *comment)
	}
	return runSingleSourceIntake(ctx, store, rec, stdout, stderr, *label, *limit, *dryRun, *comment)
}

// runMultiSourceIntake iterates all sources from the project config.
func runMultiSourceIntake(
	ctx context.Context,
	store interface {
		Audit(registry.Event) error
	},
	rec *registry.Record,
	cfg *project.Config,
	stdout, stderr io.Writer,
	labelOverride string,
	limitOverride int,
	dryRun bool,
	commentExplicit bool, commentVal bool,
) int {
	var commentPtr *bool
	if commentExplicit {
		commentPtr = &commentVal
	}

	results, err := intake.RunMulti(ctx, intake.MultiOptions{
		Project:             rec,
		Sources:             cfg.Intake,
		OverrideLabel:       labelOverride,
		OverrideLimit:       limitOverride,
		OverrideCommentBack: commentPtr,
		DryRun:              dryRun,
	})

	totalIngested, totalSkipped := 0, 0
	for _, sr := range results {
		printIntakeResult(stdout, sr.Result, dryRun)
		totalIngested += len(sr.Result.Ingested)
		totalSkipped += len(sr.Result.Skipped)
	}
	if len(results) > 1 {
		fmt.Fprintf(stdout, "\ntotal across %d sources: ingested %d, skipped %d\n",
			len(results), totalIngested, totalSkipped)
	}

	if err != nil {
		fmt.Fprintln(stderr, "koryph: intake error(s):", err)
	}

	// Audit all sources in a single event.
	sourceNames := make([]string, 0, len(results))
	for _, sr := range results {
		sourceNames = append(sourceNames, sr.Provider+":"+sr.Source)
	}
	if aerr := store.Audit(registry.Event{
		Kind:      "intake",
		ProjectID: rec.ProjectID,
		Detail: map[string]any{
			"mode":     "multi",
			"sources":  sourceNames,
			"ingested": totalIngested,
			"skipped":  totalSkipped,
			"dry_run":  dryRun,
		},
	}); aerr != nil {
		fmt.Fprintln(stderr, "koryph: warning: intake audit write failed:", aerr)
	}

	if err != nil {
		return engine.ExitFatal
	}
	return 0
}

// runSingleSourceIntake is the pre-config-era path: one source derived from the
// project's registry remote, driven entirely by CLI flags.
func runSingleSourceIntake(
	ctx context.Context,
	store interface {
		Audit(registry.Event) error
	},
	rec *registry.Record,
	stdout, stderr io.Writer,
	label string, limit int, dryRun bool, comment bool,
) int {
	if label == "" {
		label = intake.DefaultLabel
	}
	if limit <= 0 {
		limit = intake.DefaultLimit
	}

	res, err := intake.Run(ctx, intake.Options{
		Project:     rec,
		Label:       label,
		Limit:       limit,
		DryRun:      dryRun,
		CommentBack: comment,
	})
	if err != nil {
		return fail(stderr, err)
	}

	printIntakeResult(stdout, res, dryRun)

	if aerr := store.Audit(registry.Event{
		Kind:      "intake",
		ProjectID: rec.ProjectID,
		Detail: map[string]any{
			"mode":     "single",
			"label":    label,
			"repo":     res.Owner + "/" + res.Repo,
			"ingested": len(res.Ingested),
			"skipped":  len(res.Skipped),
			"dry_run":  dryRun,
		},
	}); aerr != nil {
		fmt.Fprintln(stderr, "koryph: warning: intake audit write failed:", aerr)
	}
	return 0
}

// printIntakeResult renders the ingested/skipped issues as a table.
func printIntakeResult(w io.Writer, res *intake.Result, dryRun bool) {
	fmt.Fprintf(w, "intake %s/%s\n", res.Owner, res.Repo)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tISSUE\tBEAD\tTITLE\tNOTE")
	ingestAction := "ingested"
	if dryRun {
		ingestAction = "would-ingest"
	}
	for _, it := range res.Ingested {
		fmt.Fprintf(tw, "%s\t#%d\t%s\t%s\t%s\n", ingestAction, it.Number, orDash(it.BeadID), truncateTitle(it.Title), orDash(it.Reason))
	}
	for _, it := range res.Skipped {
		fmt.Fprintf(tw, "%s\t#%d\t%s\t%s\t%s\n", "skipped", it.Number, orDash(it.BeadID), truncateTitle(it.Title), orDash(it.Reason))
	}
	tw.Flush()
	fmt.Fprintf(w, "\ningested %d, skipped %d\n", len(res.Ingested), len(res.Skipped))
}

// truncateTitle keeps table rows readable.
func truncateTitle(s string) string {
	const max = 48
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
