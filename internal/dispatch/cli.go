// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
)

// CLIBackend dispatches agents by shelling out to the claude CLI via a
// generated launch.sh, detached from the koryph process.
type CLIBackend struct {
	ClaudeBin string // path or name of the claude binary; default "claude"
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
	// 1. Identity + billing verification — before any filesystem effect.
	id, err := account.VerifyExpected(ctx, s.Profile, s.ExpectedIdentity)
	if err != nil {
		return Handle{}, err
	}
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

	// 4. Build launch.sh (inspectable artifact).
	logPath := filepath.Join(s.PhaseDir, "session.log")
	summaryPath := filepath.Join(s.PhaseDir, "SUMMARY.md")
	launchPath := filepath.Join(s.PhaseDir, "launch.sh")
	streamPath := filepath.Join(s.PhaseDir, "stream.jsonl")
	stderrPath := filepath.Join(s.PhaseDir, "stderr.log")

	args := []string{
		"-p",
		"--agent", sq(s.Persona),
		"--session-id", sq(s.SessionID),
		"--permission-mode", "dontAsk",
		"--model", sq(s.Model),
	}
	if s.Effort != "" {
		args = append(args, "--effort", sq(s.Effort))
	}
	if s.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", sq(strconv.FormatFloat(s.MaxBudgetUSD, 'f', -1, 64)))
	}
	args = append(args, "--fallback-model", "sonnet")
	if s.SessionName != "" {
		args = append(args, "--name", sq(s.SessionName))
	}
	if s.ResumeSessionID != "" {
		args = append(args, "--resume", sq(s.ResumeSessionID), "--fork-session")
	}
	args = append(args,
		"--add-dir", sq(s.PhaseDir),
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
	)

	// KORYPH_HOME is exported (resolved absolute) so the guard hooks registered
	// in .claude/settings.json as ${KORYPH_HOME:-$HOME/.koryph}/hooks/*.sh
	// resolve to the central, agent-unwritable copy — never the worktree.
	koryphHome := paths.KoryphHome()

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
	sb.WriteString("exec " + sq(claudeBin) + " " + strings.Join(args, " ") + " < " + sq(promptPath) + "\n")

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
	cmd.Env = account.ChildEnv(account.ChildEnvSpec{
		Profile:     s.Profile,
		Billing:     s.Billing,
		APIKey:      s.APIKey,
		SSHAuthSock: s.SSHAuthSock,
		Passthrough: s.EnvPassthrough,
	})
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
		VerifiedIdentity: id.Email,
	}, nil
}

// sq single-quotes v for /bin/sh. Values are pre-screened for embedded
// single quotes in Dispatch; this is layout, not escaping.
func sq(v string) string {
	return "'" + v + "'"
}

// resultLine is the tolerant shape of a stream-json "result" line.
type resultLine struct {
	Type         string   `json:"type"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
}

// ParseResultCost scans a stream.jsonl for the LAST line whose JSON has
// "type":"result" and returns its total_cost_usd. A result line with
// is_error==true still returns its cost (we never filter on is_error —
// false is a valid value). Returns (0, false) when no result line with a
// cost is found or the file is unreadable.
func ParseResultCost(streamPath string) (float64, bool) {
	f, err := os.Open(streamPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var cost float64
	var found bool
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec resultLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type != "result" {
			continue
		}
		if rec.TotalCostUSD != nil {
			cost, found = *rec.TotalCostUSD, true
		} else {
			cost, found = 0, false
		}
	}
	return cost, found
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
