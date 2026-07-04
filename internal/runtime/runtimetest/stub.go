// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package runtimetest provides a stub runtime.Runtime implementation for
// exercising the internal/runtime contract in tests (koryph-v8u.1). It lives
// in its own subpackage (rather than an _test.go file in internal/runtime)
// so other packages that later gain runtime.Runtime-shaped test needs
// (e.g. a future selection/routing package) can import it too, the same way
// httptest is importable outside net/http.
package runtimetest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/koryph/koryph/internal/runtime"
)

// Stub is a minimal, fully-functional runtime.Runtime used to prove the
// interface is implementable end-to-end and to give internal/runtime's own
// tests (and future callers') something concrete to register/dispatch
// against without shelling out to a real agent CLI. Every field has a
// working zero-value-friendly default; override only what a given test
// cares about.
type Stub struct {
	// StubName is returned by Name; defaults to "stub" when empty.
	StubName string
	// StubProvider is returned by Provider; defaults to "stub-provider" when
	// empty (deliberately NOT "anthropic" — a stub should never be mistaken
	// for sharing Claude's governor pool).
	StubProvider string
	// Present/Version are returned verbatim by Detect.
	Present bool
	Version string
	// AuthErr is returned verbatim by AuthCheck (nil means "authenticated").
	AuthErr error
	// VerifyErr, when non-nil, is returned verbatim by VerifyIdentity (koryph
	// -v8u.5) — the knob a test uses to simulate a failing fail-closed
	// identity gate (e.g. "not logged in", "identity mismatch") independently
	// of AuthErr, mirroring how claude's VerifyIdentity/AuthCheck are two
	// distinct real checks (see runtime.Runtime.VerifyIdentity's doc).
	VerifyErr error
	// VerifiedGot is the identity VerifyIdentity returns on success (when
	// VerifyErr is nil); "" defaults to echoing back the caller's expected
	// identity, so a test that only cares about the fail-closed path need not
	// set this.
	VerifiedGot string
	// Caps is returned verbatim by Capabilities.
	Caps runtime.Capabilities
	// Instruction is returned by InstructionFile; defaults to "AGENTS.md"
	// when empty, matching the epic's cross-runtime convention.
	Instruction string
	// Models is returned verbatim by ModelMap (koryph-v8u.10); nil (the
	// zero value) means "this stub declares no tier mapping", exercising
	// the same missing-key fallback a real adapter's caller must handle.
	Models runtime.ModelMap
}

// Name implements runtime.Runtime.
func (s Stub) Name() string {
	if s.StubName == "" {
		return "stub"
	}
	return s.StubName
}

// Provider implements runtime.Runtime.
func (s Stub) Provider() string {
	if s.StubProvider == "" {
		return "stub-provider"
	}
	return s.StubProvider
}

// Detect implements runtime.Runtime.
func (s Stub) Detect(_ context.Context) (bool, string) {
	return s.Present, s.Version
}

// AuthCheck implements runtime.Runtime.
func (s Stub) AuthCheck(_ context.Context, _ runtime.Profile) error {
	return s.AuthErr
}

// VerifyIdentity implements runtime.Runtime (koryph-v8u.5).
func (s Stub) VerifyIdentity(_ context.Context, _ runtime.Profile, expected string) (string, error) {
	if s.VerifyErr != nil {
		return "", s.VerifyErr
	}
	if s.VerifiedGot != "" {
		return s.VerifiedGot, nil
	}
	return expected, nil
}

// Capabilities implements runtime.Runtime.
func (s Stub) Capabilities() runtime.Capabilities {
	return s.Caps
}

// InstructionFile implements runtime.Runtime.
func (s Stub) InstructionFile() string {
	if s.Instruction == "" {
		return "AGENTS.md"
	}
	return s.Instruction
}

// ModelMap implements runtime.Runtime.
func (s Stub) ModelMap() runtime.ModelMap {
	return s.Models
}

// AccountEnv implements runtime.Runtime. It mirrors the shape (not the
// value) of internal/account.ChildEnv's CLAUDE_CONFIG_DIR handling: a
// non-empty profile.ConfigDir contributes one STUB_CONFIG_DIR=<dir> entry.
func (s Stub) AccountEnv(profile runtime.Profile) []string {
	if profile.ConfigDir == "" {
		return nil
	}
	return []string{"STUB_CONFIG_DIR=" + profile.ConfigDir}
}

// Command implements runtime.Runtime. It builds a small, deterministic argv
// so tests can assert on exact output, and enforces the same
// capability-gating contract a real adapter must: a spec field that maps to
// an unsupported capability is a hard error, never a silent drop.
func (s Stub) Command(spec runtime.DispatchSpec) ([]string, []string, error) {
	caps := s.Caps
	if spec.ResumeSessionID != "" && !caps.Resume {
		return nil, nil, fmt.Errorf("stub: ResumeSessionID set but Capabilities.Resume is false")
	}
	if spec.Effort != "" && !caps.EffortFlag {
		return nil, nil, fmt.Errorf("stub: Effort set but Capabilities.EffortFlag is false")
	}
	if spec.MaxBudgetUSD > 0 && !caps.BudgetFlag {
		return nil, nil, fmt.Errorf("stub: MaxBudgetUSD set but Capabilities.BudgetFlag is false")
	}
	if spec.Model != "" && !caps.ModelSelect {
		return nil, nil, fmt.Errorf("stub: Model set but Capabilities.ModelSelect is false")
	}
	if spec.Persona != "" && !caps.Personas {
		return nil, nil, fmt.Errorf("stub: Persona set but Capabilities.Personas is false")
	}

	argv := []string{s.Name(), "run", "--session-id", spec.SessionID}
	if spec.Persona != "" {
		argv = append(argv, "--persona", spec.Persona)
	}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" {
		argv = append(argv, "--effort", spec.Effort)
	}
	if spec.ResumeSessionID != "" {
		argv = append(argv, "--resume", spec.ResumeSessionID)
	}

	env := append([]string{}, spec.EnvPassthrough...)
	env = append(env, s.AccountEnv(spec.Profile)...)
	if spec.Billing == runtime.BillingAPIKey && spec.APIKey != "" {
		env = append(env, "STUB_API_KEY="+spec.APIKey)
	}
	return argv, env, nil
}

// nativeLine is the stub's fake native stream format: deliberately shaped
// like the subset of Claude's stream-json that
// internal/dispatch/cli.go's ParseResultCost/ParseRateLimited scan (type,
// total_cost_usd, is_error, error.type), so ParseEvents below demonstrates
// the same normalization those functions perform, per-runtime.
type nativeLine struct {
	Type         string   `json:"type"`
	SessionID    string   `json:"session_id,omitempty"`
	TotalCostUSD *float64 `json:"total_cost_usd,omitempty"`
	IsError      *bool    `json:"is_error,omitempty"`
	Error        *struct {
		Type string `json:"type,omitempty"`
	} `json:"error,omitempty"`
}

var rateLimitMarkers = []string{"429", "rate_limit_error", "overloaded_error"}

// ParseEvents implements runtime.Runtime, normalizing nativeLine records
// (newline-delimited JSON) into runtime.Event.
func (s Stub) ParseEvents(r io.Reader) (runtime.EventStream, error) {
	return &stubEventStream{sc: bufio.NewScanner(r)}, nil
}

// stubEventStream implements runtime.EventStream over nativeLine records.
type stubEventStream struct {
	sc *bufio.Scanner
}

// Next implements runtime.EventStream.
func (e *stubEventStream) Next() (runtime.Event, bool, error) {
	for e.sc.Scan() {
		line := bytes.TrimSpace(e.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec nativeLine
		if err := json.Unmarshal(line, &rec); err != nil {
			// Tolerant of malformed lines, matching dispatch/cli.go's
			// scanning style: skip and keep reading.
			continue
		}
		raw := json.RawMessage(append([]byte(nil), line...))

		switch {
		case rec.Type == "session":
			return runtime.Event{Kind: runtime.EventSession, SessionID: rec.SessionID, Raw: raw}, true, nil

		case rec.Type == "result":
			ev := runtime.Event{Kind: runtime.EventResult, Raw: raw}
			if rec.TotalCostUSD != nil {
				ev.CostUSD, ev.HasCost = *rec.TotalCostUSD, true
			}
			return ev, true, nil

		case rec.Type == "error" || rec.Error != nil || (rec.IsError != nil && *rec.IsError):
			haystack := strings.ToLower(string(line))
			rl := false
			for _, marker := range rateLimitMarkers {
				if strings.Contains(haystack, marker) {
					rl = true
					break
				}
			}
			return runtime.Event{Kind: runtime.EventError, RateLimited: rl, Raw: raw}, true, nil

		default:
			return runtime.Event{Kind: runtime.EventOpaque, Raw: raw}, true, nil
		}
	}
	if err := e.sc.Err(); err != nil {
		return runtime.Event{}, false, err
	}
	return runtime.Event{}, false, nil
}

// Close implements runtime.EventStream. The stub holds no closable
// resources of its own.
func (e *stubEventStream) Close() error { return nil }

var _ runtime.Runtime = Stub{}
