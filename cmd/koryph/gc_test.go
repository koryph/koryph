// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"strings"
	"testing"

	koryphgc "github.com/koryph/koryph/internal/gc"
)

func TestGCWarningsAreVisibleWithoutChangingExitStatus(t *testing.T) {
	result := &koryphgc.Result{
		At: "2026-07-25T00:00:00Z",
		Classes: []koryphgc.ClassResult{{
			Class:    "project-budget",
			Warnings: []string{"protected artifacts keep project above hard budget"},
		}},
	}
	var output bytes.Buffer
	printGCTable(&output, result)
	if got := gcExitCode(result); got != 0 {
		t.Fatalf("gcExitCode = %d, want 0 for a safety warning", got)
	}
	for _, want := range []string{"WARNINGS", "project-budget", "protected artifacts keep project above hard budget"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output %q does not contain %q", output.String(), want)
		}
	}
}

func TestGCErrorsRemainNonZero(t *testing.T) {
	result := &koryphgc.Result{
		Classes: []koryphgc.ClassResult{{
			Class:  "project-budget",
			Errors: []string{"cache lock failed"},
		}},
	}
	if got := gcExitCode(result); got != 1 {
		t.Fatalf("gcExitCode = %d, want 1", got)
	}
}
