// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/modellearn"
)

func init() {
	registerCmd(command{
		name:    "models",
		summary: "model-routing insight: learn starting tiers from escalation history",
		run:     cmdModels,
		DocLinks: []string{
			"user-guide/running-waves.md",
		},
		subs: []command{
			{
				name:     "learn",
				summary:  "recommend (and --apply) learned model labels from escalation history",
				run:      cmdModelsLearn,
				DocLinks: []string{"user-guide/running-waves.md"},
			},
		},
	})
}

// cmdModels dispatches the models sub-verbs.
func cmdModels(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return cmdModelsLearn(nil, stdout, stderr)
	}
	switch args[0] {
	case "learn":
		return cmdModelsLearn(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		parentHelp(stdout, "models", "model-routing insight from run-ledger escalation history", []subVerb{
			{"learn [--project ID] [--min-evidence N] [--apply]",
				"aggregate escalated-then-merged beads by (area, size) and recommend starting tiers; --apply stamps model:<tier> + model-learned:<date> labels onto matching ready beads (default when no verb given)"},
		})
		return 0
	default:
		return usageErr(stderr, "models: unknown subcommand "+args[0])
	}
}

// cmdModelsLearn is the manual entry point to the koryph-qf6.6 learner — the
// same Collect → Recommend → Apply pass the engine runs at wave boundaries
// when adaptive_escalation.enabled, minus the gate: an operator can always
// inspect (and, with --apply, act on) the evidence by hand.
func cmdModelsLearn(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("models learn", flag.ContinueOnError)
	projectID := fs.String("project", "", "project id (defaults to the project owning the current directory)")
	minEvidence := fs.Int("min-evidence", 0, "escalated-then-merged beads required per (area,size) bucket (0 = default 2)")
	apply := fs.Bool("apply", false, "write model:<tier> + model-learned:<date> labels onto matching ready beads")
	setUsage(fs, stdout, "recommend starting model tiers from escalation history",
		"[--project ID] [--min-evidence N] [--apply]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, code := resolveProjectRecordCwd(stderr, store, *projectID, "models learn")
	if code != 0 {
		return code
	}

	evs, err := modellearn.Collect(ledger.NewStore(rec.Root))
	if err != nil {
		return fail(stderr, err)
	}
	recs := modellearn.Recommend(evs, *minEvidence)
	if len(recs) == 0 {
		fmt.Fprintf(stdout, "%s: no recommendations — %d merged beads with features, none crossing the evidence threshold\n",
			rec.ProjectID, len(evs))
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AREA\tSIZE\tTIER\tESCALATED\tCLEAN\tEVIDENCE BEADS")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n",
			r.Area, r.Size, r.Tier, r.Evidence, r.CleanMerges, strings.Join(r.Beads, ","))
	}
	tw.Flush()

	if !*apply {
		fmt.Fprintln(stdout, "\n(dry run — re-run with --apply to label matching ready beads)")
		return 0
	}

	adapter := beads.New(rec.Root)
	issues, err := adapter.Ready(ctx, beads.ReadyOpts{})
	if err != nil {
		return fail(stderr, err)
	}
	date := time.Now().UTC().Format("2006-01-02")
	applied, _, failed := modellearn.Apply(ctx, adapter, issues, recs, date)
	for _, a := range applied {
		fmt.Fprintf(stdout, "labeled %s: model:%s (matched %s %s)\n", a.BeadID, a.Tier, a.Area, a.Size)
	}
	fmt.Fprintf(stdout, "applied %d label pair(s) to %d ready beads", len(applied), len(issues))
	if failed > 0 {
		fmt.Fprintf(stdout, " (%d write(s) failed)", failed)
	}
	fmt.Fprintln(stdout)
	if failed > 0 {
		return 1
	}
	return 0
}
