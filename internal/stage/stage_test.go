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
