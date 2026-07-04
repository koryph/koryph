// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/metrics"
	"github.com/koryph/koryph/internal/quota"
)

func init() {
	registerCmd(command{
		name:    "quota",
		summary: "per-account governor snapshot",
		run:     cmdQuota,
		DocLinks: []string{
			"concepts/governors.md",
			"user-guide/billing-and-quota.md",
		},
		subs: []command{
			{
				name:     "calibrate",
				summary:  "calibrate a governor ceiling from an observed /usage reading",
				run:      cmdQuotaCalibrate,
				DocLinks: []string{"concepts/governors.md", "user-guide/billing-and-quota.md"},
			},
		},
	})
	registerCmd(command{
		name:    "metrics",
		summary: "burn + reliability rollup across projects",
		run:     cmdMetricsDispatch,
		DocLinks: []string{
			"user-guide/billing-and-quota.md",
			"concepts/governors.md",
		},
		subs: []command{
			{
				name:     "estimator",
				summary:  "per-(model,size) estimator accuracy stats",
				run:      cmdMetricsEstimator,
				DocLinks: []string{"user-guide/billing-and-quota.md"},
			},
		},
	})
}

// resolvedRuntimeName is the runtime `koryph quota` resolves an account's
// config dir for today (koryph-v8u.5): real per-project runtime SELECTION is
// koryph-v8u.3's job — until it lands, registry.Record.AccountFor always
// falls back to the flat ClaudeConfigDir field this command read directly
// before this bead, so the snapshot is unchanged end-to-end.
const resolvedRuntimeName = "claude"

// cmdQuota dispatches the quota show/calibrate verbs.
func cmdQuota(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "calibrate" {
		return cmdQuotaCalibrate(args[1:], stdout, stderr)
	}
	return cmdQuotaShow(args, stdout, stderr)
}

// quotaSnapshot is one account's rendered governor snapshot.
type quotaSnapshot struct {
	Account    string      `json:"account"`
	Level      quota.Level `json:"level"`
	Calibrated bool        `json:"calibrated"`
	Usage      quota.Usage `json:"usage"`
}

func cmdQuotaShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("quota", stderr)
	acct := fs.String("account", "", "limit to one account (default: all across records)")
	asJSON := fs.Bool("json", false, "emit JSON")
	setUsage(fs, stdout, "per-account governor snapshot (subcommand: calibrate)", "[--account A] [--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	recs, err := store.List()
	if err != nil {
		return fail(stderr, err)
	}

	// Resolve the set of accounts and a representative config dir per account.
	configDir := map[string]string{}
	var order []string
	seen := map[string]bool{}
	for _, rec := range recs {
		if _, ok := configDir[rec.AccountProfile]; !ok {
			configDir[rec.AccountProfile] = rec.AccountFor(resolvedRuntimeName).ConfigDir
		}
		if !seen[rec.AccountProfile] {
			seen[rec.AccountProfile] = true
			order = append(order, rec.AccountProfile)
		}
	}
	if *acct != "" {
		order = []string{*acct}
	}
	if len(order) == 0 {
		fmt.Fprintln(stdout, "no accounts (register a project or pass --account)")
		return 0
	}

	snaps := make([]quotaSnapshot, 0, len(order))
	for _, a := range order {
		cfg, cerr := quota.LoadConfig(a)
		if cerr != nil {
			return fail(stderr, cerr)
		}
		prof := account.Profile{Name: a, ConfigDir: configDir[a]}
		u, _ := quota.Snapshot(ctx, prof, cfg)
		level, calibrated := quota.State(u, cfg)
		snaps = append(snaps, quotaSnapshot{Account: a, Level: level, Calibrated: calibrated, Usage: u})
	}

	if *asJSON {
		if err := printJSON(stdout, snaps); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tLEVEL\tCALIBRATED\t5H\tWEEKLY")
	for _, s := range snaps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.Account, s.Level, yesno(s.Calibrated), windowCell(s.Usage.Window5h), windowCell(s.Usage.Weekly))
	}
	tw.Flush()
	return 0
}

// windowCell renders a usage window as "$spent/$ceiling (source)".
func windowCell(w quota.Window) string {
	return fmt.Sprintf("$%.2f/$%.2f (%s)", w.SpentUSD, w.CeilingUSD, orDash(w.Source))
}

func cmdQuotaCalibrate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("quota calibrate", stderr)
	acct := fs.String("account", "", "account to calibrate (required)")
	window := fs.String("window", "", "window to calibrate: 5h|weekly (required)")
	observedUSD := fs.Float64("observed-usd", 0, "observed ccusage spend (USD)")
	observedPct := fs.Float64("observed-pct", 0, "observed /usage percentage")
	planTier := fs.String("plan-tier", "", "plan tier label (e.g. max20x)")
	setUsage(fs, stdout, "calibrate a governor ceiling from an observed /usage reading",
		"--account A --window <5h|weekly> --observed-usd X --observed-pct Y [--plan-tier T]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *acct == "" || *window == "" {
		return usageErr(stderr, "quota calibrate: --account and --window are required")
	}

	// Lock-guarded read-modify-write: re-reads fresh under the flock so a
	// concurrent run's EWMA Record calls are not clobbered (koryph-8iu.1).
	cfg, err := quota.UpdateConfig(*acct, func(c *quota.Config) error {
		if *planTier != "" {
			c.PlanTier = *planTier
		}
		return quota.Calibrate(c, *observedUSD, *observedPct, *window)
	})
	if err != nil {
		return fail(stderr, err)
	}
	ceiling := cfg.WindowCeilingUSD
	if *window == "weekly" {
		ceiling = cfg.WeeklyCeilingUSD
	}
	fmt.Fprintf(stdout, "calibrated %s %s ceiling: $%.2f\n", *acct, *window, ceiling)
	return 0
}

// cmdMetricsDispatch dispatches the metrics sub-verbs.
func cmdMetricsDispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "estimator" {
		return cmdMetricsEstimator(args[1:], stdout, stderr)
	}
	return cmdMetrics(args, stdout, stderr)
}

// cmdMetrics prints the burn + reliability rollup.
func cmdMetrics(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("metrics", stderr)
	projectID := fs.String("project", "", "limit to one project")
	asJSON := fs.Bool("json", false, "emit JSON")
	setUsage(fs, stdout, "burn + reliability rollup across projects", "[--project ID] [--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rep, err := metrics.Collect(store, *projectID)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, rep); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	metrics.Render(rep, stdout)
	return 0
}

// estimatorRow is one row in the estimator accuracy table.
type estimatorRow struct {
	Account string
	Key     string // "<tier>:<size>"
	N       int
	BaseUSD float64 // static base estimate (pre-bias)
	Bias    float64 // EWMA of actual/estimate ratio
	MAPE    float64 // EWMA of |actual-estimate|/estimate * 100
	Active  bool    // bias correction active (N >= threshold)
	Warn    bool    // |bias-1| > 0.5
}

// cmdMetricsEstimator prints per-(model, size) estimator accuracy stats
// across all registered accounts (koryph-6bl). Rows with |bias-1| > 0.5
// are annotated with WARN. Bias correction becomes active once N >=
// BiasCorrectionThreshold (currently 5).
func cmdMetricsEstimator(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("metrics estimator", stderr)
	acct := fs.String("account", "", "limit to one account")
	asJSON := fs.Bool("json", false, "emit JSON")
	setUsage(fs, stdout, "per-(model,size) estimator accuracy stats", "[--account A] [--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	recs, err := store.List()
	if err != nil {
		return fail(stderr, err)
	}

	// Collect unique accounts.
	var accounts []string
	seen := map[string]bool{}
	for _, rec := range recs {
		if !seen[rec.AccountProfile] {
			seen[rec.AccountProfile] = true
			accounts = append(accounts, rec.AccountProfile)
		}
	}
	sort.Strings(accounts)

	if *acct != "" {
		accounts = []string{*acct}
	}

	var rows []estimatorRow
	for _, a := range accounts {
		cfg, cerr := quota.LoadConfig(a)
		if cerr != nil {
			return fail(stderr, cerr)
		}
		if len(cfg.ErrorStats) == 0 {
			continue
		}
		// Sort keys for stable output.
		keys := make([]string, 0, len(cfg.ErrorStats))
		for k := range cfg.ErrorStats {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			es := cfg.ErrorStats[k]
			if es == nil {
				continue
			}
			// Parse tier:size from key.
			tier, size := k, ""
			for i := len(k) - 1; i >= 0; i-- {
				if k[i] == ':' {
					tier, size = k[:i], k[i+1:]
					break
				}
			}
			base := quota.EstimateItemForRuntime(cfg, resolvedRuntimeName, tier, size)
			active := es.N >= quota.BiasCorrectionThreshold
			warn := math.Abs(es.Bias-1) > 0.5
			rows = append(rows, estimatorRow{
				Account: a,
				Key:     k,
				N:       es.N,
				BaseUSD: base,
				Bias:    es.Bias,
				MAPE:    es.MAPE,
				Active:  active,
				Warn:    warn,
			})
		}
	}

	if *asJSON {
		if err := printJSON(stdout, rows); err != nil {
			return fail(stderr, err)
		}
		return 0
	}

	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no estimator data yet (dispatches accumulate it automatically)")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tKEY\tN\tBASE\tBIAS\tMAPE\tCORR\tSTATUS")
	for _, r := range rows {
		corrStr := "no"
		if r.Active {
			corrStr = "yes"
		}
		status := "ok"
		if r.Warn {
			status = "WARN(|bias-1|>0.5)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t$%.2f\t%.2f\t%.0f%%\t%s\t%s\n",
			r.Account, r.Key, r.N, r.BaseUSD, r.Bias, r.MAPE, corrStr, status)
	}
	tw.Flush()

	// Collect projects to look up account registry entries.
	// Use a thin annotation block below the table.
	hasWarn := false
	for _, r := range rows {
		if r.Warn {
			hasWarn = true
			break
		}
	}
	if hasWarn {
		fmt.Fprintln(stdout, "\nWARN rows: |bias-1| > 0.5 — estimator is significantly off for this bucket.")
		fmt.Fprintln(stdout, "Run `koryph quota calibrate` to update ceilings; bias corrects automatically after more dispatches.")
	}

	_ = ctx
	return 0
}
