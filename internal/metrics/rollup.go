// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package metrics

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/worktree"
)

// Collect rolls up burn and reliability baselines across managed projects from
// their run ledgers. projectID "" aggregates every registered project. It is
// read-only: it only reads ledger.json files and runs read-only git/worktree
// probes.
func Collect(store *registry.Store, projectID string) (*Report, error) {
	recs, err := store.List()
	if err != nil {
		return nil, err
	}
	rep := &Report{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	for _, rec := range recs {
		if projectID != "" && rec.ProjectID != projectID {
			continue
		}
		stat := collectProject(rec)
		rep.Projects = append(rep.Projects, stat)
		rep.TotalUSD += stat.CostUSD
	}
	return rep, nil
}

// collectProject aggregates one project's runs, model breakdown, and worktree
// hygiene counters.
func collectProject(rec *registry.Record) ProjectStat {
	stat := ProjectStat{
		ProjectID: rec.ProjectID,
		Account:   rec.AccountProfile,
		ByModel:   map[string]ModelStat{},
	}

	koryphRoot := paths.KoryphRoot(rec.Root)
	if entries, err := os.ReadDir(koryphRoot); err == nil {
		seen := map[string]bool{}
		for _, e := range entries {
			// Skip the `latest` symlink (dedupe) and any non-directory.
			if e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
				continue
			}
			ledgerPath := filepath.Join(koryphRoot, e.Name(), "ledger.json")
			resolved := ledgerPath
			if r, rerr := filepath.EvalSymlinks(ledgerPath); rerr == nil {
				resolved = r
			}
			if seen[resolved] {
				continue
			}
			seen[resolved] = true

			var run ledger.Run
			if err := fsx.ReadJSON(ledgerPath, &run); err != nil {
				continue // unparseable / missing ledger dir
			}
			aggregateRun(&stat, &run)
		}
	}

	for m, ms := range stat.ByModel {
		if ms.Slots > 0 {
			ms.MeanUSD = ms.CostUSD / float64(ms.Slots)
		}
		stat.ByModel[m] = ms
	}

	stat.StaleWorktrees, stat.OrphanBranches = worktreeStats(rec.Root)
	return stat
}

// aggregateRun folds one run's slots into the project stat.
func aggregateRun(stat *ProjectStat, run *ledger.Run) {
	stat.Runs++
	if run.Status == ledger.RunRunning {
		stat.UnfinalizedRuns++
	}
	for _, sl := range run.Slots {
		if sl == nil {
			continue
		}
		stat.Slots++
		stat.CostUSD += sl.CostUSD
		stat.ReviewBounces += sl.ReviewIters
		retried := sl.Attempts > 1
		if retried {
			stat.Retries++
		}
		switch sl.Status {
		case ledger.SlotMerged:
			stat.Merged++
		case ledger.SlotFailed:
			stat.Failed++
		case ledger.SlotBlocked:
			stat.Blocked++
		}

		// Key the per-model cost/outcome breakdown on the model that ACTUALLY
		// served (ModelActual), not the model dispatch REQUESTED (Model). The
		// two diverge when the CLI's hardcoded --fallback-model downgrades a
		// session mid-flight (see ledger.Slot.ModelActual): a cost dataset keyed
		// on Model alone mis-attributes those sessions — a bead requested on
		// opus but served by sonnet would inflate the opus row it never spent.
		// Fall back to Model when ModelActual is empty (crash before a result
		// line, or a ledger predating the field). Matches the identical
		// convention in internal/cockpit/efficiency.go's per-model token rollup,
		// so the cost table and the token table agree on which row a slot lands
		// in.
		modelKey := sl.ModelActual
		if modelKey == "" {
			modelKey = sl.Model
		}
		ms := stat.ByModel[modelKey]
		ms.Slots++
		ms.CostUSD += sl.CostUSD
		if retried {
			ms.Retries++
		}
		switch sl.Status {
		case ledger.SlotMerged:
			ms.Merged++
		case ledger.SlotFailed:
			ms.Failed++
		}
		stat.ByModel[modelKey] = ms
	}
}

// worktreeStats counts stale agent/* worktrees and orphaned agent/* branches
// (branches with no live worktree). Probe failures yield zeroes.
func worktreeStats(root string) (stale, orphan int) {
	ctx := context.Background()
	live := map[string]bool{}
	if wts, err := worktree.List(ctx, root); err == nil {
		for _, w := range wts {
			if strings.HasPrefix(w.Branch, "agent/") {
				stale++
				live[w.Branch] = true
			}
		}
	}
	res, err := execx.Run(ctx, execx.Cmd{
		Dir:  root,
		Name: "git",
		Args: []string{"for-each-ref", "--format=%(refname:short)", "refs/heads/agent/"},
	})
	if err == nil && res.ExitCode == 0 {
		for _, line := range strings.Split(res.Stdout, "\n") {
			b := strings.TrimSpace(line)
			if b == "" || live[b] {
				continue
			}
			orphan++
		}
	}
	return stale, orphan
}

// Render writes an aligned per-project table, a per-model breakdown, and a
// total line. For machine-readable output use --json (cmdMetrics).
func Render(r *Report, w io.Writer) {
	if r == nil {
		fmt.Fprintln(w, "no metrics")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tACCOUNT\tRUNS\tUNFIN\tSLOTS\tMERGED\tFAILED\tBLOCKED\tRETRIES\tBOUNCES\tSTALE\tORPHAN\tCOST")
	for _, p := range r.Projects {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t$%.2f\n",
			p.ProjectID, p.Account, p.Runs, p.UnfinalizedRuns, p.Slots, p.Merged,
			p.Failed, p.Blocked, p.Retries, p.ReviewBounces, p.StaleWorktrees,
			p.OrphanBranches, p.CostUSD)
	}
	tw.Flush()

	if hasModels(r) {
		mw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(mw, "PROJECT\tMODEL\tSLOTS\tCOST\tMEAN\tMERGED\tFAILED\tRETRIES")
		for _, p := range r.Projects {
			for _, m := range sortedModelKeys(p.ByModel) {
				ms := p.ByModel[m]
				label := m
				if label == "" {
					label = "-"
				}
				fmt.Fprintf(mw, "%s\t%s\t%d\t$%.2f\t$%.2f\t%d\t%d\t%d\n",
					p.ProjectID, label, ms.Slots, ms.CostUSD, ms.MeanUSD, ms.Merged, ms.Failed, ms.Retries)
			}
		}
		mw.Flush()
	}

	fmt.Fprintf(w, "TOTAL: $%.2f across %d project(s)\n", r.TotalUSD, len(r.Projects))
}

// hasModels reports whether any project has model breakdown rows.
func hasModels(r *Report) bool {
	for _, p := range r.Projects {
		if len(p.ByModel) > 0 {
			return true
		}
	}
	return false
}

// sortedModelKeys returns the model keys of m in stable order.
func sortedModelKeys(m map[string]ModelStat) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
