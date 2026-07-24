// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sysdeps

// Tool is one of koryph's external command-line dependencies that sysdeps
// knows how to plan an install for.
type Tool string

const (
	ToolBD     Tool = "bd"     // github.com/gastownhall/beads
	ToolClaude Tool = "claude" // Claude Code CLI
	ToolCodex  Tool = "codex"  // OpenAI Codex CLI
	ToolGH     Tool = "gh"     // GitHub CLI
)

// InstallPlan is the (unexecuted) result of planning an install for one tool
// on one platform. The caller — the adopt wizard — is responsible for
// showing Argv (or Manual) to the user, getting per-item consent (§3.3), and
// only then running it; this package never runs it.
type InstallPlan struct {
	Tool Tool
	// Route is the manager the plan uses, or ManagerManual when no
	// package-manager route is available (or none was detected on PATH).
	Route Manager
	// Argv is the exact command to run, e.g.
	// ["brew", "install", "gastownhall/beads/bd"]. Nil when Route is
	// ManagerManual.
	Argv []string
	// Manual holds human install instructions (an official docs/releases
	// URL plus the command to run by hand) when Route is ManagerManual.
	// Never a curl-pipe-sh — always a link the user reads before acting.
	Manual string
	// Verify is the post-install check argv, e.g. ["bd", "version"]. The
	// caller runs this after Argv succeeds (or after following Manual) to
	// confirm the tool is now on PATH and answers to its version flag.
	Verify []string
	// NeedsSudo is true when Argv's first element is "sudo", so the wizard
	// can call the elevation out explicitly before asking for consent
	// (docs/designs/2026-07-adopt.md §3.3: "Never sudo without showing the
	// command").
	NeedsSudo bool
}

// routeSpec is the argv for one tool × manager route.
type routeSpec struct {
	Argv []string
}

// toolSpec is the data-driven install recipe for one tool: its known routes
// keyed by Manager, an optional RoutePreference overriding the platform's
// general Managers order, and the manual fallback + verify command shared
// across every route.
type toolSpec struct {
	Routes map[Manager]routeSpec
	// RoutePreference, when non-empty, is tried in this exact order instead
	// of Platform.Managers. Used by claude: the npm install is the more
	// reliably documented route than a Homebrew cask whose exact name
	// drifts, so npm is preferred over brew wherever npm is present at all,
	// independent of the platform's general manager ordering.
	RoutePreference []Manager
	Manual          string
	Verify          []string
}

// toolSpecs is the tool table: install routes as DATA, not code branches.
// Adding a tool or a route is an entry here, never a new switch case.
var toolSpecs = map[Tool]toolSpec{
	ToolBD: {
		Routes: map[Manager]routeSpec{
			ManagerBrew: {Argv: []string{"brew", "install", "gastownhall/beads/bd"}},
			// The beads flake exposes bd as a nix-profile-installable
			// package; there is no reliable native apt/dnf/pacman package.
			ManagerNix: {Argv: []string{"nix", "profile", "install", "github:gastownhall/beads"}},
		},
		Manual: "bd (beads) has no native apt/dnf/pacman/zypper package. " +
			"Download the release archive for your OS/arch from " +
			"https://github.com/gastownhall/beads/releases, extract it, and place " +
			"`bd` on PATH.",
		Verify: []string{"bd", "version"},
	},
	ToolClaude: {
		RoutePreference: []Manager{ManagerNpm, ManagerBrew},
		Routes: map[Manager]routeSpec{
			ManagerNpm:  {Argv: []string{"npm", "install", "-g", "@anthropic-ai/claude-code"}},
			ManagerBrew: {Argv: []string{"brew", "install", "--cask", "claude-code"}},
		},
		Manual: "Install Claude Code following the official docs at " +
			"https://docs.claude.com/en/docs/claude-code/setup " +
			"(`npm install -g @anthropic-ai/claude-code`, or the platform-specific " +
			"instructions there).",
		Verify: []string{"claude", "--version"},
	},
	ToolCodex: {
		// Codex's supported distribution is its npm package. Keep the same
		// npm-first preference as Claude so an available JavaScript toolchain
		// produces a deterministic, portable plan.
		RoutePreference: []Manager{ManagerNpm},
		Routes: map[Manager]routeSpec{
			ManagerNpm: {Argv: []string{"npm", "install", "-g", "@openai/codex"}},
		},
		Manual: "Install the Codex CLI following the official documentation at " +
			"https://developers.openai.com/codex/cli/ " +
			"(`npm install -g @openai/codex`), then run `codex login`.",
		Verify: []string{"codex", "--version"},
	},
	ToolGH: {
		Routes: map[Manager]routeSpec{
			ManagerBrew:   {Argv: []string{"brew", "install", "gh"}},
			ManagerApt:    {Argv: []string{"sudo", "apt-get", "install", "-y", "gh"}},
			ManagerDnf:    {Argv: []string{"sudo", "dnf", "install", "-y", "gh"}},
			ManagerPacman: {Argv: []string{"sudo", "pacman", "-S", "--noconfirm", "github-cli"}},
			ManagerZypper: {Argv: []string{"sudo", "zypper", "install", "-y", "gh"}},
			ManagerNix:    {Argv: []string{"nix", "profile", "install", "nixpkgs#gh"}},
		},
		Manual: "Install the GitHub CLI following the official instructions at " +
			"https://github.com/cli/cli/blob/trunk/docs/install_linux.md " +
			"(or https://cli.github.com for other platforms).",
		Verify: []string{"gh", "--version"},
	},
}

// Plan builds the install plan for tool t on platform p: it walks the tool's
// route preference (RoutePreference when set, else p.Managers' platform
// order), and returns the first route whose manager was actually detected on
// PATH. An unknown tool or no matching route both degrade to Route =
// ManagerManual — sysdeps never fails a Plan call, since "no route" is
// itself a valid, expected outcome the wizard must present to the user
// (docs/designs/2026-07-adopt.md §3.3: "manual is always the fallback").
func Plan(p Platform, t Tool) InstallPlan {
	spec, ok := toolSpecs[t]
	if !ok {
		return InstallPlan{Tool: t, Route: ManagerManual, Manual: "unknown tool: " + string(t)}
	}

	order := spec.RoutePreference
	if len(order) == 0 {
		order = p.Managers
	}
	for _, m := range order {
		if !hasManager(p, m) {
			continue
		}
		route, ok := spec.Routes[m]
		if !ok {
			continue
		}
		return InstallPlan{
			Tool:      t,
			Route:     m,
			Argv:      route.Argv,
			Verify:    spec.Verify,
			NeedsSudo: len(route.Argv) > 0 && route.Argv[0] == "sudo",
		}
	}

	return InstallPlan{
		Tool:   t,
		Route:  ManagerManual,
		Manual: spec.Manual,
		Verify: spec.Verify,
	}
}
