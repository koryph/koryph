// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"testing"

	"github.com/koryph/koryph/internal/registry"
)

// TestResolveBatchCachePrefix is the koryph-6au acceptance test for the batch
// shared-prefix breakpoint resolution: an explicit --cache-prefix always wins,
// --project defaults the breakpoint from the project's prompt_cache_policy
// (the live consumer of the re-introduced registry field), and an unknown
// project surfaces the load error rather than silently defaulting.
func TestResolveBatchCachePrefix(t *testing.T) {
	isolate(t)
	ctx := context.Background()

	// A project whose policy is the default "on".
	onRec := addProject(t, "on-proj")
	if !onRec.PromptCacheEnabled() {
		t.Fatalf("addProject seed prompt_cache_policy = %q, want on", onRec.PromptCachePolicy)
	}

	// A project explicitly opted out.
	offRec := addProject(t, "off-proj")
	offRec.PromptCachePolicy = registry.PromptCacheOff
	if err := registry.NewStore().Save(ctx, offRec); err != nil {
		t.Fatalf("save off-proj: %v", err)
	}

	cases := []struct {
		name           string
		explicitCache  bool
		explicitPassed bool
		project        string
		want           bool
		wantErr        bool
	}{
		{"no project, no flag -> flag default off", false, false, "", false, false},
		{"no project, explicit on", true, true, "", true, false},
		{"project on, no flag -> policy on", false, false, "on-proj", true, false},
		{"project off, no flag -> policy off", false, false, "off-proj", false, false},
		{"explicit off overrides project on", false, true, "on-proj", false, false},
		{"explicit on overrides project off", true, true, "off-proj", true, false},
		{"unknown project errors", false, false, "nope", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBatchCachePrefix(tc.explicitCache, tc.explicitPassed, tc.project)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveBatchCachePrefix(%v,%v,%q) err = nil, want error",
						tc.explicitCache, tc.explicitPassed, tc.project)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBatchCachePrefix(%v,%v,%q) err = %v",
					tc.explicitCache, tc.explicitPassed, tc.project, err)
			}
			if got != tc.want {
				t.Errorf("resolveBatchCachePrefix(%v,%v,%q) = %v, want %v",
					tc.explicitCache, tc.explicitPassed, tc.project, got, tc.want)
			}
		})
	}
}
