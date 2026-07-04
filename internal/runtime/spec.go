// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

// DispatchSpec fully describes one runtime-neutral dispatch request. It is a
// deliberate field-for-field mirror of internal/dispatch.Spec, minus the
// fields that package derives/hardcodes rather than reads (see the package
// doc's mapping table) — the intent is that koryph-v8u.2's Claude adapter
// extraction can convert dispatch.Spec <-> DispatchSpec with a flat struct
// literal, and no logic changes.
//
// Fields gated by a Capabilities flag are honored by Command only when the
// target Runtime reports that capability; Command must return an error
// (never a silent no-op) when a gated field is set on a Runtime that does
// not support it — see Runtime.Command's doc comment.
type DispatchSpec struct {
	ProjectID string
	RepoRoot  string
	RunID     string
	PhaseID   string // bead id
	PhaseDir  string // <run>/<phase>/
	Worktree  string
	Branch    string

	// Persona is gated by Capabilities.Personas.
	Persona string
	// Model is gated by Capabilities.ModelSelect.
	Model string
	// Effort is gated by Capabilities.EffortFlag.
	Effort string

	Profile          Profile
	ExpectedIdentity string
	Billing          BillingMode
	APIKey           string // resolved key when Billing==BillingAPIKey; never logged

	// MaxBudgetUSD is gated by Capabilities.BudgetFlag.
	MaxBudgetUSD float64
	Prompt       string
	SessionID    string // fresh uuid (deterministic transcript path)
	SessionName  string
	// ResumeSessionID is gated by Capabilities.Resume.
	ResumeSessionID string
	BeadsDir        string
	Attempt         int

	// SSHAuthSock is the koryph-managed signing-agent socket (holds ONLY the
	// signing key), injected so agent commits sign without the operator's
	// ambient socket (and its other keys) ever reaching the agent. Empty
	// when signing is not required.
	SSHAuthSock string
	// EnvPassthrough forwards extra operator env vars into the agent (the
	// registry-declared escape hatch for projects that genuinely need one).
	EnvPassthrough []string
}
