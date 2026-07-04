// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

// org_rulesets.go — org-level ruleset check/apply (design §3.2).
//
// Org-level rulesets use the same GitHub Rulesets JSON schema as repo-level
// ones; only the API endpoints differ:
//
//	Repo:  /repos/{owner}/{repo}/rulesets
//	Org:   /orgs/{org}/rulesets
//
// Profiles may carry an org-rulesets/ subdirectory (alongside the existing
// rulesets/ and repo-settings.json).  `koryph posture check|apply <profile>
// --org ORG` targets org scope.
//
// The same normalization quirks apply as for repo rulesets (the API echoes
// volatile server-assigned fields; normalizeRuleset strips them before
// comparison).
//
// When the caller lacks org owner / admin access GitHub returns HTTP 403.
// applyOrgRulesets detects this and surfaces a typed [PermissionError] so
// callers can show a clean, actionable message instead of a raw exit code.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CheckOrgRulesets compares each desired ruleset in src against the live
// GitHub org-level rulesets for org.  Output is written to w in the same
// style as the repo-scoped variant: "OK <name>", "MISSING <name>", or
// "DRIFT <name>" with an indented diff.
//
// Returns (true, nil) when drift is detected and (false, nil) when everything
// matches.  An error return means a hard failure (API, filesystem, or
// permission).  A [*PermissionError] is returned when the caller lacks org
// owner / admin access.
func CheckOrgRulesets(ctx context.Context, ghBin, org string, src Source, w io.Writer) (bool, error) {
	return applyOrgRulesets(ctx, ghBin, org, src, w, false)
}

// ApplyOrgRulesets creates or updates each desired org-level ruleset in src on
// the live GitHub organisation org, printing CREATED / UPDATED / OK for each.
//
// It never deletes org rulesets it does not know about (same rule as the
// repo-scoped apply).  A [*PermissionError] is returned when the caller lacks
// org owner / admin access.
func ApplyOrgRulesets(ctx context.Context, ghBin, org string, src Source, w io.Writer) error {
	_, err := applyOrgRulesets(ctx, ghBin, org, src, w, true)
	return err
}

// applyOrgRulesets is the shared implementation for CheckOrgRulesets and
// ApplyOrgRulesets.
func applyOrgRulesets(ctx context.Context, ghBin, org string, src Source, w io.Writer, apply bool) (bool, error) {
	dir, err := src.OrgRulesetsDir()
	if err != nil {
		return false, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("posture: read org-rulesets dir: %w", err)
	}

	// Count JSON files before making any API calls — an empty dir is a no-op.
	hasFiles := false
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			hasFiles = true
			break
		}
	}
	if !hasFiles {
		return false, nil
	}

	// Fetch the live org ruleset list once for name → id lookups.
	liveList, err := fetchOrgRulesetList(ctx, ghBin, org)
	if err != nil {
		return false, err
	}

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
			return false, fmt.Errorf("posture: read org ruleset %s: %w", path, err)
		}

		// Extract the name field from the desired-state file.
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(wantRaw, &meta); err != nil {
			return false, fmt.Errorf("posture: parse org ruleset %s: %w", path, err)
		}
		name := meta.Name

		liveID, exists := nameToID[name]
		if !exists {
			// Ruleset is missing on GitHub.
			if !apply {
				fmt.Fprintf(w, "MISSING  %s (no live org ruleset)\n", name)
				drift = true
				continue
			}
			// apply: POST to create.
			endpoint := fmt.Sprintf("orgs/%s/rulesets", org)
			out, code, err := ghRun(ctx, ghBin, []string{"api", "-X", "POST", endpoint}, wantRaw)
			if err != nil {
				return false, fmt.Errorf("posture: create org ruleset %s: %w", name, err)
			}
			if code != 0 {
				if pe := parseOrgPermissionError(out, fmt.Sprintf("create org ruleset %q for %s", name, org)); pe != nil {
					return false, pe
				}
				return false, fmt.Errorf("posture: create org ruleset %s: gh exited %d", name, code)
			}
			fmt.Fprintf(w, "CREATED  %s\n", name)
			continue
		}

		// Ruleset exists — fetch full live state and compare.
		endpoint := fmt.Sprintf("orgs/%s/rulesets/%d", org, liveID)
		liveRaw, code, err := ghRun(ctx, ghBin, []string{"api", endpoint}, nil)
		if err != nil {
			return false, fmt.Errorf("posture: fetch org ruleset %s: %w", name, err)
		}
		if code != 0 {
			if pe := parseOrgPermissionError(liveRaw, fmt.Sprintf("read org ruleset %q for %s", name, org)); pe != nil {
				return false, pe
			}
			return false, fmt.Errorf("posture: fetch org ruleset %s: gh exited %d", name, code)
		}

		liveNorm, err := normalizeRuleset(liveRaw)
		if err != nil {
			return false, fmt.Errorf("posture: normalize live org ruleset %s: %w", name, err)
		}
		wantNorm, err := normalizeRuleset(wantRaw)
		if err != nil {
			return false, fmt.Errorf("posture: normalize want org ruleset %s: %w", name, err)
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
		putEndpoint := fmt.Sprintf("orgs/%s/rulesets/%d", org, liveID)
		out, code, err := ghRun(ctx, ghBin, []string{"api", "-X", "PUT", putEndpoint}, wantRaw)
		if err != nil {
			return false, fmt.Errorf("posture: update org ruleset %s: %w", name, err)
		}
		if code != 0 {
			if pe := parseOrgPermissionError(out, fmt.Sprintf("update org ruleset %q for %s", name, org)); pe != nil {
				return false, pe
			}
			return false, fmt.Errorf("posture: update org ruleset %s: gh exited %d", name, code)
		}
		fmt.Fprintf(w, "UPDATED  %s\n", name)
	}

	return drift, nil
}

// fetchOrgRulesetList calls GET /orgs/{org}/rulesets and returns the slice of
// (name, id) pairs.  Returns a [*PermissionError] when the caller lacks org
// owner / admin access.
func fetchOrgRulesetList(ctx context.Context, ghBin, org string) ([]rulesetRef, error) {
	endpoint := fmt.Sprintf("orgs/%s/rulesets", org)
	raw, code, err := ghRun(ctx, ghBin, []string{"api", endpoint}, nil)
	if err != nil {
		return nil, fmt.Errorf("posture: list org rulesets: %w", err)
	}
	if code != 0 {
		if pe := parseOrgPermissionError(raw, fmt.Sprintf("list org rulesets for %s", org)); pe != nil {
			return nil, pe
		}
		return nil, fmt.Errorf("posture: list org rulesets for %s: gh exited %d", org, code)
	}

	var items []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("posture: parse org ruleset list: %w", err)
	}

	out := make([]rulesetRef, len(items))
	for i, it := range items {
		out[i] = rulesetRef{name: it.Name, id: it.ID}
	}
	return out, nil
}

// parseOrgPermissionError inspects the raw response body from a failed gh api
// call and returns a *PermissionError if the body indicates a 403 caused by
// insufficient org permissions.  Returns nil for any other kind of failure.
func parseOrgPermissionError(body []byte, action string) *PermissionError {
	if len(body) == 0 {
		return nil
	}
	var resp struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	msg := strings.ToLower(resp.Message)
	permIndicators := []string{
		"must be an organization owner",
		"must be a member of the organization",
		"resource not accessible",
		"not authorized",
		"permission denied",
		"requires admin",
		"insufficient scopes",
	}
	for _, indicator := range permIndicators {
		if strings.Contains(msg, indicator) {
			return &PermissionError{
				Action: action,
				Needed: "org owner / admin access",
				Detail: resp.Message,
			}
		}
	}
	return nil
}
