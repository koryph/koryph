// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package stage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/account"
)

const testIdentity = "test@example.com"

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// stageRepo builds a repo checked out on agent/x1 (one commit ahead of main),
// so a stage runs against a real diff.
func stageRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	gitIn(t, repo, "config", "user.name", "test")
	gitIn(t, repo, "config", "user.email", "test@example.com")
	gitIn(t, repo, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(repo, "README.md"), "seed\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--no-verify", "-m", "seed")
	gitIn(t, repo, "checkout", "-b", "agent/x1")
	writeFile(t, filepath.Join(repo, "feature.go"), "package x\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--no-verify", "-m", "feat(x1): change")
	return repo
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// idProfile writes a .claude.json under a fresh config dir and returns a
// matching profile.
func idProfile(t *testing.T, email string) account.Profile {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".claude.json"),
		`{"oauthAccount":{"emailAddress":"`+email+`","organizationName":"Test"}}`)
	return account.Profile{Name: "work", ConfigDir: dir}
}

// fakeStageClaude captures stdin to $KORYPH_TEST_STAGE_STDIN (when set), commits a stage
// file to prove write capability, prints a result envelope, and exits with
// exitCode.
func fakeStageClaude(t *testing.T, exitCode int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		"if [ -n \"$KORYPH_TEST_STAGE_STDIN\" ]; then cat > \"$KORYPH_TEST_STAGE_STDIN\"; else cat > /dev/null; fi\n" +
		"echo staged > STAGEFILE.txt\n" +
		"git add STAGEFILE.txt\n" +
		"git commit -q --no-verify -m 'docs(x1): stage work'\n" +
		"printf '{\"type\":\"result\",\"total_cost_usd\":0.11,\"result\":\"done\"}\\n'\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	writeFile(t, path, script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func baseStageOpts(t *testing.T, repo, bin string) Opts {
	t.Helper()
	return Opts{
		RepoRoot:         repo,
		Worktree:         repo,
		Branch:           "agent/x1",
		Base:             "main",
		Stage:            "docs",
		Persona:          "feature-docs-author",
		Model:            "sonnet",
		ExtraPrompt:      "Regenerate the API reference.",
		BeadID:           "x1",
		BeadTitle:        "Add the widget",
		Profile:          idProfile(t, testIdentity),
		ExpectedIdentity: testIdentity,
		Billing:          account.BillingSubscription,
		PhaseDir:         filepath.Join(t.TempDir(), "phase"),
		ClaudeBin:        bin,
	}
}

func TestRunStageCommitsAndReportsCost(t *testing.T) {
	repo := stageRepo(t)
	capture := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("KORYPH_TEST_STAGE_STDIN", capture)

	o := baseStageOpts(t, repo, fakeStageClaude(t, 0))
	r := Run(context.Background(), o)

	if !r.Ran || !r.OK {
		t.Fatalf("Result = %+v, want ran+ok", r)
	}
	if r.CostUSD != 0.11 {
		t.Errorf("CostUSD = %v, want 0.11", r.CostUSD)
	}

	// The stage committed its file on the branch.
	log := gitOut(t, repo, "log", "--format=%s", "main..agent/x1")
	if !strings.Contains(log, "docs(x1): stage work") {
		t.Errorf("stage commit missing from branch log:\n%s", log)
	}

	// The envelope was persisted for inspection.
	if _, err := os.Stat(filepath.Join(o.PhaseDir, "stage-docs.json")); err != nil {
		t.Errorf("stage envelope not persisted: %v", err)
	}

	// The prompt carried stage/bead context, boundary rules, and extra text.
	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("captured stdin: %v", err)
	}
	for _, want := range []string{"docs", "x1", "feature.go", "Do NOT", "Regenerate the API reference."} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// fakeStageClaudeEnvDump dumps the stage agent's environment to envCapture
// (via `env`) then prints a minimal result envelope and exits 0. No commit is
// made — this variant is only used to inspect the child env.
func fakeStageClaudeEnvDump(t *testing.T, envCapture string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude-env")
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		"env > " + envCapture + "\n" +
		"printf '{\"type\":\"result\",\"total_cost_usd\":0,\"result\":\"done\"}\\n'\n"
	writeFile(t, path, script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunStageThreadsProxyAndSpawnKind is the koryph-3l1.1 acceptance test
// for this spawn site: o.ProxyBaseURL reaches the stage agent's actual child
// env as ANTHROPIC_BASE_URL, and Run unconditionally stamps
// KORYPH_SPAWN_KIND=stage (its ChildEnvSpec literal).
func TestRunStageThreadsProxyAndSpawnKind(t *testing.T) {
	repo := stageRepo(t)
	envCapture := filepath.Join(t.TempDir(), "env.txt")

	o := baseStageOpts(t, repo, fakeStageClaudeEnvDump(t, envCapture))
	o.ProxyBaseURL = "http://127.0.0.1:8091"

	r := Run(context.Background(), o)
	if !r.Ran || !r.OK {
		t.Fatalf("Result = %+v, want ran+ok", r)
	}

	env, err := os.ReadFile(envCapture)
	if err != nil {
		t.Fatalf("read captured env: %v", err)
	}
	if !strings.Contains(string(env), "ANTHROPIC_BASE_URL=http://127.0.0.1:8091\n") {
		t.Errorf("captured env missing ANTHROPIC_BASE_URL:\n%s", env)
	}
	if !strings.Contains(string(env), "KORYPH_SPAWN_KIND=stage\n") {
		t.Errorf("captured env missing KORYPH_SPAWN_KIND=stage:\n%s", env)
	}
}

func TestRunStageNonZeroExitNotOK(t *testing.T) {
	repo := stageRepo(t)
	o := baseStageOpts(t, repo, fakeStageClaude(t, 3))
	r := Run(context.Background(), o)
	if !r.Ran {
		t.Errorf("Ran = false, want true")
	}
	if r.OK {
		t.Errorf("OK = true, want false on non-zero exit")
	}
	if !strings.Contains(r.Note, "exited 3") {
		t.Errorf("Note = %q, want it to mention exit 3", r.Note)
	}
}

// fakeStageClaudeSleep hangs for `seconds` so a small TimeoutSec kills it. It
// `exec`s sleep so the shell is replaced (no grandchild left holding the stdout
// pipe, which would make Wait block past the kill).
func fakeStageClaudeSleep(t *testing.T, seconds int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude-sleep")
	writeFile(t, path, "#!/bin/sh\nexec sleep "+strconv.Itoa(seconds)+"\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunStageTimeoutReportsTimedOut is the regression test for koryph-a59: a
// stage that exceeds its TimeoutSec is reported as TimedOut (not a plain
// failure), and its Note names the timeout — so the pipeline can tell "ran out
// of time" apart from "failed".
func TestRunStageTimeoutReportsTimedOut(t *testing.T) {
	repo := stageRepo(t)
	o := baseStageOpts(t, repo, fakeStageClaudeSleep(t, 60))
	o.TimeoutSec = 1
	start := time.Now()
	r := Run(context.Background(), o)
	if elapsed := time.Since(start); elapsed >= 30*time.Second {
		t.Fatalf("stage ran %v; expected the 1s timeout to kill it early", elapsed)
	}
	if r.OK {
		t.Errorf("OK = true, want false on timeout")
	}
	if !r.TimedOut {
		t.Errorf("TimedOut = false, want true — the stage exceeded its TimeoutSec")
	}
	if !strings.Contains(r.Note, "timed out") {
		t.Errorf("Note = %q, want it to mention the timeout", r.Note)
	}
}

func TestRunStageIdentityMismatchFailsClosed(t *testing.T) {
	repo := stageRepo(t)
	o := baseStageOpts(t, repo, fakeStageClaude(t, 0))
	o.ExpectedIdentity = "someone-else@example.com"
	r := Run(context.Background(), o)
	if r.Ran || r.OK {
		t.Errorf("Result = %+v, want not-ran, not-ok on identity mismatch", r)
	}
	if !strings.Contains(r.Note, "identity") {
		t.Errorf("Note = %q, want identity failure", r.Note)
	}
}

func TestRunStageMissingConfigRejected(t *testing.T) {
	repo := stageRepo(t)
	o := baseStageOpts(t, repo, fakeStageClaude(t, 0))
	o.Persona = "" // caller must resolve persona+model
	r := Run(context.Background(), o)
	if r.Ran || r.OK {
		t.Errorf("Result = %+v, want rejected for missing persona", r)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
