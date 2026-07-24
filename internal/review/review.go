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

	"github.com/koryph/koryph/internal/agentjson"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/claude"
	"github.com/koryph/koryph/internal/timeoutcfg"
)

// Defaults per the package contract.
const (
	defaultPersona    = "koryph-security-reviewer"
	defaultModel      = "opus"
	defaultClaudeBin  = "claude"
	defaultAttempts   = 4
	diffStatTailLines = 40

	// DefaultTimeoutSec is the reviewer's single wall-clock timeout (20 min)
	// when no bead/project/system override applies (koryph-w82i). The former
	// start(600)/escalate-to-1200 two-tier pair collapsed to one unified value,
	// the built-in default of the timeout hierarchy (timeoutcfg.BuiltinDefaultSec).
	// There is no longer a hard ceiling: a project/bead/system override may set a
	// larger value; only the break-glass KORYPH_REVIEW_TIMEOUT_SEC env sits above
	// the caller-supplied value.
	DefaultTimeoutSec = timeoutcfg.BuiltinDefaultSec
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
// reviewer's wall-clock timeout and sits ABOVE the whole timeout hierarchy
// (bead > project > system > built-in): whatever value the caller resolved and
// threaded into Opts.TimeoutSec, a set env var wins (same convention as
// KORYPH_POLL_SEC over project.poll_seconds). The reviewer runs opus at xhigh
// effort and reads the changed files, so a large diff can need well over the
// default; exceeding the deadline signal-kills the process, which previously
// surfaced as an opaque "reviewer exit -1" (koryph review-timeout fix). There is
// no longer any hard ceiling clamping this (koryph-w82i removed the 20-minute
// cap) — the operator is trusted to pick a sane break-glass value.
func envTimeoutSec() int {
	if v := strings.TrimSpace(os.Getenv("KORYPH_REVIEW_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// resolveTimeout resolves the reviewer's single wall-clock timeout (koryph-w82i,
// collapsing the former start/escalate pair). Precedence:
// KORYPH_REVIEW_TIMEOUT_SEC env (break-glass) > the caller-supplied value (itself
// the bead > project > system winner, resolved by timeoutcfg.Resolve at the
// engine call site) > DefaultTimeoutSec (the built-in 1200). There is no policy
// ceiling — an explicit override may exceed the default — but the result is
// passed through timeoutcfg.Clamp so an absurd env/caller value can never
// overflow the time.Duration deadline. The returned value is always > 0.
func resolveTimeout(timeoutSec int) int {
	if env := envTimeoutSec(); env > 0 {
		return timeoutcfg.Clamp(env)
	}
	if timeoutSec > 0 {
		return timeoutcfg.Clamp(timeoutSec)
	}
	return DefaultTimeoutSec
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
	o.TimeoutSec = resolveTimeout(o.TimeoutSec)
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

	prompt := buildPrompt(o.Branch, base, tailLines(stat.Stdout, diffStatTailLines), names.Stdout, o.Contract)

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
		// koryph-w82i: the reviewer now runs a single unified timeout with no
		// per-attempt escalation. A transient failure (timeout, rate/usage
		// limit, bad reply) is still retried up to Opts.Attempts times with
		// exponential backoff; each retry reuses the same resolved timeout.
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
	// Route the one-shot JSON spawn through the resolved Runtime seam
	// (koryph-fiv finding #1) instead of hand-building claude's argv here — a
	// read-only reviewer is `--permission-mode plan`, no fallback/max-budget.
	rt := o.Runtime
	if rt == nil {
		rt = claude.New(o.ClaudeBin)
	}
	spec := runtime.JSONSpec{
		RepoRoot:       o.RepoRoot,
		Persona:        o.Persona,
		Model:          o.Model,
		Effort:         o.Effort,
		PermissionMode: "plan",
		SpawnKind:      "review",
		Profile:        runtime.Profile{Name: o.Profile.Name, ConfigDir: o.Profile.ConfigDir},
		Billing:        runtime.BillingSubscription,
		ProxyBaseURL:   o.ProxyBaseURL,
	}
	// obs.Span adoption (koryph-5a1 #59): the reviewer spawn is a genuine hot
	// path — one blocking external call per attempt, up to defaultAttempts
	// times per review — so it gets the same latency/status/error span shape
	// as forge.api and vault.resolve, giving real correlation across an
	// entire reviewer attempt instead of scattered log lines.
	sp := obs.StartSpan(ctx, log, slog.LevelDebug, "review.reviewer_spawn", obs.ForgeAttrs(rt.Name(), o.Model, o.Persona)...)
	res, err := runtime.SpawnJSON(ctx, rt, spec, runtime.JSONExec{
		Dir:     o.Worktree,
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
			return degradedTimeout(fmt.Sprintf("reviewer timed out after %ds (opus %s effort on this diff); raise review.timeout_seconds, add a bead `timeout:<seconds>` label, or set the machine-wide default_timeout_seconds — or split the change into smaller beads", o.TimeoutSec, o.Effort))
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
	if criteria := splitAcceptance(o.Contract.AcceptanceCriteria); len(criteria) > 0 {
		enforceContract(&v, criteria)
		if normalized, err := json.Marshal(v); err == nil {
			// Persist the enforced verdict, not the reviewer's pre-enforcement
			// claim. Otherwise review.json could say clean while the engine
			// correctly blocked missing/unsatisfied acceptance evidence.
			v.Raw = string(normalized)
		}
	}
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
// flags TimedOut so the per-attempt history records that this attempt was killed
// for exceeding its deadline (as opposed to a rate limit or bad reply), which the
// degraded artifact annotates. Since koryph-w82i the loop no longer escalates the
// timeout on retry — the flag is diagnostic only.
func degradedTimeout(reason string) Verdict {
	v := degradedReason(reason)
	v.TimedOut = true
	return v
}

// buildPrompt renders the reviewer prompt: diffstat tail + changed-file list
// plus the strict-JSON response contract.
func buildPrompt(branch, base, stat, names string, contract Contract) string {
	var b strings.Builder
	b.WriteString("Review the branch `")
	b.WriteString(branch)
	b.WriteString("` against `")
	b.WriteString(base)
	b.WriteString("` for correctness and security issues.\n\n")

	if contract.ID != "" || contract.AcceptanceCriteria != "" {
		b.WriteString("## Bead contract\n")
		fmt.Fprintf(&b, "- ID: %s\n- Title: %s\n- Effective runtime: %s\n- Completion state: %s\n",
			contract.ID, contract.Title, contract.Runtime, contract.CompletionState)
		if len(contract.Labels) > 0 {
			b.WriteString("- Declared labels/footprint: ")
			b.WriteString(strings.Join(contract.Labels, ", "))
			b.WriteString("\n")
		}
		if strings.TrimSpace(contract.Description) != "" {
			b.WriteString("\n### Description and scope\n")
			b.WriteString(strings.TrimSpace(contract.Description))
			b.WriteString("\n")
		}
		criteria := splitAcceptance(contract.AcceptanceCriteria)
		if len(criteria) > 0 {
			b.WriteString("\n### Acceptance criteria\n")
			for i, criterion := range criteria {
				fmt.Fprintf(&b, "- AC%d: %s\n", i+1, criterion)
			}
		}
		b.WriteString("\nReview the implementation against every criterion and the declared scope. " +
			"A locally correct diff in the wrong subsystem, a missing deliverable, or unexplained " +
			"footprint drift is blocking.\n\n")
	}

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
	b.WriteString(`{"blocking": <bool>, "criteria": [{"id": "AC1", "status": "satisfied|unsatisfied|not-applicable", "evidence": "<specific file/test/result>"}], "findings": [{"severity": "blocking|major|minor", "file": "<path>", "line": <1-based line or omit>, "summary": "<one line>"}]}`)
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

func splitAcceptance(raw string) []string {
	raw = strings.ReplaceAll(raw, `\n`, "\n")
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimLeft(line, "-*•"))
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// enforceContract makes omission of the acceptance audit fail closed. The
// reviewer may find no code defect and still block when it implemented the
// wrong subsystem or skipped a required deliverable.
func enforceContract(v *Verdict, criteria []string) {
	if len(criteria) == 0 {
		return
	}
	seen := make(map[string]CriterionAssessment, len(v.Criteria))
	for _, assessment := range v.Criteria {
		seen[strings.ToUpper(strings.TrimSpace(assessment.ID))] = assessment
	}
	for i := range criteria {
		id := fmt.Sprintf("AC%d", i+1)
		assessment, ok := seen[id]
		status := strings.ToLower(strings.TrimSpace(assessment.Status))
		evidence := strings.TrimSpace(assessment.Evidence)
		if !ok || evidence == "" ||
			(status != "satisfied" && status != "unsatisfied" && status != "not-applicable") {
			v.Blocking = true
			v.Findings = append(v.Findings, Finding{
				Severity: "blocking",
				Summary:  fmt.Sprintf("%s was not evaluated with a valid status and evidence", id),
			})
			continue
		}
		if status == "unsatisfied" {
			v.Blocking = true
		}
	}
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
