// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/posture"
)

func init() {
	registerCmd(command{
		name:    "repo",
		summary: "check or apply .github IaC (rulesets, repo settings)",
		run:     cmdRepo,
		subs: []command{
			{name: "describe", summary: "explain every setting in .github IaC and why", run: cmdRepoDescribe},
			{name: "check", summary: "diff live GitHub settings/rulesets against .github IaC (exit 1 on drift)", run: cmdRepoCheck},
			{name: "apply", summary: "apply .github IaC (rulesets, repo settings) to the live repo", run: cmdRepoApply},
		},
	})
}

// cmdRepo dispatches the repo sub-verbs.
func cmdRepo(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "repo", "check or apply .github IaC (rulesets, repo settings)", []subVerb{
			{"describe [--repo owner/name]", "explain every setting in .github IaC and why"},
			{"check [--repo owner/name]", "diff live GitHub settings/rulesets against .github IaC (exit 1 on drift)"},
			{"apply [--repo owner/name]", "apply .github IaC (rulesets, repo settings) to the live repo"},
			{"rollback [--repo owner/name] [--to <timestamp>|latest]", "roll back to a pre-apply snapshot"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "describe":
		return cmdRepoDescribe(rest, stdout, stderr)
	case "check":
		return cmdRepoCheck(rest, stdout, stderr)
	case "apply":
		return cmdRepoApply(rest, stdout, stderr)
	case "rollback":
		return cmdRepoRollback(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown repo subcommand %q", sub))
	}
}

// cmdRepoDescribe implements `koryph repo describe [--repo owner/name]`.
//
// It reads desired state from .github/rulesets/*.json and
// .github/repo-settings.json in the current working directory and prints
// a human-readable explanation of every managed setting and ruleset rule:
// the target value and the security rationale for each entry.
// When --repo is given, the live GitHub value is also shown beside each
// entry with a "no change / WOULD CHANGE" marker.
func cmdRepoDescribe(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("repo describe", stderr)
	repo := fs.String("repo", "", "repository in owner/name form — when given, shows live value per setting")
	setUsage(fs, stdout,
		"explain every setting in .github IaC and why",
		"[--repo owner/name]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	ghBin := posture.GHBin()

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	src := posture.LocalSource{Root: cwd}

	// Resolve repo slug only when --repo is given.
	repoSlug := ""
	if *repo != "" {
		repoSlug, err = resolveRepo(ctx, ghBin, *repo)
		if err != nil {
			return fail(stderr, err)
		}
	}

	desc, err := posture.DescribeSource(ctx, ghBin, src, repoSlug, nil)
	if err != nil {
		return fail(stderr, err)
	}

	if len(desc.Settings) == 0 && len(desc.Rulesets) == 0 {
		fmt.Fprintln(stdout, "No .github IaC found in current directory.")
		fmt.Fprintln(stdout, "Expected .github/repo-settings.json and/or .github/rulesets/*.json")
		return 0
	}

	posture.PrintDescription(stdout, desc, "", "")
	return 0
}

// cmdRepoCheck implements `koryph repo check [--repo owner/name]`.
//
// It reads desired state from .github/rulesets/*.json and
// .github/repo-settings.json in the current working directory (or the
// worktree root when running under the engine) and compares it against the
// live GitHub repository settings.  Exits 1 on drift, 0 when everything
// matches, 2 on usage error.
func cmdRepoCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("repo check", stderr)
	repo := fs.String("repo", "", "repository in owner/name form (default: detected from git remote via gh)")
	setUsage(fs, stdout,
		"diff live GitHub settings/rulesets against .github IaC (exit 1 on drift)",
		"[--repo owner/name]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	ghBin := posture.GHBin()

	repoSlug, err := resolveRepo(ctx, ghBin, *repo)
	if err != nil {
		return fail(stderr, err)
	}

	src := posture.LocalSource{Root: "."}
	drift := false

	// --- rulesets -----------------------------------------------------------
	if dir, err := src.RulesetsDir(); err == nil {
		_ = dir // just checking existence; CheckRulesets reads it
		d, err := posture.CheckRulesets(ctx, ghBin, repoSlug, src, stdout)
		if err != nil {
			return fail(stderr, err)
		}
		if d {
			drift = true
		}
	}

	// --- repo settings ------------------------------------------------------
	if f, err := src.RepoSettingsFile(); err == nil {
		_ = f
		d, err := posture.CheckSettings(ctx, ghBin, repoSlug, src, stdout)
		if err != nil {
			return fail(stderr, err)
		}
		if d {
			drift = true
		}
	}

	if drift {
		return engine.ExitFatal // exit 1
	}
	return 0
}

// cmdRepoApply implements `koryph repo apply [--repo owner/name]`.
//
// It reads desired state from .github/rulesets/*.json and
// .github/repo-settings.json and creates-or-updates the live GitHub
// repository settings to match.  Never deletes rulesets it does not know
// about.
//
// Before applying, it captures a pre-change snapshot of the live state into
// <cwd>/.koryph/snapshots/settings-<timestamp>.json (skipped when the diff
// is empty — nothing would change).  Roll back with `koryph repo rollback`.
func cmdRepoApply(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("repo apply", stderr)
	repo := fs.String("repo", "", "repository in owner/name form (default: detected from git remote via gh)")
	setUsage(fs, stdout,
		"apply .github IaC (rulesets, repo settings) to the live repo",
		"[--repo owner/name]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	ghBin := posture.GHBin()

	repoSlug, err := resolveRepo(ctx, ghBin, *repo)
	if err != nil {
		return fail(stderr, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	src := posture.LocalSource{Root: cwd}

	// --- diff to decide whether a snapshot is needed ----------------------
	driftRulesets := false
	driftSettings := false

	if _, err2 := src.RulesetsDir(); err2 == nil {
		d, err2 := posture.CheckRulesets(ctx, ghBin, repoSlug, src, stdout)
		if err2 != nil {
			return fail(stderr, err2)
		}
		driftRulesets = d
	}

	if _, err2 := src.RepoSettingsFile(); err2 == nil {
		d, err2 := posture.CheckSettings(ctx, ghBin, repoSlug, src, stdout)
		if err2 != nil {
			return fail(stderr, err2)
		}
		driftSettings = d
	}

	anyDrift := driftRulesets || driftSettings
	if !anyDrift {
		// Nothing to do; diff already printed above.
		return 0
	}

	// --- capture pre-change snapshot --------------------------------------
	snapPath, err := posture.CaptureSnapshot(ctx, ghBin, repoSlug, cwd, "iac")
	if err != nil {
		// Non-fatal: warn but proceed with apply.
		fmt.Fprintf(stderr, "warning: could not capture pre-change snapshot: %v\n", err)
	} else {
		fmt.Fprintf(stdout, "captured pre-change state → %s; rollback with koryph repo rollback\n", snapPath)
	}

	// --- apply -------------------------------------------------------------
	if driftRulesets {
		if err := posture.ApplyRulesets(ctx, ghBin, repoSlug, src, stdout); err != nil {
			return fail(stderr, err)
		}
	}

	if driftSettings {
		if err := posture.ApplySettings(ctx, ghBin, repoSlug, src, stdout); err != nil {
			return fail(stderr, err)
		}
	}

	return 0
}

// cmdRepoRollback implements `koryph repo rollback [--repo owner/name] [--to <ts>|latest]`.
//
// It lists available pre-apply snapshots under <cwd>/.koryph/snapshots/, shows
// the diff between the chosen snapshot and the current live state, then applies
// the snapshot through the standard apply machinery.
func cmdRepoRollback(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("repo rollback", stderr)
	repo := fs.String("repo", "", "repository in owner/name form (default: detected from git remote via gh)")
	to := fs.String("to", "latest", `snapshot selector: "latest" or a RFC3339 timestamp (or prefix, e.g. "2026-07-04T16")`)
	setUsage(fs, stdout,
		"roll back live repo settings to a pre-apply snapshot",
		"[--repo owner/name] [--to <timestamp>|latest]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()
	ghBin := posture.GHBin()

	repoSlug, err := resolveRepo(ctx, ghBin, *repo)
	if err != nil {
		return fail(stderr, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	if _, err := posture.Rollback(ctx, ghBin, repoSlug, cwd, *to, stdout, stderr); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// resolveRepo returns explicit when non-empty; otherwise calls gh to detect
// the current repository slug.
func resolveRepo(ctx context.Context, ghBin, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return posture.DetectRepo(ctx, ghBin)
}
