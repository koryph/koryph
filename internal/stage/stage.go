// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package stage

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/claude"
)

// Defaults per the package contract.
const (
	defaultClaudeBin = "claude"
	// DefaultTimeoutSec bounds a stage agent when the pipeline stage sets no
	// explicit TimeoutSec. Exported so the engine can name it when a stage times
	// out (koryph-a59).
	DefaultTimeoutSec = 600
	changedFilesTail  = 60
)

// Run executes one post-implement stage in o.Worktree. See the package
// contract. It never panics and always returns a Result: any failure is
// reported via OK=false + Note so the caller can apply its own policy
// (optional stages continue; required stages block).
func Run(ctx context.Context, o Opts) Result {
	base := o.Base
	if base == "" {
		base = "main"
	}
	bin := o.ClaudeBin
	if bin == "" {
		bin = defaultClaudeBin
	}
	timeout := o.TimeoutSec
	if timeout <= 0 {
		timeout = DefaultTimeoutSec
	}
	if o.Persona == "" || o.Model == "" {
		return Result{Note: "stage misconfigured: persona and model are required"}
	}

	// Identity fail-closed, BEFORE any exec — belt-and-braces with the
	// implementer dispatch that already verified this profile. Reached
	// through the runtime seam (koryph-v8u.5): claude's VerifyIdentity
	// delegates to account.VerifyExpected, unchanged — see
	// runtime.Runtime.VerifyIdentity's doc.
	rt := claude.New(bin)
	if _, err := rt.VerifyIdentity(ctx, runtime.Profile{Name: o.Profile.Name, ConfigDir: o.Profile.ConfigDir}, o.ExpectedIdentity); err != nil {
		return Result{Note: "identity: " + err.Error()}
	}
	if !o.Billing.Valid() {
		return Result{Note: fmt.Sprintf("invalid billing mode %q", o.Billing)}
	}

	changed := changedFiles(ctx, o.Worktree, base, o.Branch)
	prompt := buildPrompt(o, base, changed)

	args := []string{
		"-p",
		"--agent", o.Persona,
		"--permission-mode", "dontAsk",
		"--model", o.Model,
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(o.MaxBudgetUSD, 'f', -1, 64))
	}
	// FallbackModel (koryph-v8u.2): the same "sonnet" value dispatch/cli.go's
	// claude adapter uses, now a single shared constant instead of two
	// independently-duplicated literals (flagged by the koryph-v8u.2
	// architecture review).
	args = append(args, "--fallback-model", claude.FallbackModel, "--output-format", "json")

	// obs.Span adoption (koryph-5a1 #59): the stage agent spawn is the same
	// shape of hot path as a reviewer/forge/vault call — one blocking
	// external process per pipeline stage — so it gets the same
	// latency/status/error span instead of being invisible to trace
	// correlation entirely.
	sp := obs.StartSpan(ctx, log, slog.LevelDebug, "stage.agent_spawn", obs.ForgeAttrs("claude", o.Model, o.Persona)...)
	res, err := execx.Run(ctx, execx.Cmd{
		Dir:     o.Worktree,
		Env:     account.ChildEnv(account.ChildEnvSpec{Profile: o.Profile, Billing: o.Billing, APIKey: o.APIKey, SSHAuthSock: o.SSHAuthSock, ProxyBaseURL: o.ProxyBaseURL, SpawnKind: "stage"}),
		Name:    bin,
		Args:    args,
		Stdin:   prompt,
		Timeout: time.Duration(timeout) * time.Second,
	})
	switch {
	case err != nil:
		sp.End(0, err)
	case res.ExitCode != 0:
		sp.End(0, fmt.Errorf("exit %d (timed_out=%v)", res.ExitCode, res.TimedOut))
	default:
		sp.EndOK()
	}

	out := Result{Ran: true}
	// Persist the raw envelope and the full stderr for inspection — mirrors
	// dispatch's session.log/stderr.log (koryph-5a1 #56): before this, a stage
	// agent's stderr was swallowed into a 400-char Note with nothing durable
	// left once the process exited. Written unconditionally (not just on
	// failure) so a clean run's stderr (deprecation warnings, etc.) is still
	// recoverable.
	if o.PhaseDir != "" {
		envPath := filepath.Join(o.PhaseDir, "stage-"+o.Stage+".json")
		if werr := fsx.WriteAtomic(envPath, []byte(res.Stdout+"\n"), 0o644); werr == nil {
			if cost, ok := dispatch.ParseResultCost(envPath); ok {
				out.CostUSD = cost
			}
		}
		if res.Stderr != "" {
			stderrPath := filepath.Join(o.PhaseDir, "stage-"+o.Stage+"-stderr.log")
			_ = fsx.WriteAtomic(stderrPath, []byte(res.Stderr), 0o644)
		}
	}

	// A timeout is not a failure — the stage may simply have needed more time
	// (koryph-a59). It surfaces two ways: as a spawn-style error (err != nil) or,
	// when the kill lands as a non-zero exit, via res.TimedOut. Flag it distinctly
	// either way so the caller reports it honestly and points the operator at the
	// stage's timeout_sec rather than treating complete-but-slow work as broken.
	if err != nil {
		out.TimedOut = res.TimedOut
		out.Note = err.Error()
		return out
	}
	if res.ExitCode != 0 {
		out.TimedOut = res.TimedOut
		if res.TimedOut {
			out.Note = fmt.Sprintf("timed out after %s", time.Duration(timeout)*time.Second)
		} else {
			out.Note = fmt.Sprintf("agent exited %d: %s", res.ExitCode, tail(res.Stderr, 400))
			if o.PhaseDir != "" && res.Stderr != "" {
				out.Note += fmt.Sprintf(" (full stderr: %s)", filepath.Join(o.PhaseDir, "stage-"+o.Stage+"-stderr.log"))
			}
		}
		return out
	}
	out.OK = true
	return out
}

// changedFiles lists the branch's changed files vs base (empty on any error —
// the stage still runs, just without the diff hint).
func changedFiles(ctx context.Context, worktree, base, branch string) []string {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: worktree, Name: "git",
		Args: []string{"diff", "--name-only", base + "..." + branch},
	})
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

// buildPrompt renders the stage prompt: goal, bead context, changed-file list,
// the agent boundary rules (mirrors the dispatch preamble), and the per-stage
// extra instructions.
func buildPrompt(o Opts, base string, changed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the %q stage of the koryph pipeline for bead %s", o.Stage, o.BeadID)
	if o.BeadTitle != "" {
		fmt.Fprintf(&b, " (%s)", o.BeadTitle)
	}
	b.WriteString(".\n\n")
	fmt.Fprintf(&b, "The implementer has already committed its work on branch `%s`. Your job is the "+
		"`%s` stage: make the changes that stage requires and COMMIT them in this worktree.\n\n",
		o.Branch, o.Stage)

	b.WriteString("## Files changed so far (" + base + "...HEAD)\n")
	if len(changed) == 0 {
		b.WriteString("(none reported)\n")
	} else {
		for i, f := range changed {
			if i >= changedFilesTail {
				fmt.Fprintf(&b, "- ...and %d more\n", len(changed)-changedFilesTail)
				break
			}
			b.WriteString("- " + f + "\n")
		}
	}

	b.WriteString(`
## Boundaries (enforced)
- Work ONLY in this worktree, on this branch. Commit early and often — commits
  are your checkpoints.
- Do NOT: git checkout main, git merge, git push, gh pr merge, or bd close.
  The koryph merges and closes.
- If this stage has nothing to do for this change, make no commits and exit 0.
`)

	if strings.TrimSpace(o.ExtraPrompt) != "" {
		b.WriteString("\n## Stage instructions\n")
		b.WriteString(strings.TrimSpace(o.ExtraPrompt))
		b.WriteString("\n")
	}
	return b.String()
}

// tail returns the last n bytes of s.
func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
