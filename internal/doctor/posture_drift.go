// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

// posture_drift.go — checkPostureDrift + checkOrgPostureDrift +
// checkFragmentDrift: doctor checks that flag hygiene drift between a
// project's live GitHub repo / org / installed files and its declared posture
// profile / opted-in scanner fragments.
//
// checkPostureDrift:
//   - skipped (LevelOK) when koryph.project.json has no posture block.
//   - renders the named profile and runs the same drift detection as
//     `koryph posture check`, then reports LevelOK or LevelWarn.
//   - degrades gracefully (LevelOK + note) when gh is unavailable.
//
// checkOrgPostureDrift (design §3.2):
//   - skipped (LevelOK) when posture.org is empty.
//   - renders the named profile and checks org-level rulesets via
//     `koryph posture check --org`.
//   - degrades gracefully when gh is unavailable or lacks org admin access.
//
// checkFragmentDrift (design §3.3):
//   - skipped (LevelOK) when posture.fragments is empty.
//   - compares installed fragment files against embedded versions via SHA-256.
//   - reports missing or stale files as LevelWarn with a remediation command.
//   - no gh network calls required — fragment files live in the project tree.
//
// The injectable PostureDriftCheck and OrgPostureDriftCheck fields on
// ProjectOptions let tests bypass the real gh network calls.

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	ghpkg "github.com/koryph/koryph/internal/forge/github"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/posture"
	"github.com/koryph/koryph/internal/project"
)

const checkNamePostureDrift = "posture-drift"

// checkPostureDrift reports whether the live GitHub repository drifts from the
// posture profile declared in cfg.Posture.
//
// The check degrades gracefully (LevelOK with an explanatory note) when:
//   - cfg.Posture is nil (no profile declared — drift check is not applicable)
//   - the gh CLI is unavailable, unauthenticated, or lacks admin access
//
// When drift is found the finding message contains the exact
// `koryph posture apply <profile> [--param k=v]...` command to remediate.
func checkPostureDrift(opts ProjectOptions, repoRoot string, cfg *project.Config) Finding {
	if cfg == nil || cfg.Posture == nil {
		return Finding{
			Check:   checkNamePostureDrift,
			Level:   LevelOK,
			Message: "no posture profile declared (skipped)",
		}
	}

	// Use the injectable function when provided (test seam); otherwise perform
	// the real check via gh.
	if opts.PostureDriftCheck != nil {
		drift, err := opts.PostureDriftCheck(repoRoot, cfg.Posture)
		if err != nil {
			// Degrade gracefully — gh errors are not fatal for the doctor.
			return Finding{
				Check:   checkNamePostureDrift,
				Level:   LevelOK,
				Message: fmt.Sprintf("posture check skipped: %v", err),
			}
		}
		if !drift {
			return Finding{
				Check:   checkNamePostureDrift,
				Level:   LevelOK,
				Message: fmt.Sprintf("profile %q: no drift detected", cfg.Posture.Profile),
			}
		}
		return Finding{
			Check:   checkNamePostureDrift,
			Level:   LevelWarn,
			Message: fmt.Sprintf("posture drift from profile %q (run `%s`)", cfg.Posture.Profile, cfg.Posture.PostureApplyCmd()),
		}
	}

	// Real check: render the profile and call posture.CheckRulesets /
	// posture.CheckSettings using the gh CLI.
	home := paths.KoryphHome()
	ghBin := posture.GHBin()
	ctx := context.Background()
	ghProv := ghpkg.New()

	repoSlug, err := posture.DetectRepo(ctx, ghBin)
	if err != nil {
		// gh unavailable or unauthenticated — degrade gracefully.
		return Finding{
			Check:   checkNamePostureDrift,
			Level:   LevelOK,
			Message: "posture drift check skipped: cannot detect GitHub repo (gh unavailable or not authenticated)",
		}
	}

	profileSrc, cleanup, err := posture.RenderProfile(cfg.Posture.Profile, cfg.Posture.Parameters, home)
	if err != nil {
		return Finding{
			Check:   checkNamePostureDrift,
			Level:   LevelWarn,
			Message: fmt.Sprintf("posture profile %q not found or failed to render: %v", cfg.Posture.Profile, err),
		}
	}
	defer cleanup()

	var out bytes.Buffer
	drift := false

	// Eject: if the project has local .github/ IaC, those sections take precedence
	// (same logic as koryph posture check/apply).
	hasRulesets, hasSettings := posture.EjectCheck(repoRoot)
	localSrc := posture.LocalSource{Root: repoRoot}

	// --- rulesets ---
	var rulesetSrc posture.Source = profileSrc
	if hasRulesets {
		rulesetSrc = localSrc
	}
	if _, err := rulesetSrc.RulesetsDir(); err == nil {
		d, err := posture.CheckRulesets(ctx, repoSlug, rulesetSrc, &out, ghProv.Protection())
		if err != nil {
			// Degrade gracefully on gh errors.
			return Finding{
				Check:   checkNamePostureDrift,
				Level:   LevelOK,
				Message: "posture drift check skipped: ruleset check failed (likely no admin access)",
			}
		}
		if d {
			drift = true
		}
	}

	// --- settings ---
	var settingsSrc posture.Source = profileSrc
	if hasSettings {
		settingsSrc = localSrc
	}
	if _, err := settingsSrc.RepoSettingsFile(); err == nil {
		d, err := posture.CheckSettings(ctx, repoSlug, settingsSrc, &out, ghProv.Repo())
		if err != nil {
			return Finding{
				Check:   checkNamePostureDrift,
				Level:   LevelOK,
				Message: "posture drift check skipped: settings check failed (likely no admin access)",
			}
		}
		if d {
			drift = true
		}
	}

	if drift {
		return Finding{
			Check:   checkNamePostureDrift,
			Level:   LevelWarn,
			Message: fmt.Sprintf("posture drift from profile %q (run `%s`)", cfg.Posture.Profile, cfg.Posture.PostureApplyCmd()),
		}
	}
	return Finding{
		Check:   checkNamePostureDrift,
		Level:   LevelOK,
		Message: fmt.Sprintf("profile %q: no drift detected", cfg.Posture.Profile),
	}
}

// checkNameOrgPostureDrift is the check name for org-level posture drift.
const checkNameOrgPostureDrift = "org-posture-drift"

// checkOrgPostureDrift reports whether the live GitHub org-level rulesets
// drift from the posture profile declared in cfg.Posture.
//
// The check is skipped (LevelOK) when:
//   - cfg.Posture is nil or cfg.Posture.Org is empty
//   - gh is unavailable or unauthenticated
//   - the caller lacks org owner / admin access (degrades gracefully)
//
// When drift is found the finding message contains the exact
// `koryph posture apply <profile> --org ORG [--param k=v]...` command.
func checkOrgPostureDrift(opts ProjectOptions, repoRoot string, cfg *project.Config) Finding {
	if cfg == nil || cfg.Posture == nil || cfg.Posture.Org == "" {
		return Finding{
			Check:   checkNameOrgPostureDrift,
			Level:   LevelOK,
			Message: "no org declared in posture block (skipped)",
		}
	}

	orgName := cfg.Posture.Org

	// Use injectable function when provided (test seam).
	if opts.OrgPostureDriftCheck != nil {
		drift, err := opts.OrgPostureDriftCheck(repoRoot, cfg.Posture)
		if err != nil {
			return Finding{
				Check:   checkNameOrgPostureDrift,
				Level:   LevelOK,
				Message: fmt.Sprintf("org posture check skipped: %v", err),
			}
		}
		if !drift {
			return Finding{
				Check:   checkNameOrgPostureDrift,
				Level:   LevelOK,
				Message: fmt.Sprintf("org %q profile %q: no drift detected", orgName, cfg.Posture.Profile),
			}
		}
		return Finding{
			Check:   checkNameOrgPostureDrift,
			Level:   LevelWarn,
			Message: fmt.Sprintf("org posture drift for %q from profile %q (run `%s`)", orgName, cfg.Posture.Profile, cfg.Posture.OrgPostureApplyCmd()),
		}
	}

	// Real check via gh CLI.
	home := paths.KoryphHome()
	ghProv := ghpkg.New()
	ctx := context.Background()

	profileSrc, cleanup, err := posture.RenderProfile(cfg.Posture.Profile, cfg.Posture.Parameters, home)
	if err != nil {
		return Finding{
			Check:   checkNameOrgPostureDrift,
			Level:   LevelWarn,
			Message: fmt.Sprintf("org posture profile %q not found or failed to render: %v", cfg.Posture.Profile, err),
		}
	}
	defer cleanup()

	// Skip if the profile has no org-rulesets directory.
	if _, err := profileSrc.OrgRulesetsDir(); err != nil {
		return Finding{
			Check:   checkNameOrgPostureDrift,
			Level:   LevelOK,
			Message: fmt.Sprintf("profile %q has no org-rulesets/ (skipped)", cfg.Posture.Profile),
		}
	}

	var out bytes.Buffer
	d, err := posture.CheckOrgRulesets(ctx, orgName, profileSrc, &out, ghProv.Protection())
	if err != nil {
		// Degrade gracefully — permission errors and gh errors are not fatal.
		return Finding{
			Check:   checkNameOrgPostureDrift,
			Level:   LevelOK,
			Message: fmt.Sprintf("org posture drift check skipped: %v", err),
		}
	}

	if d {
		return Finding{
			Check:   checkNameOrgPostureDrift,
			Level:   LevelWarn,
			Message: fmt.Sprintf("org posture drift for %q from profile %q (run `%s`)", orgName, cfg.Posture.Profile, cfg.Posture.OrgPostureApplyCmd()),
		}
	}
	return Finding{
		Check:   checkNameOrgPostureDrift,
		Level:   LevelOK,
		Message: fmt.Sprintf("org %q profile %q: no drift detected", orgName, cfg.Posture.Profile),
	}
}

// checkNameFragmentDrift is the check name used for fragment-drift findings.
const checkNameFragmentDrift = "fragment-drift"

// checkFragmentDrift checks that every security-scanner fragment declared in
// cfg.Posture.Fragments is present and up-to-date in repoRoot.
//
// Unlike checkPostureDrift this check requires no gh network calls — fragment
// files live entirely in the project working tree.
//
// Returns one Finding per opted-in fragment:
//   - LevelOK   — all files for the fragment are installed and current
//   - LevelWarn — one or more files are missing or stale
//
// The check is skipped (returns nil) when no fragments are opted in.
func checkFragmentDrift(opts ProjectOptions, repoRoot string, cfg *project.Config) []Finding {
	if cfg == nil || cfg.Posture == nil || len(cfg.Posture.Fragments) == 0 {
		return nil // no fragments declared — check not applicable
	}

	// Use injectable check when provided (test seam).
	if opts.FragmentDriftCheck != nil {
		drifts, err := opts.FragmentDriftCheck(repoRoot, cfg.Posture.Fragments)
		if err != nil {
			return []Finding{{
				Check:   checkNameFragmentDrift,
				Level:   LevelOK,
				Message: fmt.Sprintf("fragment drift check skipped: %v", err),
			}}
		}
		return fragmentDriftFindings(cfg, drifts)
	}

	// Real check: delegate to posture.CheckFragmentDrift.
	result, err := posture.CheckFragmentDrift(repoRoot, cfg.Posture.Fragments)
	if err != nil {
		return []Finding{{
			Check:   checkNameFragmentDrift,
			Level:   LevelOK,
			Message: fmt.Sprintf("fragment drift check skipped: %v", err),
		}}
	}
	return fragmentDriftFindings(cfg, result.Drifts)
}

// fragmentDriftFindings converts []posture.FragmentDrift into doctor Findings.
func fragmentDriftFindings(cfg *project.Config, drifts []posture.FragmentDrift) []Finding {
	if len(drifts) == 0 {
		return []Finding{{
			Check:   checkNameFragmentDrift,
			Level:   LevelOK,
			Message: "all fragments up to date",
		}}
	}
	applyCmd := cfg.Posture.PostureApplyCmd()
	var findings []Finding
	for _, d := range drifts {
		if !d.HasDrift {
			findings = append(findings, Finding{
				Check:   checkNameFragmentDrift,
				Level:   LevelOK,
				Message: fmt.Sprintf("fragment %q: all files installed", d.Fragment),
			})
			continue
		}
		// Enumerate the missing/stale files for a precise message.
		var missingStale []string
		for _, f := range d.Files {
			if f.Status != "ok" {
				missingStale = append(missingStale, fmt.Sprintf("%s(%s)", f.Path, f.Status))
			}
		}
		msg := fmt.Sprintf("fragment %q: drift in %s (run `%s --force` to reinstall)",
			d.Fragment, strings.Join(missingStale, ", "), applyCmd)
		findings = append(findings, Finding{
			Check:   checkNameFragmentDrift,
			Level:   LevelWarn,
			Message: msg,
		})
	}
	return findings
}
