// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package doctor runs system-health checks against the ~/.koryph state tree.
// All I/O and OS interactions are injected so the checks are unit-testable
// without touching the real filesystem or spawning real processes.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/procx"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
)

// Level classifies a finding's severity.
type Level string

const (
	LevelOK    Level = "ok"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Finding is one check result.
type Finding struct {
	Check   string `json:"check"`
	Level   Level  `json:"level"`
	Message string `json:"message"`
	Fixed   bool   `json:"fixed,omitempty"`
}

// Report is the full doctor output.
type Report struct {
	At         string    `json:"at"`
	Home       string    `json:"home"`
	Project    string    `json:"project,omitempty"` // set in project mode (--project)
	Findings   []Finding `json:"findings"`
	FixedCount int       `json:"fixed_count,omitempty"`
}

// ExitCode maps the worst finding level onto a process exit code:
//
//	ok    → 0
//	warn  → 1
//	error → 2
func (r *Report) ExitCode() int {
	for _, f := range r.Findings {
		if f.Level == LevelError {
			return 2
		}
	}
	for _, f := range r.Findings {
		if f.Level == LevelWarn {
			return 1
		}
	}
	return 0
}

// Options configures a doctor run. Zero values use production defaults.
type Options struct {
	// Home overrides paths.KoryphHome() (useful in tests).
	Home string
	// Fix removes zombie lease files and stale demand heartbeats when true.
	Fix bool
	// Now supplies the current time (injectable for tests).
	Now func() time.Time
	// Alive reports whether a pid is a live process (injectable for tests).
	Alive func(pid int) bool
	// LookPath locates a binary on PATH (injectable for tests).
	LookPath func(name string) (string, error)
	// ProxyHTTPGet is injectable for tests; the real implementation issues an
	// HTTP GET with a 5-second timeout. Signature: (url) → (statusCode, body, err).
	ProxyHTTPGet func(url string) (int, []byte, error)
	// RegistryList is injectable for tests; the real implementation calls
	// registry.NewStoreAt(opts.home()).List(). Returning (nil, nil) means no
	// records: proxy check is a no-op.
	RegistryList func() ([]*registry.Record, error)
	// RunProbe executes one resource kind's operator-authored leak-detection
	// probe command (koryph-4ql.8, design L7 "per-kind probe (opt-in)") and
	// returns its stdout. Injectable for tests; the real implementation is
	// RunProbeShell (`sh -c <cmd>`, bounded by resourceProbeTimeout).
	RunProbe func(ctx context.Context, cmd string) (string, error)
	// BeadsVersion reports the resolved bd binary's version/capability.
	// Injectable for tests; the real implementation is beads.ProbeVersion.
	BeadsVersion func() beads.VersionInfo
}

func (o *Options) beadsVersion() beads.VersionInfo {
	if o.BeadsVersion != nil {
		return o.BeadsVersion()
	}
	return beads.ProbeVersion(context.Background())
}

func (o *Options) home() string {
	if o.Home != "" {
		return o.Home
	}
	return paths.KoryphHome()
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Options) alive(pid int) bool {
	if o.Alive != nil {
		return o.Alive(pid)
	}
	return defaultAlive(pid)
}

func (o *Options) lookPath(name string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath(name)
	}
	return exec.LookPath(name)
}

func (o *Options) proxyHTTPGet(url string) (int, []byte, error) {
	if o.ProxyHTTPGet != nil {
		return o.ProxyHTTPGet(url)
	}
	return defaultProxyHTTPGet(url)
}

func defaultProxyHTTPGet(url string) (int, []byte, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url) //nolint:noctx // doctor is a short CLI check; no ctx plumbing needed
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

func (o *Options) registryList() ([]*registry.Record, error) {
	if o.RegistryList != nil {
		return o.RegistryList()
	}
	return registry.NewStoreAt(o.home()).List()
}

func (o *Options) runProbe() func(ctx context.Context, cmd string) (string, error) {
	if o.RunProbe != nil {
		return o.RunProbe
	}
	return RunProbeShell
}

// Run executes all global checks and returns the report.
func Run(opts Options) (*Report, error) {
	r := &Report{
		At:   opts.now().UTC().Format(time.RFC3339),
		Home: opts.home(),
	}

	r.add(checkLayout(opts))
	r.addAll(checkBinaries(opts))
	r.add(checkBeadsVersion(opts))
	r.add(checkRegistry(opts))
	r.addAll(checkAuthMode(opts))
	r.addAll(checkGovernorConfig(opts))
	r.addAll(checkAdaptiveCapPinned(opts))
	r.addAll(checkCircuitBreaker(opts))
	r.addAll(checkZombieLeases(opts))
	r.addAll(checkResourceProbes(opts))
	r.addAll(checkStaleDemand(opts))
	r.addAll(checkQuotaCalibration(opts))
	r.addAll(checkQuotaGuardOverride(opts))
	r.addAll(checkVaultProviders(opts))
	r.addAll(checkObs(opts))
	r.addAll(checkGCFootprint(opts))
	r.addAll(checkProxy(opts))

	for _, f := range r.Findings {
		if f.Fixed {
			r.FixedCount++
		}
	}
	return r, nil
}

func (r *Report) add(f Finding) {
	r.Findings = append(r.Findings, f)
}

func (r *Report) addAll(fs []Finding) {
	r.Findings = append(r.Findings, fs...)
}

// --- check functions -------------------------------------------------------

const checkNameLayout = "layout"
const checkNameBinaries = "binaries"
const checkNameBeadsVersion = "beads-version"
const checkNameRegistry = "registry"
const checkNameGovernor = "governor"
const checkNameAdaptiveCap = "adaptive-cap"
const checkNameBreaker = "circuit-breaker"
const checkNameZombies = "zombie-leases"
const checkNameDemand = "stale-demand"
const checkNameQuota = "quota-calibration"
const checkNameVault = "vault-providers"
const checkNameObs = "obs"
const checkNameProxy = "proxy"

// checkLayout verifies the required subdirectory skeleton under Home.
func checkLayout(opts Options) Finding {
	h := opts.home()
	subdirs := []string{"registry.d", "quota", "slots"}
	var missing []string
	for _, sub := range subdirs {
		if _, err := os.Stat(filepath.Join(h, sub)); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, sub)
		}
	}
	if _, err := os.Stat(h); errors.Is(err, os.ErrNotExist) {
		return Finding{Check: checkNameLayout, Level: LevelError,
			Message: "~/.koryph does not exist (run `koryph init`)"}
	}
	if len(missing) == 0 {
		return Finding{Check: checkNameLayout, Level: LevelOK, Message: "layout ok"}
	}
	return Finding{
		Check:   checkNameLayout,
		Level:   LevelError,
		Message: fmt.Sprintf("missing dirs: %s (run `koryph init`)", strings.Join(missing, ", ")),
	}
}

// checkBinaries verifies required tools are on PATH.
func checkBinaries(opts Options) []Finding {
	tools := []string{"git", "claude", "bd"}
	var findings []Finding
	for _, t := range tools {
		if _, err := opts.lookPath(t); err != nil {
			findings = append(findings, Finding{
				Check:   checkNameBinaries,
				Level:   LevelWarn,
				Message: fmt.Sprintf("%s: not found on PATH", t),
			})
		} else {
			findings = append(findings, Finding{
				Check:   checkNameBinaries,
				Level:   LevelOK,
				Message: fmt.Sprintf("%s: ok", t),
			})
		}
	}
	return findings
}

// checkBeadsVersion verifies the resolved bd binary is new enough to emit the
// `parent` field koryph's queue grouping and parent-linked views depend on. An
// older bd (<= 1.0.3) omits `parent` from `bd list --json`, which silently
// degrades the TUI Queue tab to a flat, ungrouped list — a failure with no
// error, so doctor is the surface that must catch it.
func checkBeadsVersion(opts Options) Finding {
	info := opts.beadsVersion()
	switch {
	case !info.Found:
		return Finding{Check: checkNameBeadsVersion, Level: LevelWarn, Message: info.Remediation()}
	case !info.OK:
		return Finding{Check: checkNameBeadsVersion, Level: LevelWarn, Message: info.Remediation()}
	default:
		return Finding{Check: checkNameBeadsVersion, Level: LevelOK,
			Message: fmt.Sprintf("bd %s (parent-capable, >= %s)", info.Version, beads.MinVersion)}
	}
}

// checkRegistry parses every *.json in registry.d to detect corruption.
func checkRegistry(opts Options) Finding {
	dir := filepath.Join(opts.home(), "registry.d")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Finding{Check: checkNameRegistry, Level: LevelWarn, Message: "registry.d not found"}
		}
		return Finding{Check: checkNameRegistry, Level: LevelError,
			Message: fmt.Sprintf("read registry.d: %v", err)}
	}
	total, bad := 0, 0
	var badNames []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		total++
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil || !json.Valid(data) {
			bad++
			badNames = append(badNames, e.Name())
		}
	}
	if bad == 0 {
		return Finding{Check: checkNameRegistry, Level: LevelOK,
			Message: fmt.Sprintf("%d record(s) parse ok", total)}
	}
	return Finding{
		Check:   checkNameRegistry,
		Level:   LevelError,
		Message: fmt.Sprintf("corrupt record(s): %s", strings.Join(badNames, ", ")),
	}
}

// sortedPoolNames returns pools' keys sorted, for deterministic per-pool
// Finding ordering (koryph-v8u.11: every governor pool check now iterates
// pools rather than assuming a single flat governor.json).
func sortedPoolNames(pools map[string]govern.Config) []string {
	out := make([]string, 0, len(pools))
	for p := range pools {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// checkGovernorConfig validates governor.json when present, one Finding per
// pool (koryph-v8u.11 — governor.json is now {"pools": {<provider>: {...}}};
// govern.File's UnmarshalJSON transparently migrates a legacy single-pool
// document into the anthropic pool, so a pre-koryph-v8u.11 store still
// yields exactly the one Finding it always has).
func checkGovernorConfig(opts Options) []Finding {
	path := filepath.Join(opts.home(), "governor.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Finding{{Check: checkNameGovernor, Level: LevelOK,
			Message: fmt.Sprintf("governor.json absent (default cap %d/pool, machine ceiling %d in use)",
				govern.DefaultMaxGlobalAgents, govern.DefaultMaxMachineAgents)}}
	}
	if err != nil {
		return []Finding{{Check: checkNameGovernor, Level: LevelError,
			Message: fmt.Sprintf("read governor.json: %v", err)}}
	}
	var f govern.File
	if err := json.Unmarshal(data, &f); err != nil {
		return []Finding{{Check: checkNameGovernor, Level: LevelError,
			Message: fmt.Sprintf("parse governor.json: %v", err)}}
	}
	pools := sortedPoolNames(f.Pools)
	if len(pools) == 0 {
		return []Finding{{Check: checkNameGovernor, Level: LevelOK, Message: "governor.json: no pools configured"}}
	}
	findings := make([]Finding, 0, len(pools)+1)
	// Machine-wide ceiling across ALL pools (koryph-4rk6.2): a single finding,
	// since it is a machine property, not a per-pool one. Reported first so the
	// operator sees the combined ceiling above the individual pool caps that
	// sum against it.
	if f.MaxMachineAgents <= 0 {
		findings = append(findings, Finding{Check: checkNameGovernor, Level: LevelOK,
			Message: fmt.Sprintf("machine ceiling: %d agents across all pools (default)", govern.DefaultMaxMachineAgents)})
	} else {
		findings = append(findings, Finding{Check: checkNameGovernor, Level: LevelOK,
			Message: fmt.Sprintf("machine ceiling: %d agents across all pools", f.MaxMachineAgents)})
	}
	for _, pool := range pools {
		cfg := f.Pools[pool]
		if cfg.MaxGlobalAgents <= 0 {
			findings = append(findings, Finding{Check: checkNameGovernor, Level: LevelWarn,
				Message: fmt.Sprintf("pool %s: max_global_agents=0 (default %d in use)", pool, govern.DefaultMaxGlobalAgents)})
			continue
		}
		findings = append(findings, Finding{Check: checkNameGovernor, Level: LevelOK,
			Message: fmt.Sprintf("pool %s: cap=%d", pool, cfg.MaxGlobalAgents)})
	}
	return findings
}

// adaptiveCapPinnedThreshold is how long the dynamic cap must have sat at (or
// near) its floor since the last decrease before checkAdaptiveCapPinned warns
// — long enough that a single recent halving (which is expected, normal AIMD
// behavior) does not itself trigger the warning.
const adaptiveCapPinnedThreshold = 30 * time.Minute

// checkAdaptiveCapPinned flags the AIMD overlay (koryph-2im.4,
// docs/designs/2026-07-scheduler-throughput.md L5) sitting at its floor for a
// long time, PER POOL (koryph-v8u.11): either the account is being
// persistently rate-limited, or --hard-max was set too low for the additive
// probe to recover past 1. Informational only (warn, never error) — the
// halve-then-probe design already degrades safely to a floor of 1 on its own.
func checkAdaptiveCapPinned(opts Options) []Finding {
	path := filepath.Join(opts.home(), "governor.json")
	data, err := os.ReadFile(path)
	if err != nil {
		// Absent/unreadable is not this check's concern — checkGovernorConfig
		// already reports on governor.json's readability.
		return []Finding{{Check: checkNameAdaptiveCap, Level: LevelOK, Message: "adaptive overlay not configured"}}
	}
	var f govern.File
	if jerr := json.Unmarshal(data, &f); jerr != nil {
		return []Finding{{Check: checkNameAdaptiveCap, Level: LevelOK, Message: "adaptive overlay off"}}
	}
	pools := sortedPoolNames(f.Pools)
	if len(pools) == 0 {
		return []Finding{{Check: checkNameAdaptiveCap, Level: LevelOK, Message: "adaptive overlay not configured"}}
	}

	findings := make([]Finding, 0, len(pools))
	for _, pool := range pools {
		cfg := f.Pools[pool]
		if !cfg.Adaptive {
			findings = append(findings, Finding{Check: checkNameAdaptiveCap, Level: LevelOK,
				Message: fmt.Sprintf("pool %s: adaptive overlay off", pool)})
			continue
		}

		last, perr := time.Parse(time.RFC3339, cfg.LastDecreaseAt)
		if perr != nil {
			// Adaptive is on but no decrease has ever been recorded — nothing
			// to flag yet (dynamic cap sitting at the operator's chosen
			// starting point is not evidence of throttling).
			findings = append(findings, Finding{Check: checkNameAdaptiveCap, Level: LevelOK,
				Message: fmt.Sprintf("pool %s: adaptive: dynamic cap %d (hard max %d), no rate-limit decrease recorded yet",
					pool, cfg.DynamicCap, cfg.HardMax)})
			continue
		}

		pinned := cfg.DynamicCap <= 1 && opts.now().Sub(last) > adaptiveCapPinnedThreshold
		if !pinned {
			findings = append(findings, Finding{Check: checkNameAdaptiveCap, Level: LevelOK,
				Message: fmt.Sprintf("pool %s: adaptive: dynamic cap %d (hard max %d), last decrease %s, %d rate-limit event(s)",
					pool, cfg.DynamicCap, cfg.HardMax, cfg.LastDecreaseAt, cfg.RateLimitEvents)})
			continue
		}
		findings = append(findings, Finding{Check: checkNameAdaptiveCap, Level: LevelWarn,
			Message: fmt.Sprintf(
				"pool %s: adaptive: dynamic cap pinned at %d for >%v since the last decrease (%s, %d rate-limit event(s) total) — "+
					"the account may be persistently rate-limited, or --hard-max (%d) is too low to recover",
				pool, cfg.DynamicCap, adaptiveCapPinnedThreshold, cfg.LastDecreaseAt, cfg.RateLimitEvents, cfg.HardMax)})
	}
	return findings
}

// breakerFlapReopenThreshold is how many CONSECUTIVE re-opens (a half-open
// probe that itself rate-limited) checkCircuitBreaker calls out as
// "flapping" in its message rather than an ordinary single trip — the
// counter resets to 0 on a clean close (see govern.closeBreaker), so
// reaching this threshold while still open means the account has failed
// several probes in a row without recovering (koryph-2im.11).
const breakerFlapReopenThreshold = 2

// checkCircuitBreaker flags the koryph-2im.11 circuit breaker sitting open
// (or half-open) in any pool (koryph-v8u.11) — evidence that pool's provider
// is persistently rate-limiting, admission is 0 IN THAT POOL while it holds
// (every other pool is unaffected — that is the whole point of per-provider
// pools). Informational only (warn, never error): the breaker's own
// exponential backoff already degrades safely on its own; a closed breaker
// (the steady state) is always OK regardless of how many times it has
// re-opened in the past, since BreakerReopenCount resets on every clean
// close.
func checkCircuitBreaker(opts Options) []Finding {
	path := filepath.Join(opts.home(), "governor.json")
	data, err := os.ReadFile(path)
	if err != nil {
		// Absent/unreadable is not this check's concern — checkGovernorConfig
		// already reports on governor.json's readability.
		return []Finding{{Check: checkNameBreaker, Level: LevelOK, Message: "circuit breaker not configured"}}
	}
	var f govern.File
	if jerr := json.Unmarshal(data, &f); jerr != nil {
		return []Finding{{Check: checkNameBreaker, Level: LevelOK, Message: "circuit breaker off (adaptive overlay off)"}}
	}
	pools := sortedPoolNames(f.Pools)
	if len(pools) == 0 {
		return []Finding{{Check: checkNameBreaker, Level: LevelOK, Message: "circuit breaker not configured"}}
	}

	findings := make([]Finding, 0, len(pools))
	for _, pool := range pools {
		cfg := f.Pools[pool]
		if !cfg.Adaptive {
			findings = append(findings, Finding{Check: checkNameBreaker, Level: LevelOK,
				Message: fmt.Sprintf("pool %s: circuit breaker off (adaptive overlay off)", pool)})
			continue
		}
		if cfg.BreakerState != "open" && cfg.BreakerState != "half-open" {
			findings = append(findings, Finding{Check: checkNameBreaker, Level: LevelOK,
				Message: fmt.Sprintf("pool %s: circuit breaker closed", pool)})
			continue
		}
		flap := ""
		if cfg.BreakerReopenCount >= breakerFlapReopenThreshold {
			flap = " — flapping: it has re-opened repeatedly without a clean close"
		}
		findings = append(findings, Finding{Check: checkNameBreaker, Level: LevelWarn,
			Message: fmt.Sprintf("pool %s: circuit breaker %s (reopen count %d)%s — admission is 0 in this pool while it holds",
				pool, cfg.BreakerState, cfg.BreakerReopenCount, flap)})
	}
	return findings
}

// checkZombieLeases scans governor slot files for leases whose PID is dead,
// across every provider pool (koryph-v8u.11 — a lease's Provider field
// identifies its pool; "" decodes as DefaultPool, a lease written before
// pools existed).
func checkZombieLeases(opts Options) []Finding {
	slotsDir := filepath.Join(opts.home(), "slots")
	entries, err := os.ReadDir(slotsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Finding{{Check: checkNameZombies, Level: LevelOK, Message: "no slots dir (no leases)"}}
		}
		return []Finding{{Check: checkNameZombies, Level: LevelError,
			Message: fmt.Sprintf("read slots dir: %v", err)}}
	}

	var zombies []Finding
	clean := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(slotsDir, e.Name())
		var l govern.Lease
		data, rerr := os.ReadFile(path)
		if rerr != nil || json.Unmarshal(data, &l) != nil || l.Project == "" {
			continue
		}

		// Mirror the govern.Store.prune() logic: before Bind the agent PID is 0,
		// so fall back to the engine PID to avoid pruning un-launched reservations.
		probePID := l.PID
		if probePID <= 0 {
			probePID = l.EnginePID
		}
		if probePID > 0 && opts.alive(probePID) {
			clean++
			continue
		}
		// Agent PID is dead (or zero). A live engine holding this lease means the
		// slot is in a post-build stage (review / rebase / gate / merge) — the
		// NORMAL shape once the agent process exits after building. Only flag as a
		// zombie when the engine itself is also gone (koryph-p42).
		if l.EnginePID > 0 && opts.alive(l.EnginePID) {
			clean++
			continue
		}

		pidStr := "-"
		if probePID > 0 {
			pidStr = fmt.Sprintf("%d", probePID)
		}
		pool := govern.NormalizeProvider(l.Provider)
		f := Finding{
			Check:   checkNameZombies,
			Level:   LevelWarn,
			Message: fmt.Sprintf("zombie lease: pool %s: %s/%s pid=%s (dead)", pool, l.Project, l.Bead, pidStr),
		}
		if opts.Fix {
			if rerr := os.Remove(path); rerr == nil {
				f.Level = LevelOK
				f.Message = fmt.Sprintf("zombie removed: pool %s: %s/%s pid=%s", pool, l.Project, l.Bead, pidStr)
				f.Fixed = true
			}
		}
		zombies = append(zombies, f)
	}

	if len(zombies) == 0 {
		return []Finding{{Check: checkNameZombies, Level: LevelOK,
			Message: fmt.Sprintf("%d active lease(s), none zombie", clean)}}
	}
	return zombies
}

// checkStaleDemand scans demand heartbeats for dead engines or expired TTLs,
// across every provider pool (koryph-v8u.11 — see checkZombieLeases).
func checkStaleDemand(opts Options) []Finding {
	const demandTTL = 10 * time.Minute

	demandDir := filepath.Join(opts.home(), "slots", "demand")
	entries, err := os.ReadDir(demandDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Finding{{Check: checkNameDemand, Level: LevelOK, Message: "no demand heartbeats"}}
		}
		return []Finding{{Check: checkNameDemand, Level: LevelError,
			Message: fmt.Sprintf("read demand dir: %v", err)}}
	}

	var stale []Finding
	fresh := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(demandDir, e.Name())
		var d govern.Demand
		data, rerr := os.ReadFile(path)
		if rerr != nil || json.Unmarshal(data, &d) != nil || d.Project == "" {
			continue
		}

		engineDead := d.EnginePID > 0 && !opts.alive(d.EnginePID)
		heartbeatStale := false
		if t, perr := time.Parse(time.RFC3339, d.UpdatedAt); perr == nil {
			heartbeatStale = opts.now().Sub(t) > demandTTL
		}

		if !engineDead && !heartbeatStale {
			fresh++
			continue
		}

		reason := "engine dead"
		if heartbeatStale && !engineDead {
			reason = fmt.Sprintf("heartbeat stale >%v", demandTTL)
		}
		pool := govern.NormalizeProvider(d.Provider)
		f := Finding{
			Check:   checkNameDemand,
			Level:   LevelWarn,
			Message: fmt.Sprintf("stale demand: pool %s: %s engine_pid=%d (%s)", pool, d.Project, d.EnginePID, reason),
		}
		if opts.Fix {
			if rerr := os.Remove(path); rerr == nil {
				f.Level = LevelOK
				f.Message = fmt.Sprintf("stale demand removed: pool %s: %s (%s)", pool, d.Project, reason)
				f.Fixed = true
			}
		}
		stale = append(stale, f)
	}

	if len(stale) == 0 {
		return []Finding{{Check: checkNameDemand, Level: LevelOK,
			Message: fmt.Sprintf("%d demand heartbeat(s), none stale", fresh)}}
	}
	return stale
}

// checkQuotaCalibration checks whether each per-account quota config has been
// calibrated (both ceiling fields > 0).
func checkQuotaCalibration(opts Options) []Finding {
	quotaDir := filepath.Join(opts.home(), "quota")
	entries, err := os.ReadDir(quotaDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Finding{{Check: checkNameQuota, Level: LevelOK, Message: "no quota configs"}}
		}
		return []Finding{{Check: checkNameQuota, Level: LevelError,
			Message: fmt.Sprintf("read quota dir: %v", err)}}
	}

	var findings []Finding
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		account := strings.TrimSuffix(e.Name(), ".json")
		data, rerr := os.ReadFile(filepath.Join(quotaDir, e.Name()))
		if rerr != nil {
			findings = append(findings, Finding{
				Check:   checkNameQuota,
				Level:   LevelWarn,
				Message: fmt.Sprintf("account %s: cannot read quota config", account),
			})
			continue
		}
		var cfg quota.Config
		if jerr := json.Unmarshal(data, &cfg); jerr != nil {
			findings = append(findings, Finding{
				Check:   checkNameQuota,
				Level:   LevelWarn,
				Message: fmt.Sprintf("account %s: cannot parse quota config: %v", account, jerr),
			})
			continue
		}
		if cfg.WindowCeilingUSD <= 0 && cfg.WeeklyCeilingUSD <= 0 {
			findings = append(findings, Finding{
				Check:   checkNameQuota,
				Level:   LevelWarn,
				Message: fmt.Sprintf("account %s: uncalibrated (run `koryph quota calibrate --account %s ...`)", account, account),
			})
		} else {
			eff := cfg.Ladder.Effective()
			ladderStr := fmt.Sprintf(" ladder=%.0f/%.0f/%.0f/%.0f%%", eff.Warn*100, eff.Throttle*100, eff.GracefulStop*100, eff.HardStop*100)
			if cfg.Ladder.IsDefault() {
				ladderStr = "" // don't clutter OK line when defaults are in use
			}
			findings = append(findings, Finding{
				Check:   checkNameQuota,
				Level:   LevelOK,
				Message: fmt.Sprintf("account %s: 5h=$%.2f wk=$%.2f%s", account, cfg.WindowCeilingUSD, cfg.WeeklyCeilingUSD, ladderStr),
			})
			// If the account has a non-default ladder, report it as a note (LevelOK - it's intentional).
			if !cfg.Ladder.IsDefault() {
				findings = append(findings, Finding{
					Check: checkNameQuota,
					Level: LevelOK,
					Message: fmt.Sprintf("account %s: custom ladder warn=%.0f%% throttle=%.0f%% graceful-stop=%.0f%% hard-stop=%.0f%%",
						account, eff.Warn*100, eff.Throttle*100, eff.GracefulStop*100, eff.HardStop*100),
				})
			}
		}
		// CalibrationStale: proxy config changed since last calibrate run
		// (koryph-3l1.2). Emit a WARN so the operator knows to re-calibrate;
		// the governor still operates on the old slope in the meantime.
		if cfg.CalibrationStale {
			reason := cfg.CalibrationStaleReason
			if reason == "" {
				reason = "proxy config changed"
			}
			findings = append(findings, Finding{
				Check:   checkNameQuota,
				Level:   LevelWarn,
				Message: fmt.Sprintf("account %s: calibration stale — %s; run `koryph quota calibrate --account %s`", account, reason, account),
			})
		}
	}
	if len(findings) == 0 {
		return []Finding{{Check: checkNameQuota, Level: LevelOK, Message: "no quota configs"}}
	}
	return findings
}

// checkQuotaGuardOverride warns when any account has a live billing-guard
// advisory override written by `koryph quota guard`. An override that is
// still within its --until window is intentional, but operators should know
// it is active (the guard is bypassed for that account). Expired overrides
// are silently OK — they revert automatically in the engine. (koryph-i25)
func checkQuotaGuardOverride(opts Options) []Finding {
	const checkName = "quota-guard-override"
	quotaDir := filepath.Join(opts.home(), "quota")
	entries, err := os.ReadDir(quotaDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return nil // quota-calibration already reports on readability
	}

	now := opts.now()
	var findings []Finding
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		account := strings.TrimSuffix(e.Name(), ".json")
		data, rerr := os.ReadFile(filepath.Join(quotaDir, e.Name()))
		if rerr != nil {
			continue
		}
		var cfg quota.Config
		if jerr := json.Unmarshal(data, &cfg); jerr != nil {
			continue
		}
		if advisory, reason := quota.ConfigGuardAdvisory(&cfg, now); advisory {
			findings = append(findings, Finding{
				Check:   checkName,
				Level:   LevelWarn,
				Message: fmt.Sprintf("account %s: billing guard advisory — %s (disable with `koryph quota guard --account %s on`)", account, reason, account),
			})
		}
	}
	return findings
}

// vaultCfg is a minimal vault.json representation — enough to extract the
// binary name for each configured provider without importing the signing
// package (which would add circular-ish import complexity).
type vaultCfg struct {
	Providers map[string]struct {
		Fetch []string `json:"fetch,omitempty"`
	} `json:"providers"`
}

// checkVaultProviders verifies that the binary named as the first token of each
// provider's Fetch template exists on PATH. This only runs when vault.json is
// present under Home (i.e. the user has explicitly configured vault providers).
func checkVaultProviders(opts Options) []Finding {
	vaultPath := filepath.Join(opts.home(), "vault.json")
	data, err := os.ReadFile(vaultPath)
	if errors.Is(err, os.ErrNotExist) {
		return []Finding{{Check: checkNameVault, Level: LevelOK, Message: "vault.json absent (no provider check)"}}
	}
	if err != nil {
		return []Finding{{Check: checkNameVault, Level: LevelWarn,
			Message: fmt.Sprintf("read vault.json: %v", err)}}
	}
	var v vaultCfg
	if jerr := json.Unmarshal(data, &v); jerr != nil {
		return []Finding{{Check: checkNameVault, Level: LevelError,
			Message: fmt.Sprintf("parse vault.json: %v", jerr)}}
	}

	if len(v.Providers) == 0 {
		return []Finding{{Check: checkNameVault, Level: LevelOK, Message: "vault.json: no providers"}}
	}

	seen := map[string]bool{}
	var findings []Finding
	for name, pt := range v.Providers {
		if len(pt.Fetch) == 0 {
			continue
		}
		bin := pt.Fetch[0]
		if seen[bin] {
			continue
		}
		seen[bin] = true
		if _, lerr := opts.lookPath(bin); lerr != nil {
			findings = append(findings, Finding{
				Check:   checkNameVault,
				Level:   LevelWarn,
				Message: fmt.Sprintf("provider %s: binary %q not on PATH", name, bin),
			})
		} else {
			findings = append(findings, Finding{
				Check:   checkNameVault,
				Level:   LevelOK,
				Message: fmt.Sprintf("provider %s: %s ok", name, bin),
			})
		}
	}
	if len(findings) == 0 {
		return []Finding{{Check: checkNameVault, Level: LevelOK, Message: "vault.json: no Fetch templates to check"}}
	}
	return findings
}

// defaultAlive is the production process-liveness probe (signal-0), the default
// behind the injectable Alive seam (Options.Alive) that tests replace.
func defaultAlive(pid int) bool { return procx.Alive(pid) }
