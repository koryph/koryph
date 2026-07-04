// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/project"
	pkgrelease "github.com/koryph/koryph/internal/release"
)

// cmdRelease dispatches the release sub-verbs.
func cmdRelease(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "release", "configure and operate the project release pipeline", []subVerb{
			{"setup --project ID [--mode goreleaser|commands] [--version V] [--bot]", "render and install release workflow + release-please config into the project"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "setup":
		return cmdReleaseSetup(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown release subcommand %q", sub))
	}
}

// cmdReleaseSetup renders and installs the release pipeline files into the
// target project. It:
//
//  1. Loads the project registry record + koryph.project.json.
//  2. Optionally creates or updates the project's release block
//     (--mode selects the build toolchain when the block is absent).
//  3. Calls internal/release.Setup to render templates + write files.
//  4. Saves the updated koryph.project.json.
//  5. With --bot, runs scripts/provision-release-bot.sh --attach in the
//     project root.
//  6. Prints the remaining HUMAN steps.
func cmdReleaseSetup(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("release setup", stderr)
	flagProject := fs.String("project", "", "project id (required)")
	flagMode := fs.String("mode", "", "build mode: goreleaser (mode A) or commands (mode B); required when the project has no release block yet")
	flagVersion := fs.String("version", "0.0.0", "initial version for the release-please manifest (only used when the manifest does not yet exist)")
	flagBot := fs.Bool("bot", false, "run scripts/provision-release-bot.sh --attach after setup")
	setUsage(fs, stdout,
		"render and install the release workflow + release-please config into a project",
		"--project ID [--mode goreleaser|commands] [--version V] [--bot]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	id, code := resolveProjectID(stderr, "release setup", "", *flagProject)
	if code != 0 {
		return code
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, err := store.Get(id)
	if err != nil {
		return fail(stderr, err)
	}

	cfg, err := project.Load(rec.Root)
	if err != nil {
		return fail(stderr, err)
	}

	// If the project has no release block yet, we must create one using --mode.
	if cfg.Release == nil {
		if *flagMode == "" {
			return usageErr(stderr,
				"release setup: project has no release block; specify --mode goreleaser or --mode commands to create one")
		}
		rc, merr := newReleaseConfig(*flagMode)
		if merr != nil {
			return fail(stderr, merr)
		}
		cfg.Release = rc
	} else if *flagMode != "" {
		// --mode explicitly overrides the build mode in an existing block.
		if err := applyBuildMode(cfg.Release, *flagMode); err != nil {
			return fail(stderr, err)
		}
	}

	// Validate the updated config before touching any files.
	if err := cfg.Validate(); err != nil {
		return fail(stderr, fmt.Errorf("project config invalid after release block update: %w", err))
	}

	// Render and install release pipeline files.
	res, err := pkgrelease.Setup(rec.Root, cfg.Release, *flagVersion)
	if err != nil {
		return fail(stderr, err)
	}

	// Save the (possibly updated) project config back to disk.
	if err := cfg.Save(rec.Root); err != nil {
		return fail(stderr, fmt.Errorf("save project config: %w", err))
	}

	// Print what was installed.
	relPath := func(p string) string {
		rel, rerr := filepath.Rel(rec.Root, p)
		if rerr != nil {
			return p
		}
		return rel
	}
	fmt.Fprintf(stdout, "installed: %s\n", relPath(res.WorkflowPath))
	fmt.Fprintf(stdout, "installed: %s\n", relPath(res.ConfigPath))
	if res.ManifestCreated {
		fmt.Fprintf(stdout, "installed: %s (initial version %s)\n", relPath(res.ManifestPath), *flagVersion)
	} else {
		fmt.Fprintf(stdout, "skipped:   %s (already exists — managed by release-please)\n", relPath(res.ManifestPath))
	}

	// Optional: provision the release bot.
	if *flagBot {
		botScript := filepath.Join(rec.Root, "scripts", "provision-release-bot.sh")
		fmt.Fprintf(stdout, "\nrunning %s --attach ...\n", botScript)
		cmd := exec.CommandContext(ctx, botScript, "--attach")
		cmd.Dir = rec.Root
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(stderr, "koryph: provision-release-bot.sh --attach: %v\n", err)
			// Non-fatal: the rest of setup succeeded.
		}
	}

	// Print remaining human steps.
	if len(res.HumanSteps) > 0 {
		fmt.Fprintln(stdout, "\nRemaining HUMAN steps:")
		for i, step := range res.HumanSteps {
			fmt.Fprintf(stdout, "  %d. %s\n", i+1, step)
		}
	}
	return 0
}

// newReleaseConfig creates a minimal ReleaseConfig for the given build mode.
// The operator can enrich it further in koryph.project.json after setup.
func newReleaseConfig(mode string) (*project.ReleaseConfig, error) {
	rc := &project.ReleaseConfig{
		Type: "go", // sensible default; operator edits koryph.project.json to change
	}
	return rc, applyBuildMode(rc, mode)
}

// applyBuildMode sets the build mode on an existing ReleaseConfig, clearing
// the complementary mode. Recognized modes: "goreleaser" (A), "commands" (B).
func applyBuildMode(rc *project.ReleaseConfig, mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "goreleaser", "a":
		rc.Build.Goreleaser = &project.GoreleaserBuild{Version: "~> v2"}
		rc.Build.Commands = nil
	case "commands", "b":
		rc.Build.Goreleaser = nil
		if len(rc.Build.Commands) == 0 {
			// Placeholder — the operator fills in actual commands.
			rc.Build.Commands = []string{"make build"}
		}
	default:
		return fmt.Errorf("unknown build mode %q (want goreleaser or commands)", mode)
	}
	return nil
}
