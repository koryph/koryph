// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/posture"
)

// cmdRepo dispatches the repo sub-verbs.
func cmdRepo(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "repo", "check or apply .github IaC (rulesets, repo settings)", []subVerb{
			{"check [--repo owner/name]", "diff live GitHub settings/rulesets against .github IaC (exit 1 on drift)"},
			{"apply [--repo owner/name]", "apply .github IaC (rulesets, repo settings) to the live repo"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "check":
		return cmdRepoCheck(rest, stdout, stderr)
	case "apply":
		return cmdRepoApply(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown repo subcommand %q", sub))
	}
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

	src := posture.LocalSource{Root: "."}

	// --- rulesets -----------------------------------------------------------
	if _, err := src.RulesetsDir(); err == nil {
		if err := posture.ApplyRulesets(ctx, ghBin, repoSlug, src, stdout); err != nil {
			return fail(stderr, err)
		}
	}

	// --- repo settings ------------------------------------------------------
	if _, err := src.RepoSettingsFile(); err == nil {
		if err := posture.ApplySettings(ctx, ghBin, repoSlug, src, stdout); err != nil {
			return fail(stderr, err)
		}
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
