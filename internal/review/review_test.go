// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"context"
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

// fakeClaude writes a script that captures stdin to $REVIEW_STDIN_CAPTURE (when
// set) and prints body as its whole stdout.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		"if [ -n \"$REVIEW_STDIN_CAPTURE\" ]; then cat > \"$REVIEW_STDIN_CAPTURE\"; else cat > /dev/null; fi\n" +
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
	t.Setenv("REVIEW_STDIN_CAPTURE", capture)

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
