// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package runtime defines the pluggable agent-runtime contract (koryph-v8u.1,
// docs/designs/2026-07-enhancement-roadmap.md section B "Pluggable agent
// runtime"). It is phase (1) of that epic: interface + capability flags +
// normalized event envelope + registry, as a PURE ADDITION — nothing in
// internal/dispatch, internal/review, internal/stage, or internal/engine is
// modified or moved here. Claude stays hard-wired at its three existing call
// sites until koryph-v8u.2 extracts a Claude Runtime adapter
// behavior-identically.
//
// # Why this package imports nothing from the packages it describes
//
// internal/dispatch.Spec, internal/account.Profile/BillingMode, and the
// stream-json parsing in internal/dispatch/cli.go (ParseResultCost,
// ParseRateLimited) are the CONCRETE, Claude-shaped types this contract must
// eventually describe in a runtime-neutral way. Importing them here would
// wire this contract package to Claude's shape before a second adapter
// (Codex, per the epic's phasing) ever exists, and would risk an import
// cycle once internal/dispatch starts depending on internal/runtime
// (koryph-v8u.2). So every type below is a small, local mirror of the
// corresponding dispatch/account field set, with the mapping documented in
// the type's doc comment rather than expressed as a shared Go type. When
// koryph-v8u.2 extracts the Claude adapter, that CL is expected to convert
// dispatch.Spec <-> runtime.DispatchSpec at the boundary.
//
// # Field mapping: dispatch.Spec -> runtime.DispatchSpec
//
//	dispatch.Spec field    runtime.DispatchSpec field   notes
//	ProjectID               ProjectID                    verbatim
//	RepoRoot                RepoRoot                     verbatim
//	RunID                   RunID                        verbatim
//	PhaseID                 PhaseID                      verbatim
//	PhaseDir                PhaseDir                      verbatim
//	Worktree                Worktree                     verbatim
//	Branch                  Branch                       verbatim
//	Persona                 Persona                      gated by Capabilities.Personas
//	Model                   Model                        gated by Capabilities.ModelSelect
//	Effort                  Effort                       gated by Capabilities.EffortFlag
//	Profile (account.Profile) Profile (runtime.Profile)  field-for-field mirror, see Profile
//	ExpectedIdentity        ExpectedIdentity             verbatim
//	Billing (account.BillingMode) Billing (runtime.BillingMode) mirrored constants, see BillingMode
//	APIKey                  APIKey                       verbatim; never logged, same invariant
//	MaxBudgetUSD            MaxBudgetUSD                 gated by Capabilities.BudgetFlag
//	Prompt                  Prompt                       verbatim
//	SessionID               SessionID                    verbatim
//	SessionName             SessionName                  verbatim
//	ResumeSessionID         ResumeSessionID              gated by Capabilities.Resume
//	BeadsDir                BeadsDir                     verbatim
//	Attempt                 Attempt                      verbatim
//	SSHAuthSock             SSHAuthSock                  verbatim
//	EnvPassthrough          EnvPassthrough               verbatim
//
// Fields NOT mirrored are the ones cli.go itself derives/hardcodes rather
// than reading from Spec (e.g. --permission-mode dontAsk, --output-format
// stream-json, --fallback-model sonnet, --add-dir <phaseDir>) — those become
// each Runtime.Command implementation's own business, not part of the
// runtime-neutral request.
//
// # Normalized event envelope
//
// internal/dispatch/cli.go today extracts exactly three signals from a raw
// Claude stream-json transcript: the terminal result's total_cost_usd
// (ParseResultCost), a rate-limit/overload marker inside an error-flagged
// event (ParseRateLimited), and (elsewhere) the session id used for
// --resume. Event and EventKind normalize precisely those three signals plus
// an opaque passthrough for everything else (text/tool events the engine
// does not currently parse) — see events.go for the full rationale.
package runtime
