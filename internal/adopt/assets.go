// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"fmt"
	"io"
	"strings"

	"github.com/koryph/koryph/internal/agentsmd"
	"github.com/koryph/koryph/internal/commands"
	"github.com/koryph/koryph/internal/personas"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/rules"
	"github.com/koryph/koryph/internal/runtime"
	_ "github.com/koryph/koryph/internal/runtime/codex"
	"github.com/koryph/koryph/internal/scaffold"
)

// InstallAssets runs the koryph asset install sequence — AGENTS.md, agent
// personas, commands, and rules (hooks + settings.json) — honoring runtime
// capability gating, in the exact order and with the exact skip logic
// `koryph project add` has always used (cmd/koryph/project.go's
// cmdProjectAdd). It is shared verbatim by cmdProjectAdd and the adopt
// wizard's "assets" execute step (koryph-14p.9) so the two entry points can
// never drift.
//
// Order and rationale (unchanged from cmdProjectAdd):
//  1. AGENTS.md — always installed; the canonical, runtime-neutral
//     instruction file read natively by Codex, Cursor, Grok, Copilot,
//     opencode, amp, and Claude Code.
//  2. agents (.claude/agents) — always installed; personas render correctly
//     for any runtime via InstallForRuntime.
//  3. commands (commands/*.md) — always seeded, then linked into Claude
//     slash commands or Codex repository skills from that same source.
//  4. rules (hooks + settings.json) — Claude Code only; skipped when the
//     project's runtime does not support lifecycle hooks (Capabilities.Hooks).
//
// All output is diagnostic (warnings/notes) and goes to stderr — this
// function never fails onboarding; a conflict or skip is surfaced, not
// escalated to an error. force is always false here (matching
// cmdProjectAdd's first-install behavior); re-installing with --force is
// `koryph project install-assets <root> --force`.
func InstallAssets(stderr io.Writer, root string) {
	// Keep every enabled runtime projection installed. A project that enables
	// Claude and Codex therefore has one canonical corpus and two live native
	// views, rather than whichever runtime happened to be default at onboarding.
	runtimeNames := []string{"claude"}
	if cfg, err := project.Load(root); err == nil {
		runtimeNames = cfg.EnabledRuntimeNames()
	}

	installAgentsMD(stderr, root)
	for _, runtimeName := range runtimeNames {
		installAgents(stderr, root, runtimeName)
		installCommands(stderr, root, runtimeName)
		caps := resolveCapabilities(runtimeName)
		if caps.Hooks {
			installRules(stderr, root, runtimeName)
		} else {
			fmt.Fprintf(stderr, "koryph: rules skipped for %s (no native hooks; containment via worktree isolation + merge gate)\n", runtimeName)
		}
	}
}

func installAgentsMD(stderr io.Writer, root string) {
	action, err := agentsmd.Install(root, false)
	if err != nil {
		fmt.Fprintf(stderr, "koryph: warning: could not install AGENTS.md: %v\n", err)
		return
	}
	switch action {
	case scaffold.ActionInstalled:
		fmt.Fprintln(stderr, "koryph: installed AGENTS.md (koryph operating contract)")
	case scaffold.ActionSkipped:
		fmt.Fprintln(stderr, "koryph: warning: AGENTS.md already exists with different content, left unchanged — run `koryph project install-assets <root> agentsmd --force` to update")
	}
}

func installAgents(stderr io.Writer, root, runtimeName string) {
	results, _, err := personas.InstallForRuntime(root, false, runtimeName)
	reportAssetInstall(stderr, "agents ("+runtimeName+")", results, err)
}

func installCommands(stderr io.Writer, root, runtimeName string) {
	results, err := commands.InstallForRuntime(root, false, runtimeName)
	reportAssetInstall(stderr, "commands ("+runtimeName+")", results, err)
}

func installRules(stderr io.Writer, root, runtimeName string) {
	hookResults, settings, err := rules.InstallForRuntime(root, false, runtimeName)
	if err != nil {
		fmt.Fprintf(stderr, "koryph: warning: could not install rules: %v\n", err)
		return
	}
	if n := scaffold.Count(hookResults, scaffold.ActionInstalled); n > 0 {
		fmt.Fprintf(stderr, "koryph: installed %d hook script(s) for %s\n", n, runtimeName)
	}
	if c := scaffold.Conflicts(hookResults); len(c) > 0 {
		fmt.Fprintf(stderr, "koryph: warning: %d hook script(s) differ, left unchanged: %s — run `koryph rules install <root> --force`\n",
			len(c), strings.Join(c, ", "))
	}
	switch settings {
	case rules.SettingsCreated:
		fmt.Fprintf(stderr, "koryph: wrote %s hook wiring\n", runtimeName)
	case rules.SettingsMerged:
		fmt.Fprintf(stderr, "koryph: merged koryph hook wiring for %s\n", runtimeName)
	case rules.SettingsSkipped:
		fmt.Fprintf(stderr, "koryph: warning: %s hook configuration unparseable/incompatible — left unchanged (fix it or `koryph rules install --force`)\n", runtimeName)
	}
}

// reportAssetInstall runs an install and reports it in onboard's
// warning-only voice (never a hard failure — mirrors cmd/koryph's
// onboardInstall).
func reportAssetInstall(stderr io.Writer, label string, results []scaffold.Result, err error) {
	if err != nil {
		fmt.Fprintf(stderr, "koryph: warning: could not install %s: %v\n", label, err)
		return
	}
	if n := scaffold.Count(results, scaffold.ActionInstalled); n > 0 {
		fmt.Fprintf(stderr, "koryph: installed %d %s\n", n, label)
	}
	if conflicts := scaffold.Conflicts(results); len(conflicts) > 0 {
		fmt.Fprintf(stderr, "koryph: warning: %d %s already exist with different content, left unchanged: %s\n",
			len(conflicts), label, strings.Join(conflicts, ", "))
		fmt.Fprintf(stderr, "koryph: run `koryph %s install <root> --force` to update them.\n", label)
	}
}

// resolveCapabilities mirrors cmd/koryph's resolveRuntimeCapabilities: the
// named runtime's Capabilities, falling back to claude's full capability set
// when the name is empty/unregistered — so a project with no config yet
// (adopt's own register+config step may run AFTER assets in a re-adopt, or
// this may be called before config is written on a fresh repo) still gets
// every asset installed, matching pre-capability-gating behavior.
func resolveCapabilities(runtimeName string) runtime.Capabilities {
	if runtimeName == "" {
		runtimeName = "claude"
	}
	rt, ok := runtime.Default.Get(runtimeName)
	if !ok {
		return claudeCapabilities()
	}
	return rt.Capabilities()
}

// claudeCapabilities returns claude's registered Capabilities, or a
// hard-coded permissive default when (only possible in a stripped test
// binary that never imports internal/runtime/claude) claude itself is not
// registered.
func claudeCapabilities() runtime.Capabilities {
	if rt, ok := runtime.Default.Get("claude"); ok {
		return rt.Capabilities()
	}
	return runtime.Capabilities{
		JSONStream: true, Personas: true, Hooks: true, Resume: true,
		EffortFlag: true, BudgetFlag: true, Sandbox: true, ModelSelect: true, UsageSource: true,
	}
}
