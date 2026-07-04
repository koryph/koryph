// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

// console_golden_test.go verifies that the engine's progress() calls produce
// the expected human-readable console lines (byte-identical at INFO level to
// the obs structured log records). Uses a golden-file pattern so the expected
// output is easy to review and update.
//
// The golden file is normalised before comparison: dynamic values (PIDs, SHAs,
// timestamps, model rationale text) are replaced with stable placeholders so
// the test remains deterministic across machines and runs.
//
// Run with -update-golden to regenerate testdata/golden/engine-console.txt.

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "regenerate golden console output files")

// normalizeConsole replaces dynamic values in engine console output with
// stable placeholders so the golden comparison is deterministic.
//
// Replacements:
//   - "pid N"         -> "pid PID"
//   - "merged (XXXXXX)"  -> "merged (SHA)"
//   - model rationale parenthetical (anything after the model tier) -> normalised
//   - governor uncalibrated / est $... / window ...%   -> "(governor uncalibrated)"
func normalizeConsole(s string) string {
	// PID
	s = rePID.ReplaceAllString(s, "pid PID")
	// merged SHA
	s = reSHA.ReplaceAllString(s, "merged (SHA)")
	// model rationale: "model sonnet — <anything>;" -> "model sonnet — RATIONALE;"
	s = reModelWhy.ReplaceAllString(s, "model $1 — RATIONALE;")
	// window note: "(est $X.XX / window Y%)" or "(governor uncalibrated)"
	s = reWindowNote.ReplaceAllString(s, "(WINDOW)")
	// rebased SHA messages
	s = reRebasedSHA.ReplaceAllString(s, "BASESHA")
	return s
}

var (
	rePID        = regexp.MustCompile(`pid \d+`)
	reSHA        = regexp.MustCompile(`merged \([0-9a-f]{6,}\)`)
	reModelWhy   = regexp.MustCompile(`model (\S+) — [^;]+;`)
	reWindowNote = regexp.MustCompile(`\((?:est \$[\d.]+ (?:\+/-\d+%% )?/ window [\d.]+%|governor uncalibrated)\)`)
	reRebasedSHA = regexp.MustCompile(`[0-9a-f]{7,40}`)
)

// goldenConsoleOutput runs the engine once with the standard fixture, captures
// console output, normalises it, and returns the sorted (stable) lines.
func goldenConsoleOutput(t *testing.T) string {
	t.Helper()
	newFixture(t, fixOpts{})
	var out bytes.Buffer
	ctx := context.Background()
	_, err := Run(ctx, baseOptions(&out))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw := out.String()
	// Normalize and collect non-empty lines. Health-patrol lines are emitted by
	// the in-loop health checker (a separate subsystem); they contain non-
	// deterministic file paths and are not part of the dispatch-flow golden.
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := normalizeConsole(sc.Text())
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "health patrol") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestConsoleOutputGolden(t *testing.T) {
	got := goldenConsoleOutput(t)
	goldenPath := filepath.Join("testdata", "golden", "engine-console.txt")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden file updated: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("golden file missing — run with -update-golden to generate: %v", err)
	}

	if string(want) != got {
		t.Errorf("console output does not match golden file %s\n--- want ---\n%s\n--- got ---\n%s",
			goldenPath, want, got)
	}
}
