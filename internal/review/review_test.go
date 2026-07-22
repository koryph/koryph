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
		Persona:   "security-reviewer",
		Model:     "opus",
		OutPath:   filepath.Join(t.TempDir(), "review.json"),
		ClaudeBin: claudeBin,
	}
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

	// Raw verdict persisted to OutPath.
	raw, err := os.ReadFile(o.OutPath)
	if err != nil {
		t.Fatalf("review.json: %v", err)
	}
	if !strings.Contains(string(raw), `"blocking":true`) {
		t.Errorf("review.json = %q", raw)
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

func TestEscalateTimeout(t *testing.T) {
	cases := []struct{ cur, max, want int }{
		{600, 1200, 1200},     // doubles to the ceiling
		{300, 1200, 600},      // room to grow
		{700, 1200, 1200},     // 1400 -> capped
		{1200, 1200, 1200},    // already at ceiling
		{1 << 40, 1200, 1200}, // overflow-safe -> capped
	}
	for _, tc := range cases {
		if got := escalateTimeout(tc.cur, tc.max); got != tc.want {
			t.Errorf("escalateTimeout(%d, %d) = %d, want %d", tc.cur, tc.max, got, tc.want)
		}
	}
}

// TestResolveTimeouts pins the precedence and the 20-minute hard cap: env >
// caller/project value > default for the start, the ceiling defaults to and is
// clamped down to MaxTimeoutSec, and the start is clamped to the ceiling — so no
// source can produce a spawn longer than 20 minutes.
func TestResolveTimeouts(t *testing.T) {
	cases := []struct {
		name               string
		env                string
		timeoutSec         int
		maxTimeoutSec      int
		wantStart, wantMax int
	}{
		{"defaults", "", 0, 0, defaultTimeoutSec, MaxTimeoutSec},
		{"config start", "", 300, 0, 300, MaxTimeoutSec},
		{"config max lower", "", 0, 900, defaultTimeoutSec, 900},
		{"env overrides config start", "450", 300, 0, 450, MaxTimeoutSec},
		{"env clamped to hard cap", "5000", 0, 0, MaxTimeoutSec, MaxTimeoutSec},
		{"config start clamped to hard cap", "", 5000, 0, MaxTimeoutSec, MaxTimeoutSec},
		{"config max clamped to hard cap", "", 0, 5000, defaultTimeoutSec, MaxTimeoutSec},
		{"start clamped to configured max", "", 800, 500, 500, 500},
		{"env clamped to configured max", "800", 0, 500, 500, 500},
		{"invalid env ignored", "nope", 300, 0, 300, MaxTimeoutSec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KORYPH_REVIEW_TIMEOUT_SEC", tc.env)
			start, max := resolveTimeouts(tc.timeoutSec, tc.maxTimeoutSec)
			if start != tc.wantStart || max != tc.wantMax {
				t.Errorf("resolveTimeouts(%d, %d) env=%q = (%d, %d), want (%d, %d)",
					tc.timeoutSec, tc.maxTimeoutSec, tc.env, start, max, tc.wantStart, tc.wantMax)
			}
			if max > MaxTimeoutSec || start > MaxTimeoutSec {
				t.Errorf("resolved (%d, %d) exceeds the %ds hard cap", start, max, MaxTimeoutSec)
			}
		})
	}
}

// TestReviewEscalatesTimeoutThenSucceeds is the acceptance test for the loop
// reacting to a timeout by increasing it: attempt 1 (1s deadline) is killed
// mid-sleep, the loop escalates to 2s, and attempt 2 finishes the same 1.3s
// spawn cleanly. Proves the second attempt got room the first didn't.
func TestReviewEscalatesTimeoutThenSucceeds(t *testing.T) {
	old := backoffUnit
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = old })
	t.Setenv("KORYPH_REVIEW_TIMEOUT_SEC", "") // no break-glass override

	repo := reviewRepo(t)
	ok := `{"type":"result","is_error":false,"result":"{\"blocking\":false,\"findings\":[]}"}`
	o := baseOpts(t, repo, fakeClaudeSleep(t, "1.3", ok))
	o.TimeoutSec = 1 // attempt 1: 1s < 1.3s sleep -> timeout; escalates to 2s
	o.Attempts = 3
	v := Review(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded; escalation should have given attempt 2 enough time: %+v", v)
	}
	if v.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (timeout, then escalated success)", v.Attempts)
	}
}

// TestReviewTimeoutAtCeilingDegrades verifies the 20-minute cap is a hard floor
// on escalation: with the start already AT the ceiling, a persistent timeout
// does not escalate further and degrades with the ceiling-worded reason.
func TestReviewTimeoutAtCeilingDegrades(t *testing.T) {
	old := backoffUnit
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = old })
	t.Setenv("KORYPH_REVIEW_TIMEOUT_SEC", "")

	repo := reviewRepo(t)
	ok := `{"type":"result","is_error":false,"result":"{\"blocking\":false}"}`
	o := baseOpts(t, repo, fakeClaudeSleep(t, "2", ok))
	o.TimeoutSec = 1
	o.MaxTimeoutSec = 1 // start clamps to 1 == ceiling: no room to escalate
	o.Attempts = 2
	v := Review(context.Background(), o)

	if !v.Degraded {
		t.Fatalf("want degraded after persistent timeout at the ceiling: %+v", v)
	}
	if !v.TimedOut {
		t.Errorf("degraded verdict from a timeout must set TimedOut")
	}
	if v.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (both attempts run at the ceiling)", v.Attempts)
	}
	if !strings.Contains(v.Reason, "per-task ceiling") {
		t.Errorf("ceiling-worded reason expected, got %q", v.Reason)
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
