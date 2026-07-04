// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/posture"
)

// cmdPosture dispatches the posture sub-verbs.
func cmdPosture(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "posture", "apply a named desired-state profile to a GitHub repo", []subVerb{
			{"list", "list built-in and user-defined profiles"},
			{"check <profile> [--repo owner/name] [--param k=v]...", "diff live GitHub state against profile (exit 1 on drift)"},
			{"diff <profile> [--repo owner/name] [--param k=v]...", "show drift between live state and profile (always exit 0)"},
			{"apply <profile> [--repo owner/name] [--param k=v]...", "show diff then apply profile to live GitHub repo"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdPostureList(rest, stdout, stderr)
	case "check":
		return cmdPostureCheck(rest, stdout, stderr)
	case "diff":
		return cmdPostureDiff(rest, stdout, stderr)
	case "apply":
		return cmdPostureApply(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown posture subcommand %q", sub))
	}
}

// cmdPostureList lists built-in and user profiles.
func cmdPostureList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("posture list", stderr)
	setUsage(fs, stdout, "list built-in and user-defined posture profiles", "")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	home := paths.KoryphHome()

	builtins, err := posture.ListBuiltins()
	if err != nil {
		return fail(stderr, err)
	}
	user, err := posture.ListUserProfiles(home)
	if err != nil {
		return fail(stderr, err)
	}

	all := append(builtins, user...)
	if len(all) == 0 {
		fmt.Fprintln(stdout, "No profiles found.")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSOURCE\tDESCRIPTION")
	for _, p := range all {
		desc := p.Manifest.Description
		if len(desc) > 70 {
			desc = desc[:67] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Source, desc)
	}
	tw.Flush()
	return 0
}

// cmdPostureCheck implements `koryph posture check <profile> [--repo owner/name] [--param k=v]...`
// Exits 1 on drift, 0 when everything matches, 2 on usage error.
func cmdPostureCheck(args []string, stdout, stderr io.Writer) int {
	return runPostureVerb(args, "posture check", stdout, stderr, false, false)
}

// cmdPostureDiff implements `koryph posture diff <profile> [--repo owner/name] [--param k=v]...`
// Always exits 0 (drift is informational, not a failure).
func cmdPostureDiff(args []string, stdout, stderr io.Writer) int {
	return runPostureVerb(args, "posture diff", stdout, stderr, false, true)
}

// cmdPostureApply implements `koryph posture apply <profile> [--repo owner/name] [--param k=v]...`
// Prints the diff first, then applies.
func cmdPostureApply(args []string, stdout, stderr io.Writer) int {
	return runPostureVerb(args, "posture apply", stdout, stderr, true, false)
}

// runPostureVerb is the shared implementation for check/diff/apply.
//
//   - apply=true: print diff, then apply changes.
//   - alwaysExit0=true: never return exit-1 on drift (diff mode).
func runPostureVerb(args []string, cmdName string, stdout, stderr io.Writer, apply, alwaysExit0 bool) int {
	fs := newFlagSet(cmdName, stderr)
	repo := fs.String("repo", "", "repository in owner/name form (default: detected from git remote via gh)")
	var rawParams multiFlag
	fs.Var(&rawParams, "param", "profile parameter as key=value (repeatable, e.g. --param required_checks=\"pre-commit,make gate\")")
	setUsage(fs, stdout,
		cmdName+" — compare or apply a named posture profile",
		"<profile> [--repo owner/name] [--param k=v]...")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) < 1 {
		return usageErr(stderr, cmdName+": <profile> is required")
	}
	profileName := pos[0]

	params, err := parseParams(rawParams)
	if err != nil {
		return usageErr(stderr, cmdName+": "+err.Error())
	}

	ctx := context.Background()
	ghBin := posture.GHBin()

	repoSlug, err := resolveRepo(ctx, ghBin, *repo)
	if err != nil {
		return fail(stderr, err)
	}

	home := paths.KoryphHome()

	// Render the profile to a temp directory.
	profileSrc, cleanup, err := posture.RenderProfile(profileName, params, home)
	if err != nil {
		return fail(stderr, err)
	}
	defer cleanup()

	// Ejectability: if the CWD has local .github files, those sections win.
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	hasRulesets, hasSettings := posture.EjectCheck(cwd)
	localSrc := posture.LocalSource{Root: cwd}

	// Print ejection notices once per section.
	if hasRulesets {
		fmt.Fprintf(stdout, "INFO     rulesets: repo has .github/rulesets/ — using local IaC (profile rulesets ignored)\n")
	}
	if hasSettings {
		fmt.Fprintf(stdout, "INFO     settings: repo has .github/repo-settings.json — using local IaC (profile settings ignored)\n")
	}

	drift := false

	// ---- rulesets -------------------------------------------------------
	var rulesetSrc posture.Source = profileSrc
	if hasRulesets {
		rulesetSrc = localSrc
	}
	if _, err2 := rulesetSrc.RulesetsDir(); err2 == nil {
		if apply {
			// Diff first.
			fmt.Fprintln(stdout, "--- rulesets diff ---")
			d, err2 := posture.CheckRulesets(ctx, ghBin, repoSlug, rulesetSrc, stdout)
			if err2 != nil {
				return fail(stderr, err2)
			}
			if d {
				drift = true
				fmt.Fprintln(stdout, "--- applying rulesets ---")
				if err2 := posture.ApplyRulesets(ctx, ghBin, repoSlug, rulesetSrc, stdout); err2 != nil {
					return fail(stderr, err2)
				}
			}
		} else {
			d, err2 := posture.CheckRulesets(ctx, ghBin, repoSlug, rulesetSrc, stdout)
			if err2 != nil {
				return fail(stderr, err2)
			}
			if d {
				drift = true
			}
		}
	}

	// ---- repo settings --------------------------------------------------
	var settingsSrc posture.Source = profileSrc
	if hasSettings {
		settingsSrc = localSrc
	}
	if _, err2 := settingsSrc.RepoSettingsFile(); err2 == nil {
		if apply {
			fmt.Fprintln(stdout, "--- settings diff ---")
			d, err2 := posture.CheckSettings(ctx, ghBin, repoSlug, settingsSrc, stdout)
			if err2 != nil {
				return fail(stderr, err2)
			}
			if d {
				drift = true
				fmt.Fprintln(stdout, "--- applying settings ---")
				if err2 := posture.ApplySettings(ctx, ghBin, repoSlug, settingsSrc, stdout); err2 != nil {
					return fail(stderr, err2)
				}
			}
		} else {
			d, err2 := posture.CheckSettings(ctx, ghBin, repoSlug, settingsSrc, stdout)
			if err2 != nil {
				return fail(stderr, err2)
			}
			if d {
				drift = true
			}
		}
	}

	if drift && !apply && !alwaysExit0 {
		return engine.ExitFatal // exit 1 for `check`
	}
	return 0
}

// parseParams converts raw "key=value" strings into a map.
func parseParams(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, kv := range raw {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			return nil, fmt.Errorf("--param %q must be in key=value form", kv)
		}
		out[kv[:idx]] = kv[idx+1:]
	}
	return out, nil
}
