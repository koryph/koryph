// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/koryph/koryph/internal/forge"
)

// CheckRulesets compares each desired ruleset in src against the live
// rulesets for repo as returned by prot.  Output is written to w in the same
// style as the former ensure-rulesets.sh: "OK <name>", "MISSING <name>", or
// "DRIFT <name>" with an indented diff.
//
// Returns (true, nil) when drift is detected and (false, nil) when everything
// matches.  An error return means a hard failure (API or filesystem).
func CheckRulesets(ctx context.Context, repo string, src Source, w io.Writer, prot forge.ProtectionService) (bool, error) {
	return applyRulesets(ctx, repo, src, w, prot, false)
}

// ApplyRulesets creates or updates each desired ruleset in src on the live
// repository repo via prot, printing CREATED / UPDATED / OK for each.
//
// It never deletes rulesets it does not know about (same rule as the former
// ensure-rulesets.sh).
func ApplyRulesets(ctx context.Context, repo string, src Source, w io.Writer, prot forge.ProtectionService) error {
	_, err := applyRulesets(ctx, repo, src, w, prot, true)
	return err
}

// applyRulesets is the shared implementation for CheckRulesets and
// ApplyRulesets.  When apply is false it performs a dry-run check; when true
// it creates/updates rulesets via the forge ProtectionService.
func applyRulesets(ctx context.Context, repo string, src Source, w io.Writer, prot forge.ProtectionService, apply bool) (bool, error) {
	dir, err := src.RulesetsDir()
	if err != nil {
		return false, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("posture: read rulesets dir: %w", err)
	}

	// Fetch the live ruleset list once so we can do name → id lookups.
	liveList, err := prot.List(ctx, repo)
	if err != nil {
		return false, fmt.Errorf("posture: list rulesets: %w", err)
	}

	// Build name → id map.
	nameToID := make(map[string]string, len(liveList))
	for _, r := range liveList {
		nameToID[r.Name] = r.ID
	}

	drift := false
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		wantRaw, err := os.ReadFile(path)
		if err != nil {
			return false, fmt.Errorf("posture: read ruleset %s: %w", path, err)
		}

		// Extract the name field from the desired-state file.
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(wantRaw, &meta); err != nil {
			return false, fmt.Errorf("posture: parse ruleset %s: %w", path, err)
		}
		name := meta.Name

		liveID, exists := nameToID[name]
		if !exists {
			// Ruleset is missing on the forge.
			if !apply {
				fmt.Fprintf(w, "MISSING  %s (no live ruleset)\n", name)
				drift = true
				continue
			}
			// apply: create via the forge service.
			if _, err := prot.Create(ctx, repo, &forge.Ruleset{Name: name, Raw: wantRaw}); err != nil {
				return false, fmt.Errorf("posture: create ruleset %s: %w", name, err)
			}
			fmt.Fprintf(w, "CREATED  %s\n", name)
			continue
		}

		// Ruleset exists — fetch full live state and compare.
		liveRS, err := prot.Get(ctx, repo, liveID)
		if err != nil {
			return false, fmt.Errorf("posture: fetch ruleset %s: %w", name, err)
		}

		liveNorm, err := normalizeRuleset(liveRS.Raw)
		if err != nil {
			return false, fmt.Errorf("posture: normalize live ruleset %s: %w", name, err)
		}
		wantNorm, err := normalizeRuleset(wantRaw)
		if err != nil {
			return false, fmt.Errorf("posture: normalize want ruleset %s: %w", name, err)
		}

		if string(liveNorm) == string(wantNorm) {
			fmt.Fprintf(w, "OK       %s\n", name)
			continue
		}

		if !apply {
			fmt.Fprintf(w, "DRIFT    %s (live differs from %s):\n", name, path)
			printDiff(w, liveNorm, wantNorm)
			drift = true
			continue
		}
		// apply: update via the forge service.
		if err := prot.Update(ctx, repo, &forge.Ruleset{ID: liveID, Name: name, Raw: wantRaw}); err != nil {
			return false, fmt.Errorf("posture: update ruleset %s: %w", name, err)
		}
		fmt.Fprintf(w, "UPDATED  %s\n", name)
	}

	return drift, nil
}
