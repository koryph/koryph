// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/claude"
)

// CLIBackend dispatches agents by shelling out to the claude CLI via a
// generated launch.sh, detached from the koryph process.
type CLIBackend struct {
	ClaudeBin string // path or name of the claude binary; default "claude"

	// Runtime is the resolved runtime.Runtime adapter Dispatch uses for
	// identity verification (VerifyIdentity) and argv/env construction
	// (Command) — koryph-v8u.5. nil (the default, and every existing call
	// site's zero value) resolves to claude.New(ClaudeBin), preserving every
	// pre-v8u.5 behavior byte-for-byte. Real per-project/per-bead runtime
	// SELECTION (bead runtime:<name> label, project default_runtime) is
	// koryph-v8u.3's job; this field exists so tests (and, eventually, that
	// selection logic) can inject a non-claude runtime.Runtime without
	// CLIBackend growing runtime-specific branches of its own.
	Runtime runtime.Runtime
}

// resolvedRuntime returns b.Runtime, defaulting to the claude adapter built
// from b.ClaudeBin. See CLIBackend.Runtime's doc for why this default exists.
func (b CLIBackend) resolvedRuntime() runtime.Runtime {
	if b.Runtime != nil {
		return b.Runtime
	}
	return claude.New(b.ClaudeBin)
}

// statusSeed is the initial status.json contract document.
type statusSeed struct {
	PhaseID   string `json:"phase_id"`
	State     string `json:"state"`
	Step      string `json:"step"`
	Pct       int    `json:"pct"`
	UpdatedAt string `json:"updated_at"`
}

// Dispatch launches one work item per the package contract:
// verify identity (fail closed, BEFORE any file writes), seed the phase
// directory, build an inspectable launch.sh, and start it detached.
func (b CLIBackend) Dispatch(ctx context.Context, s Spec) (Handle, error) {
	rt := b.resolvedRuntime()

	// 1. Identity + billing verification — before any filesystem effect.
	// VerifyIdentity is the runtime-generic seam (koryph-v8u.5); the default
	// claude adapter delegates to account.VerifyExpected, so this is the
	// SAME fail-closed check and error text as before this bead.
	identity, err := rt.VerifyIdentity(ctx, toRuntimeProfile(s.Profile), s.ExpectedIdentity)
	if err != nil {
		log.Warn("dispatch.identity.failed",
			slog.String(obs.KeyPhase, s.PhaseID),
			slog.String(obs.KeyProject, s.ProjectID),
			obs.Err(err),
		)
		return Handle{}, err
	}
	log.Info("dispatch.identity.verified",
		slog.String(obs.KeyPhase, s.PhaseID),
		slog.String(obs.KeyProject, s.ProjectID),
		slog.String("result", "ok"),
	)
	if !s.Billing.Valid() {
		return Handle{}, fmt.Errorf("dispatch %s: invalid billing mode %q (want %q or %q)", s.PhaseID, s.Billing, account.BillingSubscription, account.BillingAPIKey)
	}
	if s.Billing == account.BillingAPIKey && s.APIKey == "" {
		return Handle{}, fmt.Errorf("dispatch %s: billing mode %q requires a resolved API key — refusing dispatch", s.PhaseID, account.BillingAPIKey)
	}

	claudeBin := b.ClaudeBin
	if claudeBin == "" {
		claudeBin = "claude"
	}

	// 2. Single-quote guard on every value embedded in launch.sh —
	//    still before any file writes.
	quoted := map[string]string{
		"worktree":          s.Worktree,
		"phase dir":         s.PhaseDir,
		"claude binary":     claudeBin,
		"persona":           s.Persona,
		"model":             s.Model,
		"effort":            s.Effort,
		"run id":            s.RunID,
		"phase id":          s.PhaseID,
		"session id":        s.SessionID,
		"session name":      s.SessionName,
		"resume session id": s.ResumeSessionID,
		"beads dir":         s.BeadsDir,
	}
	for what, v := range quoted {
		if strings.ContainsRune(v, '\'') {
			return Handle{}, fmt.Errorf("dispatch %s: %s %q contains a single quote — refusing to build launch.sh", s.PhaseID, what, v)
		}
	}

	// 3. Seed the phase directory.
	if len(s.EnvPassthrough) > 0 {
		names := make([]string, len(s.EnvPassthrough))
		copy(names, s.EnvPassthrough)
		log.Log(context.TODO(), obs.LevelTrace, "dispatch.env.passthrough",
			slog.String(obs.KeyPhase, s.PhaseID),
			slog.Any("var_names", names),
		)
	}
	if err := os.MkdirAll(s.PhaseDir, 0o755); err != nil {
		return Handle{}, fmt.Errorf("dispatch %s: creating phase dir: %w", s.PhaseID, err)
	}
	promptPath := filepath.Join(s.PhaseDir, "prompt.md")
	if err := fsx.WriteAtomic(promptPath, []byte(s.Prompt), 0o644); err != nil {
		return Handle{}, fmt.Errorf("dispatch %s: writing prompt.md: %w", s.PhaseID, err)
	}
	statusPath := filepath.Join(s.PhaseDir, "status.json")
	seed := statusSeed{
		PhaseID:   s.PhaseID,
		State:     "queued",
		Step:      "dispatched",
		Pct:       0,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := fsx.WriteJSONAtomic(statusPath, seed); err != nil {
		return Handle{}, fmt.Errorf("dispatch %s: seeding status.json: %w", s.PhaseID, err)
	}
	inboxPath := filepath.Join(s.PhaseDir, "INBOX.md")
	if !fsx.Exists(inboxPath) {
		if err := fsx.WriteAtomic(inboxPath, []byte("(operator nudges appear here)\n"), 0o644); err != nil {
			return Handle{}, fmt.Errorf("dispatch %s: writing INBOX.md: %w", s.PhaseID, err)
		}
	}

	// 4. Build launch.sh (inspectable artifact). argv/env are the claude
	//    adapter's Command output (koryph-v8u.2) — this package no longer
	//    decides the flag sequence or child-env construction itself; it only
	//    embeds the result in the shell-script/detached-process shape that
	//    is dispatch's own concern, not the Runtime interface's.
	logPath := filepath.Join(s.PhaseDir, "session.log")
	summaryPath := filepath.Join(s.PhaseDir, "SUMMARY.md")
	launchPath := filepath.Join(s.PhaseDir, "launch.sh")
	streamPath := filepath.Join(s.PhaseDir, "stream.jsonl")
	stderrPath := filepath.Join(s.PhaseDir, "stderr.log")

	argv, env, err := rt.Command(toRuntimeSpec(s))
	if err != nil {
		return Handle{}, fmt.Errorf("dispatch %s: building claude command: %w", s.PhaseID, err)
	}

	// KORYPH_HOME is exported (resolved absolute) so the guard hooks registered
	// in .claude/settings.json as ${KORYPH_HOME:-$HOME/.koryph}/hooks/*.sh
	// resolve to the central, agent-unwritable copy — never the worktree.
	koryphHome := paths.KoryphHome()

	// Quote every argv token for shell-embedding. Command returns plain,
	// exec-ready values (see its doc); quoting a constant flag literal like
	// -p or --permission-mode is a no-op once /bin/sh resolves the single
	// quotes, so this is behavior-identical to the pre-extraction code that
	// quoted only the dynamic subset.
	quotedArgv := make([]string, len(argv))
	for i, a := range argv {
		quotedArgv[i] = sq(a)
	}

	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n")
	sb.WriteString("export" +
		" KORYPH_RUN_ID=" + sq(s.RunID) +
		" KORYPH_PHASE_ID=" + sq(s.PhaseID) +
		" KORYPH_DIR=" + sq(s.PhaseDir) +
		" KORYPH_HOME=" + sq(koryphHome) +
		" KORYPH_LOG_PATH=" + sq(logPath) +
		" KORYPH_STATUS_PATH=" + sq(statusPath) +
		" KORYPH_SUMMARY_PATH=" + sq(summaryPath) +
		" KORYPH_SESSION_ID=" + sq(s.SessionID) +
		" BEADS_DIR=" + sq(s.BeadsDir) + "\n")
	sb.WriteString("cd " + sq(s.Worktree) + " || exit 97\n")
	sb.WriteString("exec " + strings.Join(quotedArgv, " ") + " < " + sq(promptPath) + "\n")

	if err := fsx.WriteAtomic(launchPath, []byte(sb.String()), 0o755); err != nil {
		return Handle{}, fmt.Errorf("dispatch %s: writing launch.sh: %w", s.PhaseID, err)
	}

	// 5. Launch detached. Plain exec.Command (NOT CommandContext): a ctx
	//    cancel in the koryph must never kill a running agent.
	stdout, err := os.Create(streamPath)
	if err != nil {
		return Handle{}, fmt.Errorf("dispatch %s: creating stream.jsonl: %w", s.PhaseID, err)
	}
	defer stdout.Close()
	stderr, err := os.Create(stderrPath)
	if err != nil {
		return Handle{}, fmt.Errorf("dispatch %s: creating stderr.log: %w", s.PhaseID, err)
	}
	defer stderr.Close()

	cmd := exec.Command("/bin/sh", launchPath)
	cmd.Dir = s.Worktree
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Handle{}, fmt.Errorf("dispatch %s: starting launch.sh: %w", s.PhaseID, err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return Handle{}, fmt.Errorf("dispatch %s: releasing pid %d: %w", s.PhaseID, pid, err)
	}

	return Handle{
		PID:              pid,
		SessionID:        s.SessionID,
		LaunchPath:       launchPath,
		StreamPath:       streamPath,
		StderrPath:       stderrPath,
		StatusPath:       statusPath,
		VerifiedIdentity: identity,
	}, nil
}

// sq single-quotes v for /bin/sh. Values are pre-screened for embedded
// single quotes in Dispatch; this is layout, not escaping.
func sq(v string) string {
	return "'" + v + "'"
}

// toRuntimeProfile mirrors account.Profile -> runtime.Profile field-for-field
// (see runtime.Profile's doc) — the same conversion the claude adapter
// performs internally, done here so Dispatch's identity check goes through
// the resolved runtime.Runtime rather than internal/account directly.
func toRuntimeProfile(p account.Profile) runtime.Profile {
	return runtime.Profile{Name: p.Name, ConfigDir: p.ConfigDir}
}

// toRuntimeSpec converts a dispatch.Spec into the runtime-neutral
// DispatchSpec the claude adapter's Command expects (koryph-v8u.2; see
// internal/runtime's package doc for the field-for-field mapping this
// mirrors). Every field not read here (ProjectID, RepoRoot, Branch, Attempt)
// carries through unused by Command today, exactly as it was unused by
// Dispatch's own former argv construction.
func toRuntimeSpec(s Spec) runtime.DispatchSpec {
	return runtime.DispatchSpec{
		ProjectID:        s.ProjectID,
		RepoRoot:         s.RepoRoot,
		RunID:            s.RunID,
		PhaseID:          s.PhaseID,
		PhaseDir:         s.PhaseDir,
		Worktree:         s.Worktree,
		Branch:           s.Branch,
		Persona:          s.Persona,
		Model:            s.Model,
		Effort:           s.Effort,
		Profile:          runtime.Profile{Name: s.Profile.Name, ConfigDir: s.Profile.ConfigDir},
		ExpectedIdentity: s.ExpectedIdentity,
		Billing:          runtime.BillingMode(s.Billing),
		APIKey:           s.APIKey,
		MaxBudgetUSD:     s.MaxBudgetUSD,
		Prompt:           s.Prompt,
		SessionID:        s.SessionID,
		SessionName:      s.SessionName,
		ResumeSessionID:  s.ResumeSessionID,
		BeadsDir:         s.BeadsDir,
		Attempt:          s.Attempt,
		SSHAuthSock:      s.SSHAuthSock,
		EnvPassthrough:   s.EnvPassthrough,
		ProxyBaseURL:     s.ProxyBaseURL,
	}
}

// ParseResultCost scans a stream.jsonl for the LAST line whose JSON has
// "type":"result" and returns its total_cost_usd. A result line with
// is_error==true still returns its cost (we never filter on is_error —
// false is a valid value). Returns (0, false) when no result line with a
// cost is found or the file is unreadable.
//
// The scan itself lives in internal/runtime/claude (koryph-v8u.2) — this is
// now a thin path-opening wrapper over the claude adapter's reader-based
// ParseResultCost, so dispatch, review (via stage.go's reuse), and any
// future generic Event consumer share exactly one parsing implementation.
func ParseResultCost(streamPath string) (float64, bool) {
	f, err := os.Open(streamPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return claude.ParseResultCost(f)
}

// TokenUsage is the dispatch-layer name for claude.TokenUsage (koryph-77r.1):
// the per-attempt token composition off a stream-json result line's usage
// block. Aliased rather than duplicated — unlike Spec/runtime.DispatchSpec
// (deliberately decoupled via toRuntimeSpec so dispatch's public Spec shape
// never leaks internal/runtime's), this is a plain data tuple with no
// independent reason to diverge from its one source of truth.
type TokenUsage = claude.TokenUsage

// ParseResultUsage scans a stream.jsonl for the LAST "result" line and
// returns its token composition (koryph-77r.1, design
// docs/designs/2026-07-token-economy.md §3 L1) — the usage-block counterpart
// to ParseResultCost; see its doc for the shared scan mechanics
// (internal/runtime/claude, koryph-v8u.2) and last-wins semantics.
func ParseResultUsage(streamPath string) (TokenUsage, bool) {
	f, err := os.Open(streamPath)
	if err != nil {
		return TokenUsage{}, false
	}
	defer f.Close()
	return claude.ParseResultUsage(f)
}

// ParseCleanExit scans a stream.jsonl for the LAST "result" line and reports
// whether the agent exited successfully (is_error absent or false). Returns
// false when no result line is found or the file is unreadable.
//
// Use this to distinguish a clean-exit agent (work concluded, no new commits)
// from a crashed agent (killed before writing its final JSON). See
// internal/runtime/claude.ParseCleanExit for the reader-based implementation.
func ParseCleanExit(streamPath string) bool {
	f, err := os.Open(streamPath)
	if err != nil {
		return false
	}
	defer f.Close()
	return claude.ParseCleanExit(f)
}

// ParseRateLimited scans a stream.jsonl for an API rate-limit/overload marker
// inside an error-flagged event: a top-level "error" event, a "result" event
// with is_error true, or an embedded "error" object. Matching is deliberately
// liberal (any of those event shapes, several candidate fields) because the
// exact error shape is not a stable contract; it is scoped to error-flagged
// events so ordinary conversation text mentioning "429" cannot false-positive.
// Unlike ParseResultCost this does not care which line is last — any
// qualifying event anywhere in the stream marks the whole run rate-limited,
// since it is the agent's subsequent death that completeSlot reacts to.
// Returns false when the file is unreadable or no line qualifies.
//
// The scan itself lives in internal/runtime/claude (koryph-v8u.2); see
// ParseResultCost's doc above for why this is now a thin wrapper.
func ParseRateLimited(streamPath string) bool {
	f, err := os.Open(streamPath)
	if err != nil {
		return false
	}
	defer f.Close()
	return claude.ParseRateLimited(f)
}

// Alive reports whether pid is a live process (signal 0 probe).
// ESRCH → false; EPERM → true (it exists, we just can't signal it).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// StopGraceful sends SIGTERM to the process group of pid (agents are
// launched with Setsid, so -pid targets the whole session), falling back
// to the single pid. It never sends SIGKILL.
func StopGraceful(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("stop: invalid pid %d", pid)
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop: SIGTERM pid %d: %w", pid, err)
	}
	return nil
}

// StopForce sends SIGKILL to the process group of pid (falling back to the
// single pid). Unlike StopGraceful it gives the agent NO chance to commit, so
// uncommitted worktree work is lost — use it only when a graceful stop did not
// take. It is the deliberate, opt-in escape hatch to the "never SIGKILL"
// dispatch invariant.
func StopForce(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("stop: invalid pid %d", pid)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("stop: SIGKILL pid %d: %w", pid, err)
	}
	return nil
}
