// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

import (
	"context"
	"io"
)

// Runtime is one pluggable agent CLI (claude, codex, cursor-agent, grok,
// gemini, copilot, opencode, ...) as koryph needs to drive it headlessly
// (koryph-v8u.1, docs/designs/2026-07-enhancement-roadmap.md section B). An
// implementation owns exactly the translation between koryph's
// runtime-neutral request/response shapes (DispatchSpec, Event) and that
// CLI's actual argv/env/stdout contract; it owns NO scheduling, governor, or
// ledger logic — those stay in the engine and call through this interface.
//
// Every method must be safe to call from multiple goroutines: the engine
// dispatches concurrently across projects and phases, and a Runtime value is
// expected to be a stateless (or internally-synchronized) singleton
// registered once in a Registry.
type Runtime interface {
	// Name returns the runtime's stable identifier (e.g. "claude", "codex",
	// "cursor", "grok", "gemini", "copilot", "opencode"). It is the Registry
	// key, the value stored in a bead's `runtime:<name>` label, and the
	// `runtimes:{}` key in koryph.project.json — so it must never change
	// once shipped (treat a rename as a breaking migration).
	Name() string

	// Provider returns the governor pool key this runtime's API calls are
	// billed/rate-limited against (koryph-v8u.11): "anthropic" for claude,
	// "openai" for codex, "google" for gemini, "xai" for grok. It is
	// deliberately a plain string, not an enum shared with internal/govern,
	// to keep this package import-free of internal/govern; callers pass it
	// straight through to govern's pool-keyed entry points (see
	// govern.DefaultPool == "anthropic", the value the claude adapter must
	// return). A runtime backed by an unfamiliar or self-hosted provider may
	// return any non-empty opaque string; govern.NormalizeProvider treats
	// only "" as "use the default pool", so Provider must never return "".
	Provider() string

	// Detect reports whether this runtime's CLI binary is available on the
	// current machine (PATH lookup or configured path) and, if so, its
	// reported version string (e.g. via `--version`). present is false when
	// the binary cannot be found; version is "" whenever present is false or
	// the version could not be parsed out of the binary's own output —
	// Detect never returns an error for "not installed", since that is an
	// expected, common outcome (koryph doctor and onboarding both probe
	// every registered runtime unconditionally). Detect MUST NOT burn any
	// API quota or require network access; it is a local, cheap probe only.
	Detect(ctx context.Context) (present bool, version string)

	// AuthCheck verifies the given profile is authenticated with this
	// runtime's backing service WITHOUT spending API quota (e.g. reading a
	// local credentials/config file rather than making a billed call). It
	// returns a non-nil, human-readable error describing exactly what is
	// missing or mismatched (e.g. "not logged in", "config dir has no
	// oauthAccount") whenever the profile cannot be confirmed authenticated;
	// a nil error means the profile is ready for Command/AccountEnv.
	// AuthCheck must fail closed: any ambiguity (file unreadable, unexpected
	// shape) is an error, never a silent pass.
	AuthCheck(ctx context.Context, profile Profile) error

	// Capabilities reports which optional CLI features this runtime
	// supports, so the engine can gate spec fields and behavior generically
	// (see Capabilities' own doc). The returned value is treated as static
	// for the lifetime of the process — a Runtime whose capabilities
	// legitimately vary by installed version should resolve that once, at
	// Detect/registration time, not per call.
	Capabilities() Capabilities

	// Command translates a runtime-neutral DispatchSpec into this runtime's
	// concrete argv and env, for the caller to exec (or embed in an
	// inspectable launch.sh, as internal/dispatch/cli.go does today). argv
	// is the full command line INCLUDING argv[0] (the binary name/path);
	// env is a set of "KEY=VALUE" strings to be layered onto (not replacing)
	// the process's base environment — see AccountEnv for the
	// account/profile-scoped subset of env. Command returns an error when
	// the spec requests a capability this runtime does not support (e.g.
	// ResumeSessionID set but Capabilities().Resume is false) rather than
	// silently dropping the field — callers should gate unsupported fields
	// before calling Command when a soft degrade is preferred over a hard
	// error.
	Command(spec DispatchSpec) (argv []string, env []string, err error)

	// ParseEvents adapts this runtime's native streaming output format
	// (Claude stream-json, a Codex JSONL transcript, ...) into the
	// normalized Event envelope the engine consumes for cost/rate-limit/
	// session-id extraction (see events.go). The returned EventStream reads
	// lazily from r — ParseEvents itself must not block or read r to
	// completion — so callers can attach it to a live, still-growing
	// stream.jsonl the way the engine tails a running agent's output today.
	ParseEvents(r io.Reader) (EventStream, error)

	// InstructionFile names the per-worktree instruction file this runtime
	// reads natively (CLAUDE.md, AGENTS.md, GEMINI.md, ...), relative to the
	// worktree root. Multiple runtimes may name the same file (AGENTS.md has
	// effectively won as the cross-runtime convention per the epic's
	// research verdict) — koryph's installer is expected to write/symlink
	// whichever files the project's configured runtimes collectively need.
	InstructionFile() string

	// AccountEnv returns the "KEY=VALUE" env vars that select the given
	// profile's account/config (e.g. CLAUDE_CONFIG_DIR, CODEX_HOME,
	// CURSOR_API_KEY) — the runtime-scoped counterpart of
	// internal/account.ChildEnv's CLAUDE_CONFIG_DIR handling. An empty
	// Profile (zero value) returns the env for this runtime's default/
	// personal account, which may be an empty slice when the runtime needs
	// no env to select its default account.
	AccountEnv(profile Profile) []string
}

// Profile is the runtime-neutral account identity a Runtime uses to select
// config-dir/env for AuthCheck and AccountEnv. It mirrors
// internal/account.Profile (Name, ConfigDir) field-for-field — see the
// package doc's import-boundary rationale for why this is a local type
// rather than a reuse of account.Profile.
type Profile struct {
	// Name is the registry profile identifier (e.g. "personal", "work", or a
	// project-specific custom name); "" means the default profile.
	Name string
	// ConfigDir is the runtime-specific config directory for this profile
	// ("" means the runtime's own default, e.g. $HOME-relative).
	ConfigDir string
}

// BillingMode mirrors internal/account.BillingMode's two values as local
// constants (see the package doc's import-boundary rationale). Not every
// runtime distinguishes subscription vs. API-key billing the way Claude
// does; a runtime for which the distinction is meaningless may simply ignore
// DispatchSpec.Billing.
type BillingMode string

const (
	// BillingSubscription mirrors account.BillingSubscription.
	BillingSubscription BillingMode = "subscription"
	// BillingAPIKey mirrors account.BillingAPIKey.
	BillingAPIKey BillingMode = "api-key"
)

// Capabilities reports which optional CLI features a Runtime supports. It is
// a struct of booleans rather than a bit-flag integer: Capabilities values
// are constructed once per runtime (not on a hot path), are far more useful
// in a debugger/log/JSON dump as named fields than as an opaque bitmask, and
// a struct's zero value ("supports nothing") is exactly the safe default for
// a Runtime stub or an unrecognized future runtime — no flag arithmetic
// required to read or construct one. Every field defaults to false, so
// json.Unmarshal of an old/partial capabilities document degrades to "this
// capability is unsupported" rather than panicking or lying.
type Capabilities struct {
	// JSONStream: the runtime can emit a structured, line-delimited event
	// stream (Claude's --output-format stream-json) rather than only plain
	// text — required for ParseEvents to produce anything but opaque
	// passthrough events.
	JSONStream bool `json:"json_stream"`
	// Personas: the runtime supports named agent personas/subagent
	// definitions (Claude's --agent + .claude/agents) distinct from a raw
	// system prompt.
	Personas bool `json:"personas"`
	// Hooks: the runtime supports lifecycle hooks (Claude's
	// settings.json hooks + CLAUDE_PROJECT_DIR) for containment/guard
	// scripts. Runtimes without this rely on worktree isolation +
	// merge-time protected-path refusal instead (documented trust delta,
	// see the epic's design doc).
	Hooks bool `json:"hooks"`
	// Resume: the runtime supports resuming a prior session by id
	// (DispatchSpec.ResumeSessionID); Command must error if this is false
	// and ResumeSessionID is set.
	Resume bool `json:"resume"`
	// EffortFlag: the runtime accepts a reasoning-effort selector
	// (DispatchSpec.Effort, e.g. Claude's --effort).
	EffortFlag bool `json:"effort_flag"`
	// BudgetFlag: the runtime accepts a hard spend cap per invocation
	// (DispatchSpec.MaxBudgetUSD, e.g. Claude's --max-budget-usd).
	BudgetFlag bool `json:"budget_flag"`
	// Sandbox: the runtime has its own filesystem/network sandboxing flag
	// (e.g. Codex's --sandbox workspace-write) independent of worktree
	// isolation.
	Sandbox bool `json:"sandbox"`
	// ModelSelect: the runtime accepts an explicit model/tier selector
	// (DispatchSpec.Model, e.g. Claude's --model).
	ModelSelect bool `json:"model_select"`
}
