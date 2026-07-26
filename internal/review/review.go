// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/agentjson"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/claude"
	"github.com/koryph/koryph/internal/timeoutcfg"
)

// Defaults per the package contract.
const (
	defaultPersona         = "koryph-reviewer"
	defaultModel           = "sonnet"
	defaultSecurityPersona = "koryph-security-reviewer"
	defaultSecurityModel   = "opus"
	defaultClaudeBin       = "claude"
	defaultAttempts        = 4
	diffStatTailLines      = 40

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
// KORYPH_POLL_SEC over project.poll_seconds). The selected runtime/model reads
// the changed files, so a large diff can need well over the default; exceeding
// the deadline signal-kills the process, which previously surfaced as an opaque
// "reviewer exit -1" (koryph review-timeout fix). There is no longer any hard
// ceiling clamping this (koryph-w82i removed the 20-minute cap) — the operator
// is trusted to pick a sane break-glass value.
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
		if o.Security {
			o.Persona = defaultSecurityPersona
		}
	}
	if o.Model == "" {
		o.Model = defaultModel
		if o.Security {
			o.Model = defaultSecurityModel
		}
	}
	if o.ClaudeBin == "" {
		o.ClaudeBin = defaultClaudeBin
	}
	o.TimeoutSec = resolveTimeout(o.TimeoutSec)
	attempts := o.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}

	baseRef := base
	candidateRef := o.Branch
	if strings.TrimSpace(o.BaseSHA) != "" || strings.TrimSpace(o.CandidateSHA) != "" {
		if strings.TrimSpace(o.BaseSHA) == "" || strings.TrimSpace(o.CandidateSHA) == "" {
			return degradedReason("review requires both base and candidate SHA when either is pinned")
		}
		baseRef = strings.TrimSpace(o.BaseSHA)
		candidateRef = strings.TrimSpace(o.CandidateSHA)
		if err := verifyPinnedCandidate(ctx, o); err != nil {
			return degradedReason(err.Error())
		}
	}
	var (
		criteria []AcceptanceCriterion
		err      error
	)
	if !o.Security && strings.TrimSpace(o.Contract.AcceptanceCriteria) != "" {
		criteria, err = ParseAcceptanceCriteria(o.Contract.AcceptanceCriteria)
		if err != nil {
			return degradedReason("parse canonical acceptance criteria: " + err.Error())
		}
		if err := ValidateCompletionEvidence(criteria, o.Contract.CompletionEvidence); err != nil {
			return degradedReason("audit worker completion evidence: " + err.Error())
		}
	}
	priorFindings, priorArtifactDigests, err := loadPriorBlockingFindings(o)
	if err != nil {
		return degradedReason("load prior blocking review: " + err.Error())
	}

	// The diff is deterministic — a git error here is not transient, so it is
	// not retried.
	stat, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.Worktree, Name: "git",
		Args: []string{"diff", "--stat", baseRef + "..." + candidateRef},
	})
	if err != nil {
		return degradedReason("git diff --stat failed: " + err.Error())
	}
	names, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.Worktree, Name: "git",
		Args: []string{"diff", "--name-only", baseRef + "..." + candidateRef},
	})
	if err != nil {
		return degradedReason("git diff --name-only failed: " + err.Error())
	}

	prompt := buildPrompt(
		o.Branch, baseRef, candidateRef,
		tailLines(stat.Stdout, diffStatTailLines), names.Stdout,
		o.Contract, criteria, priorFindings, o.Security,
	)

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
		v := attemptReview(ctx, o, prompt, criteria)
		v.Attempts = i + 1
		if !v.Degraded {
			if err := verifyPinnedCandidate(ctx, o); err != nil {
				v = degradedReason(err.Error())
				v.Attempts = i + 1
				history = append(history, attemptDiagnosis{Attempt: i + 1, Reason: v.Reason})
				persistDegraded(o, v, history)
				return v
			}
			enforcePriorFindings(&v, priorFindings)
			normalizeBlocking(&v)
			if normalized, err := json.Marshal(v); err == nil {
				v.Raw = string(normalized)
			}
			if err := persistArtifact(o, &v, priorFindings, priorArtifactDigests); err != nil {
				v = degradedReason("persist immutable review artifact: " + err.Error())
				v.Attempts = i + 1
				history = append(history, attemptDiagnosis{Attempt: i + 1, Reason: v.Reason})
				persistDegraded(o, v, history)
				return v
			}
			if v.ArtifactPath != "" && v.Envelope != "" {
				// The usage envelope is auxiliary to the authenticated verdict.
				// Give it the same versioned stem and never overwrite it.
				envPath := relatedArtifactPath(v.ArtifactPath, "envelope")
				_ = writeImmutable(envPath, []byte(v.Envelope+"\n"), 0o644)
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

func verifyPinnedCandidate(ctx context.Context, o Opts) error {
	if strings.TrimSpace(o.CandidateSHA) == "" {
		return nil
	}
	expected := strings.TrimSpace(o.CandidateSHA)
	branch, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.Worktree, Name: "git",
		Args: []string{"rev-parse", "--verify", o.Branch + "^{commit}"},
	})
	if err != nil {
		return fmt.Errorf("verify gated candidate branch: %w", err)
	}
	if got := strings.TrimSpace(branch.Stdout); got != expected {
		return fmt.Errorf("gated candidate moved during review: branch=%s expected=%s", got, expected)
	}
	head, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.Worktree, Name: "git",
		Args: []string{"rev-parse", "--verify", "HEAD^{commit}"},
	})
	if err != nil {
		return fmt.Errorf("verify review worktree HEAD: %w", err)
	}
	if got := strings.TrimSpace(head.Stdout); got != expected {
		return fmt.Errorf("review worktree HEAD is not the gated candidate: head=%s expected=%s", got, expected)
	}
	status, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.Worktree, Name: "git",
		Args: []string{"status", "--porcelain", "--untracked-files=normal"},
	})
	if err != nil {
		return fmt.Errorf("verify review worktree cleanliness: %w", err)
	}
	if strings.TrimSpace(status.Stdout) != "" {
		return errors.New("review worktree is not clean at the exact gated candidate")
	}
	return nil
}

type priorBlockingFinding struct {
	ID      string
	Finding Finding
	History HistoricalFinding
}

// attemptDiagnosis records one reviewer attempt's outcome for the degraded
// artifact — see persistDegraded.
type attemptDiagnosis struct {
	Attempt  int    `json:"attempt"`
	Reason   string `json:"reason"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// persistDegraded writes every attempt's diagnosis to an immutable degraded
// artifact (koryph-5a1 #55), so a full-degrade run leaves durable,
// per-attempt-annotated evidence instead of nothing. It is best-effort (an
// already-degraded verdict must not itself fail harder) and a no-op when no
// persistence destination is configured.
func persistDegraded(o Opts, v Verdict, history []attemptDiagnosis) {
	if o.OutPath == "" && o.ArtifactDir == "" {
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
	data = append(data, '\n')
	path := ""
	if o.ArtifactDir != "" {
		digest := strings.TrimPrefix(contentDigest(data), "sha256:")
		path = filepath.Join(o.ArtifactDir,
			fmt.Sprintf("%s-review-degraded-%s.json", reviewKind(o.Security), digest[:16]))
	} else {
		path = relatedArtifactPath(o.OutPath, "degraded")
	}
	_ = writeImmutable(path, data, 0o644)
}

// attemptReview runs one reviewer spawn + parse. On any failure it returns a
// degraded verdict whose Reason explains the failure, so a degradation is never
// a black box in the logs.
func attemptReview(
	ctx context.Context,
	o Opts,
	prompt string,
	criteria []AcceptanceCriterion,
) Verdict {
	// Route the one-shot JSON spawn through the resolved Runtime seam
	// (koryph-fiv finding #1) instead of hand-building claude's argv here — a
	// read-only reviewer is `--permission-mode plan`, no fallback/max-budget.
	rt := o.Runtime
	if rt == nil {
		rt = claude.New(o.ClaudeBin)
	}
	scratchDir, err := prepareReviewScratch(o)
	if err != nil {
		return degradedReason("prepare reviewer scratch: " + err.Error())
	}
	spec := runtime.JSONSpec{
		RepoRoot:       o.RepoRoot,
		ScratchDir:     scratchDir,
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
			return degradedTimeout(fmt.Sprintf("reviewer timed out after %ds (%s %s effort on this diff); raise review.timeout_seconds, add a bead `timeout:<seconds>` label, or set the machine-wide default_timeout_seconds — or split the change into smaller beads", o.TimeoutSec, o.Model, o.Effort))
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

	v, err := parseReviewerVerdict([]byte(raw), o.Security)
	if err != nil {
		return degradedReason("verdict JSON invalid: " + err.Error() + ": " +
			strings.TrimSpace(agentjson.Tail(raw, 300)))
	}
	for i := range v.Findings {
		v.Findings[i].TrackHistory = true
	}
	v.Degraded = false
	v.Raw = raw
	if !o.Security && len(criteria) > 0 {
		EnforceCriteria(&v, criteria)
		if normalized, err := json.Marshal(v); err == nil {
			// Persist the enforced verdict, not the reviewer's pre-enforcement
			// claim. Otherwise the artifact could say clean while the engine
			// correctly blocked missing/unsatisfied acceptance evidence.
			v.Raw = string(normalized)
		}
	}
	// Capture the full runtime envelope so Review can persist it for
	// audit/metrics beside the parsed verdict (koryph-qbc). res.Stdout is the raw
	// JSON output including usage and cost fields when the runtime reports them.
	v.Envelope = res.Stdout
	return v
}

// prepareReviewScratch gives every persisted review an invocation-owned
// mutable cache/temp root beside its engine-private evidence. Review runtimes
// must not inherit ambient compiler, XDG, or pre-commit caches: those defeat
// both phase isolation and terminal cleanup. Standalone PR review callers that
// intentionally request no persisted artifact retain the compatibility
// behavior of an empty scratch directory.
func prepareReviewScratch(o Opts) (string, error) {
	parent := strings.TrimSpace(o.ArtifactDir)
	if parent == "" && strings.TrimSpace(o.OutPath) != "" {
		parent = filepath.Dir(o.OutPath)
	}
	if parent == "" {
		return "", nil
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	scratch := filepath.Join(parent, ".runtime-scratch")
	if info, err := os.Lstat(scratch); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is not a real directory", scratch)
		}
		if err := os.Chmod(scratch, 0o700); err != nil {
			return "", err
		}
		return scratch, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Mkdir(scratch, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		// Concurrent review lanes share the engine-private artifact directory.
		// Another lane may create the scratch directory after Lstat and before
		// Mkdir. Revalidate the winner instead of turning that harmless race
		// into a retry that changes the content-addressed verdict.
		info, statErr := os.Lstat(scratch)
		if statErr != nil {
			return "", statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is not a real directory", scratch)
		}
		if err := os.Chmod(scratch, 0o700); err != nil {
			return "", err
		}
	}
	return scratch, nil
}

func parseReviewerVerdict(raw []byte, security bool) (Verdict, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Verdict{}, err
	}
	if _, ok := fields["blocking"]; !ok {
		return Verdict{}, errors.New(`required field "blocking" is missing`)
	}
	allowed := map[string]bool{
		"blocking":       true,
		"prior_findings": true,
		"findings":       true,
	}
	if !security {
		allowed["criteria"] = true
		allowed["security_review_required"] = true
		allowed["security_evidence"] = true
	}
	for key := range fields {
		if !allowed[key] {
			return Verdict{}, fmt.Errorf("field %q is not allowed in the %s review schema",
				key, reviewKind(security))
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var verdict Verdict
	if err := dec.Decode(&verdict); err != nil {
		return Verdict{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Verdict{}, errors.New("verdict contains trailing JSON")
		}
		return Verdict{}, err
	}
	if err := validateVerdictValue(verdict); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

func validateVerdictValue(verdict Verdict) error {
	for i, finding := range verdict.Findings {
		switch finding.Severity {
		case "blocking", "major", "minor":
		default:
			return fmt.Errorf("finding %d has invalid severity %q (want blocking, major, or minor)",
				i+1, finding.Severity)
		}
		if strings.TrimSpace(finding.Summary) == "" {
			return fmt.Errorf("finding %d has an empty summary", i+1)
		}
		if finding.Line < 0 {
			return fmt.Errorf("finding %d has a negative line number", i+1)
		}
	}
	return nil
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
func buildPrompt(
	branch, base, candidate, stat, names string,
	contract Contract,
	criteria []AcceptanceCriterion,
	prior []priorBlockingFinding,
	security bool,
) string {
	var b strings.Builder
	if security {
		b.WriteString("Perform a fail-closed security review of branch `")
	} else {
		b.WriteString("Review the branch `")
	}
	b.WriteString(branch)
	b.WriteString("` at exact candidate `")
	b.WriteString(candidate)
	b.WriteString("` against gated base `")
	b.WriteString(base)
	if security {
		b.WriteString("` for exploitable boundary, authentication, authorization, secret, signing, workflow-privilege, merge-enforcement, and cryptographic defects.\n\n")
	} else {
		b.WriteString("` for acceptance, correctness, regression, and scope defects. Identify whether a dedicated security review is required, but do not substitute a broad security audit for this acceptance review.\n\n")
	}

	if contract.ID != "" || contract.Title != "" || contract.Description != "" || len(criteria) > 0 {
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
		if !security && len(criteria) > 0 {
			evidenceByID := acceptanceEvidenceByID(contract.CompletionEvidence)
			b.WriteString("\n### Acceptance criteria\n")
			for _, criterion := range criteria {
				fmt.Fprintf(&b, "- %s: %s\n", criterion.ID, criterion.Text)
				b.WriteString("  - Authenticated worker evidence:\n")
				for _, ref := range evidenceByID[criterion.ID].References {
					switch strings.ToLower(strings.TrimSpace(ref.Kind)) {
					case "file":
						fmt.Fprintf(&b, "    - file `%s` (%s)\n",
							strings.TrimSpace(ref.Path), strings.TrimSpace(ref.Digest))
					case "focused-test":
						test := focusedTestByCommand(contract.CompletionEvidence, ref.Command)
						fmt.Fprintf(&b, "    - focused test `%s` (exit 0; log `%s`; %s)\n",
							strings.TrimSpace(ref.Command), strings.TrimSpace(test.LogPath),
							strings.TrimSpace(test.LogDigest))
					}
				}
			}
			b.WriteString("\nAudit the worker evidence independently against the exact gated candidate; " +
				"do not accept a path, digest, or focused-test claim without checking the relevant implementation.\n")
		}
		if security {
			b.WriteString("\nUse this scope only to identify trust boundaries and attack surface. " +
				"Do not repeat the acceptance audit or require criterion assessments.\n\n")
		} else {
			b.WriteString("\nReview the implementation against every criterion and the declared scope. " +
				"A locally correct diff in the wrong subsystem, a missing deliverable, or unexplained " +
				"footprint drift is blocking.\n\n")
		}
	}

	if len(prior) > 0 {
		b.WriteString("## Prior blocking findings\n")
		b.WriteString("This is a repair review. Re-evaluate every item independently and return resolved/unresolved evidence for each ID.\n")
		for _, item := range prior {
			fmt.Fprintf(&b, "- %s: %s", item.ID, item.Finding.Summary)
			if item.Finding.File != "" {
				fmt.Fprintf(&b, " (%s", item.Finding.File)
				if item.Finding.Line > 0 {
					fmt.Fprintf(&b, ":%d", item.Finding.Line)
				}
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
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
	if security {
		b.WriteString(`{"blocking": <bool>, "prior_findings": [{"id": "PF-<stable-id>", "status": "resolved|unresolved", "evidence": "<specific file/test/result>"}], "findings": [{"severity": "blocking|major|minor", "file": "<path>", "line": <1-based line or omit>, "summary": "<one line>"}]}`)
	} else {
		b.WriteString(`{"blocking": <bool>, "criteria": [{"id": "AC1", "status": "satisfied|unsatisfied|not-applicable", "evidence": "<specific file/test/result>"}], "prior_findings": [{"id": "PF-<stable-id>", "status": "resolved|unresolved", "evidence": "<specific file/test/result>"}], "security_review_required": <bool>, "security_evidence": "<why or empty>", "findings": [{"severity": "blocking|major|minor", "file": "<path>", "line": <1-based line or omit>, "summary": "<one line>"}]}`)
	}
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

func enforcePriorFindings(v *Verdict, prior []priorBlockingFinding) {
	if len(prior) == 0 {
		if len(v.PriorFindings) > 0 {
			v.Blocking = true
			v.Findings = append(v.Findings, Finding{
				Severity: "blocking",
				Summary:  "review returned prior-finding assessments when no prior blockers were supplied",
			})
		}
		return
	}
	seen := make(map[string]PriorFindingAssessment, len(v.PriorFindings))
	duplicates := make(map[string]bool)
	expected := make(map[string]bool, len(prior))
	for _, item := range prior {
		expected[canonicalPriorFindingID(item.ID)] = true
	}
	for _, assessment := range v.PriorFindings {
		id := canonicalPriorFindingID(assessment.ID)
		if _, exists := seen[id]; exists {
			duplicates[id] = true
		}
		seen[id] = assessment
		if !expected[id] {
			v.Blocking = true
			v.Findings = append(v.Findings, Finding{
				Severity: "blocking",
				Summary:  fmt.Sprintf("unexpected prior finding assessment %s", printableCriterionID(id)),
			})
		}
	}
	for id := range duplicates {
		v.Blocking = true
		v.Findings = append(v.Findings, Finding{
			Severity: "blocking",
			Summary:  id + " was evaluated more than once",
		})
	}
	for _, item := range prior {
		assessment, ok := seen[canonicalPriorFindingID(item.ID)]
		status := strings.ToLower(strings.TrimSpace(assessment.Status))
		if !ok || strings.TrimSpace(assessment.Evidence) == "" ||
			(status != "resolved" && status != "unresolved") {
			v.Blocking = true
			v.Findings = append(v.Findings, Finding{
				Severity: "blocking",
				Summary:  item.ID + " was not re-evaluated with resolution evidence",
			})
			continue
		}
		if status == "unresolved" {
			v.Blocking = true
		}
	}
}

func canonicalPriorFindingID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

func normalizeBlocking(v *Verdict) {
	hasActionableBlocker := false
	for i := range v.Findings {
		v.Findings[i].Severity = strings.ToLower(strings.TrimSpace(v.Findings[i].Severity))
		v.Findings[i].File = filepath.ToSlash(strings.TrimSpace(v.Findings[i].File))
		v.Findings[i].Summary = strings.TrimSpace(v.Findings[i].Summary)
		if blockingSeverity(v.Findings[i].Severity) {
			hasActionableBlocker = true
			v.Blocking = true
		}
	}
	if v.Blocking && !hasActionableBlocker {
		v.Findings = append(v.Findings, Finding{
			Severity: "blocking",
			Summary:  "reviewer marked the verdict blocking without an actionable blocking finding",
		})
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
