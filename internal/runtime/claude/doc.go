// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package claude implements runtime.Runtime for the claude CLI — the first
// real adapter (koryph-v8u.2, docs/designs/2026-07-enhancement-roadmap.md
// section B, phase (1): "interface + Claude adapter extraction (behavior-
// identical, refactor-core)"). It is a PURE EXTRACTION: every argv/env/parse
// decision here is copied byte-for-byte from the one place that built a
// dispatch-shaped claude invocation before this bead —
// internal/dispatch/cli.go's launch.sh argv — and from ParseResultCost/
// ParseRateLimited's stream-json line-scanning logic. Nothing about the
// CLI's actual behavior changes; only WHERE that logic lives.
//
// # Placement: why internal/runtime/claude, not internal/dispatch
//
// This adapter needs to import internal/account (account.Profile,
// account.BillingMode, account.ChildEnv, account.Verify) to be the single
// source of truth for env construction and auth checks — reusing
// account.ChildEnv rather than re-implementing its allowlist logic here.
// internal/runtime itself deliberately does NOT import internal/account (see
// runtime's own package doc): it must stay neutral so a second adapter
// (Codex, per the epic's phasing) never has to fight Claude's shape. A
// subpackage does not have that constraint — internal/runtime/claude is free
// to import internal/account precisely because it is the Claude-specific
// translation layer runtime's package doc already anticipates ("When
// koryph-v8u.2 extracts the Claude adapter, that CL is expected to convert
// dispatch.Spec <-> runtime.DispatchSpec at the boundary").
//
// Putting this package under internal/dispatch instead was rejected: two of
// the three original call sites (internal/review, internal/stage) do not
// import internal/dispatch's Spec/Handle types at all, and a "claude adapter
// living inside the dispatch package" would force them to newly depend on
// dispatch just to reach argv-building helpers that have nothing to do with
// dispatch's launch.sh/detached-process contract. Housing the adapter as a
// runtime subpackage lets all interested packages (and, transitively, the
// Registry) depend on ONE neutral location without inventing a dependency
// direction that only makes sense for one of the three call sites.
//
// # What this bead actually unifies, and what it deliberately does not
//
// internal/dispatch.CLIBackend.Dispatch's launch.sh argv (-p --agent
// --session-id --permission-mode dontAsk --model ... --output-format
// stream-json --include-partial-messages --verbose, with --resume/--name/
// --effort/--max-budget-usd gated) is the ONE shape runtime.DispatchSpec
// models (session id, resume, phase-dir add-dir, streaming output) — see
// Command. That shape becomes this package's single source of truth;
// internal/dispatch now calls Command instead of building argv inline.
//
// internal/review.Review and internal/stage.Run build a GENUINELY DIFFERENT
// shape: a synchronous one-shot exec via internal/execx (not launch.sh), a
// single JSON result envelope (not a stream), no --session-id/--add-dir/
// --include-partial-messages, and (review only) --permission-mode plan
// instead of dontAsk. runtime.DispatchSpec/Command, as landed by
// koryph-v8u.1, models the dispatch shape only — it has no field for "one-
// shot json, no session" versus "streaming, resumable, session-scoped", and
// Capabilities has no flag for "plan mode" versus "dontAsk mode". Bending
// review/stage's calls through Command would require either inventing new
// interface surface (out of scope for a bead whose job is a behavior-
// identical extraction of what already exists) or silently mis-describing
// their invocation as the dispatch shape. So review.go and stage.go are left
// building their own argv, UNCHANGED, except for one literal: stage.go's
// argv now reads FallbackModel instead of duplicating the "sonnet" string —
// the one piece of literal duplication the koryph-v8u.2 architecture review
// flagged as trivially unifiable without touching behavior. ParseResultCost's
// scanning logic — which stage.go already reuses against its own single-
// object JSON envelope, "documented ... works only by accident of line-
// tolerant parsing" per that same review — is left exactly as tolerant as
// before; this bead does not change what counts as a parseable result line,
// only where the scan lives (see events.go).
//
// # Registration
//
// This package's init (register.go) adds the default-binary adapter to
// runtime.Default — the registry's first real entry. Nothing consults the
// registry for dispatch decisions yet (that is koryph-v8u.3's job); the
// registration exists so `koryph doctor`-style enumeration and this
// package's own tests have something real to look up.
package claude
