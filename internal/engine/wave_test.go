// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/sched"
)

func TestSummarizeReasons(t *testing.T) {
	rs := []sched.Reason{
		{ID: "a", Reason: "footprint conflict with x"},
		{ID: "b", Reason: "wave full"},
		{ID: "c", Reason: "container bead"},
		{ID: "d", Reason: "no-dispatch label"},
	}
	got := summarizeReasons(rs, 3)
	for _, want := range []string{"a(footprint conflict with x)", "b(wave full)", "c(container bead)", "+1 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("summarizeReasons = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "d(") {
		t.Errorf("summarizeReasons should have elided the 4th entry: %q", got)
	}
	// Under the cap: no "+N more".
	if got := summarizeReasons(rs[:2], 3); strings.Contains(got, "more") {
		t.Errorf("no elision expected under the cap: %q", got)
	}
}
