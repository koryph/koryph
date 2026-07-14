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
)

// TestReviewTimeoutHardCapMirrorsReviewPackage is the drift guard between the
// two layers that both encode the 20-minute per-task ceiling: project.Validate
// rejects an over-cap config against project.ReviewTimeoutHardCapSec, while
// review.Review clamps the actual spawn against review.MaxTimeoutSec. If those
// diverged, a config could validate yet be silently re-clamped at runtime (or
// vice versa). The engine imports both packages, so it is the natural place to
// assert they stay equal.
func TestReviewTimeoutHardCapMirrorsReviewPackage(t *testing.T) {
	if project.ReviewTimeoutHardCapSec != review.MaxTimeoutSec {
		t.Errorf("hard-cap drift: project.ReviewTimeoutHardCapSec=%d, review.MaxTimeoutSec=%d",
			project.ReviewTimeoutHardCapSec, review.MaxTimeoutSec)
	}
}

// captureReviewer records the review.Opts it was handed and returns v, so a test
// can assert the engine threaded the resolved timeouts into the reviewer.
func captureReviewer(v review.Verdict, got *review.Opts) PRReviewer {
	return func(_ context.Context, o review.Opts) review.Verdict {
		*got = o
		return v
	}
}

// TestReviewPRThreadsProjectTimeouts verifies the review-pr call site resolves
// the per-project review block (EffectiveReview) and threads the starting
// timeout and escalation ceiling into review.Opts — both when the block is set
// and when it is absent (defaults).
func TestReviewPRThreadsProjectTimeouts(t *testing.T) {
	clean := review.Verdict{Blocking: false}

	cases := []struct {
		name             string
		cfg              *project.Config
		wantTimeout, max int
	}{
		{
			name:        "absent block resolves to defaults",
			cfg:         &project.Config{},
			wantTimeout: project.DefaultReviewTimeoutSec,
			max:         project.ReviewTimeoutHardCapSec,
		},
		{
			name:        "configured block threads through",
			cfg:         &project.Config{Review: &project.ReviewConfig{TimeoutSeconds: 300, MaxTimeoutSeconds: 900}},
			wantTimeout: 300,
			max:         900,
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
			if got.MaxTimeoutSec != tc.max {
				t.Errorf("MaxTimeoutSec = %d, want %d", got.MaxTimeoutSec, tc.max)
			}
		})
	}
}
