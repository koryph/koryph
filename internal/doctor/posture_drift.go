// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

// posture_drift.go — checkPostureDrift: doctor check that flags hygiene drift
// between a project's live GitHub repo and its declared posture profile.
//
// The check is skipped (LevelOK) when koryph.project.json has no posture block.
// When a posture block is present, the check renders the named profile and runs
// the same drift detection used by `koryph posture check`, then reports:
//
//   - LevelOK   — live repo matches the profile
//   - LevelWarn — drift detected; message includes the exact apply command
//   - LevelOK   — gh is unavailable / no admin access (graceful degrade)
//
// The injectable PostureDriftCheck field on ProjectOptions lets tests bypass the
// real gh network calls.

import (
	"bytes"
	"context"
	"fmt"

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
		d, err := posture.CheckRulesets(ctx, ghBin, repoSlug, rulesetSrc, &out)
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
		d, err := posture.CheckSettings(ctx, ghBin, repoSlug, settingsSrc, &out)
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
