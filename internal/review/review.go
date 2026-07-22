// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/agentjson"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/obs"
)

// Defaults per the package contract.
const (
	defaultPersona    = "koryph-security-reviewer"
	defaultModel      = "opus"
	defaultClaudeBin  = "claude"
	defaultTimeoutSec = 600
	defaultAttempts   = 4
	diffStatTailLines = 40

	// MaxTimeoutSec is the HARD ceiling on any single reviewer attempt: 20
	// minutes. It is the authoritative "cannot be exceeded for any single task"
	// bound — no per-project review config, KORYPH_REVIEW_TIMEOUT_SEC override,
	// or adaptive escalation may push a spawn past it; a review that still
	// times out at the ceiling degrades. internal/project mirrors this value
	// (project.ReviewTimeoutHardCapSec) for config validation, and an
	// engine-level drift guard asserts the two stay equal.
	MaxTimeoutSec = 1200
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

// envTimeoutSec returns KORYPH_REVIEW_TIMEOUT_SEC parsed as a positive integer,
// or 0 when unset/invalid. It is the break-glass runtime override for the
// reviewer's STARTING timeout and takes precedence over the per-project review
// config (same convention as KORYPH_POLL_SEC over project.poll_seconds). The
// reviewer runs opus at xhigh effort and reads the changed files, so a large
// diff can need well over the old 240s ceiling; exceeding the deadline
// signal-kills the process, which previously surfaced as an opaque "reviewer
// exit -1" (koryph review-timeout fix). Whatever this returns is still clamped
// by resolveTimeouts to the effective ceiling (the project max, itself <=
// MaxTimeoutSec) — no override may exceed the 20-minute per-task hard cap.
func envTimeoutSec() int {
	if v := strings.TrimSpace(os.Getenv("KORYPH_REVIEW_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// resolveTimeouts resolves the starting per-attempt timeout and the escalation
// ceiling from the caller/project values, applying the env override, defaults,
// and the 20-minute hard cap. Precedence for the starting timeout:
// KORYPH_REVIEW_TIMEOUT_SEC env (break-glass) > caller/project value >
// defaultTimeoutSec. The ceiling defaults to (and is clamped down to)
// MaxTimeoutSec, and the starting timeout is clamped to the ceiling — so NO
// source (env, project config, or a directly-constructed Opts) can produce a
// spawn longer than 20 minutes. Returned values satisfy
// 0 < start <= max <= MaxTimeoutSec.
func resolveTimeouts(timeoutSec, maxTimeoutSec int) (start, max int) {
	max = maxTimeoutSec
	if max <= 0 || max > MaxTimeoutSec {
		max = MaxTimeoutSec
	}
	start = timeoutSec
	if env := envTimeoutSec(); env > 0 {
		start = env
	} else if start <= 0 {
		start = defaultTimeoutSec
	}
	if start > max {
		start = max
	}
	return start, max
}

// escalateTimeout returns the timeout for the next reviewer attempt after a
// wall-clock timeout: double the current value, capped at max. It lets a large
// diff that ran out of thinking time get progressively more room on retry,
// bounded by the 20-minute ceiling. cur is assumed already <= max.
func escalateTimeout(cur, max int) int {
	next := cur * 2
	if next <= 0 || next > max { // <=0 also absorbs int overflow for huge cur
		next = max
	}
	return next
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
	o.TimeoutSec, o.MaxTimeoutSec = resolveTimeouts(o.TimeoutSec, o.MaxTimeoutSec)
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

	// history accumulates every attempt's diagnosis (koryph-5a1 #55): before
	// this, only the LAST attempt's Reason survived on the returned Verdict —
	// an earlier attempt's distinct failure (a rate limit that then cleared,
	// followed by a JSON-parse failure) was silently discarded, and a full
	// degrade persisted no artifact at all, leaving the operator with nothing
	// but a 300-char stderr tail baked into one string.
	var history []attemptDiagnosis
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
				history = append(history, attemptDiagnosis{Attempt: i, Reason: last.Reason})
				persistDegraded(o, last, history)
				return last
			case <-time.After(backoffFor(i)):
			}
		}
		v := attemptReview(ctx, o, prompt)
		v.Attempts = i + 1
		if !v.Degraded {
			if o.OutPath != "" {
				// Persist the raw Claude envelope beside the parsed verdict
				// (same pattern as stage-*.json, koryph-qbc) so usage/cost data
				// is available for audit. Best-effort: a write failure here is
				// non-fatal (we still have the parsed verdict).
				envPath := filepath.Join(filepath.Dir(o.OutPath), "review-envelope.json")
				_ = fsx.WriteAtomic(envPath, []byte(v.Envelope+"\n"), 0o644)
				if err := fsx.WriteAtomic(o.OutPath, []byte(v.Raw+"\n"), 0o644); err != nil {
					v = degradedReason("persist review.json failed: " + err.Error())
					v.Attempts = i + 1
					history = append(history, attemptDiagnosis{Attempt: i + 1, Reason: v.Reason})
					persistDegraded(o, v, history)
					return v
				}
			}
			return v
		}
		history = append(history, attemptDiagnosis{Attempt: i + 1, Reason: v.Reason, TimedOut: v.TimedOut})
		// Adaptive: when an attempt runs out of wall-clock, give the next one
		// more room — double the timeout up to MaxTimeoutSec — before retrying.
		// A rate/usage limit (the other dominant transient failure) leaves the
		// timeout unchanged; only the backoff grows. Once at the ceiling the
		// timeout stays put and remaining attempts retry at 20 min.
		if v.TimedOut && o.TimeoutSec < o.MaxTimeoutSec {
			o.TimeoutSec = escalateTimeout(o.TimeoutSec, o.MaxTimeoutSec)
		}
		last = v
	}
	persistDegraded(o, last, history)
	return last
}

// attemptDiagnosis records one reviewer attempt's outcome for the degraded
// artifact — see persistDegraded.
type attemptDiagnosis struct {
	Attempt  int    `json:"attempt"`
	Reason   string `json:"reason"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// persistDegraded writes every attempt's diagnosis to review-degraded.json
// beside o.OutPath (koryph-5a1 #55) — the degraded counterpart of the
// success path's review.json/review-envelope.json, so a full-degrade run
// leaves a durable, per-attempt-annotated artifact instead of nothing.
// Best-effort (an already-degraded verdict must not itself fail harder) and
// a no-op when OutPath is unset — `koryph review-pr`/review-queue call
// Review outside any phase dir and have no directory to write beside.
func persistDegraded(o Opts, v Verdict, history []attemptDiagnosis) {
	if o.OutPath == "" {
		return
	}
	data, err := json.MarshalIndent(struct {
		Degraded bool               `json:"degraded"`
		Reason   string             `json:"reason"`
		Attempts []attemptDiagnosis `json:"attempts"`
		At       string             `json:"at"`
	}{
		Degraded: true,
		Reason:   v.Reason,
		Attempts: history,
		At:       time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(filepath.Dir(o.OutPath), "review-degraded.json")
	_ = fsx.WriteAtomic(path, data, 0o644)
}

// attemptReview runs one reviewer spawn + parse. On any failure it returns a
// degraded verdict whose Reason explains the failure, so a degradation is never
// a black box in the logs.
func attemptReview(ctx context.Context, o Opts, prompt string) Verdict {
	args := []string{
		"-p",
		"--agent", o.Persona,
		"--permission-mode", "plan",
		"--model", o.Model,
		"--output-format", "json",
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	// obs.Span adoption (koryph-5a1 #59): the reviewer spawn is a genuine hot
	// path — one blocking external call per attempt, up to defaultAttempts
	// times per review — so it gets the same latency/status/error span shape
	// as forge.api and vault.resolve, giving real correlation across an
	// entire reviewer attempt instead of scattered log lines.
	sp := obs.StartSpan(ctx, log, slog.LevelDebug, "review.reviewer_spawn", obs.ForgeAttrs("claude", o.Model, o.Persona)...)
	res, err := execx.Run(ctx, execx.Cmd{
		Dir:     o.Worktree,
		Env:     account.ChildEnv(account.ChildEnvSpec{Profile: o.Profile, Billing: account.BillingSubscription, ProxyBaseURL: o.ProxyBaseURL, SpawnKind: "review"}),
		Name:    o.ClaudeBin,
		Args:    args,
		Stdin:   prompt,
		Timeout: time.Duration(o.TimeoutSec) * time.Second,
	})
	if err != nil {
		sp.End(0, err)
		return degradedReason("reviewer spawn error: " + err.Error())
	}
	if res.ExitCode != 0 {
		sp.End(0, fmt.Errorf("exit %d (timed_out=%v)", res.ExitCode, res.TimedOut))
		if res.TimedOut {
			if o.TimeoutSec >= o.MaxTimeoutSec {
				return degradedTimeout(fmt.Sprintf("reviewer timed out after %ds — the %d-minute per-task ceiling (opus %s effort on this diff); split the change into smaller beads", o.TimeoutSec, o.MaxTimeoutSec/60, o.Effort))
			}
			return degradedTimeout(fmt.Sprintf("reviewer timed out after %ds (opus %s effort on this diff); the loop escalates the timeout on retry up to %ds — tune review.timeout_seconds / review.max_timeout_seconds", o.TimeoutSec, o.Effort, o.MaxTimeoutSec))
		}
		return degradedReason(fmt.Sprintf("reviewer exit %d: %s", res.ExitCode, strings.TrimSpace(agentjson.Tail(res.Stderr, 300))))
	}
	sp.EndOK()

	// The CLI emits a result envelope; its "result" field holds the model
	// text, which should itself be strict JSON. Extract the verdict schema-aware
	// (requiring the "blocking" key) so a stray brace token the model quoted from
	// the diff — a Svelte {@html}, a {glob%-*} — is never mistaken for the verdict.
	out := strings.TrimSpace(res.Stdout)
	raw, err := agentjson.ParseEnvelopeVerdict(out, "blocking")
	if err != nil {
		return degradedReason("reviewer " + err.Error())
	}

	var v Verdict
	if json.Unmarshal([]byte(raw), &v) != nil {
		return degradedReason("verdict JSON invalid: " + strings.TrimSpace(agentjson.Tail(raw, 300)))
	}
	v.Degraded = false
	v.Raw = raw
	// Capture the full Claude envelope so Review can persist it for audit/metrics
	// beside the parsed verdict (koryph-qbc). res.Stdout is the raw --output-format
	// json output including usage and cost fields.
	v.Envelope = res.Stdout
	return v
}

// degradedReason builds a non-blocking, degraded verdict with a human-readable
// explanation. Blocking stays false: the engine treats a degraded review as a
// merge blocker via policy, not by flipping Blocking (which means "the reviewer
// found something").
func degradedReason(reason string) Verdict {
	return Verdict{Degraded: true, Blocking: false, Reason: reason}
}

// degradedTimeout is degradedReason for a wall-clock timeout: it additionally
// flags TimedOut so the retry loop escalates the timeout before the next
// attempt (the only degradation the loop responds to by growing the deadline
// rather than just backing off).
func degradedTimeout(reason string) Verdict {
	v := degradedReason(reason)
	v.TimedOut = true
	return v
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

	b.WriteString("\nRead the changed files in this worktree as needed. Respond with your" +
		" verdict as STRICT JSON inside a single ```json fenced block, and nothing" +
		" after the closing fence, in exactly this shape:\n\n")
	b.WriteString("```json\n")
	b.WriteString(`{"blocking": <bool>, "findings": [{"severity": "blocking|major|minor", "file": "<path>", "line": <1-based line or omit>, "summary": "<one line>"}]}`)
	b.WriteString("\n```\n")
	b.WriteString(`
Include "line" (a 1-based line number in "file") when a finding is about a
specific line, so it can be posted as an inline PR comment; omit it for
whole-file or general findings. Set "blocking" to true only when at least one
finding must be fixed before this branch may merge. An empty findings list with
"blocking": false means the diff is clean. If you must quote diff tokens like
{@html} in a summary, keep them inside JSON string values only.
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
