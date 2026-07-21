// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sysdeps

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlan_DarwinBrew(t *testing.T) {
	p := Platform{OS: "darwin", Managers: []Manager{ManagerBrew}}

	got := Plan(p, ToolBD)
	want := InstallPlan{
		Tool:   ToolBD,
		Route:  ManagerBrew,
		Argv:   []string{"brew", "install", "gastownhall/beads/bd"},
		Verify: []string{"bd", "version"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Plan(darwin, bd) = %+v, want %+v", got, want)
	}

	gh := Plan(p, ToolGH)
	if gh.Route != ManagerBrew || !reflect.DeepEqual(gh.Argv, []string{"brew", "install", "gh"}) {
		t.Errorf("Plan(darwin, gh) = %+v, want brew install gh", gh)
	}
	if gh.NeedsSudo {
		t.Errorf("brew route must not be flagged NeedsSudo")
	}
}

func TestPlan_UbuntuApt(t *testing.T) {
	p := Platform{OS: "linux", DistroID: "ubuntu", DistroLike: []string{"debian"}, Managers: []Manager{ManagerApt}}

	gh := Plan(p, ToolGH)
	wantArgv := []string{"sudo", "apt-get", "install", "-y", "gh"}
	if gh.Route != ManagerApt || !reflect.DeepEqual(gh.Argv, wantArgv) {
		t.Errorf("Plan(ubuntu, gh) = %+v, want route=apt argv=%v", gh, wantArgv)
	}
	if !gh.NeedsSudo {
		t.Errorf("apt-get install must be flagged NeedsSudo")
	}

	// bd has no apt route: falls straight through to manual on a plain
	// Ubuntu box with neither brew nor nix present.
	bd := Plan(p, ToolBD)
	if bd.Route != ManagerManual {
		t.Errorf("Plan(ubuntu-only-apt, bd) route = %q, want manual", bd.Route)
	}
	if bd.Argv != nil {
		t.Errorf("manual route must carry a nil Argv, got %v", bd.Argv)
	}
	if !strings.Contains(bd.Manual, "github.com/gastownhall/beads/releases") {
		t.Errorf("bd manual instructions must reference the releases page: %q", bd.Manual)
	}
	if bd.Verify == nil {
		t.Errorf("manual route must still carry the verify command")
	}
}

func TestPlan_ArchPacman(t *testing.T) {
	p := Platform{OS: "linux", DistroID: "arch", Managers: []Manager{ManagerPacman}}
	gh := Plan(p, ToolGH)
	want := []string{"sudo", "pacman", "-S", "--noconfirm", "github-cli"}
	if gh.Route != ManagerPacman || !reflect.DeepEqual(gh.Argv, want) {
		t.Errorf("Plan(arch, gh) = %+v, want route=pacman argv=%v", gh, want)
	}
	if !gh.NeedsSudo {
		t.Errorf("pacman install must be flagged NeedsSudo")
	}
}

func TestPlan_DnfAndZypperSudoFlagging(t *testing.T) {
	cases := []struct {
		name    string
		manager Manager
		argv    []string
	}{
		{"fedora dnf", ManagerDnf, []string{"sudo", "dnf", "install", "-y", "gh"}},
		{"opensuse zypper", ManagerZypper, []string{"sudo", "zypper", "install", "-y", "gh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Platform{OS: "linux", Managers: []Manager{tc.manager}}
			gh := Plan(p, ToolGH)
			if gh.Route != tc.manager || !reflect.DeepEqual(gh.Argv, tc.argv) {
				t.Errorf("Plan(%s, gh) = %+v, want route=%v argv=%v", tc.name, gh, tc.manager, tc.argv)
			}
			if !gh.NeedsSudo {
				t.Errorf("%s install must be flagged NeedsSudo", tc.name)
			}
		})
	}
}

func TestPlan_NixProfileFallback(t *testing.T) {
	// A distro sysdeps doesn't map to a native manager, with only nix on
	// PATH: bd and gh should both route through nix-profile, and neither of
	// those argvs needs sudo.
	p := Platform{OS: "linux", DistroID: "voidlinux", Managers: []Manager{ManagerNix}}

	bd := Plan(p, ToolBD)
	if bd.Route != ManagerNix || !reflect.DeepEqual(bd.Argv, []string{"nix", "profile", "install", "github:gastownhall/beads"}) {
		t.Errorf("Plan(nix-only, bd) = %+v, want nix-profile route", bd)
	}
	if bd.NeedsSudo {
		t.Errorf("nix profile install must not need sudo")
	}

	gh := Plan(p, ToolGH)
	if gh.Route != ManagerNix || !reflect.DeepEqual(gh.Argv, []string{"nix", "profile", "install", "nixpkgs#gh"}) {
		t.Errorf("Plan(nix-only, gh) = %+v, want nix-profile route", gh)
	}
}

func TestPlan_NoManager_Manual(t *testing.T) {
	p := Platform{OS: "linux", DistroID: "voidlinux"} // no managers detected at all
	for _, tool := range []Tool{ToolBD, ToolClaude, ToolGH} {
		plan := Plan(p, tool)
		if plan.Route != ManagerManual {
			t.Errorf("Plan(no-manager, %s) route = %q, want manual", tool, plan.Route)
		}
		if plan.Argv != nil {
			t.Errorf("Plan(no-manager, %s) Argv = %v, want nil", tool, plan.Argv)
		}
		if plan.Manual == "" {
			t.Errorf("Plan(no-manager, %s) Manual is empty", tool)
		}
		if strings.Contains(plan.Manual, "curl") || strings.Contains(plan.Manual, "| sh") || strings.Contains(plan.Manual, "|sh") {
			t.Errorf("Plan(no-manager, %s) Manual must never suggest curl-pipe-sh: %q", tool, plan.Manual)
		}
	}
}

func TestPlan_ClaudePrefersNpmOverBrew(t *testing.T) {
	// Both npm and brew present: claude must prefer npm (the more reliably
	// documented route — the brew cask name is not double-checked against
	// the live tap) regardless of platform manager ordering.
	p := Platform{OS: "darwin", Managers: []Manager{ManagerBrew, ManagerNpm}}
	got := Plan(p, ToolClaude)
	want := []string{"npm", "install", "-g", "@anthropic-ai/claude-code"}
	if got.Route != ManagerNpm || !reflect.DeepEqual(got.Argv, want) {
		t.Errorf("Plan(darwin+npm+brew, claude) = %+v, want npm route %v", got, want)
	}
}

func TestPlan_ClaudeFallsBackToBrewWithoutNpm(t *testing.T) {
	p := Platform{OS: "darwin", Managers: []Manager{ManagerBrew}}
	got := Plan(p, ToolClaude)
	want := []string{"brew", "install", "--cask", "claude-code"}
	if got.Route != ManagerBrew || !reflect.DeepEqual(got.Argv, want) {
		t.Errorf("Plan(darwin-brew-only, claude) = %+v, want brew cask route %v", got, want)
	}
}

func TestPlan_ClaudeManualWithoutNpmOrBrew(t *testing.T) {
	p := Platform{OS: "linux", DistroID: "ubuntu", Managers: []Manager{ManagerApt}}
	got := Plan(p, ToolClaude)
	if got.Route != ManagerManual {
		t.Errorf("Plan(ubuntu-apt-only, claude) route = %q, want manual", got.Route)
	}
	if !strings.Contains(got.Manual, "claude-code") {
		t.Errorf("claude manual instructions should mention claude-code: %q", got.Manual)
	}
}

func TestPlan_UnknownTool(t *testing.T) {
	p := Platform{OS: "darwin", Managers: []Manager{ManagerBrew}}
	got := Plan(p, Tool("nonexistent"))
	if got.Route != ManagerManual || got.Argv != nil {
		t.Errorf("Plan(unknown tool) = %+v, want manual/nil-argv", got)
	}
}

func TestPlan_ManualNeverSudo(t *testing.T) {
	// Manual routes carry no Argv at all, so NeedsSudo must always be false
	// for them — sudo-flagging only applies to a real command.
	p := Platform{OS: "linux", DistroID: "voidlinux"}
	for _, tool := range []Tool{ToolBD, ToolClaude, ToolGH} {
		if plan := Plan(p, tool); plan.NeedsSudo {
			t.Errorf("Plan(no-manager, %s).NeedsSudo = true, want false for a manual route", tool)
		}
	}
}
