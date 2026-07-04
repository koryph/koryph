// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/paths"
)

func init() {
	registerCmd(command{
		name:    "obs",
		summary: "manage observability: status, level, enable, disable, tail",
		run:     cmdObs,
		subs: []command{
			{
				name:     "status",
				summary:  "print current observability configuration",
				run:      cmdObsStatus,
				DocLinks: []string{"user-guide/observability.md"},
			},
			{
				name:     "level",
				summary:  "set the log level for a component (or default)",
				run:      cmdObsLevel,
				DocLinks: []string{"user-guide/observability.md"},
			},
			{
				name:     "enable",
				summary:  "enable observability (set default level to info)",
				run:      cmdObsEnable,
				DocLinks: []string{"user-guide/observability.md"},
			},
			{
				name:     "disable",
				summary:  "silence all output (set all levels to error)",
				run:      cmdObsDisable,
				DocLinks: []string{"user-guide/observability.md"},
			},
			{
				name:     "tail",
				summary:  "tail the telemetry JSONL stream in human-readable form",
				run:      cmdObsTail,
				DocLinks: []string{"user-guide/observability.md"},
			},
		},
		DocLinks: []string{
			"user-guide/observability.md",
		},
	})
}

// cmdObs dispatches to the appropriate sub-command or prints usage.
func cmdObs(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		obsUsage(stdout)
		return 0
	}
	parent := lookupCommand("obs")
	sub := findSub(parent, args[0])
	if sub == nil {
		fmt.Fprintf(stderr, "koryph obs: unknown sub-command %q\n\n", args[0])
		obsUsage(stderr)
		return 1
	}
	return sub.run(args[1:], stdout, stderr)
}

func obsUsage(w io.Writer) {
	fmt.Fprintln(w, `koryph obs — manage observability configuration and telemetry

USAGE
  koryph obs <sub-command> [flags]

SUB-COMMANDS
  status                        print current observability configuration
  level <component> <level>     set the log level for a component
  level default <level>         set the global default level
  enable                        enable observability (set default to info if silenced)
  disable                       silence all output (set all levels to error)
  tail [--component C] [-n N] [--follow] [--level L]
                                tail the telemetry JSONL stream in human-readable form

LEVELS
  trace  debug  info  warn  error

EXAMPLES
  koryph obs status
  koryph obs level engine debug
  koryph obs level govern trace
  koryph obs level default warn
  koryph obs enable
  koryph obs disable
  koryph obs tail --component govern
  koryph obs tail -n 100 --follow
  koryph obs tail --level warn --follow`)
}

// cmdObsStatus prints the current observability configuration.
func cmdObsStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("obs status", stderr)
	jsonOut := fs.Bool("json", false, "emit as JSON")
	setUsage(fs, stdout, "print current observability configuration", "[--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	cfg, err := obs.LoadConfig()
	if err != nil {
		return fail(stderr, fmt.Errorf("obs status: %w", err))
	}

	if *jsonOut {
		if jerr := printJSON(stdout, cfg); jerr != nil {
			return fail(stderr, jerr)
		}
		return 0
	}

	configPath := paths.ObsConfig()
	fmt.Fprintf(stdout, "obs config: %s\n\n", configPath)

	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "default_level\t%s\n", cfg.DefaultLevel)
	fmt.Fprintf(tw, "format\t%s\n", cfg.Format)
	if cfg.OTELEndpoint != "" {
		fmt.Fprintf(tw, "otel_endpoint\t%s\n", cfg.OTELEndpoint)
	} else {
		fmt.Fprintf(tw, "otel_endpoint\t(not set — local JSONL only)\n")
	}
	if cfg.File != "" {
		fmt.Fprintf(tw, "file\t%s\n", cfg.File)
	}
	_ = tw.Flush()

	if len(cfg.Components) == 0 {
		fmt.Fprintln(stdout, "\ncomponent overrides: (none — all using default_level)")
	} else {
		fmt.Fprintln(stdout, "\ncomponent overrides:")
		tw2 := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw2, "  COMPONENT\tLEVEL")
		for _, c := range obsComponentsSorted(cfg.Components) {
			fmt.Fprintf(tw2, "  %s\t%s\n", c, cfg.Components[c])
		}
		_ = tw2.Flush()
	}

	telDir := paths.TelemetryDir()
	if fi, sterr := os.Stat(telDir); sterr == nil && fi.IsDir() {
		fmt.Fprintf(stdout, "\ntelemetry dir: %s\n", telDir)
	} else {
		fmt.Fprintf(stdout, "\ntelemetry dir: %s (not created yet)\n", telDir)
	}

	// Advisory: show active env overrides.
	var envHints []string
	if v := os.Getenv("KORYPH_LOG_LEVEL"); v != "" {
		envHints = append(envHints, fmt.Sprintf("KORYPH_LOG_LEVEL=%s (overrides default_level)", v))
	}
	if v := os.Getenv("KORYPH_LOG_FORMAT"); v != "" {
		envHints = append(envHints, fmt.Sprintf("KORYPH_LOG_FORMAT=%s (overrides format)", v))
	}
	if v := os.Getenv("KORYPH_OTEL_ENDPOINT"); v != "" {
		envHints = append(envHints, fmt.Sprintf("KORYPH_OTEL_ENDPOINT=%s (overrides otel_endpoint)", v))
	}
	if len(envHints) > 0 {
		fmt.Fprintln(stdout, "\nactive env overrides:")
		for _, h := range envHints {
			fmt.Fprintln(stdout, "  "+h)
		}
	}

	return 0
}

// cmdObsLevel sets the log level for a named component, or the global default.
//
//	koryph obs level <component> <level>
//	koryph obs level default <level>
func cmdObsLevel(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("obs level", stderr)
	setUsage(fs, stdout, "set log level for a component or the default",
		"<component|default> <trace|debug|info|warn|error>")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) < 2 {
		return usageErr(stderr, "obs level: <component> and <level> are required")
	}
	component, levelStr := pos[0], strings.ToLower(pos[1])

	if component == "default" {
		cfg, setErr := obs.SetDefaultLevel(levelStr)
		if setErr != nil {
			return fail(stderr, setErr)
		}
		fmt.Fprintf(stdout, "obs: default level set to %s\n", cfg.DefaultLevel)
	} else {
		cfg, setErr := obs.SetComponentLevel(component, levelStr)
		if setErr != nil {
			return fail(stderr, setErr)
		}
		fmt.Fprintf(stdout, "obs: %s level set to %s\n", component, cfg.Components[component])
	}
	fmt.Fprintf(stdout, "config written to %s (live loops pick it up at next tick)\n", paths.ObsConfig())
	return 0
}

// cmdObsEnable enables observability: sets default_level to info if it was
// previously silenced (error), otherwise preserves the current level.
func cmdObsEnable(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("obs enable", stderr)
	setUsage(fs, stdout, "enable observability (set default level to info if silenced)", "")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	cfg, err := obs.EnableObs()
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "obs enabled: default_level=%s\n", cfg.DefaultLevel)
	fmt.Fprintf(stdout, "config written to %s\n", paths.ObsConfig())
	return 0
}

// cmdObsDisable silences all observability output by setting all levels to error.
func cmdObsDisable(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("obs disable", stderr)
	setUsage(fs, stdout, "disable observability (set all levels to error)", "")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	if _, err := obs.DisableObs(); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "obs disabled: all levels set to error")
	fmt.Fprintf(stdout, "config written to %s\n", paths.ObsConfig())
	return 0
}

// cmdObsTail tails the telemetry JSONL stream from ~/.koryph/telemetry/ in
// human-readable form. With --follow it streams new records until Ctrl-C.
func cmdObsTail(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("obs tail", stderr)
	component := fs.String("component", "", "filter to this component (engine|govern|sched|…)")
	n := fs.Int("n", 40, "number of trailing records to show (0 = all)")
	follow := fs.Bool("follow", false, "stream new records as they arrive (Ctrl-C to stop)")
	levelStr := fs.String("level", "", "minimum level to display (trace|debug|info|warn|error)")
	setUsage(fs, stdout, "tail the telemetry JSONL stream in human-readable form",
		"[--component C] [-n 40] [--follow] [--level L]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	var minLevel slog.Level
	hasLvlFilter := false
	if *levelStr != "" {
		l, ok := obs.ParseLevel(*levelStr)
		if !ok {
			return usageErr(stderr, fmt.Sprintf("obs tail: unknown level %q", *levelStr))
		}
		minLevel = l
		hasLvlFilter = true
	}

	telDir := paths.TelemetryDir()

	// Print historical records first.
	recs, err := obs.TailRecords(obs.TailOptions{
		Dir:            telDir,
		Component:      *component,
		Level:          minLevel,
		HasLevelFilter: hasLvlFilter,
		N:              *n,
	})
	if err != nil {
		return fail(stderr, err)
	}
	if len(recs) == 0 && !*follow {
		comp := ""
		if *component != "" {
			comp = fmt.Sprintf(" for component %q", *component)
		}
		fmt.Fprintf(stdout, "no telemetry records found%s in %s\n", comp, telDir)
		return 0
	}
	for _, r := range recs {
		obs.FormatRecord(stdout, r)
	}

	if !*follow {
		return 0
	}

	fmt.Fprintln(stdout, "\n-- following (Ctrl-C to stop) --")
	sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	obs.TailFollow(sctx, stdout, telDir, *component, minLevel, hasLvlFilter)
	return 0
}

// obsComponentsSorted returns the component names from m in sorted order.
func obsComponentsSorted(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Insertion sort — component maps are tiny (O(n^2) acceptable).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
