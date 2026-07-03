// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/metrics"
	"github.com/koryph/koryph/internal/quota"
)

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
	if _, err := parseFlags(fs, args); err != nil {
		return engine.ExitUsage
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
			configDir[rec.AccountProfile] = rec.ClaudeConfigDir
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
	if _, err := parseFlags(fs, args); err != nil {
		return engine.ExitUsage
	}
	if *acct == "" || *window == "" {
		return usageErr(stderr, "quota calibrate: --account and --window are required")
	}

	cfg, err := quota.LoadConfig(*acct)
	if err != nil {
		return fail(stderr, err)
	}
	if *planTier != "" {
		cfg.PlanTier = *planTier
	}
	if err := quota.Calibrate(cfg, *observedUSD, *observedPct, *window); err != nil {
		return fail(stderr, err)
	}
	ceiling := cfg.WindowCeilingUSD
	if *window == "weekly" {
		ceiling = cfg.WeeklyCeilingUSD
	}
	fmt.Fprintf(stdout, "calibrated %s %s ceiling: $%.2f\n", *acct, *window, ceiling)
	return 0
}

// cmdMetrics prints the burn + reliability rollup.
func cmdMetrics(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("metrics", stderr)
	projectID := fs.String("project", "", "limit to one project")
	asJSON := fs.Bool("json", false, "emit JSON")
	if _, err := parseFlags(fs, args); err != nil {
		return engine.ExitUsage
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
