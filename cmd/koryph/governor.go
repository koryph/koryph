// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/govern"
)

// cmdGovernor dispatches the global concurrency governor sub-verbs.
func cmdGovernor(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return cmdGovernorShow(stdout, stderr)
	}
	switch args[0] {
	case "show":
		return cmdGovernorShow(stdout, stderr)
	case "set":
		return cmdGovernorSet(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		parentHelp(stdout, "governor", "inspect and set the machine-wide agent concurrency cap", []subVerb{
			{"show", "show the cap, active leases, and demanding projects (default)"},
			{"set --max-global N [--adaptive] [--hard-max M]", "set the machine-wide cap; --adaptive enables the AIMD overlay"},
		})
		return 0
	default:
		return usageErr(stderr, fmt.Sprintf("unknown governor subcommand %q (want show|set)", args[0]))
	}
}

// cmdGovernorShow prints the machine-wide cap plus the live leases and demand,
// pruning stale state as a side effect.
func cmdGovernorShow(stdout, stderr io.Writer) int {
	gs := govern.NewStore()
	cap, leases, demand, err := gs.Snapshot()
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "global concurrency cap: %d\n", cap)

	// AIMD overlay (koryph-2im.4): show adaptive status and, when on, the
	// dynamic cap that Acquire actually admits against.
	aimd, err := gs.AIMDStatus()
	if err != nil {
		return fail(stderr, err)
	}
	eff := aimd.EffectiveCap()
	if aimd.Adaptive {
		lastDecrease := aimd.LastDecreaseAt
		if lastDecrease == "" {
			lastDecrease = "never"
		}
		fmt.Fprintf(stdout, "adaptive: on (dynamic cap %d, hard max %d, last decrease %s, rate-limit events %d)\n",
			aimd.DynamicCap, aimd.HardMax, lastDecrease, aimd.RateLimitEvents)
	} else {
		fmt.Fprintln(stdout, "adaptive: off")
	}

	fmt.Fprintf(stdout, "in use: %d    free: %d\n", len(leases), max0(eff-len(leases)))

	if len(demand) > 0 {
		projects := make([]string, 0, len(demand))
		for _, d := range demand {
			projects = append(projects, d.Project)
		}
		fmt.Fprintf(stdout, "demanding projects (%d): %v\n", len(demand), projects)
	}

	if len(leases) == 0 {
		fmt.Fprintln(stdout, "no active leases")
		return 0
	}
	fmt.Fprintln(stdout, "\nactive leases:")
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  PROJECT\tBEAD\tPID\tMODEL\tSINCE")
	for _, l := range leases {
		pid := "-"
		if l.PID > 0 {
			pid = fmt.Sprintf("%d", l.PID)
		}
		model := l.Model
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", l.Project, l.Bead, pid, model, l.AcquiredAt)
	}
	_ = tw.Flush()
	return 0
}

// cmdGovernorSet writes a new machine-wide cap to governor.json. Without
// --adaptive it is exactly today's static cap (and clears/disables any
// previously-enabled AIMD overlay, since it overwrites governor.json wholesale
// — koryph-2im.4). With --adaptive, it seeds the AIMD overlay: --max-global is
// the starting/floor cap and --hard-max bounds upward probing (default
// 2x --max-global).
func cmdGovernorSet(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("governor set", stderr)
	maxGlobal := fs.Int("max-global", 0, "machine-wide cap on concurrently running agents (required, > 0)")
	adaptive := fs.Bool("adaptive", false, "enable the AIMD overlay: probe the cap up on quiet, halve it on rate-limit")
	hardMax := fs.Int("hard-max", 0, "absolute ceiling for upward probing under --adaptive (default 2x --max-global)")
	setUsage(fs, stdout, "set the machine-wide cap on concurrently running agents", "--max-global N [--adaptive] [--hard-max M]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *maxGlobal <= 0 {
		return usageErr(stderr, "governor set: --max-global must be a positive integer")
	}
	gs := govern.NewStore()
	if *adaptive {
		if err := gs.SetAdaptiveCap(*maxGlobal, *hardMax); err != nil {
			return fail(stderr, err)
		}
		hm := *hardMax
		if hm <= 0 {
			hm = *maxGlobal * 2
		}
		fmt.Fprintf(stdout, "global concurrency cap set to %d (adaptive: dynamic cap %d, hard max %d)\n",
			*maxGlobal, *maxGlobal, hm)
		return 0
	}
	if err := gs.SetCap(*maxGlobal); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "global concurrency cap set to %d\n", *maxGlobal)
	return 0
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
