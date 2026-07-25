// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/metrics"
)

const defaultOfflineAutonomyReportPath = "offline-autonomy-diagnostic.json"

// cmdMetricsAutonomy derives the canary decision from strict typed evidence
// and publishes the report before returning a failing SLO exit code. Keeping
// publication ahead of the exit preserves rollback evidence on every failure.
func cmdMetricsAutonomy(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("metrics autonomy", stderr)
	projectID := fs.String("project", "", "inspect the native report for this project")
	inputPath := fs.String("input", "", "schema-versioned canary evidence input")
	outputPath := fs.String("out", defaultOfflineAutonomyReportPath, "offline immutable report path (native fixed path is reserved)")
	asJSON := fs.Bool("json", false, "also emit the complete report as JSON")
	setUsage(fs, stdout,
		"inspect the native immutable autonomy report, or explicitly publish typed evidence",
		"[--project ID] [--json] | --input PATH [--out PATH] [--json]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 0 || (*inputPath != "" && *projectID != "") || *outputPath == "" {
		return usageErr(stderr, "metrics autonomy: use either --project ID or --input PATH with a non-empty --out PATH")
	}
	if *inputPath == "" {
		rec, code := resolveLoopProject(*projectID, "metrics autonomy", stderr)
		if code != 0 {
			return code
		}
		path := filepath.Join(rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath))
		report, err := metrics.LoadAutonomyReport(path)
		if err != nil {
			return fail(stderr, fmt.Errorf("metrics autonomy: load native report %s: %w", path, err))
		}
		return renderAutonomyReport(path, report, *asJSON, stdout, stderr)
	}
	out := filepath.Clean(*outputPath)
	if reservedNativeAutonomyReportPath(out) {
		return usageErr(stderr,
			"metrics autonomy: the native autonomous-loop-reliability.json path is reserved; choose a different --out path for offline diagnostics")
	}
	inputFile, err := os.Open(*inputPath)
	if err != nil {
		return fail(stderr, fmt.Errorf("metrics autonomy: open input: %w", err))
	}
	input, decodeErr := metrics.DecodeAutonomyInput(inputFile)
	closeErr := inputFile.Close()
	if decodeErr != nil {
		return fail(stderr, fmt.Errorf("metrics autonomy: decode input: %w", decodeErr))
	}
	if closeErr != nil {
		return fail(stderr, fmt.Errorf("metrics autonomy: close input: %w", closeErr))
	}
	report, err := metrics.PublishAutonomyReport(out, input, time.Now())
	if err != nil {
		return fail(stderr, fmt.Errorf("metrics autonomy: publish %s: %w", out, err))
	}
	return renderAutonomyReport(out, report, *asJSON, stdout, stderr)
}

func reservedNativeAutonomyReportPath(path string) bool {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "./")
	reserved := filepath.ToSlash(metrics.DefaultAutonomyReportRelativePath)
	return clean == reserved || strings.HasSuffix(clean, "/"+reserved)
}

func renderAutonomyReport(
	path string,
	report *metrics.AutonomyReport,
	asJSON bool,
	stdout, stderr io.Writer,
) int {
	if asJSON {
		if err := printJSON(stdout, report); err != nil {
			return fail(stderr, err)
		}
	} else {
		fmt.Fprintf(stdout,
			"autonomy report: %s passed=%t eligible=%d/%d completion=%.1f%% first-review=%.1f%% retries=%.1f%% median=%s p95=%s\n",
			path, report.Decision.Passed, report.Metrics.EligibleBeads, report.Metrics.CohortBeads,
			report.Metrics.AutonomousCompletion.Rate*100,
			report.Metrics.FirstReviewPass.Rate*100,
			report.Metrics.RetryDispatches.Rate*100,
			time.Duration(report.Metrics.DispatchToTerminal.MedianMS)*time.Millisecond,
			time.Duration(report.Metrics.DispatchToTerminal.P95MS)*time.Millisecond,
		)
	}
	if !report.Decision.Passed {
		fmt.Fprintf(stderr, "metrics autonomy: canary posture failed (%d safety violation(s))\n",
			len(report.Decision.SafetyViolations))
		return engine.ExitFatal
	}
	return engine.ExitOK
}
