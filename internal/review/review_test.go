// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/phasecontrol"
)

// gitIn runs git in dir, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// reviewRepo builds a repo with main + an agent branch carrying one change.
func reviewRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	gitIn(t, repo, "config", "user.name", "test")
	gitIn(t, repo, "config", "user.email", "test@example.com")
	gitIn(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--no-verify", "-m", "seed")
	gitIn(t, repo, "checkout", "-b", "agent/x1")
	if err := os.WriteFile(filepath.Join(repo, "feature.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--no-verify", "-m", "feat(x1): change")
	gitIn(t, repo, "checkout", "main")
	return repo
}

// fakeClaude writes a script that captures stdin to $KORYPH_TEST_REVIEW_STDIN (when
// set) and prints body as its whole stdout.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		"if [ -n \"$KORYPH_TEST_REVIEW_STDIN\" ]; then cat > \"$KORYPH_TEST_REVIEW_STDIN\"; else cat > /dev/null; fi\n" +
		"cat <<'FAKE_EOF'\n" + body + "\nFAKE_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func baseOpts(t *testing.T, repo, claudeBin string) Opts {
	t.Helper()
	return Opts{
		RepoRoot:  repo,
		Worktree:  repo,
		Branch:    "agent/x1",
		Base:      "main",
		OutPath:   filepath.Join(t.TempDir(), "review.json"),
		ClaudeBin: claudeBin,
	}
}

func evidenceContract(criteria ...string) Contract {
	contract := Contract{}
	for i, text := range criteria {
		id := "AC" + strconv.Itoa(i+1)
		if i > 0 {
			contract.AcceptanceCriteria += "\n"
		}
		contract.AcceptanceCriteria += id + ": " + text
		contract.CompletionEvidence.Acceptance = append(
			contract.CompletionEvidence.Acceptance,
			phasecontrol.AcceptanceEvidence{
				CriterionID: id,
				References: []phasecontrol.EvidenceReference{{
					Kind: "file", Path: "feature.go", Digest: "sha256:" + strings.Repeat(strconv.Itoa(i+1), 64),
				}},
			},
		)
	}
	return contract
}

func TestReviewBlocking(t *testing.T) {
	repo := reviewRepo(t)
	envelope := `{"type":"result","is_error":false,"result":"{\"blocking\":true,\"findings\":[{\"severity\":\"blocking\",\"file\":\"feature.go\",\"summary\":\"hardcoded secret\"}]}"}`
	capture := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("KORYPH_TEST_REVIEW_STDIN", capture)

	o := baseOpts(t, repo, fakeClaude(t, envelope))
	v := Review(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}
	if !v.Blocking {
		t.Errorf("Blocking = false, want true")
	}
	if len(v.Findings) != 1 || v.Findings[0].Severity != "blocking" ||
		v.Findings[0].File != "feature.go" || v.Findings[0].Summary != "hardcoded secret" {
		t.Errorf("Findings = %+v", v.Findings)
	}

	// The normalized verdict is wrapped in an authenticated cumulative artifact.
	raw, err := os.ReadFile(o.OutPath)
	if err != nil {
		t.Fatalf("review.json: %v", err)
	}
	var artifact Artifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	if !artifact.Verdict.Blocking || len(artifact.History) != 1 {
		t.Errorf("review artifact = %+v", artifact)
	}

	// The prompt carried the diff context and the strict-JSON contract.
	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("captured stdin: %v", err)
	}
	for _, want := range []string{"feature.go", "agent/x1", "STRICT JSON", `"blocking"`} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestReviewNonBlockingWithProse(t *testing.T) {
	repo := reviewRepo(t)
	// Model text has prose around the JSON block; extraction must tolerate it.
	envelope := `{"type":"result","result":"Here is my verdict:\n{\"blocking\": false, \"findings\": [{\"severity\":\"minor\",\"summary\":\"nit: naming\"}]}\nDone."}`
	o := baseOpts(t, repo, fakeClaude(t, envelope))
	v := Review(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}
	if v.Blocking {
		t.Errorf("Blocking = true, want false")
	}
	if len(v.Findings) != 1 || v.Findings[0].Severity != "minor" {
		t.Errorf("Findings = %+v", v.Findings)
	}
	if _, err := os.Stat(o.OutPath); err != nil {
		t.Errorf("review.json not persisted: %v", err)
	}
}

func TestReviewContractOmissionFailsClosed(t *testing.T) {
	repo := reviewRepo(t)
	envelope := `{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`
	capture := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("KORYPH_TEST_REVIEW_STDIN", capture)
	o := baseOpts(t, repo, fakeClaude(t, envelope))
	o.Contract = evidenceContract(
		"Engine enforces the behavior",
		"Regression test passes",
	)
	o.Contract.ID = "bd-42"
	o.Contract.Title = "Implement the engine behavior"
	o.Contract.Description = "Change internal/engine, not an unrelated subsystem."
	o.Contract.Labels = []string{"fp:go:engine"}
	o.Contract.Runtime = "codex"
	o.Contract.CompletionState = "done"

	v := Review(context.Background(), o)
	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}
	if !v.Blocking {
		t.Fatal("review without criterion assessments was not blocked")
	}
	if len(v.Findings) != 2 {
		t.Fatalf("Findings=%+v, want one omission finding per criterion", v.Findings)
	}
	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bd-42", "Effective runtime: codex", "fp:go:engine", "AC1:", "AC2:", "wrong subsystem"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(string(prompt), "AC1: AC1:") ||
		!strings.Contains(string(prompt), "Authenticated worker evidence") {
		t.Errorf("prompt did not preserve canonical criteria/evidence:\n%s", prompt)
	}
}

func TestReviewUnsatisfiedCriterionIsBlocking(t *testing.T) {
	repo := reviewRepo(t)
	envelope := `{"type":"result","result":"{\"blocking\":false,\"criteria\":[{\"id\":\"AC1\",\"status\":\"satisfied\",\"evidence\":\"feature.go\"},{\"id\":\"AC2\",\"status\":\"unsatisfied\",\"evidence\":\"no regression test exists\"}],\"findings\":[]}"}`
	o := baseOpts(t, repo, fakeClaude(t, envelope))
	o.Contract = evidenceContract("Feature exists", "Regression test exists")

	v := Review(context.Background(), o)
	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}
	if !v.Blocking {
		t.Fatal("unsatisfied acceptance criterion did not force blocking")
	}
}

func TestReviewRejectsNonCanonicalCriteriaBeforeSpawn(t *testing.T) {
	repo := reviewRepo(t)
	capture := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("KORYPH_TEST_REVIEW_STDIN", capture)
	o := baseOpts(t, repo, fakeClaude(t,
		`{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`))
	o.Contract.AcceptanceCriteria = "- legacy unnumbered criterion"

	v := Review(context.Background(), o)
	if !v.Degraded || !strings.Contains(v.Reason, "canonical ID AC1") {
		t.Fatalf("non-canonical criteria verdict = %+v", v)
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatalf("reviewer spawned before contract validation: %v", err)
	}
}

func TestReviewRejectsIncompleteCompletionEvidenceBeforeSpawn(t *testing.T) {
	repo := reviewRepo(t)
	capture := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("KORYPH_TEST_REVIEW_STDIN", capture)
	o := baseOpts(t, repo, fakeClaude(t,
		`{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`))
	o.Contract = evidenceContract("feature exists", "regression exists")
	o.Contract.CompletionEvidence.Acceptance = o.Contract.CompletionEvidence.Acceptance[:1]

	v := Review(context.Background(), o)
	if !v.Degraded || !strings.Contains(v.Reason, "omits criterion AC2") {
		t.Fatalf("incomplete worker evidence verdict = %+v", v)
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatalf("reviewer spawned before evidence validation: %v", err)
	}
}

func TestSecurityReviewIsTargetedAndDoesNotRepeatAcceptanceAudit(t *testing.T) {
	repo := reviewRepo(t)
	capture := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("KORYPH_TEST_REVIEW_STDIN", capture)
	o := baseOpts(t, repo, fakeClaude(t,
		`{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`))
	o.Security = true
	// Security review deliberately ignores this invalid general-review
	// contract: the exact gated diff and security scope are its inputs.
	o.Contract = Contract{
		ID: "bd-security", Title: "security-sensitive change",
		Description:        "Touches an authentication boundary.",
		AcceptanceCriteria: "- deliberately legacy and missing evidence",
	}

	v := Review(context.Background(), o)
	if v.Degraded || v.Blocking {
		t.Fatalf("targeted security verdict = %+v", v)
	}
	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"### Acceptance criteria", `"criteria":`, "Authenticated worker evidence"} {
		if strings.Contains(string(prompt), unwanted) {
			t.Errorf("security prompt repeated general acceptance audit %q:\n%s", unwanted, prompt)
		}
	}
	if !strings.Contains(string(prompt), "Do not repeat the acceptance audit") {
		t.Errorf("security prompt lacks role boundary:\n%s", prompt)
	}
}

func TestReviewerVerdictSchemaRejectsUnknownFieldsAndSeverities(t *testing.T) {
	tests := []struct {
		name     string
		security bool
		result   string
		want     string
	}{
		{
			name:   "general critical severity",
			result: `{"blocking":true,"findings":[{"severity":"critical","summary":"unsafe"}]}`,
			want:   `invalid severity "critical"`,
		},
		{
			name:     "security high severity",
			security: true,
			result:   `{"blocking":true,"findings":[{"severity":"high","summary":"unsafe"}]}`,
			want:     `invalid severity "high"`,
		},
		{
			name:   "severity aliases are not normalized",
			result: `{"blocking":true,"findings":[{"severity":"Major","summary":"unsafe"}]}`,
			want:   `invalid severity "Major"`,
		},
		{
			name:     "security general field",
			security: true,
			result:   `{"blocking":false,"criteria":[],"findings":[]}`,
			want:     `field "criteria" is not allowed in the security review schema`,
		},
		{
			name:   "orchestrator field injection",
			result: `{"blocking":false,"degraded":false,"findings":[]}`,
			want:   `field "degraded" is not allowed`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := reviewRepo(t)
			envelopeRaw, err := json.Marshal(map[string]any{
				"type": "result", "result": tc.result,
			})
			if err != nil {
				t.Fatal(err)
			}
			o := baseOpts(t, repo, fakeClaude(t, string(envelopeRaw)))
			o.Security = tc.security
			o.Attempts = 1
			got := Review(context.Background(), o)
			if !got.Degraded || !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("verdict = %+v, want degraded reason containing %q", got, tc.want)
			}
		})
	}
}

func TestMajorFindingNormalizesBlockingAndEntersHistory(t *testing.T) {
	repo := reviewRepo(t)
	envelope := `{"type":"result","result":"{\"blocking\":false,\"findings\":[{\"severity\":\"major\",\"file\":\"feature.go\",\"summary\":\"regression\"}]}"}`
	v := Review(context.Background(), baseOpts(t, repo, fakeClaude(t, envelope)))
	if v.Degraded || !v.Blocking || len(v.BlockingHistory) != 1 {
		t.Fatalf("major finding verdict = %+v", v)
	}
}

func TestReviewPinsExactGatedCandidate(t *testing.T) {
	repo := reviewRepo(t)
	envelope := `{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`
	o := baseOpts(t, repo, fakeClaude(t, envelope))
	o.CandidateSHA = gitOutput(t, repo, "rev-parse", "agent/x1")
	o.BaseSHA = gitOutput(t, repo, "rev-parse", "main")

	gitIn(t, repo, "branch", "-f", "agent/x1", "main")
	v := Review(context.Background(), o)
	if !v.Degraded || !strings.Contains(v.Reason, "gated candidate moved") {
		t.Fatalf("moved gated candidate verdict = %+v", v)
	}
}

func TestReviewPinsWorktreeHEADAndCleanliness(t *testing.T) {
	envelope := `{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`

	t.Run("wrong HEAD", func(t *testing.T) {
		repo := reviewRepo(t)
		o := baseOpts(t, repo, fakeClaude(t, envelope))
		o.CandidateSHA = gitOutput(t, repo, "rev-parse", "agent/x1")
		o.BaseSHA = gitOutput(t, repo, "rev-parse", "main")
		// The branch still names CandidateSHA, but the reviewer would read files
		// from main unless the package checks the actual worktree HEAD.
		v := Review(context.Background(), o)
		if !v.Degraded || !strings.Contains(v.Reason, "worktree HEAD is not the gated candidate") {
			t.Fatalf("wrong worktree HEAD verdict = %+v", v)
		}
	})

	t.Run("dirty candidate", func(t *testing.T) {
		repo := reviewRepo(t)
		gitIn(t, repo, "checkout", "agent/x1")
		o := baseOpts(t, repo, fakeClaude(t, envelope))
		o.CandidateSHA = gitOutput(t, repo, "rev-parse", "agent/x1")
		o.BaseSHA = gitOutput(t, repo, "rev-parse", "main")
		if err := os.WriteFile(filepath.Join(repo, "untrusted.txt"), []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		v := Review(context.Background(), o)
		if !v.Degraded || !strings.Contains(v.Reason, "worktree is not clean") {
			t.Fatalf("dirty worktree verdict = %+v", v)
		}
	})
}

func TestRepairReviewRequiresEveryPriorBlockingFinding(t *testing.T) {
	repo := reviewRepo(t)
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	prior := `{"blocking":true,"findings":[{"severity":"blocking","file":"feature.go","line":1,"summary":"missing guard"}]}`
	if err := os.WriteFile(priorPath, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	missing := `{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`
	o := baseOpts(t, repo, fakeClaude(t, missing))
	o.PriorVerdictPath = priorPath
	v := Review(context.Background(), o)
	priorID, _ := findingIdentity(Finding{
		Severity: "blocking", File: "feature.go", Line: 1, Summary: "missing guard",
	})
	if v.Degraded || !v.Blocking ||
		!strings.Contains(v.Findings[len(v.Findings)-1].Summary, priorID) {
		t.Fatalf("omitted prior finding verdict = %+v", v)
	}

	resolved := `{"type":"result","result":"{\"blocking\":false,\"prior_findings\":[{\"id\":\"` +
		priorID + `\",\"status\":\"resolved\",\"evidence\":\"feature.go:1 plus TestGuard\"}],\"findings\":[]}"}`
	o = baseOpts(t, repo, fakeClaude(t, resolved))
	o.PriorVerdictPath = priorPath
	v = Review(context.Background(), o)
	if v.Degraded || v.Blocking {
		t.Fatalf("resolved prior finding verdict = %+v", v)
	}
}

// TestReviewImmuneToFrontendDiffTokens is the regression for the live bug: a
// Svelte-heavy diff makes the reviewer quote frontend template tokens ({@html},
// a {other_namespace%-*} glob, a raw {looks:like,json} snippet) in its prose
// before the real verdict. The old first-brace extraction latched onto {@html}
// and failed "verdict JSON invalid: {@html}"; the fenced, schema-anchored
// extraction must recover the true verdict cleanly.
func TestReviewImmuneToFrontendDiffTokens(t *testing.T) {
	repo := reviewRepo(t)
	// Model result: prose full of frontend brace tokens, then the fenced verdict.
	result := `The component renders {@html body} and matches {other_namespace%-*}; ` +
		`a raw {looks:like,json} appears too. Verdict follows:\n` +
		"```json\\n" +
		`{\"blocking\": true, \"findings\": [{\"severity\":\"blocking\",\"file\":\"App.svelte\",\"summary\":\"unescaped {@html} sink\"}]}` +
		"\\n```"
	envelope := `{"type":"result","is_error":false,"result":"` + result + `"}`

	o := baseOpts(t, repo, fakeClaude(t, envelope))
	o.Attempts = 1
	v := Review(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded on a frontend diff (the live bug): %+v", v)
	}
	if !v.Blocking {
		t.Errorf("Blocking = false, want true")
	}
	if len(v.Findings) != 1 || v.Findings[0].Severity != "blocking" || v.Findings[0].File != "App.svelte" {
		t.Errorf("Findings = %+v", v.Findings)
	}
}

func TestReviewGarbageOutputDegraded(t *testing.T) {
	repo := reviewRepo(t)
	o := baseOpts(t, repo, fakeClaude(t, "I am not JSON at all, sorry."))
	o.Attempts = 1
	v := Review(context.Background(), o)

	if !v.Degraded {
		t.Errorf("Degraded = false, want true for garbage output")
	}
	if v.Blocking {
		t.Errorf("Blocking = true; degraded verdicts must never block")
	}
	if v.Reason == "" {
		t.Errorf("degraded verdict must carry a Reason, got empty")
	}
}

func TestReviewMissingBinaryDegraded(t *testing.T) {
	repo := reviewRepo(t)
	o := baseOpts(t, repo, filepath.Join(t.TempDir(), "no-such-claude"))
	o.Attempts = 1
	v := Review(context.Background(), o)

	if !v.Degraded {
		t.Errorf("Degraded = false, want true for missing binary")
	}
	if v.Blocking {
		t.Errorf("Blocking = true; degraded verdicts must never block")
	}
	if v.Reason == "" {
		t.Errorf("degraded verdict must carry a Reason, got empty")
	}
}

// fakeClaudeFlaky writes a script that fails (exit 1) for its first failN
// invocations, then prints okBody. Invocation count persists in a temp file so
// it survives across the separate process spawns Review makes.
func fakeClaudeFlaky(t *testing.T, failN int, okBody string) string {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	path := filepath.Join(dir, "fake-claude-flaky")
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		"n=0; [ -f '" + counter + "' ] && n=$(cat '" + counter + "')\n" +
		"n=$((n+1)); echo $n > '" + counter + "'\n" +
		"if [ \"$n\" -le " + strconv.Itoa(failN) + " ]; then echo 'transient: rate limit' >&2; exit 1; fi\n" +
		"cat <<'FAKE_EOF'\n" + okBody + "\nFAKE_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeClaudeSleep writes a script that sleeps for `sleep` seconds (fractional
// ok, e.g. "1.3") after draining stdin, then prints okBody. Used to exercise
// the wall-clock timeout / escalation path: a short spawn deadline kills it
// mid-sleep, a longer one lets it finish.
func fakeClaudeSleep(t *testing.T, sleep, okBody string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude-sleep")
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		"sleep " + sleep + "\n" +
		"cat <<'FAKE_EOF'\n" + okBody + "\nFAKE_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveTimeout pins the unified single-timeout precedence (koryph-w82i):
// KORYPH_REVIEW_TIMEOUT_SEC env (break-glass) > the caller-supplied value >
// DefaultTimeoutSec. There is no longer a hard cap, so any level may exceed the
// default.
func TestResolveTimeout(t *testing.T) {
	cases := []struct {
		name       string
		env        string
		timeoutSec int
		want       int
	}{
		{"default", "", 0, DefaultTimeoutSec},
		{"caller value", "", 300, 300},
		{"caller may exceed default (no cap)", "", 5000, 5000},
		{"env overrides caller", "450", 300, 450},
		{"env may exceed default (no cap)", "5000", 0, 5000},
		{"env with no caller", "900", 0, 900},
		{"invalid env ignored", "nope", 300, 300},
		{"zero env ignored", "0", 300, 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KORYPH_REVIEW_TIMEOUT_SEC", tc.env)
			if got := resolveTimeout(tc.timeoutSec); got != tc.want {
				t.Errorf("resolveTimeout(%d) env=%q = %d, want %d", tc.timeoutSec, tc.env, got, tc.want)
			}
		})
	}
}

// TestReviewTimeoutDegrades verifies a persistent wall-clock timeout degrades
// after all attempts. Since koryph-w82i there is no escalation: every attempt
// runs at the same single timeout, and the degraded verdict is flagged TimedOut
// with a timeout-worded reason.
func TestReviewTimeoutDegrades(t *testing.T) {
	old := backoffUnit
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = old })
	t.Setenv("KORYPH_REVIEW_TIMEOUT_SEC", "")

	repo := reviewRepo(t)
	ok := `{"type":"result","is_error":false,"result":"{\"blocking\":false}"}`
	o := baseOpts(t, repo, fakeClaudeSleep(t, "2", ok))
	o.TimeoutSec = 1 // 1s < 2s sleep -> every attempt times out
	o.Attempts = 2
	v := Review(context.Background(), o)

	if !v.Degraded {
		t.Fatalf("want degraded after persistent timeout: %+v", v)
	}
	if !v.TimedOut {
		t.Errorf("degraded verdict from a timeout must set TimedOut")
	}
	if v.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (both attempts run at the single timeout)", v.Attempts)
	}
	if !strings.Contains(v.Reason, "timed out after") {
		t.Errorf("timeout-worded reason expected, got %q", v.Reason)
	}
}

func TestBackoffForExponentialAndCapped(t *testing.T) {
	oldU, oldM := backoffUnit, maxBackoff
	backoffUnit, maxBackoff = 2*time.Second, 30*time.Second
	t.Cleanup(func() { backoffUnit, maxBackoff = oldU, oldM })

	cases := []struct {
		retry int
		want  time.Duration
	}{
		{1, 2 * time.Second},    // base
		{2, 4 * time.Second},    // 2*base
		{3, 8 * time.Second},    // 4*base
		{4, 16 * time.Second},   // 8*base
		{5, 30 * time.Second},   // 32s -> capped
		{100, 30 * time.Second}, // shift overflow -> capped, never negative
	}
	for _, tc := range cases {
		if got := backoffFor(tc.retry); got != tc.want {
			t.Errorf("backoffFor(%d) = %s, want %s", tc.retry, got, tc.want)
		}
	}
}

func TestReviewRetriesTransientThenSucceeds(t *testing.T) {
	old := backoffUnit
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = old })

	repo := reviewRepo(t)
	ok := `{"type":"result","is_error":false,"result":"{\"blocking\":false,\"findings\":[]}"}`
	o := baseOpts(t, repo, fakeClaudeFlaky(t, 1, ok)) // fail once, then succeed
	o.Attempts = 3
	v := Review(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded after a transient failure that should have been retried: %+v", v)
	}
	if v.Blocking {
		t.Errorf("Blocking = true, want false")
	}
	if v.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (one transient failure + one success)", v.Attempts)
	}
	if _, err := os.Stat(o.OutPath); err != nil {
		t.Errorf("review.json not persisted after a successful retry: %v", err)
	}
}

func TestReviewDegradesAfterExhaustingAttempts(t *testing.T) {
	old := backoffUnit
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = old })

	repo := reviewRepo(t)
	ok := `{"type":"result","is_error":false,"result":"{\"blocking\":false}"}`
	o := baseOpts(t, repo, fakeClaudeFlaky(t, 5, ok)) // always fails within our budget
	o.Attempts = 3
	v := Review(context.Background(), o)

	if !v.Degraded {
		t.Fatalf("Degraded = false, want true after all attempts failed: %+v", v)
	}
	if v.Blocking {
		t.Errorf("Blocking = true; degraded verdicts must never block")
	}
	if v.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3 (all attempts consumed)", v.Attempts)
	}
	if v.Reason == "" {
		t.Errorf("degraded verdict must carry a Reason, got empty")
	}

	// koryph-5a1 #55: a full degrade must still leave a durable artifact
	// naming every attempt's own diagnosis, not just the last one.
	degradedPath := filepath.Join(filepath.Dir(o.OutPath), "review-degraded.json")
	data, err := os.ReadFile(degradedPath)
	if err != nil {
		t.Fatalf("review-degraded.json not persisted: %v", err)
	}
	var artifact struct {
		Degraded bool `json:"degraded"`
		Reason   string
		Attempts []struct {
			Attempt int
			Reason  string
		}
	}
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("review-degraded.json unmarshal: %v", err)
	}
	if !artifact.Degraded {
		t.Errorf("review-degraded.json degraded = false, want true")
	}
	if len(artifact.Attempts) != 3 {
		t.Errorf("review-degraded.json has %d attempt(s), want 3 (one per exhausted attempt)", len(artifact.Attempts))
	}
	for i, a := range artifact.Attempts {
		if a.Attempt != i+1 {
			t.Errorf("attempt[%d].Attempt = %d, want %d", i, a.Attempt, i+1)
		}
		if a.Reason == "" {
			t.Errorf("attempt[%d].Reason is empty, want a per-attempt diagnosis", i)
		}
	}
}

// TestReviewEnvelopePersisted verifies that the raw Claude JSON envelope is
// written to review-envelope.json beside review.json (koryph-qbc).
func TestReviewEnvelopePersisted(t *testing.T) {
	repo := reviewRepo(t)
	envelope := `{"type":"result","is_error":false,"result":"{\"blocking\":false,\"findings\":[]}","usage":{"input_tokens":100,"output_tokens":50},"total_cost_usd":0.001}`
	dir := t.TempDir()
	o := baseOpts(t, repo, fakeClaude(t, envelope))
	o.OutPath = filepath.Join(dir, "review.json")

	v := Review(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}

	// review-envelope.json must exist beside review.json.
	envPath := filepath.Join(dir, "review-envelope.json")
	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("review-envelope.json not persisted: %v", err)
	}
	// Must contain the full envelope (usage fields present).
	content := string(raw)
	for _, want := range []string{`"type":"result"`, `"usage"`, `"input_tokens"`} {
		if !strings.Contains(content, want) {
			t.Errorf("review-envelope.json missing %q:\n%s", want, content)
		}
	}

	// Envelope field on the returned Verdict must be populated.
	if v.Envelope == "" {
		t.Error("Verdict.Envelope must not be empty after a successful review")
	}
}

// TestReviewEnvelopeSkippedWithoutOutPath verifies that no panic or error
// occurs when OutPath is empty (PR-review path, no phase dir).
func TestReviewEnvelopeSkippedWithoutOutPath(t *testing.T) {
	repo := reviewRepo(t)
	envelope := `{"type":"result","is_error":false,"result":"{\"blocking\":false,\"findings\":[]}"}`
	o := baseOpts(t, repo, fakeClaude(t, envelope))
	o.OutPath = "" // PR-review path: no phase dir

	v := Review(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded with empty OutPath: %+v", v)
	}
	if v.Blocking {
		t.Errorf("Blocking = true, want false")
	}
}

func TestPrepareReviewScratchUsesPrivateArtifactSibling(t *testing.T) {
	artifactDir := filepath.Join(t.TempDir(), "evidence")
	scratch, err := prepareReviewScratch(Opts{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatalf("prepareReviewScratch: %v", err)
	}
	if want := filepath.Join(artifactDir, ".runtime-scratch"); scratch != want {
		t.Fatalf("scratch = %q, want %q", scratch, want)
	}
	info, err := os.Lstat(scratch)
	if err != nil {
		t.Fatalf("lstat scratch: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch mode = %v, want private real directory", info.Mode())
	}
}

func TestPrepareReviewScratchRejectsSymlinkLeaf(t *testing.T) {
	artifactDir := filepath.Join(t.TempDir(), "evidence")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(artifactDir, ".runtime-scratch")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareReviewScratch(Opts{ArtifactDir: artifactDir}); err == nil {
		t.Fatal("prepareReviewScratch accepted symlink scratch leaf")
	}
}

func TestReviewBadBranchDegraded(t *testing.T) {
	repo := reviewRepo(t)
	o := baseOpts(t, repo, fakeClaude(t, `{"type":"result","result":"{\"blocking\":false}"}`))
	o.Branch = "no/such/branch"
	v := Review(context.Background(), o)
	if !v.Degraded || v.Blocking {
		t.Errorf("verdict = %+v, want degraded non-blocking on git failure", v)
	}
}

// fakeClaudeEnvDump writes a script that dumps the reviewer's environment to
// envCapture (one KEY=value per line, via `env`) before printing body.
func fakeClaudeEnvDump(t *testing.T, envCapture, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude-env")
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		"env > " + envCapture + "\n" +
		"cat <<'FAKE_EOF'\n" + body + "\nFAKE_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReviewThreadsProxyAndSpawnKind is the koryph-3l1.1 acceptance test for
// this spawn site: o.ProxyBaseURL reaches the reviewer's actual child env as
// ANTHROPIC_BASE_URL, and the reviewer unconditionally stamps
// KORYPH_SPAWN_KIND=review (attemptReview's ChildEnvSpec literal).
func TestReviewThreadsProxyAndSpawnKind(t *testing.T) {
	repo := reviewRepo(t)
	envCapture := filepath.Join(t.TempDir(), "env.txt")
	o := baseOpts(t, repo, fakeClaudeEnvDump(t, envCapture, `{"type":"result","result":"{\"blocking\":false}"}`))
	o.ProxyBaseURL = "http://127.0.0.1:8091"

	v := Review(context.Background(), o)
	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}

	env, err := os.ReadFile(envCapture)
	if err != nil {
		t.Fatalf("read captured env: %v", err)
	}
	if !strings.Contains(string(env), "ANTHROPIC_BASE_URL=http://127.0.0.1:8091\n") {
		t.Errorf("captured env missing ANTHROPIC_BASE_URL:\n%s", env)
	}
	if !strings.Contains(string(env), "KORYPH_SPAWN_KIND=review\n") {
		t.Errorf("captured env missing KORYPH_SPAWN_KIND=review:\n%s", env)
	}
}
