// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/quota"
)

// TestMetricsEstimatorDisplaysProxySegmentedKeys is the koryph-3l1.3 carried-
// contract regression test for cmdMetricsEstimator: a Calibration/ErrorStats
// key segmented by a proxyID built from a base_url with its own colons (e.g.
// "http://127.0.0.1:8787") must render with the CORRECT tier:size and a
// correct (non-inflated/deflated) BASE estimate — the prior last-colon parse
// would have split "sonnet:L@http://127.0.0.1:8787" into tier="sonnet",
// size="L@http://127.0.0.1" (splitting on the URL's OWN last colon before
// the port), corrupting both the displayed key and the SizeMultiplier
// lookup feeding BASE.
func TestMetricsEstimatorDisplaysProxySegmentedKeys(t *testing.T) {
	isolate(t)
	addProject(t, "proj1") // registers account "personal"

	cfg := quota.DefaultConfig("personal")
	cfg.SizeMultiplier = map[string]float64{"S": 0.5, "M": 1.0, "L": 2.0}
	cfg.ErrorStats = map[string]*quota.ErrorStat{
		"sonnet:L":                       {N: 5, Bias: 1.1, MAPE: 12},
		"sonnet:L@http://127.0.0.1:8787": {N: 3, Bias: 0.9, MAPE: 8},
	}
	if err := quota.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	code, out, errb := runCmd("metrics", "estimator")
	if code != 0 {
		t.Fatalf("metrics estimator: code=%d stderr=%s", code, errb)
	}
	t.Logf("output:\n%s", out)

	if !strings.Contains(out, "sonnet:L") {
		t.Errorf("output missing direct sonnet:L row:\n%s", out)
	}
	if !strings.Contains(out, "http://127.0.0.1:8787") {
		t.Errorf("output missing proxied row's proxy identity:\n%s", out)
	}
	// The direct row's tier:size column must be the clean "sonnet:L", not a
	// corrupted parse that swallowed part of a URL from the OTHER row's key.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "personal") && !strings.Contains(line, "@") {
			if !strings.Contains(line, "sonnet:L") {
				t.Errorf("direct row line malformed: %q", line)
			}
		}
	}

	// Both rows' BASE must reflect SizeMultiplier["L"]=2.0 — a corrupted size
	// string ("L@http://...") would miss the map lookup and silently default
	// to a multiplier of 1.0, producing a different (wrong) BASE than the
	// clean "L" row's.
	code, jsonOut, errb := runCmd("metrics", "estimator", "--json")
	if code != 0 {
		t.Fatalf("metrics estimator --json: code=%d stderr=%s", code, errb)
	}
	var rows []struct {
		Tier    string
		Size    string
		ProxyID string
		BaseUSD float64
	}
	if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil {
		t.Fatalf("unmarshal --json output: %v\n%s", err, jsonOut)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2:\n%s", len(rows), jsonOut)
	}
	var directBase, proxiedBase float64
	var sawDirect, sawProxied bool
	for _, r := range rows {
		if r.Tier != "sonnet" || r.Size != "L" {
			t.Errorf("row Tier/Size = %q/%q, want sonnet/L (proxyID=%q)", r.Tier, r.Size, r.ProxyID)
		}
		if r.ProxyID == "" {
			directBase, sawDirect = r.BaseUSD, true
		} else {
			proxiedBase, sawProxied = r.BaseUSD, true
		}
	}
	if !sawDirect || !sawProxied {
		t.Fatalf("did not see both direct and proxied rows: %+v", rows)
	}
	if directBase != proxiedBase {
		t.Errorf("direct BASE = %v, proxied BASE = %v, want equal (same tier/size, size multiplier must not be lost)",
			directBase, proxiedBase)
	}
}
