// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/registry"
)

// toolOrder is the fixed presentation order for the tools category.
var toolOrder = []string{"git", "claude", "bd", "gh"}

// BuildPlan turns a detect-phase Snapshot into the ordered, printable plan
// (design §3.2). It is a pure function of snap: no I/O, no writes, and no
// prompting — the confirm phase (ResolveAccountNonInteractive et al. plus the
// CLI's interactive prompts) decides what to DO with a `needed`/`offer` step;
// this function only decides what state each step is currently in.
func BuildPlan(snap *Snapshot) []Step {
	var steps []Step
	steps = append(steps, buildToolSteps(snap.Tools)...)

	home := buildHomeStep()
	beadsStep := buildBeadsStep(snap)
	register := buildRegisterStep(snap)
	config := buildConfigStep(snap)
	assets := buildAssetsStep(snap)

	steps = append(steps, home, beadsStep, register, config, assets)
	steps = append(steps, buildSigningStep(snap), buildPostureStep(snap))

	coreDone := allDone(steps) // every step appended so far, tools included
	commit := buildCommitStep()
	if coreDone {
		commit.State = StateDone
		commit.Detail = "nothing to commit"
	}
	steps = append(steps, commit)

	steps = append(steps, buildVerifyStep(snap))
	return steps
}

// allDone reports whether every step so far is done or an (always-optional)
// offer — i.e. nothing left that execute would need consent to act on.
func allDone(steps []Step) bool {
	for _, s := range steps {
		if s.State != StateDone && s.State != StateOffer {
			return false
		}
	}
	return true
}

// --- tools -------------------------------------------------------------

func buildToolSteps(tools map[string]ToolStatus) []Step {
	var doneParts []string
	var pending []Step
	for _, name := range toolOrder {
		ts, ok := tools[name]
		if !ok {
			continue
		}
		if ts.Found && ts.VersionOK {
			doneParts = append(doneParts, toolDoneText(ts))
			continue
		}
		pending = append(pending, Step{
			ID:     StepTools,
			Title:  "tools",
			Why:    toolWhy(name),
			State:  StateNeeded,
			Detail: toolNeededDetail(ts),
		})
	}
	var steps []Step
	if len(doneParts) > 0 {
		steps = append(steps, Step{ID: StepTools, Title: "tools", State: StateDone, Detail: strings.Join(doneParts, ", ")})
	}
	return append(steps, pending...)
}

func toolDoneText(ts ToolStatus) string {
	v := ts.Version
	if v == "" {
		v = "present"
	}
	if ts.Name == "claude" {
		if ts.Authed {
			return fmt.Sprintf("%s %s (authed)", ts.Name, v)
		}
		return fmt.Sprintf("%s %s (not authed — run `claude auth login`)", ts.Name, v)
	}
	return fmt.Sprintf("%s %s", ts.Name, v)
}

func toolNeededDetail(ts ToolStatus) string {
	if ts.Found && !ts.VersionOK {
		return ts.Remediation
	}
	if ts.Plan == nil {
		return ts.Name + " not found (no install route known for this platform) — install it manually and re-run"
	}
	if ts.Plan.Route == "manual" {
		return fmt.Sprintf("%s not found → %s", ts.Name, ts.Plan.Manual)
	}
	return fmt.Sprintf("%s not found → install via %s (%s)", ts.Name, ts.Plan.Route, strings.Join(ts.Plan.Argv, " "))
}

func toolWhy(name string) string {
	switch name {
	case "git":
		return "koryph manages every project as a git worktree; without it there is no repo to adopt"
	case "claude":
		return "koryph dispatches work to the claude CLI; without it no agent can run"
	case "bd":
		return "koryph dispatches work from the beads ready-graph; without it the loop has nothing to build"
	case "gh":
		return "the GitHub CLI drives repo posture, release, and bot provisioning (optional but expected by later tracks)"
	default:
		return ""
	}
}

// --- home ----------------------------------------------------------------

func buildHomeStep() Step {
	home := paths.KoryphHome()
	// registry.d is created by Store.Init; its presence is the same signal
	// `koryph init` itself uses to decide there is nothing left to do.
	if fsx.Exists(filepath.Join(home, "registry.d")) {
		return Step{ID: StepHome, Title: "home", State: StateDone, Detail: home + " initialized"}
	}
	return Step{
		ID:     StepHome,
		Title:  "home",
		Why:    "koryph's central registry, quota state, and concurrency governor all live under ~/.koryph",
		Writes: []string{home},
		State:  StateNeeded,
		Detail: "initialize " + home,
	}
}

// --- beads -----------------------------------------------------------------

func buildBeadsStep(snap *Snapshot) Step {
	inv := snap.Inventory
	why := "koryph dispatches work from the beads ready-graph; without it the loop has nothing to build"

	if bd, ok := snap.Tools["bd"]; ok && !bd.Found {
		return Step{ID: StepBeads, Title: "beads", Why: why, State: StateBlocked, Detail: "bd (beads) is not installed — see the tools step"}
	}

	if !inv.HasBeads {
		remote := beads.DeriveSyncRemote(inv.Remote)
		detail := fmt.Sprintf("initialize issue DB (bd init --prefix %s", snap.ProjectID)
		if remote != "" {
			detail += fmt.Sprintf(" --remote %s)", remote)
		} else {
			detail += ") — no origin remote; local-only init"
		}
		return Step{ID: StepBeads, Title: "beads", Why: why, Writes: []string{".beads/"}, State: StateNeeded, Detail: detail}
	}

	if !inv.BeadsHardened {
		return Step{
			ID: StepBeads, Title: "beads", Why: why,
			Writes: []string{".beads/.gitignore"},
			State:  StateNeeded,
			Detail: "harden beads (.beads/.gitignore issues.jsonl, sync.remote, git hooks)",
		}
	}
	detail := "hardened"
	if inv.BeadsHooks {
		detail += " (+hooks)"
	}
	return Step{ID: StepBeads, Title: "beads", State: StateDone, Detail: detail}
}

// --- register --------------------------------------------------------------

func buildRegisterStep(snap *Snapshot) Step {
	if snap.ExistingRecord != nil {
		return Step{
			ID: StepRegister, Title: "register", State: StateDone,
			Detail: fmt.Sprintf("already registered as %s (account %s <%s>)",
				snap.ExistingRecord.ProjectID, snap.ExistingRecord.AccountProfile, snap.ExistingRecord.ExpectedIdentity),
		}
	}
	return Step{
		ID:     StepRegister,
		Title:  "register",
		Why:    "koryph must know which account/identity is authorized to dispatch on this project's behalf",
		Writes: []string{"~/.koryph/registry.d/" + snap.ProjectID + ".json"},
		State:  StateNeeded,
		Detail: "account " + accountProposalSummary(snap.AccountCandidates),
	}
}

// accountProposalSummary renders the account candidates for the plan's
// display text — NOT a decision (that is ResolveAccountNonInteractive's
// job); this is purely descriptive.
func accountProposalSummary(cands []account.Candidate) string {
	var verified []string
	for _, c := range cands {
		if c.Verified {
			verified = append(verified, fmt.Sprintf("%s <%s> (%s)", c.Profile.Name, c.Identity, c.Provenance))
		}
	}
	if len(verified) == 0 {
		return "no verified account candidate found — pass --account/--identity"
	}
	if len(verified) == 1 {
		return verified[0] + "; confirm"
	}
	return strings.Join(verified, ", ") + "; ambiguous — confirm one or pass --account/--identity"
}

// --- config ----------------------------------------------------------------

func buildConfigStep(snap *Snapshot) Step {
	if snap.Inventory.AdapterPresent {
		return Step{ID: StepConfig, Title: "config", State: StateDone, Detail: "existing config kept (koryph.project.json already present)"}
	}

	var parts []string
	if len(snap.GateProposals) > 0 {
		parts = append(parts, "gate: "+gateSummary(snap.GateProposals)+" (confirm)")
	} else {
		parts = append(parts, "gate: none detected — pass --gate")
	}
	if snap.ForgeProposal.Value != "" {
		parts = append(parts, fmt.Sprintf("forge: %s (%s)", snap.ForgeProposal.Value, snap.ForgeProposal.Provenance))
	} else {
		parts = append(parts, "forge: unknown ("+snap.ForgeProposal.Provenance+")")
	}
	if len(snap.AreaMap) > 0 {
		names := make([]string, 0, len(snap.AreaMap))
		for k := range snap.AreaMap {
			names = append(names, k)
		}
		sort.Strings(names)
		parts = append(parts, fmt.Sprintf("area_map: %d area(s) proposed from %s", len(names), strings.Join(names, ", ")))
	} else {
		parts = append(parts, "area_map: none proposed")
	}

	return Step{
		ID:     StepConfig,
		Title:  "config",
		Why:    "gate/forge/area_map drive dispatch safety — a wrong gate green-lights garbage (design §6)",
		Writes: []string{"koryph.project.json"},
		State:  StateNeeded,
		Detail: strings.Join(parts, "; "),
	}
}

func gateSummary(props []onboard.Proposal) string {
	vals := make([]string, len(props))
	for i, p := range props {
		vals[i] = p.Value
	}
	return strings.Join(vals, ", ")
}

// --- assets ------------------------------------------------------------------

func buildAssetsStep(snap *Snapshot) Step {
	inv := snap.Inventory
	if inv.ClaudeSettings && inv.BDPrimeHook && len(inv.Personas) > 0 {
		return Step{ID: StepAssets, Title: "assets", State: StateDone, Detail: fmt.Sprintf("%d persona(s), commands, hooks + settings.json present", len(inv.Personas))}
	}
	return Step{
		ID:    StepAssets,
		Title: "assets",
		Why:   "AGENTS.md + personas + commands + hooks make koryph semantics apply whether invoked explicitly or implied by a prompt",
		Writes: []string{
			"AGENTS.md", ".claude/agents/", ".claude/commands/", ".claude/settings.json", "hooks/",
		},
		State:  StateNeeded,
		Detail: "install AGENTS.md, agent personas, commands, hooks + settings.json (capability-gated by runtime)",
	}
}

// --- offers: signing, posture ------------------------------------------------

func buildSigningStep(snap *Snapshot) Step {
	if snap.ProjectConfig != nil && snap.ProjectConfig.Signing != nil {
		return Step{ID: StepSigning, Title: "signing", State: StateDone, Detail: "signing already configured"}
	}
	return Step{
		ID:     StepSigning,
		Title:  "signing",
		Why:    "vault-backed commit signing satisfies the merge gate's signature requirement",
		State:  StateOffer,
		Detail: "`koryph signing keygen` (no-vault path) or `koryph signing setup` (vault-backed)",
	}
}

func buildPostureStep(snap *Snapshot) Step {
	if snap.ProjectConfig != nil && snap.ProjectConfig.Posture != nil {
		return Step{ID: StepPosture, Title: "posture", State: StateDone, Detail: fmt.Sprintf("profile %q already declared", snap.ProjectConfig.Posture.Profile)}
	}
	return Step{
		ID:     StepPosture,
		Title:  "posture",
		Why:    "codifies GitHub repo hygiene (branch protection, rulesets) as desired state",
		State:  StateOffer,
		Detail: "`koryph posture apply oss-solo-maintainer` (default profile) or a named profile",
	}
}

// --- commit, verify ----------------------------------------------------------

func buildCommitStep() Step {
	return Step{
		ID:     StepCommit,
		Title:  "commit",
		Why:    "leaves the repo fully committed instead of half-onboarded",
		Writes: []string{"one commit: chore: adopt koryph"},
		State:  StateNeeded,
		Detail: "commit whatever this wizard wrote (AGENTS.md, .claude/, koryph.project.json, .beads/ tracked files, ...)",
	}
}

func buildVerifyStep(snap *Snapshot) Step {
	if snap.ExistingRecord != nil && snap.ExistingRecord.MigrationStatus == registry.StatusValidated {
		return Step{ID: StepVerify, Title: "verify", State: StateDone, Detail: "previously validated"}
	}
	return Step{
		ID:     StepVerify,
		Title:  "verify",
		Why:    "koryph validate is the pre-dispatch gate; adopt isn't done until it's green",
		State:  StateNeeded,
		Detail: "run `koryph validate` and require green",
	}
}
