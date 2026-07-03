// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package stage

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
)

// Defaults per the package contract.
const (
	defaultClaudeBin  = "claude"
	defaultTimeoutSec = 600
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
		timeout = defaultTimeoutSec
	}
	if o.Persona == "" || o.Model == "" {
		return Result{Note: "stage misconfigured: persona and model are required"}
	}

	// Identity fail-closed, BEFORE any exec — belt-and-braces with the
	// implementer dispatch that already verified this profile.
	if _, err := account.VerifyExpected(ctx, o.Profile, o.ExpectedIdentity); err != nil {
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
	args = append(args, "--fallback-model", "sonnet", "--output-format", "json")

	res, err := execx.Run(ctx, execx.Cmd{
		Dir:     o.Worktree,
		Env:     account.ChildEnv(account.ChildEnvSpec{Profile: o.Profile, Billing: o.Billing, APIKey: o.APIKey, SSHAuthSock: o.SSHAuthSock}),
		Name:    bin,
		Args:    args,
		Stdin:   prompt,
		Timeout: time.Duration(timeout) * time.Second,
	})

	out := Result{Ran: true}
	// Persist the raw envelope for inspection and parse its cost (best-effort).
	if o.PhaseDir != "" {
		envPath := filepath.Join(o.PhaseDir, "stage-"+o.Stage+".json")
		if werr := fsx.WriteAtomic(envPath, []byte(res.Stdout+"\n"), 0o644); werr == nil {
			if cost, ok := dispatch.ParseResultCost(envPath); ok {
				out.CostUSD = cost
			}
		}
	}

	if err != nil {
		out.Note = err.Error()
		return out
	}
	if res.ExitCode != 0 {
		out.Note = fmt.Sprintf("agent exited %d: %s", res.ExitCode, tail(res.Stderr, 400))
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
