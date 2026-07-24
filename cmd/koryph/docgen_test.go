// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

// TestCLIRefDrift is the green-gate drift check for docs/reference/cli.md.
// It regenerates the CLI reference from the current command registry and
// diffs it against the committed file. A command or flag change without a
// docs regeneration fails this test.
//
// Fix:  koryph __docgen > docs/reference/cli.md
// Then: git add docs/reference/cli.md && git commit -s

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCLIRefDrift(t *testing.T) {
	t.Helper()

	// Regenerate in memory.
	var got bytes.Buffer
	renderCLIDoc(&got, io.Discard)

	// Read the committed reference.
	wantBytes, err := os.ReadFile("../../docs/reference/cli.md")
	if err != nil {
		t.Fatalf(
			"docs/reference/cli.md not found or unreadable: %v\n\n"+
				"Re-generate and commit:\n"+
				"  go run ./cmd/koryph/ __docgen > docs/reference/cli.md\n"+
				"  git add docs/reference/cli.md && git commit -s\n",
			err,
		)
	}

	want := string(wantBytes)
	if got.String() == want {
		return
	}

	// Produce a compact diff (first differing line pair) for quick diagnosis.
	t.Errorf(
		"docs/reference/cli.md is stale.\n\n"+
			"Re-generate and commit:\n"+
			"  go run ./cmd/koryph/ __docgen > docs/reference/cli.md\n"+
			"  git add docs/reference/cli.md && git commit -s\n\n"+
			"First difference:\n%s",
		firstDiff(want, got.String()),
	)
}

func TestRenderNestedGroupingUsesFullCommandPath(t *testing.T) {
	var got bytes.Buffer
	renderCommandSection(&got, io.Discard, &command{
		name: "request",
		subs: []command{{name: "label-add"}},
	}, "phase")
	if !strings.Contains(got.String(), "koryph phase request <subcommand> -h") {
		t.Fatalf("nested grouping help lost its parent path:\n%s", got.String())
	}
}

// firstDiff returns a compact description of the first differing line between
// want and got. It is intentionally simple — just line numbers and content —
// since test output truncates long diffs anyway.
func firstDiff(want, got string) string {
	wLines := splitLines(want)
	gLines := splitLines(got)

	max := len(wLines)
	if len(gLines) < max {
		max = len(gLines)
	}
	for i := 0; i < max; i++ {
		if wLines[i] != gLines[i] {
			return fmt.Sprintf("line %d:\n  want: %q\n   got: %q", i+1, wLines[i], gLines[i])
		}
	}
	if len(wLines) != len(gLines) {
		return fmt.Sprintf("line count differs: want %d, got %d", len(wLines), len(gLines))
	}
	return "files are identical (this should not happen)"
}

// splitLines splits s into lines without a trailing empty element.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := bytes.Split([]byte(s), []byte("\n"))
	// bytes.Split("a\nb\n") → ["a","b",""] — drop trailing empty.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(l)
	}
	return out
}
