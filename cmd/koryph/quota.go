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
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/metrics"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
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
			{
				name:    "guard",
				summary: "live billing-guard toggle — on|advisory|off [--until <duration>]; re-read each wave without a restart",
				run:     cmdQuotaGuard,
				DocLinks: []string{
					"user-guide/billing-and-quota.md",
					"concepts/governors.md",
				},
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
			{
				name:    "tokens",
				summary: "per-bead and per-tier token composition, cache-hit ratio, and tokens-per-bead trend",
				run:     cmdMetricsTokens,
				// Only published pages belong in DocLinks — the generator emits
				// them as 'See also' links and design docs are excluded from the
				// book (a design-doc DocLink renders a dead link).
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

// cmdQuota dispatches the quota show/calibrate/guard verbs.
func cmdQuota(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "calibrate":
			return cmdQuotaCalibrate(args[1:], stdout, stderr)
		case "guard":
			return cmdQuotaGuard(args[1:], stdout, stderr)
		}
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
		quota.LogUsage(u, cfg)
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
	// Print active ladder below the table.
	for _, s := range snaps {
		cfg, cerr := quota.LoadConfig(s.Account)
		if cerr != nil {
			continue
		}
		eff := cfg.Ladder.Effective()
		custom := ""
		if !cfg.Ladder.IsDefault() {
			custom = " (custom)"
		}
		fmt.Fprintf(stdout, "  %s: ladder%s warn=%.0f%% throttle=%.0f%% graceful-stop=%.0f%% hard-stop=%.0f%%\n",
			s.Account, custom, eff.Warn*100, eff.Throttle*100, eff.GracefulStop*100, eff.HardStop*100)
	}
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
	// concurrent run's EWMA Record calls are not clobbered (koryph-8iu.1). Clear
	// the calibration-stale flag in the SAME closure: a fresh calibration is
	// exactly the remediation SetCalibrationStale's doctor warning tells the
	// operator to perform, so it must clear here (folded in so the clear is
	// atomic with the calibrate under one flock, not a second lock round-trip).
	cfg, err := quota.UpdateConfig(*acct, func(c *quota.Config) error {
		if *planTier != "" {
			c.PlanTier = *planTier
		}
		if cerr := quota.Calibrate(c, *observedUSD, *observedPct, *window); cerr != nil {
			return cerr
		}
		c.CalibrationStale = false
		c.CalibrationStaleReason = ""
		return nil
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

// cmdQuotaGuard sets the billing-guard mode for an account live, without
// requiring a loop restart. The engine re-reads the quota config at every
// wave boundary (via governor() → quota.LoadConfig), so the change takes
// effect on the very next wave. (koryph-i25)
//
// Usage: koryph quota guard --account A on|advisory|off [--until <duration>]
func cmdQuotaGuard(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("quota guard", stderr)
	acct := fs.String("account", "", "account to configure (required)")
	until := fs.String("until", "", "auto-revert duration from now (e.g. 2h, 24h); omit for permanent")
	setUsage(fs, stdout,
		"live billing-guard toggle: on|advisory|off [--until <duration>] — re-read by the loop at every wave",
		"--account A on|advisory|off [--until <duration>]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if *acct == "" {
		return usageErr(stderr, "quota guard: --account is required")
	}
	if len(pos) != 1 {
		return usageErr(stderr, "quota guard: exactly one positional argument required: on|advisory|off")
	}
	mode := pos[0]
	switch mode {
	case quota.GuardModeOn, quota.GuardModeAdvisory, quota.GuardModeOff:
		// valid
	default:
		return usageErr(stderr, fmt.Sprintf("quota guard: unknown mode %q — want on|advisory|off", mode))
	}

	var untilTime time.Time
	if *until != "" {
		d, derr := time.ParseDuration(*until)
		if derr != nil {
			return usageErr(stderr, fmt.Sprintf("quota guard: --until %q is not a valid duration: %v", *until, derr))
		}
		if d <= 0 {
			return usageErr(stderr, "quota guard: --until duration must be positive")
		}
		untilTime = time.Now().Add(d)
	}

	cfg, serr := quota.SetGuardMode(*acct, mode, untilTime)
	if serr != nil {
		return fail(stderr, serr)
	}

	// Audit: emit to the registry audit log so the change is traceable.
	ctx := context.Background()
	store, aerr := openStore(ctx)
	if aerr == nil {
		detail := map[string]any{
			"account":    *acct,
			"guard_mode": cfg.GuardMode,
		}
		if cfg.GuardUntil != "" {
			detail["guard_until"] = cfg.GuardUntil
		}
		_ = store.Audit(registry.Event{
			Kind:   "quota-guard",
			Actor:  cliActor(),
			Detail: detail,
		})
	}

	// Human-readable confirmation.
	switch mode {
	case quota.GuardModeOn:
		fmt.Fprintf(stdout, "quota guard %s: enforced (billing guard re-enabled)\n", *acct)
	default:
		msg := fmt.Sprintf("quota guard %s: advisory (billing guard disabled — usage still measured)", *acct)
		if cfg.GuardUntil != "" {
			msg += fmt.Sprintf("; auto-reverts at %s", cfg.GuardUntil)
		}
		fmt.Fprintln(stdout, msg)
	}
	return 0
}

// cmdMetricsDispatch dispatches the metrics sub-verbs.
func cmdMetricsDispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "estimator":
			return cmdMetricsEstimator(args[1:], stdout, stderr)
		case "tokens":
			return cmdMetricsTokens(args[1:], stdout, stderr)
		}
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
	Key     string // raw Calibration/ErrorStats key: "<tier>:<size>" or "<tier>:<size>@<proxyID>"
	Tier    string
	Size    string
	// ProxyID is the segment this row's observations belong to (koryph-3l1.3,
	// calibKey's proxyID segmentation): "" is the direct population — either
	// no agent_proxy was ever configured, or this account's beads were
	// dispatched through the holdout arm of one (see ledger.Slot.ProxyID's
	// doc — the two share the same key by design). Non-empty is the proxied
	// arm's own, disjoint population.
	ProxyID string
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
			// Parse tier:size[@proxyID] from key via the shared inverse of
			// calibKey (koryph-3l1.3 carried contract): a naive last-colon or
			// first-colon split corrupts once a proxyID (itself
			// "<base_url>[#pin]", e.g. "http://127.0.0.1:8787", which
			// contains colons of its own) is appended.
			tier, size, proxyID := quota.ParseCalibKey(k)
			base := quota.EstimateItemForRuntimeProxy(cfg, resolvedRuntimeName, tier, size, proxyID)
			active := es.N >= quota.BiasCorrectionThreshold
			warn := math.Abs(es.Bias-1) > 0.5
			rows = append(rows, estimatorRow{
				Account: a,
				Key:     k,
				Tier:    tier,
				Size:    size,
				ProxyID: proxyID,
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
	fmt.Fprintln(tw, "ACCOUNT\tTIER:SIZE\tPROXY\tN\tBASE\tBIAS\tMAPE\tCORR\tSTATUS")
	for _, r := range rows {
		corrStr := "no"
		if r.Active {
			corrStr = "yes"
		}
		status := "ok"
		if r.Warn {
			status = "WARN(|bias-1|>0.5)"
		}
		proxyCol := "direct"
		if r.ProxyID != "" {
			proxyCol = r.ProxyID
		}
		fmt.Fprintf(tw, "%s\t%s:%s\t%s\t%d\t$%.2f\t%.2f\t%.0f%%\t%s\t%s\n",
			r.Account, r.Tier, r.Size, proxyCol, r.N, r.BaseUSD, r.Bias, r.MAPE, corrStr, status)
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

// cmdMetricsTokens prints per-bead and per-tier token composition, cache-hit
// ratio, and a tokens-per-bead trend derived from the ledger's token fields
// (koryph-77r.2, design docs/designs/2026-07-token-economy.md §3 L1).
//
// Token data is populated by the engine for every dispatched slot; older
// ledger entries without token fields show as zero and are skipped from the
// breakdown. Use --json for machine-readable output suitable for scripting or
// further analysis. --experiment switches to the L6 two-arm (proxied vs
// holdout) standing-canary comparison (koryph-3l1.3) instead of the plain
// token report.
func cmdMetricsTokens(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("metrics tokens", stderr)
	projectID := fs.String("project", "", "limit to one project ID")
	asJSON := fs.Bool("json", false, "emit JSON")
	experiment := fs.Bool("experiment", false,
		"render the L6 two-arm (proxied vs holdout) standing-canary comparison instead")
	setUsage(fs, stdout,
		"per-bead and per-tier token composition, cache-hit ratio, and tokens-per-bead trend",
		"[--project ID] [--json] [--experiment]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}

	if *experiment {
		erep, err := metrics.CollectExperiment(store, *projectID)
		if err != nil {
			return fail(stderr, err)
		}
		if *asJSON {
			if err := printJSON(stdout, erep); err != nil {
				return fail(stderr, err)
			}
			return 0
		}
		metrics.RenderExperiment(erep, stdout)
		return 0
	}

	rep, err := metrics.CollectTokens(store, *projectID)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, rep); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	metrics.RenderTokens(rep, stdout)
	return 0
}
