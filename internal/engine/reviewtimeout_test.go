// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
	"github.com/koryph/koryph/internal/timeoutcfg"
)

// TestUnifiedReviewTimeoutDefaultDrift is the drift guard for the single unified
// timeout (koryph-w82i, replacing the former hard-cap mirror): the project-side
// built-in default, the review package's runtime default, and the timeout
// hierarchy's built-in must all be the same value. If they diverged, a review
// spawned with an unset config would use a different default than the docs and
// config layer advertise.
func TestUnifiedReviewTimeoutDefaultDrift(t *testing.T) {
	if project.DefaultReviewTimeoutSec != review.DefaultTimeoutSec {
		t.Errorf("default drift: project.DefaultReviewTimeoutSec=%d, review.DefaultTimeoutSec=%d",
			project.DefaultReviewTimeoutSec, review.DefaultTimeoutSec)
	}
	if review.DefaultTimeoutSec != timeoutcfg.BuiltinDefaultSec {
		t.Errorf("default drift: review.DefaultTimeoutSec=%d, timeoutcfg.BuiltinDefaultSec=%d",
			review.DefaultTimeoutSec, timeoutcfg.BuiltinDefaultSec)
	}
}

// captureReviewer records the review.Opts it was handed and returns v, so a test
// can assert the engine threaded the resolved timeout into the reviewer.
func captureReviewer(v review.Verdict, got *review.Opts) PRReviewer {
	return func(_ context.Context, o review.Opts) review.Verdict {
		*got = o
		return v
	}
}

// TestReviewPRThreadsProjectTimeout verifies the review-pr call site resolves
// the per-project review block through the timeout hierarchy and threads the
// single unified timeout into review.Opts — the built-in default when the block
// is absent, and the configured value when set. KORYPH_HOME is isolated so the
// absent-block case sees no machine-wide system default.
func TestReviewPRThreadsProjectTimeout(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir()) // no ~/.koryph/config.json → system tier unset
	clean := review.Verdict{Blocking: false}

	cases := []struct {
		name        string
		cfg         *project.Config
		wantTimeout int
	}{
		{
			name:        "absent block resolves to the built-in default",
			cfg:         &project.Config{},
			wantTimeout: timeoutcfg.BuiltinDefaultSec,
		},
		{
			name:        "configured block threads through",
			cfg:         &project.Config{Review: &project.ReviewConfig{TimeoutSeconds: 300}},
			wantTimeout: 300,
		},
		{
			name:        "configured value may exceed the built-in default",
			cfg:         &project.Config{Review: &project.ReviewConfig{TimeoutSeconds: 5000}},
			wantTimeout: 5000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakePRHost{meta: PRMeta{Number: 5, Author: "alice", URL: "https://x/pull/5"}}
			rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main", AccountProfile: "work"}
			var got review.Opts
			var out bytes.Buffer
			if _, err := ReviewPR(context.Background(), rec, tc.cfg, host, captureReviewer(clean, &got),
				ReviewPROpts{Selector: "5", Out: &out}); err != nil {
				t.Fatalf("ReviewPR: %v", err)
			}
			if got.TimeoutSec != tc.wantTimeout {
				t.Errorf("TimeoutSec = %d, want %d", got.TimeoutSec, tc.wantTimeout)
			}
		})
	}
}
