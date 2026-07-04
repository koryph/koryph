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

	"github.com/koryph/koryph/internal/forge"
)

// CheckOrgRulesets compares each desired ruleset in src against the live
// org-level rulesets for org as returned by prot.  Output is written to w in
// the same style as the repo-scoped variant: "OK <name>", "MISSING <name>",
// or "DRIFT <name>" with an indented diff.
//
// Returns (true, nil) when drift is detected and (false, nil) when everything
// matches.  An error return means a hard failure (API, filesystem, or
// permission).  A [*PermissionError] is returned when the forge reports an
// access error on the org.
func CheckOrgRulesets(ctx context.Context, org string, src Source, w io.Writer, prot forge.ProtectionService) (bool, error) {
	return applyOrgRulesets(ctx, org, src, w, prot, false)
}

// ApplyOrgRulesets creates or updates each desired org-level ruleset in src on
// the live organisation org via prot, printing CREATED / UPDATED / OK for each.
//
// It never deletes org rulesets it does not know about (same rule as the
// repo-scoped apply).  A [*PermissionError] is returned when the forge reports
// an access error on the org.
func ApplyOrgRulesets(ctx context.Context, org string, src Source, w io.Writer, prot forge.ProtectionService) error {
	_, err := applyOrgRulesets(ctx, org, src, w, prot, true)
	return err
}

// applyOrgRulesets is the shared implementation for CheckOrgRulesets and
// ApplyOrgRulesets.  It delegates all API calls to prot, which routes them to
// the org-level endpoints (no "/" in org means org scope in forge/github).
func applyOrgRulesets(ctx context.Context, org string, src Source, w io.Writer, prot forge.ProtectionService, apply bool) (bool, error) {
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
	liveList, err := prot.List(ctx, org)
	if err != nil {
		// Wrap forge permission errors as posture PermissionError.
		if pe := asPermissionError(err, fmt.Sprintf("list org rulesets for %s", org)); pe != nil {
			return false, pe
		}
		return false, fmt.Errorf("posture: list org rulesets for %s: %w", org, err)
	}

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
			// Ruleset is missing on the forge.
			if !apply {
				fmt.Fprintf(w, "MISSING  %s (no live org ruleset)\n", name)
				drift = true
				continue
			}
			// apply: create via the forge service.
			if _, err := prot.Create(ctx, org, &forge.Ruleset{Name: name, Raw: wantRaw}); err != nil {
				if pe := asPermissionError(err, fmt.Sprintf("create org ruleset %q for %s", name, org)); pe != nil {
					return false, pe
				}
				return false, fmt.Errorf("posture: create org ruleset %s: %w", name, err)
			}
			fmt.Fprintf(w, "CREATED  %s\n", name)
			continue
		}

		// Ruleset exists — fetch full live state and compare.
		liveRS, err := prot.Get(ctx, org, liveID)
		if err != nil {
			if pe := asPermissionError(err, fmt.Sprintf("read org ruleset %q for %s", name, org)); pe != nil {
				return false, pe
			}
			return false, fmt.Errorf("posture: fetch org ruleset %s: %w", name, err)
		}

		liveNorm, err := normalizeRuleset(liveRS.Raw)
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
		// apply: update via the forge service.
		if err := prot.Update(ctx, org, &forge.Ruleset{ID: liveID, Name: name, Raw: wantRaw}); err != nil {
			if pe := asPermissionError(err, fmt.Sprintf("update org ruleset %q for %s", name, org)); pe != nil {
				return false, pe
			}
			return false, fmt.Errorf("posture: update org ruleset %s: %w", name, err)
		}
		fmt.Fprintf(w, "UPDATED  %s\n", name)
	}

	return drift, nil
}

// asPermissionError inspects err and returns a *PermissionError when err
// contains a permission-denial message pattern, otherwise nil.
// Used to preserve the typed-error surface after extracting gh calls to forge.
func asPermissionError(err error, action string) *PermissionError {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	permIndicators := []string{
		"must be an organization owner",
		"must be a member of the organization",
		"resource not accessible",
		"not authorized",
		"permission denied",
		"requires admin",
		"insufficient scopes",
	}
	for _, ind := range permIndicators {
		if strings.Contains(msg, ind) {
			return &PermissionError{
				Action: action,
				Needed: "org owner / admin access",
				Detail: err.Error(),
			}
		}
	}
	return nil
}
