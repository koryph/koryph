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
		summary: "recommend (and --apply) learned model tiers from escalation history",
		run:     cmdModels,
		DocLinks: []string{
			"user-guide/running-waves.md",
		},
		subs: []command{
			{
				name:    "learn",
				summary: "recommend (and --apply) learned model labels from escalation history",
				run:     cmdModels,
				// koryph-b8g #24: 'models' is a single-child noun group;
				// flattened so 'models [--apply]' is the primary form (bare
				// 'models' already ran learn). The two-word 'models learn
				// ...' still works — hidden so it doesn't clutter
				// help/completion/docgen.
				hidden:   true,
				DocLinks: []string{"user-guide/running-waves.md"},
			},
		},
	})
}

// cmdModels implements `koryph models [--project ID] [--min-evidence N]
// [--apply]` — the manual entry point to the koryph-qf6.6 learner: the same
// Collect → Recommend → Apply pass the engine runs at wave boundaries when
// adaptive_escalation.enabled, minus the gate, so an operator can always
// inspect (and, with --apply, act on) the evidence by hand. koryph-b8g #24:
// 'models' was a single-child noun group ('models learn ...'); flattened so
// bare 'models [flags]' is the primary form. The two-word 'models learn ...'
// form still works as a hidden alias.
func cmdModels(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "learn" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
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
	rec, code := resolveProjectRecordCwd(stderr, store, *projectID, "models")
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
