// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/execx"
)

const ccusageTimeout = 40 * time.Second

// Snapshot measures an account's 5h and weekly usage, fail-closed. It prefers
// the ccusage CLI (run with the profile's CLAUDE_CONFIG_DIR in the child env),
// falls back to an approximate local transcript scan, and finally reports the
// window as "unavailable" (Fraction 1.0) when nothing is measurable. The error
// is encoded in each Window's Source; Snapshot itself never returns an error so
// the governor can fail closed on it.
func Snapshot(ctx context.Context, profile account.Profile, cfg *Config) (Usage, error) {
	u := Usage{
		Account: cfg.Account,
		At:      time.Now().UTC().Format(time.RFC3339),
	}
	u.Window5h.Hours = 5
	u.Window5h.CeilingUSD = cfg.WindowCeilingUSD
	u.Weekly.Hours = 24 * 7
	u.Weekly.CeilingUSD = cfg.WeeklyCeilingUSD

	env := childEnv(profile)

	// 5h window.
	if spent, okc := ccusageActiveBlock(ctx, env); okc {
		u.Window5h.SpentUSD = spent
		u.Window5h.Source = "ccusage"
		u.Window5h.Approx = false
	} else if spent, err := JSONLScan(profile.ConfigDir, 5); err == nil {
		u.Window5h.SpentUSD = spent
		u.Window5h.Source = "jsonl-scan"
		u.Window5h.Approx = true
	} else {
		u.Window5h.Source = "unavailable"
	}

	// Weekly window.
	if spent, okc := ccusageWeekly(ctx, env); okc {
		u.Weekly.SpentUSD = spent
		u.Weekly.Source = "ccusage"
		u.Weekly.Approx = false
	} else if spent, err := JSONLScan(profile.ConfigDir, 24*7); err == nil {
		u.Weekly.SpentUSD = spent
		u.Weekly.Source = "jsonl-scan"
		u.Weekly.Approx = true
	} else {
		u.Weekly.Source = "unavailable"
	}

	return u, nil
}

// childEnv builds the child environment for a ccusage invocation: the parent
// environment with CLAUDE_CONFIG_DIR scrubbed, then re-injected only for a work
// profile. A personal profile (empty ConfigDir) leaves it unset.
func childEnv(profile account.Profile) []string {
	env := execx.BaseEnv("CLAUDE_CONFIG_DIR")
	if profile.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+profile.ConfigDir)
	}
	return env
}

// runCcusage runs ccusage with the given args, preferring an on-PATH binary and
// otherwise `npx -y ccusage@latest` (unless KORYPH_NO_NPX=1). It returns stdout
// and whether the command was found and exited cleanly.
func runCcusage(ctx context.Context, env []string, args ...string) (string, bool) {
	c := execx.Cmd{Env: env, Timeout: ccusageTimeout}
	switch {
	case execx.LookPath("ccusage"):
		c.Name = "ccusage"
		c.Args = args
	case os.Getenv("KORYPH_NO_NPX") != "1":
		c.Name = "npx"
		c.Args = append([]string{"-y", "ccusage@latest"}, args...)
	default:
		return "", false
	}
	res, err := execx.Run(ctx, c)
	if err != nil || res.ExitCode != 0 {
		return "", false
	}
	return res.Stdout, true
}

// ccusageActiveBlock parses `.blocks[0].costUSD` from `ccusage blocks --json
// --active`. A clean run with no active block (or a missing cost) is a measured
// $0, not a fallback; only a spawn failure or unparseable output falls through.
func ccusageActiveBlock(ctx context.Context, env []string) (float64, bool) {
	out, ok := runCcusage(ctx, env, "blocks", "--json", "--active")
	if !ok {
		return 0, false
	}
	var doc struct {
		Blocks []struct {
			CostUSD float64 `json:"costUSD"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return 0, false
	}
	if len(doc.Blocks) == 0 {
		return 0, true
	}
	return doc.Blocks[0].CostUSD, true
}

// ccusageWeekly sums the last 7 daily entries from `ccusage daily --json`,
// tolerating either a `totalCost` or `cost` field per entry.
func ccusageWeekly(ctx context.Context, env []string) (float64, bool) {
	out, ok := runCcusage(ctx, env, "daily", "--json")
	if !ok {
		return 0, false
	}
	var doc struct {
		Daily []struct {
			TotalCost float64 `json:"totalCost"`
			Cost      float64 `json:"cost"`
		} `json:"daily"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return 0, false
	}
	start := 0
	if len(doc.Daily) > 7 {
		start = len(doc.Daily) - 7
	}
	var sum float64
	for _, e := range doc.Daily[start:] {
		if e.TotalCost != 0 {
			sum += e.TotalCost
		} else {
			sum += e.Cost
		}
	}
	return sum, true
}

// modelPrice is per-MTok pricing in USD.
type modelPrice struct{ in, out, cacheWrite, cacheRead float64 }

// priceFor returns approximate per-MTok pricing by matching a substring of the
// model id. Unknown models are priced as sonnet.
func priceFor(model string) modelPrice {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "opus"):
		return modelPrice{in: 15, out: 75, cacheWrite: 18.75, cacheRead: 1.5}
	case strings.Contains(m, "haiku"):
		return modelPrice{in: 0.8, out: 4, cacheWrite: 1, cacheRead: 0.08}
	case strings.Contains(m, "fable"):
		return modelPrice{in: 25, out: 125, cacheWrite: 31.25, cacheRead: 2.5}
	case strings.Contains(m, "sonnet"):
		return modelPrice{in: 3, out: 15, cacheWrite: 3.75, cacheRead: 0.3}
	default:
		return modelPrice{in: 3, out: 15, cacheWrite: 3.75, cacheRead: 0.3}
	}
}

// usageTokens is the token accounting on a transcript line.
type usageTokens struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// transcriptLine is the minimal shape parsed from a *.jsonl transcript. Model
// and usage are read from message.* first, then the top level, to tolerate
// alternate shapes.
type transcriptLine struct {
	Timestamp string `json:"timestamp"`
	Message   struct {
		Model string       `json:"model"`
		Usage *usageTokens `json:"usage"`
	} `json:"message"`
	Model string       `json:"model"`
	Usage *usageTokens `json:"usage"`
}

// priceUsage prices one line's token counts against the model's per-MTok rates.
func priceUsage(model string, u *usageTokens) float64 {
	p := priceFor(model)
	const mtok = 1_000_000.0
	return float64(u.InputTokens)/mtok*p.in +
		float64(u.OutputTokens)/mtok*p.out +
		float64(u.CacheCreationInputTokens)/mtok*p.cacheWrite +
		float64(u.CacheReadInputTokens)/mtok*p.cacheRead
}

// priceLine parses a single transcript line and returns its approximate cost,
// or ok=false when the line is unparseable, out of window, or has no usage.
func priceLine(line []byte, windowStart time.Time) (float64, bool) {
	var tl transcriptLine
	if err := json.Unmarshal(line, &tl); err != nil {
		return 0, false
	}
	if tl.Timestamp == "" {
		return 0, false
	}
	ts, err := time.Parse(time.RFC3339, tl.Timestamp)
	if err != nil {
		if ts, err = time.Parse(time.RFC3339Nano, tl.Timestamp); err != nil {
			return 0, false
		}
	}
	if ts.UTC().Before(windowStart) {
		return 0, false
	}
	model := tl.Message.Model
	if model == "" {
		model = tl.Model
	}
	usage := tl.Message.Usage
	if usage == nil {
		usage = tl.Usage
	}
	if usage == nil {
		return 0, false
	}
	return priceUsage(model, usage), true
}

// fiveHourWindowStart returns the start of the current fixed UTC 5h grid window,
// aligned to the Unix epoch (which is midnight UTC).
func fiveHourWindowStart(now time.Time) time.Time {
	const grid = 5 * time.Hour
	epoch := time.Unix(0, 0).UTC()
	return now.Add(-(now.Sub(epoch) % grid))
}

// scanFile streams one transcript file and sums the approximate cost of the
// in-window lines. Individual malformed lines are skipped.
func scanFile(path string, windowStart time.Time) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB max token
	var total float64
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if cost, ok := priceLine(line, windowStart); ok {
			total += cost
		}
	}
	if err := sc.Err(); err != nil {
		return total, err
	}
	return total, nil
}

// JSONLScan is the Go port of the transcript scan. It sums approximate USD over
// <configDir||~/.claude>/projects/*/*.jsonl for the given window. The 5h window
// uses the fixed UTC grid (epoch-aligned); any other span is a rolling window
// ending now. It returns an error when no transcript files exist so the caller
// can fall through to "unavailable".
func JSONLScan(configDir string, hours int) (float64, error) {
	root := configDir
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return 0, err
		}
		root = filepath.Join(home, ".claude")
	}
	pattern := filepath.Join(root, "projects", "*", "*.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no transcript files under %s", pattern)
	}

	now := time.Now().UTC()
	var windowStart time.Time
	if hours == 5 {
		windowStart = fiveHourWindowStart(now)
	} else {
		windowStart = now.Add(-time.Duration(hours) * time.Hour)
	}

	var total float64
	for _, f := range files {
		v, err := scanFile(f, windowStart)
		if err != nil {
			continue // tolerate a single unreadable file
		}
		total += v
	}
	return total, nil
}

// Calibrate sets a window ceiling from an observed ccusage spend and the /usage
// percentage the user read (ceiling = observed$ / (observed% / 100)) and
// persists the config. window is "5h" or "weekly".
func Calibrate(cfg *Config, observedUSD, observedPct float64, window string) error {
	if observedPct <= 0 {
		return fmt.Errorf("observedPct must be > 0, got %g", observedPct)
	}
	ceiling := observedUSD / (observedPct / 100)
	switch window {
	case "5h":
		cfg.WindowCeilingUSD = ceiling
	case "weekly":
		cfg.WeeklyCeilingUSD = ceiling
	default:
		return fmt.Errorf("unknown window %q (want 5h|weekly)", window)
	}
	return SaveConfig(cfg)
}
