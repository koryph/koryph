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
	ghpkg "github.com/koryph/koryph/internal/forge/github"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/posture"
	"github.com/koryph/koryph/internal/project"
)

func init() {
	registerCmd(command{
		name:    "posture",
		summary: "apply a named desired-state profile to a GitHub repo",
		run:     cmdPosture,
		DocLinks: []string{
			"concepts/postures.md",
			"user-guide/postures.md",
		},
		subs: []command{
			{
				name:     "list",
				summary:  "list built-in and user-defined profiles",
				run:      cmdPostureList,
				DocLinks: []string{"user-guide/postures.md"},
			},
			{
				name:     "describe",
				summary:  "explain every setting a profile enforces and why",
				run:      cmdPostureDescribe,
				DocLinks: []string{"concepts/postures.md", "user-guide/postures.md"},
			},
			{
				name:     "check",
				summary:  "diff live GitHub state against a profile (exit 1 on drift, or 0 with --no-fail)",
				run:      cmdPostureCheck,
				DocLinks: []string{"concepts/postures.md", "user-guide/postures.md"},
			},
			{
				name:     "diff",
				summary:  "deprecated alias for `check --no-fail` (always exit 0)",
				run:      cmdPostureDiff,
				DocLinks: []string{"concepts/postures.md", "user-guide/postures.md"},
			},
			{
				name:     "apply",
				summary:  "show diff then apply a profile to the live GitHub repo",
				run:      cmdPostureApply,
				DocLinks: []string{"concepts/postures.md", "user-guide/postures.md"},
			},
			{
				name:     "rollback",
				summary:  "roll back to a pre-apply snapshot (alias for `repo rollback`)",
				run:      cmdPostureRollback,
				DocLinks: []string{"user-guide/postures.md"},
			},
		},
	})
}

// cmdPosture dispatches the posture sub-verbs.
func cmdPosture(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "posture", "apply a named desired-state profile to a GitHub repo", []subVerb{
			{"list", "list built-in and user-defined profiles"},
			{"list --fragments", "list built-in security-scanner fragments"},
			{"describe <profile> [--repo owner/name] [--param k=v]...", "explain every setting a profile enforces and why"},
			{"check <profile> [--repo owner/name] [--org ORG] [--param k=v]... [--no-fail]", "diff live GitHub state against profile (exit 1 on drift, or 0 with --no-fail)"},
			{"diff <profile> [--repo owner/name] [--org ORG] [--param k=v]...", "deprecated alias for `check --no-fail` (always exit 0)"},
			{"apply <profile> [--repo owner/name] [--org ORG] [--param k=v]...", "show diff then apply profile to live GitHub repo"},
			{"rollback [--repo owner/name] [--to <timestamp>|latest]", "roll back to a pre-apply snapshot (alias for `repo rollback`)"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdPostureList(rest, stdout, stderr)
	case "describe":
		return cmdPostureDescribe(rest, stdout, stderr)
	case "check":
		return cmdPostureCheck(rest, stdout, stderr)
	case "diff":
		return cmdPostureDiff(rest, stdout, stderr)
	case "apply":
		return cmdPostureApply(rest, stdout, stderr)
	case "rollback":
		return cmdPostureRollback(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown posture subcommand %q", sub))
	}
}

// cmdPostureList lists built-in and user profiles, or fragments with --fragments.
func cmdPostureList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("posture list", stderr)
	fragments := fs.Bool("fragments", false, "list built-in security-scanner fragments instead of profiles")
	setUsage(fs, stdout, "list built-in and user-defined posture profiles (or --fragments for scanner fragments)", "")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	if *fragments {
		return cmdPostureListFragments(stdout, stderr)
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
	fmt.Fprintln(tw, "NAME\tSOURCE\tDESCRIPTION\tRECOMMENDS")
	for _, p := range all {
		desc := p.Manifest.Description
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		recommends := strings.Join(p.Manifest.RecommendedFragments, ", ")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, p.Source, desc, recommends)
	}
	tw.Flush()
	return 0
}

// cmdPostureListFragments lists the built-in security-scanner fragments.
func cmdPostureListFragments(stdout, stderr io.Writer) int {
	frags, err := posture.ListFragments()
	if err != nil {
		return fail(stderr, err)
	}
	if len(frags) == 0 {
		fmt.Fprintln(stdout, "No fragments found.")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDESCRIPTION\tINSTALLS")
	for _, f := range frags {
		desc := f.Manifest.Description
		if len(desc) > 55 {
			desc = desc[:52] + "..."
		}
		installs := strings.Join(f.Manifest.InstalledFiles, ", ")
		fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Name, desc, installs)
	}
	tw.Flush()
	return 0
}

// cmdPostureCheck implements `koryph posture check <profile> [--repo owner/name] [--param k=v]...`
// Exits 1 on drift, 0 when everything matches (or --no-fail was passed), 2 on
// usage error.
func cmdPostureCheck(args []string, stdout, stderr io.Writer) int {
	return runPostureVerb(args, "posture check", stdout, stderr, false, false)
}

// cmdPostureDiff implements `koryph posture diff <profile> [--repo owner/name] [--param k=v]...`
// Deprecated alias for `posture check --no-fail` (koryph-b8g #22: check and
// diff differed only by exit code) — always exits 0, drift is informational.
func cmdPostureDiff(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "koryph: warning: `posture diff` is deprecated; use `posture check --no-fail` instead")
	return runPostureVerb(args, "posture diff", stdout, stderr, false, true)
}

// cmdPostureApply implements `koryph posture apply <profile> [--repo owner/name] [--param k=v]...`
// Prints the diff first, then applies.
func cmdPostureApply(args []string, stdout, stderr io.Writer) int {
	return runPostureVerb(args, "posture apply", stdout, stderr, true, false)
}

// cmdPostureRollback implements `koryph posture rollback` — a posture-side
// alias for `repo rollback` (koryph-b8g #21), sharing its implementation so
// the two spellings can never drift.
func cmdPostureRollback(args []string, stdout, stderr io.Writer) int {
	return runRepoRollback("posture rollback", args, stdout, stderr)
}

// runPostureVerb is the shared implementation for check/diff/apply.
//
//   - apply=true: print diff, then apply changes.
//   - alwaysExit0=true: never return exit-1 on drift (diff mode); this is
//     also the --no-fail flag's default, so `posture diff` keeps behaving
//     exactly like `posture check --no-fail` without the operator having to
//     pass the flag explicitly (koryph-b8g #22).
func runPostureVerb(args []string, cmdName string, stdout, stderr io.Writer, apply, alwaysExit0 bool) int {
	fs := newFlagSet(cmdName, stderr)
	repo := fs.String("repo", "", "repository in owner/name form (default: detected from git remote via gh)")
	org := fs.String("org", "", "GitHub organisation for org-level ruleset check/apply (requires org owner/admin)")
	force := fs.Bool("force", false, "with apply: overwrite stale fragment files (default: only install missing fragments)")
	// --no-fail only means something for check/diff (apply's exit code never
	// reflects drift); registering it just for those two keeps `posture apply
	// -h` free of a flag that would silently do nothing.
	var noFail *bool
	if !apply {
		noFail = fs.Bool("no-fail", alwaysExit0, "always exit 0, even when drift is found (informational; same as `posture diff`)")
	}
	var rawParams multiFlag
	fs.Var(&rawParams, "param", "profile parameter as key=value (repeatable, e.g. --param required_checks=\"pre-commit,make gate\")")
	setUsage(fs, stdout,
		cmdName+" — compare or apply a named posture profile",
		"<profile> [--repo owner/name] [--org ORG] [--param k=v]... [--force]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if noFail != nil {
		alwaysExit0 = alwaysExit0 || *noFail
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
	ghProv := ghpkg.New()

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

	// snapshotOnce captures a pre-change snapshot the first time it is called
	// with a drifting section (apply mode only). Idempotent after the first
	// call. Snapshot is skipped when diff is empty (nothing will change).
	snapshotCaptured := false
	snapshotOnce := func() {
		if snapshotCaptured || !apply {
			return
		}
		snapshotCaptured = true
		snapPath, serr := posture.CaptureSnapshot(ctx, ghProv.Repo(), ghProv.Protection(), repoSlug, cwd, "profile:"+profileName)
		if serr != nil {
			fmt.Fprintf(stderr, "warning: could not capture pre-change snapshot: %v\n", serr)
			return
		}
		fmt.Fprintf(stdout, "captured pre-change state → %s; rollback with koryph posture rollback\n", snapPath)
	}

	// ---- rulesets -------------------------------------------------------
	rulesetSrc := profileSrc
	if hasRulesets {
		rulesetSrc = localSrc
	}
	if _, err2 := rulesetSrc.RulesetsDir(); err2 == nil {
		if apply {
			// Diff first.
			fmt.Fprintln(stdout, "--- rulesets diff ---")
			d, err2 := posture.CheckRulesets(ctx, repoSlug, rulesetSrc, stdout, ghProv.Protection())
			if err2 != nil {
				return fail(stderr, err2)
			}
			if d {
				drift = true
				snapshotOnce()
				fmt.Fprintln(stdout, "--- applying rulesets ---")
				if err2 := posture.ApplyRulesets(ctx, repoSlug, rulesetSrc, stdout, ghProv.Protection()); err2 != nil {
					return fail(stderr, err2)
				}
			}
		} else {
			d, err2 := posture.CheckRulesets(ctx, repoSlug, rulesetSrc, stdout, ghProv.Protection())
			if err2 != nil {
				return fail(stderr, err2)
			}
			if d {
				drift = true
			}
		}
	}

	// ---- repo settings --------------------------------------------------
	settingsSrc := profileSrc
	if hasSettings {
		settingsSrc = localSrc
	}
	if _, err2 := settingsSrc.RepoSettingsFile(); err2 == nil {
		if apply {
			fmt.Fprintln(stdout, "--- settings diff ---")
			d, err2 := posture.CheckSettings(ctx, repoSlug, settingsSrc, stdout, ghProv.Repo())
			if err2 != nil {
				return fail(stderr, err2)
			}
			if d {
				drift = true
				snapshotOnce()
				fmt.Fprintln(stdout, "--- applying settings ---")
				if err2 := posture.ApplySettings(ctx, repoSlug, settingsSrc, stdout, ghProv.Repo()); err2 != nil {
					return fail(stderr, err2)
				}
			}
		} else {
			d, err2 := posture.CheckSettings(ctx, repoSlug, settingsSrc, stdout, ghProv.Repo())
			if err2 != nil {
				return fail(stderr, err2)
			}
			if d {
				drift = true
			}
		}
	}

	// ---- org-level rulesets ---------------------------------------------
	// Only run when --org is supplied.  Missing org-rulesets dir in the
	// profile is silently skipped (the profile may not carry any).
	if *org != "" {
		if _, err2 := profileSrc.OrgRulesetsDir(); err2 == nil {
			if apply {
				fmt.Fprintln(stdout, "--- org rulesets diff ---")
				d, err2 := posture.CheckOrgRulesets(ctx, *org, profileSrc, stdout, ghProv.Protection())
				if err2 != nil {
					return fail(stderr, err2)
				}
				if d {
					drift = true
					snapshotOnce()
					fmt.Fprintln(stdout, "--- applying org rulesets ---")
					if err2 := posture.ApplyOrgRulesets(ctx, *org, profileSrc, stdout, ghProv.Protection()); err2 != nil {
						return fail(stderr, err2)
					}
				}
			} else {
				d, err2 := posture.CheckOrgRulesets(ctx, *org, profileSrc, stdout, ghProv.Protection())
				if err2 != nil {
					return fail(stderr, err2)
				}
				if d {
					drift = true
				}
			}
		}
	}

	// ---- security-scanner fragments -------------------------------------
	// Fragments are opted into per-project via koryph.project.json
	// posture.fragments. When no project config is found the fragment section
	// is skipped gracefully (not every invocation is inside a koryph project).
	if frags := loadProjectFragments(cwd, profileName); len(frags) > 0 {
		if apply {
			fmt.Fprintln(stdout, "--- fragments diff ---")
			d, err2 := posture.CheckFragments(cwd, frags, stdout)
			if err2 != nil {
				return fail(stderr, err2)
			}
			if d {
				drift = true
				// Note: fragments are local files, not GitHub API state; they
				// are not included in the repo-level snapshot. snapshotOnce
				// still fires here to capture repo state before any changes.
				snapshotOnce()
				fmt.Fprintln(stdout, "--- applying fragments ---")
				if _, err2 := posture.ApplyFragments(cwd, frags, *force, stdout); err2 != nil {
					return fail(stderr, err2)
				}
			}
		} else {
			d, err2 := posture.CheckFragments(cwd, frags, stdout)
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

// loadProjectFragments reads the opted-in fragment list from koryph.project.json
// in root. Returns nil (no fragments, no error) when no project config is present
// or no fragments are declared. The profile name is informational only (used for
// logging in future; not needed for the lookup itself).
func loadProjectFragments(root, _ string) []string {
	cfg, err := project.Load(root)
	if err != nil {
		return nil // no project config — fragments silently skipped
	}
	if cfg.Posture == nil || len(cfg.Posture.Fragments) == 0 {
		return nil
	}
	return cfg.Posture.Fragments
}

// cmdPostureDescribe implements `koryph posture describe <profile> [--repo owner/name] [--param k=v]...`
//
// It renders the named profile, then prints a human-readable explanation of
// every managed setting and ruleset rule: the target value and the security
// rationale for each entry.  When --repo is given, the live GitHub value is
// also shown beside each entry with a "no change / WOULD CHANGE" marker.
func cmdPostureDescribe(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("posture describe", stderr)
	repo := fs.String("repo", "", "repository in owner/name form — when given, shows live value per setting")
	var rawParams multiFlag
	fs.Var(&rawParams, "param", "profile parameter as key=value (repeatable, e.g. --param required_checks=\"pre-commit,make gate\")")
	setUsage(fs, stdout,
		"explain every setting a profile enforces and why",
		"<profile> [--repo owner/name] [--param k=v]...")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) < 1 {
		return usageErr(stderr, "posture describe: <profile> is required")
	}
	profileName := pos[0]

	params, err := parseParams(rawParams)
	if err != nil {
		return usageErr(stderr, "posture describe: "+err.Error())
	}

	ctx := context.Background()
	ghBin := posture.GHBin()
	ghProv := ghpkg.New()
	home := paths.KoryphHome()

	// Load the manifest for the profile description and any manifest-level
	// rationale overrides.
	builtins, _ := posture.ListBuiltins()
	user, _ := posture.ListUserProfiles(home)
	var manifest posture.Manifest
	for _, e := range append(user, builtins...) { // user profiles take precedence
		if e.Name == profileName {
			manifest = e.Manifest
			break
		}
	}
	if manifest.Name == "" {
		fmt.Fprintf(stderr, "koryph: profile %q not found\n", profileName)
		return 1
	}

	// Render the profile to a temp directory.
	profileSrc, cleanup, err := posture.RenderProfile(profileName, params, home)
	if err != nil {
		return fail(stderr, err)
	}
	defer cleanup()

	// Resolve the repo slug when --repo was given (or skip live comparison).
	repoSlug := ""
	if *repo != "" {
		repoSlug, err = resolveRepo(ctx, ghBin, *repo)
		if err != nil {
			return fail(stderr, err)
		}
	}

	desc, err := posture.DescribeSource(ctx, ghProv.Repo(), ghProv.Protection(), profileSrc, repoSlug, manifest.Descriptions)
	if err != nil {
		return fail(stderr, err)
	}

	posture.PrintDescription(stdout, desc, manifest.Name, manifest.Description)
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
