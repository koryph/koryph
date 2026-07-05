// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/agentjson"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
)

// Defaults per the package contract.
const (
	defaultPersona    = "koryph-security-reviewer"
	defaultModel      = "opus"
	defaultClaudeBin  = "claude"
	defaultTimeoutSec = 240
	defaultAttempts   = 4
	diffStatTailLines = 40
)

// Exponential backoff between reviewer attempts: the nth retry waits
// backoffUnit * 2^(n-1), capped at maxBackoff. Reviewer failures are dominated
// by API rate/usage limits, so each retry backs off progressively rather than
// hammering the limit. backoffUnit/maxBackoff are package vars so tests can
// shrink them; production keeps the real delays.
var (
	backoffUnit = 2 * time.Second
	maxBackoff  = 30 * time.Second
)

// backoffFor returns the exponential wait before the given retry (1-based):
// backoffUnit * 2^(retry-1), capped at maxBackoff. The cap also absorbs shift
// overflow for large retry counts.
func backoffFor(retry int) time.Duration {
	if retry < 1 {
		return 0
	}
	d := backoffUnit << (retry - 1)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

// Review runs the post-implementation review pass over o.Branch vs o.Base and
// returns the verdict. A transient reviewer failure (rate/usage limit, timeout,
// a one-off unparseable reply) is retried up to o.Attempts times with backoff;
// only when every attempt fails does it return Verdict{Degraded:true} carrying
// a Reason. It never panics the loop, but it never SILENTLY passes either — the
// caller decides policy (the engine fails closed on a degraded verdict).
func Review(ctx context.Context, o Opts) Verdict {
	base := o.Base
	if base == "" {
		base = "main"
	}
	if o.Persona == "" {
		o.Persona = defaultPersona
	}
	if o.Model == "" {
		o.Model = defaultModel
	}
	if o.ClaudeBin == "" {
		o.ClaudeBin = defaultClaudeBin
	}
	if o.TimeoutSec <= 0 {
		o.TimeoutSec = defaultTimeoutSec
	}
	attempts := o.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}

	// The diff is deterministic — a git error here is not transient, so it is
	// not retried.
	stat, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.Worktree, Name: "git",
		Args: []string{"diff", "--stat", base + "..." + o.Branch},
	})
	if err != nil {
		return degradedReason("git diff --stat failed: " + err.Error())
	}
	names, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.Worktree, Name: "git",
		Args: []string{"diff", "--name-only", base + "..." + o.Branch},
	})
	if err != nil {
		return degradedReason("git diff --name-only failed: " + err.Error())
	}

	prompt := buildPrompt(o.Branch, base, tailLines(stat.Stdout, diffStatTailLines), names.Stdout)

	var last Verdict
	for i := 0; i < attempts; i++ {
		if i > 0 {
			// Exponential backoff (base, 2*base, 4*base, ... capped) so a rate or
			// usage limit — the dominant transient reviewer failure — is given
			// progressively more time to clear instead of being hammered.
			select {
			case <-ctx.Done():
				last = degradedReason("context cancelled during review retry")
				last.Attempts = i
				return last
			case <-time.After(backoffFor(i)):
			}
		}
		v := attemptReview(ctx, o, prompt)
		v.Attempts = i + 1
		if !v.Degraded {
			if o.OutPath != "" {
				if err := fsx.WriteAtomic(o.OutPath, []byte(v.Raw+"\n"), 0o644); err != nil {
					v = degradedReason("persist review.json failed: " + err.Error())
					v.Attempts = i + 1
					return v
				}
			}
			return v
		}
		last = v
	}
	return last
}

// attemptReview runs one reviewer spawn + parse. On any failure it returns a
// degraded verdict whose Reason explains the failure, so a degradation is never
// a black box in the logs.
func attemptReview(ctx context.Context, o Opts, prompt string) Verdict {
	res, err := execx.Run(ctx, execx.Cmd{
		Dir:  o.Worktree,
		Env:  account.ChildEnv(account.ChildEnvSpec{Profile: o.Profile, Billing: account.BillingSubscription}),
		Name: o.ClaudeBin,
		Args: []string{
			"-p",
			"--agent", o.Persona,
			"--permission-mode", "plan",
			"--model", o.Model,
			"--output-format", "json",
		},
		Stdin:   prompt,
		Timeout: time.Duration(o.TimeoutSec) * time.Second,
	})
	if err != nil {
		return degradedReason("reviewer spawn error: " + err.Error())
	}
	if res.ExitCode != 0 {
		return degradedReason(fmt.Sprintf("reviewer exit %d: %s", res.ExitCode, strings.TrimSpace(agentjson.Tail(res.Stderr, 300))))
	}

	// The CLI emits a result envelope; its "result" field holds the model
	// text, which should itself be strict JSON (extracted tolerantly).
	out := strings.TrimSpace(res.Stdout)
	raw, err := agentjson.ParseEnvelope(out)
	if err != nil {
		return degradedReason("reviewer " + err.Error())
	}

	var v Verdict
	if json.Unmarshal([]byte(raw), &v) != nil {
		return degradedReason("verdict JSON invalid: " + strings.TrimSpace(agentjson.Tail(raw, 300)))
	}
	v.Degraded = false
	v.Raw = raw
	return v
}

// degradedReason builds a non-blocking, degraded verdict with a human-readable
// explanation. Blocking stays false: the engine treats a degraded review as a
// merge blocker via policy, not by flipping Blocking (which means "the reviewer
// found something").
func degradedReason(reason string) Verdict {
	return Verdict{Degraded: true, Blocking: false, Reason: reason}
}

// buildPrompt renders the reviewer prompt: diffstat tail + changed-file list
// plus the strict-JSON response contract.
func buildPrompt(branch, base, stat, names string) string {
	var b strings.Builder
	b.WriteString("Review the branch `")
	b.WriteString(branch)
	b.WriteString("` against `")
	b.WriteString(base)
	b.WriteString("` for correctness and security issues.\n\n")

	b.WriteString("## Diff stat (tail)\n```\n")
	b.WriteString(strings.TrimSpace(stat))
	b.WriteString("\n```\n\n## Changed files\n")
	for _, line := range strings.Split(names, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			b.WriteString("- ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	b.WriteString(`
Read the changed files in this worktree as needed. Respond with STRICT JSON
only — no prose, no markdown fences — in exactly this shape:

{"blocking": <bool>, "findings": [{"severity": "blocking|major|minor", "file": "<path>", "line": <1-based line or omit>, "summary": "<one line>"}]}

Include "line" (a 1-based line number in "file") when a finding is about a
specific line, so it can be posted as an inline PR comment; omit it for
whole-file or general findings. Set "blocking" to true only when at least one
finding must be fixed before this branch may merge. An empty findings list with
"blocking": false means the diff is clean.
`)
	return b.String()
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
