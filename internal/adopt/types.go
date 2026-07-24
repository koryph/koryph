// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package adopt implements the `koryph adopt` wizard's orchestration:
// detect -> plan -> confirm -> execute -> verify (docs/designs/2026-07-adopt.md
// §3). It is the koryphization tail promoted to a standalone front door for
// existing repos — sequencing the same primitives `koryph project add`
// already uses (internal/onboard, internal/beads, internal/sysdeps,
// internal/account) rather than reimplementing any of them.
//
// Package split: this package holds the orchestration LOGIC — detection,
// plan construction, the non-interactive fail-closed resolvers, and the
// mutating execute-phase helpers. cmd/koryph/adopt.go holds the CLI-facing
// glue: flag parsing, interactive prompts (stdin/stderr), streaming
// stdout/JSON output, and sequencing the calls into this package.
package adopt

import (
	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/sysdeps"
)

// StepState is where one adoption step stands relative to execution.
type StepState string

const (
	StateDone    StepState = "done"    // already satisfied; execute skips it
	StateNeeded  StepState = "needed"  // required; execute will act on consent
	StateOffer   StepState = "offer"   // optional, default-off (signing, posture)
	StateBlocked StepState = "blocked" // a system-scope consent was declined or failed
)

// StepID names one of the fixed adoption steps, in plan/execute order
// (docs/designs/2026-07-adopt.md §3.2/§3.4).
type StepID string

const (
	StepTools    StepID = "tools"
	StepHome     StepID = "home"
	StepBeads    StepID = "beads"
	StepRegister StepID = "register"
	StepConfig   StepID = "config"
	StepAssets   StepID = "assets"
	StepSigning  StepID = "signing"
	StepPosture  StepID = "posture"
	StepCommit   StepID = "commit"
	StepVerify   StepID = "verify"
)

// Step is one row of the printed adoption plan (design §3.2): what it does,
// why (plain language, shown for a `needed` step), what it writes, and its
// current state. The "tools" category may contribute several Steps — one
// aggregated `done` row plus one row per tool still needing action — every
// other category contributes exactly one.
type Step struct {
	ID     StepID    `json:"id"`
	Title  string    `json:"title"`
	Why    string    `json:"why,omitempty"`
	Writes []string  `json:"writes,omitempty"`
	State  StepState `json:"state"`
	Detail string    `json:"detail,omitempty"`
}

// ToolStatus is one external binary's detected state, plus (when relevant)
// the sysdeps install plan a `needed`/`offer` tools row would execute.
type ToolStatus struct {
	Name      string `json:"name"`
	Found     bool   `json:"found"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	VersionOK bool   `json:"version_ok"`
	Authed    bool   `json:"authed,omitempty"` // set when this runtime's local auth was verified
	// Plan is the install route for this tool, nil when the tool is already
	// satisfied or (git) has no sysdeps route at all.
	Plan *sysdeps.InstallPlan `json:"plan,omitempty"`
	// Remediation carries a non-install remedy (e.g. bd found but older than
	// beads.MinVersion — an upgrade, not a fresh install).
	Remediation string `json:"remediation,omitempty"`
}

// Snapshot is the read-only result of the detect phase (design §3.1).
// Nothing that produces a Snapshot writes anywhere; BuildPlan, the confirm
// resolvers, and the execute-phase helpers all consume it as pure input.
type Snapshot struct {
	Root string `json:"root"`
	// ProjectID is the PREDICTED project slug (same algorithm
	// onboard.Register falls back to), shown in the plan header and used as
	// the beads --prefix / registration id unless overridden by --id. It is
	// always non-empty so the plan header and the eventual registration can
	// never disagree.
	ProjectID string `json:"project_id"`

	Platform        sysdeps.Platform `json:"platform"`
	FlakeNixPresent bool             `json:"flake_nix_present"`

	// RuntimeName selects the runtime being adopted. Empty means Claude for
	// backward compatibility; it is intentionally independent of what other
	// binaries happen to be installed on the machine.
	RuntimeName string `json:"runtime_name,omitempty"`
	// Tools is keyed by tool name: "git", runtime CLIs, "bd", and "gh".
	Tools map[string]ToolStatus `json:"tools"`

	Inventory *onboard.Inventory `json:"inventory"`

	AccountCandidates []account.Candidate `json:"account_candidates,omitempty"`

	ForgeProposal     onboard.Proposal    `json:"forge_proposal"`
	GateProposals     []onboard.Proposal  `json:"gate_proposals,omitempty"`
	AreaMap           map[string][]string `json:"area_map,omitempty"`
	AreaMapProvenance string              `json:"area_map_provenance,omitempty"`

	// ExistingRecord is the registry record for Root, nil when unregistered.
	ExistingRecord *registry.Record `json:"existing_record,omitempty"`
	// ProjectConfig is the loaded koryph.project.json, nil when absent or
	// unreadable (Inventory.AdapterPresent is the authoritative presence
	// flag; a present-but-unreadable file still leaves this nil).
	ProjectConfig *project.Config `json:"project_config,omitempty"`
}
