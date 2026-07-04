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
)

// CheckRulesets compares each desired ruleset in src against the live GitHub
// rulesets for repo.  Output is written to w in the same style as the
// former ensure-rulesets.sh: "OK <name>", "MISSING <name>", or "DRIFT <name>"
// with an indented diff.
//
// Returns (true, nil) when drift is detected and (false, nil) when everything
// matches.  An error return means a hard failure (API or filesystem).
func CheckRulesets(ctx context.Context, ghBin, repo string, src Source, w io.Writer) (bool, error) {
	return applyRulesets(ctx, ghBin, repo, src, w, false)
}

// ApplyRulesets creates or updates each desired ruleset in src on the live
// GitHub repository repo, printing CREATED / UPDATED / OK for each.
//
// It never deletes rulesets it does not know about (same rule as the former
// ensure-rulesets.sh).
func ApplyRulesets(ctx context.Context, ghBin, repo string, src Source, w io.Writer) error {
	_, err := applyRulesets(ctx, ghBin, repo, src, w, true)
	return err
}

// applyRulesets is the shared implementation for CheckRulesets and
// ApplyRulesets.  When apply is false it performs a dry-run check; when true
// it creates/updates rulesets on GitHub.
func applyRulesets(ctx context.Context, ghBin, repo string, src Source, w io.Writer, apply bool) (bool, error) {
	dir, err := src.RulesetsDir()
	if err != nil {
		return false, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("posture: read rulesets dir: %w", err)
	}

	// Fetch the live ruleset list once so we can do name → id lookups.
	liveList, err := fetchRulesetList(ctx, ghBin, repo)
	if err != nil {
		return false, err
	}

	// Build name → id map.
	nameToID := make(map[string]int64, len(liveList))
	for _, r := range liveList {
		nameToID[r.name] = r.id
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
			// Ruleset is missing on GitHub.
			if !apply {
				fmt.Fprintf(w, "MISSING  %s (no live ruleset)\n", name)
				drift = true
				continue
			}
			// apply: POST to create.
			endpoint := fmt.Sprintf("repos/%s/rulesets", repo)
			_, code, err := ghRun(ctx, ghBin, []string{"api", "-X", "POST", endpoint}, wantRaw)
			if err != nil {
				return false, fmt.Errorf("posture: create ruleset %s: %w", name, err)
			}
			if code != 0 {
				return false, fmt.Errorf("posture: create ruleset %s: gh exited %d", name, code)
			}
			fmt.Fprintf(w, "CREATED  %s\n", name)
			continue
		}

		// Ruleset exists — fetch full live state and compare.
		endpoint := fmt.Sprintf("repos/%s/rulesets/%d", repo, liveID)
		liveRaw, code, err := ghRun(ctx, ghBin, []string{"api", endpoint}, nil)
		if err != nil {
			return false, fmt.Errorf("posture: fetch ruleset %s: %w", name, err)
		}
		if code != 0 {
			return false, fmt.Errorf("posture: fetch ruleset %s: gh exited %d", name, code)
		}

		liveNorm, err := normalizeRuleset(liveRaw)
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
		// apply: PUT to update.
		putEndpoint := fmt.Sprintf("repos/%s/rulesets/%d", repo, liveID)
		_, code, err = ghRun(ctx, ghBin, []string{"api", "-X", "PUT", putEndpoint}, wantRaw)
		if err != nil {
			return false, fmt.Errorf("posture: update ruleset %s: %w", name, err)
		}
		if code != 0 {
			return false, fmt.Errorf("posture: update ruleset %s: gh exited %d", name, code)
		}
		fmt.Fprintf(w, "UPDATED  %s\n", name)
	}

	return drift, nil
}

// rulesetRef is the minimal fields needed from the list endpoint.
type rulesetRef struct {
	name string
	id   int64
}

// fetchRulesetList calls GET /repos/{owner}/{repo}/rulesets and returns the
// slice of (name, id) pairs.
func fetchRulesetList(ctx context.Context, ghBin, repo string) ([]rulesetRef, error) {
	endpoint := fmt.Sprintf("repos/%s/rulesets", repo)
	raw, code, err := ghRun(ctx, ghBin, []string{"api", endpoint}, nil)
	if err != nil {
		return nil, fmt.Errorf("posture: list rulesets: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("posture: list rulesets: gh exited %d", code)
	}

	var items []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("posture: parse ruleset list: %w", err)
	}

	out := make([]rulesetRef, len(items))
	for i, it := range items {
		out[i] = rulesetRef{name: it.Name, id: it.ID}
	}
	return out, nil
}
