// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/sysmem"
)

func init() {
	registerCmd(command{
		name:    "governor",
		summary: "inspect and set the machine-wide concurrency cap",
		run:     cmdGovernor,
		DocLinks: []string{
			"concepts/governors.md",
			"user-guide/billing-and-quota.md",
			"developer-guide/global-governor.md",
		},
		subs: []command{
			{
				name:     "show",
				summary:  "show the cap, active leases, and demand",
				DocLinks: []string{"concepts/governors.md"},
			},
			{
				name:     "set",
				summary:  "set the machine-wide cap",
				run:      cmdGovernorSet,
				DocLinks: []string{"concepts/governors.md", "developer-guide/global-governor.md"},
			},
		},
	})
}

// cmdGovernor dispatches the global concurrency governor sub-verbs.
func cmdGovernor(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return cmdGovernorShow(nil, stdout, stderr)
	}
	switch args[0] {
	case "show":
		return cmdGovernorShow(args[1:], stdout, stderr)
	case "set":
		return cmdGovernorSet(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		parentHelp(stdout, "governor", "inspect and set per-provider agent concurrency pools", []subVerb{
			{"show [--json]", "show every pool's cap, active leases, and demanding projects (default)"},
			{"set --max-global N [--provider P] [--adaptive] [--hard-max M] [--settle-sec S] [--break-sec B] [--min-dispatch-interval I]",
				"set one pool's cap (--provider omitted = anthropic); --adaptive enables the AIMD overlay (settle/breaker/smoothing apply only with --adaptive)"},
		})
		return 0
	default:
		return usageErr(stderr, fmt.Sprintf("unknown governor subcommand %q (want show|set)", args[0]))
	}
}

// governorPoolJSON is the --json shape for one pool in `governor show`. It
// includes the computed derived fields (cap, in_use, free) that the human
// table renders, so scripts can avoid re-computing them.
type governorPoolJSON struct {
	Pool   string          `json:"pool"`
	Cap    int             `json:"cap"`
	InUse  int             `json:"in_use"`
	Free   int             `json:"free"`
	AIMD   govern.Config   `json:"aimd"`
	Leases []govern.Lease  `json:"leases"`
	Demand []govern.Demand `json:"demand"`
}

// cmdGovernorShow prints EVERY provider pool's cap plus its live leases and
// demand (koryph-v8u.11: independent per-provider governor pools — see
// internal/govern's package doc), pruning stale state as a side effect. A
// machine with only the default anthropic pool in use prints exactly one
// pool block, so this is a superset of the pre-koryph-v8u.11 single-pool
// output. With --json it emits a JSON array of governorPoolJSON, one entry
// per pool.
func cmdGovernorShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("governor show", stderr)
	asJSON := fs.Bool("json", false, "emit JSON array of pool snapshots")
	setUsage(fs, stdout, "show every pool's cap, active leases, and demanding projects", "[--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	gs := govern.NewStore()
	pools, err := gs.Pools()
	if err != nil {
		return fail(stderr, err)
	}

	if *asJSON {
		snaps := make([]governorPoolJSON, 0, len(pools))
		for _, pool := range pools {
			ps, err := gs.PoolStatus(pool)
			if err != nil {
				return fail(stderr, err)
			}
			eff := ps.AIMD.EffectiveCap()
			leases := ps.Leases
			if leases == nil {
				leases = []govern.Lease{}
			}
			demand := ps.Demand
			if demand == nil {
				demand = []govern.Demand{}
			}
			snaps = append(snaps, governorPoolJSON{
				Pool:   pool,
				Cap:    gs.Cap(pool),
				InUse:  len(leases),
				Free:   max0(eff - len(leases)),
				AIMD:   ps.AIMD,
				Leases: leases,
				Demand: demand,
			})
		}
		if err := printJSON(stdout, snaps); err != nil {
			return fail(stderr, err)
		}
		return 0
	}

	for i, pool := range pools {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		if err := printPoolStatus(stdout, gs, pool); err != nil {
			return fail(stderr, err)
		}
	}
	return 0
}

// printPoolStatus prints one pool's block of `governor show` output.
func printPoolStatus(stdout io.Writer, gs *govern.Store, pool string) error {
	ps, err := gs.PoolStatus(pool)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "pool %s:\n", pool)
	fmt.Fprintf(stdout, "  global concurrency cap: %d\n", gs.Cap(pool))
	printMemoryFloor(stdout, gs.MinFreeMemoryMB(pool))

	// AIMD overlay (koryph-2im.4): show adaptive status and, when on, the
	// dynamic cap that Acquire actually admits against — for THIS pool only.
	aimd := ps.AIMD
	eff := aimd.EffectiveCap()
	if aimd.Adaptive {
		lastDecrease := aimd.LastDecreaseAt
		if lastDecrease == "" {
			lastDecrease = "never"
		}
		fmt.Fprintf(stdout, "  adaptive: on (dynamic cap %d, hard max %d, last decrease %s, rate-limit events %d)\n",
			aimd.DynamicCap, aimd.HardMax, lastDecrease, aimd.RateLimitEvents)

		// Settle/breaker/smoothing (koryph-2im.11).
		if until, perr := time.Parse(time.RFC3339, aimd.SettleUntil); perr == nil && time.Now().Before(until) {
			fmt.Fprintf(stdout, "  settle: active until %s\n", aimd.SettleUntil)
		} else {
			fmt.Fprintln(stdout, "  settle: not active")
		}
		fmt.Fprintf(stdout, "  breaker: %s\n", breakerSummary(aimd))
		fmt.Fprintf(stdout, "  smoothing: min dispatch interval %ds (jittered ±50%%)\n", minDispatchIntervalDisplay(aimd))
	} else {
		fmt.Fprintln(stdout, "  adaptive: off")
	}

	fmt.Fprintf(stdout, "  in use: %d    free: %d\n", len(ps.Leases), max0(eff-len(ps.Leases)))

	if len(ps.Demand) > 0 {
		projects := make([]string, 0, len(ps.Demand))
		for _, d := range ps.Demand {
			projects = append(projects, d.Project)
		}
		fmt.Fprintf(stdout, "  demanding projects (%d): %v\n", len(ps.Demand), projects)
	}

	if len(ps.Leases) == 0 {
		fmt.Fprintln(stdout, "  no active leases")
		return nil
	}
	fmt.Fprintln(stdout, "\n  active leases:")
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "    PROJECT\tBEAD\tPID\tMODEL\tSINCE")
	for _, l := range ps.Leases {
		pid := "-"
		if l.PID > 0 {
			pid = fmt.Sprintf("%d", l.PID)
		}
		model := l.Model
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\n", l.Project, l.Bead, pid, model, l.AcquiredAt)
	}
	_ = tw.Flush()
	return nil
}

// cmdGovernorSet writes a new cap to ONE provider pool ("" is anthropic,
// koryph-v8u.11) in governor.json. Without --adaptive it is exactly today's
// static cap for that pool (and clears/disables any previously-enabled AIMD
// overlay on that same pool, since it overwrites that pool's entry wholesale
// — koryph-2im.4; every OTHER pool is untouched). With --adaptive, it seeds
// that pool's AIMD overlay: --max-global is the starting/floor cap and
// --hard-max bounds upward probing (default 2x --max-global).
// --settle-sec/--break-sec/--min-dispatch-interval (koryph-2im.11) configure
// the settle window, circuit breaker, and dispatch smoothing; all three are
// meaningless without --adaptive (a plain `set` ignores them) and default
// when omitted or non-positive.
func cmdGovernorSet(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("governor set", stderr)
	provider := fs.String("provider", "", "governor pool to configure (default: anthropic) — koryph-v8u.11 independent per-provider pools")
	maxGlobal := fs.Int("max-global", 0, "cap on concurrently running agents in this pool (required, > 0)")
	adaptive := fs.Bool("adaptive", false, "enable the AIMD overlay: probe the cap up on quiet, halve it on rate-limit")
	hardMax := fs.Int("hard-max", 0, "absolute ceiling for upward probing under --adaptive (default 2x --max-global)")
	settleSec := fs.Int("settle-sec", 0, "settle window after any cap change, under --adaptive (default 120)")
	breakSec := fs.Int("break-sec", 0, "circuit breaker base open duration, under --adaptive (default 300, doubles per re-open, cap 3600)")
	minDispatchInterval := fs.Int("min-dispatch-interval", 0, "minimum inter-dispatch spacing in seconds, under --adaptive (default 3, jittered ±50%)")
	minFreeMem := fs.Int("min-free-memory-mb", 0, "memory admission floor (koryph-930): defer new agents while host available memory is below N MB. 0 = auto-size to physical memory (the default; the gate is ON); a negative value disables the gate. May be set alone or alongside --max-global")
	setUsage(fs, stdout, "set one provider pool's cap on concurrently running agents",
		"[--max-global N] [--min-free-memory-mb N] [--provider P] [--adaptive] [--hard-max M] [--settle-sec S] [--break-sec B] [--min-dispatch-interval I]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	// Detect whether --min-free-memory-mb was given (0 is a meaningful value —
	// "reset to auto" — so a sentinel default won't do).
	memProvided := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "min-free-memory-mb" {
			memProvided = true
		}
	})
	pool := govern.NormalizeProvider(*provider)
	gs := govern.NewStore()

	// Floor-only invocation: adjust the memory gate without resetting the pool's
	// cap or AIMD state (SetMinFreeMemoryMB preserves every other field).
	if *maxGlobal <= 0 {
		if !memProvided {
			return usageErr(stderr, "governor set: need --max-global (> 0) and/or --min-free-memory-mb")
		}
		if err := gs.SetMinFreeMemoryMB(pool, *minFreeMem); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "%s [pool %s]\n", memFloorMsg(*minFreeMem), pool)
		return 0
	}

	if *adaptive {
		if err := gs.SetAdaptiveCap(pool, *maxGlobal, *hardMax, *settleSec, *breakSec, *minDispatchInterval); err != nil {
			return fail(stderr, err)
		}
		hm := *hardMax
		if hm <= 0 {
			hm = *maxGlobal * 2
		}
		fmt.Fprintf(stdout, "global concurrency cap set to %d (adaptive: dynamic cap %d, hard max %d) [pool %s]\n",
			*maxGlobal, *maxGlobal, hm, pool)
	} else {
		if err := gs.SetCap(pool, *maxGlobal); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "global concurrency cap set to %d [pool %s]\n", *maxGlobal, pool)
	}

	// A floor requested alongside the cap must be written AFTER the cap set,
	// since SetCap/SetAdaptiveCap reset the pool wholesale.
	if memProvided {
		if err := gs.SetMinFreeMemoryMB(pool, *minFreeMem); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "%s [pool %s]\n", memFloorMsg(*minFreeMem), pool)
	}
	return 0
}

// printMemoryFloor reports the effective memory admission floor for `governor
// show` (koryph-930). The raw setting is >0 explicit, <0 disabled, or 0 auto —
// for auto it probes physical memory to show the resolved floor.
func printMemoryFloor(stdout io.Writer, setting int) {
	switch {
	case setting > 0:
		fmt.Fprintf(stdout, "  memory floor: defer new agents below %d MB free (koryph-930)\n", setting)
	case setting < 0:
		fmt.Fprintln(stdout, "  memory floor: disabled")
	default:
		if stat, err := sysmem.Available(); err == nil {
			auto := sysmem.DefaultFloorMB(stat.TotalMB())
			fmt.Fprintf(stdout, "  memory floor: auto — defer new agents below %d MB free (~1/8 of %d MB physical) [koryph-930]\n", auto, stat.TotalMB())
		} else {
			fmt.Fprintln(stdout, "  memory floor: auto (sized to physical memory) [koryph-930]")
		}
	}
}

// memFloorMsg renders the confirmation line for a memory-floor change
// (koryph-930): >0 an explicit floor, <0 disabled, 0 reset to the auto floor.
func memFloorMsg(mb int) string {
	switch {
	case mb > 0:
		return fmt.Sprintf("memory admission floor set to %d MB free", mb)
	case mb < 0:
		return "memory admission gate disabled"
	default:
		return "memory admission floor reset to auto (sized to physical memory)"
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// breakerSummary renders the circuit breaker's state for `governor show`
// (koryph-2im.11): closed, open (with its computed until-deadline and the
// consecutive-reopen count), or half-open (with the outstanding probe
// identity, if any).
func breakerSummary(cfg govern.Config) string {
	switch cfg.BreakerState {
	case "open":
		dur := cfg.BreakerBreakSeconds
		if dur <= 0 {
			dur = govern.DefaultBreakSeconds
		}
		until := "unknown"
		if opened, err := time.Parse(time.RFC3339, cfg.BreakerOpenAt); err == nil {
			until = opened.Add(time.Duration(dur) * time.Second).UTC().Format(time.RFC3339)
		}
		return fmt.Sprintf("open until %s (reopen count %d)", until, cfg.BreakerReopenCount)
	case "half-open":
		if cfg.ProbeProject == "" {
			return "half-open (no probe outstanding)"
		}
		return fmt.Sprintf("half-open (probe: %s/%s)", cfg.ProbeProject, cfg.ProbeBead)
	default:
		return "closed"
	}
}

// minDispatchIntervalDisplay returns the effective smoothing interval (the
// stored value, or this package's documented default when unset) for
// `governor show`.
func minDispatchIntervalDisplay(cfg govern.Config) int {
	if cfg.MinDispatchIntervalSeconds > 0 {
		return cfg.MinDispatchIntervalSeconds
	}
	return govern.DefaultMinDispatchIntervalSeconds
}
