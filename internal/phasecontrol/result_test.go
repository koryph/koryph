// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package phasecontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/plan"
	"golang.org/x/sys/unix"
)

var resultCriteria = []plan.Criterion{{ID: "AC1", Text: "work is complete"}}

func resultFixture(t *testing.T) (CompleteOptions, string) {
	t.Helper()
	root := t.TempDir()
	runGitResult(t, root, "init", "-b", "main")
	runGitResult(t, root, "config", "user.email", "test@example.com")
	runGitResult(t, root, "config", "user.name", "Test")
	writeResultFile(t, filepath.Join(root, "base.txt"), "base\n")
	runGitResult(t, root, "add", "base.txt")
	runGitResult(t, root, "commit", "-m", "chore: base")
	base := runGitResult(t, root, "rev-parse", "HEAD")
	writeResultFile(t, filepath.Join(root, "work.txt"), "work\n")
	runGitResult(t, root, "add", "work.txt")
	runGitResult(t, root, "commit", "-m", "feat: work")

	phaseDir := t.TempDir()
	if err := os.MkdirAll(phaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(phaseDir, "SUMMARY.md")
	writeResultFile(t, summary, "completed work\n")
	logPath := filepath.Join(phaseDir, "focused.log")
	writeResultFile(t, logPath, "ok\n")
	evidencePath := filepath.Join(phaseDir, "evidence.json")
	evidence := Evidence{
		FocusedTests: []FocusedTestEvidence{{
			Command: "go test ./internal/example", ExitStatus: 0, LogPath: logPath,
		}},
		Acceptance: []AcceptanceEvidence{{
			CriterionID: "AC1",
			References:  []EvidenceReference{{Kind: "file", Path: "work.txt"}},
		}},
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	writeResultFile(t, evidencePath, string(data))
	return CompleteOptions{
		PhaseDir: phaseDir, Worktree: root, SummaryPath: summary, EvidencePath: evidencePath,
		Dispatch: DispatchContext{
			RunID: "run-1", PhaseID: "bead-1", Attempt: 1,
			SessionID: "session-1", BaseSHA: base,
		},
	}, root
}

func TestCompleteWritesSHAAndGenerationBoundResult(t *testing.T) {
	opts, _ := resultFixture(t)
	got, err := Complete(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "done" || got.CommitCount != 1 || !got.WorktreeClean {
		t.Fatalf("result = %+v", got)
	}
	if got.Generation != DispatchGeneration(opts.Dispatch) || got.CandidateSHA == got.BaseSHA {
		t.Fatalf("generation/candidate binding missing: %+v", got)
	}
	loaded, err := LoadResult(opts.PhaseDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Generation != got.Generation || loaded.EvidenceDigest == "" || loaded.SummaryDigest == "" {
		t.Fatalf("loaded result = %+v", loaded)
	}
	if err := ValidateResult(loaded, ValidationContext{
		PhaseDir: opts.PhaseDir, Worktree: opts.Worktree, Dispatch: opts.Dispatch,
		CandidateSHA: got.CandidateSHA, CommitCount: 1, WorktreeClean: true,
		ExpectedCriteria: resultCriteria,
	}); err != nil {
		t.Fatalf("ValidateResult: %v", err)
	}
}

func TestCompleteRejectsDirtyOrCommitlessCandidate(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		opts, root := resultFixture(t)
		writeResultFile(t, filepath.Join(root, "dirty.txt"), "dirty\n")
		if _, err := Complete(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "not clean") {
			t.Fatalf("Complete error = %v", err)
		}
		if _, err := os.Stat(ResultPath(opts.PhaseDir)); !os.IsNotExist(err) {
			t.Fatalf("terminal result written for dirty worktree: %v", err)
		}
	})
	t.Run("commitless", func(t *testing.T) {
		opts, root := resultFixture(t)
		runGitResult(t, root, "reset", "--hard", opts.Dispatch.BaseSHA)
		if _, err := Complete(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "no commits") {
			t.Fatalf("Complete error = %v", err)
		}
	})
}

func TestCompleteRejectsDivergedCandidateAndEscapingEvidence(t *testing.T) {
	t.Run("dispatch base is not ancestor", func(t *testing.T) {
		opts, root := resultFixture(t)
		runGitResult(t, root, "checkout", "--orphan", "unrelated")
		runGitResult(t, root, "rm", "-rf", ".")
		writeResultFile(t, filepath.Join(root, "unrelated.txt"), "unrelated\n")
		runGitResult(t, root, "add", "unrelated.txt")
		runGitResult(t, root, "commit", "-m", "feat: unrelated")
		if _, err := Complete(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "not an ancestor") {
			t.Fatalf("Complete error = %v", err)
		}
	})
	t.Run("symlink escapes allowed roots", func(t *testing.T) {
		opts, _ := resultFixture(t)
		outside := filepath.Join(t.TempDir(), "outside.json")
		writeResultFile(t, outside, `{"focused_tests":[{"command":"go test ./...","exit_status":0,"log_path":"x"}]}`)
		if err := os.Remove(opts.EvidencePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, opts.EvidencePath); err != nil {
			t.Fatal(err)
		}
		if _, err := Complete(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("Complete error = %v", err)
		}
	})
	t.Run("symlinked parent is rejected even when target stays inside root", func(t *testing.T) {
		opts, _ := resultFixture(t)
		realDir := filepath.Join(opts.PhaseDir, "real-evidence")
		if err := os.Mkdir(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		realEvidence := filepath.Join(realDir, "evidence.json")
		data, err := os.ReadFile(opts.EvidencePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(realEvidence, data, 0o644); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(opts.PhaseDir, "worker-swapped-parent")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		opts.EvidencePath = filepath.Join(linkDir, "evidence.json")
		if _, err := Complete(context.Background(), opts); err == nil {
			t.Fatal("Complete accepted evidence through a symlinked parent")
		}
	})
}

func TestCompleteRejectsUnsafeOrOversizedEvidenceWithoutBlocking(t *testing.T) {
	t.Run("fifo", func(t *testing.T) {
		opts, _ := resultFixture(t)
		if err := os.Remove(opts.EvidencePath); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(opts.EvidencePath, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := Complete(context.Background(), opts)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "regular") {
				t.Fatalf("Complete error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Complete blocked opening FIFO evidence")
		}
	})
	t.Run("directory", func(t *testing.T) {
		opts, _ := resultFixture(t)
		opts.EvidencePath = opts.PhaseDir
		if _, err := Complete(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "regular") {
			t.Fatalf("Complete error = %v", err)
		}
	})
	t.Run("hard size ceiling", func(t *testing.T) {
		opts, _ := resultFixture(t)
		if err := os.WriteFile(opts.EvidencePath, bytes.Repeat([]byte("x"), maxEvidenceBytes+1), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Complete(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("Complete error = %v", err)
		}
	})
}

func TestLoadResultRejectsFIFOWithoutBlocking(t *testing.T) {
	phaseDir := t.TempDir()
	if err := unix.Mkfifo(ResultPath(phaseDir), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := LoadResult(phaseDir)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular") {
			t.Fatalf("LoadResult error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("LoadResult blocked opening FIFO")
	}
}

func TestCompleteRejectsUnauthenticatedAcceptanceReferences(t *testing.T) {
	t.Run("unknown focused test", func(t *testing.T) {
		opts, _ := resultFixture(t)
		logPath := filepath.Join(opts.PhaseDir, "focused.log")
		evidence := Evidence{
			FocusedTests: []FocusedTestEvidence{{
				Command: "go test ./internal/example", LogPath: logPath,
			}},
			Acceptance: []AcceptanceEvidence{{
				CriterionID: "AC1",
				References: []EvidenceReference{{
					Kind: "focused-test", Command: "go test ./other",
				}},
			}},
		}
		data, _ := json.Marshal(evidence)
		writeResultFile(t, opts.EvidencePath, string(data))
		if _, err := Complete(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "unknown or unsuccessful") {
			t.Fatalf("Complete error = %v", err)
		}
	})
	t.Run("file outside authenticated roots", func(t *testing.T) {
		opts, _ := resultFixture(t)
		outside := filepath.Join(t.TempDir(), "outside.txt")
		writeResultFile(t, outside, "outside\n")
		evidence := Evidence{Acceptance: []AcceptanceEvidence{{
			CriterionID: "AC1",
			References:  []EvidenceReference{{Kind: "file", Path: outside}},
		}}}
		data, _ := json.Marshal(evidence)
		writeResultFile(t, opts.EvidencePath, string(data))
		if _, err := Complete(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("Complete error = %v", err)
		}
	})
}

func TestValidateResultRejectsStaleIdentityAndTampering(t *testing.T) {
	opts, _ := resultFixture(t)
	result, err := Complete(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := ValidationContext{
		PhaseDir: opts.PhaseDir, Worktree: opts.Worktree, Dispatch: opts.Dispatch,
		CandidateSHA: result.CandidateSHA, CommitCount: result.CommitCount, WorktreeClean: true,
		ExpectedCriteria: resultCriteria,
	}
	stale := want
	stale.Dispatch.SessionID = "next-generation"
	if err := ValidateResult(result, stale); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("stale generation error = %v", err)
	}
	writeResultFile(t, result.SummaryPath, "tampered\n")
	if err := ValidateResult(result, want); err == nil || !strings.Contains(err.Error(), "summary digest") {
		t.Fatalf("tampered summary error = %v", err)
	}
}

func TestValidateResultRequiresExactAcceptanceMatrixAndAuthenticatedReferences(t *testing.T) {
	opts, root := resultFixture(t)
	result, err := Complete(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := ValidationContext{
		PhaseDir: opts.PhaseDir, Worktree: opts.Worktree, Dispatch: opts.Dispatch,
		CandidateSHA: result.CandidateSHA, CommitCount: result.CommitCount, WorktreeClean: true,
		ExpectedCriteria: resultCriteria,
	}
	if err := ValidateResult(result, want); err != nil {
		t.Fatal(err)
	}

	missing := want
	missing.ExpectedCriteria = append(resultCriteria, plan.Criterion{ID: "AC2", Text: "second"})
	if err := ValidateResult(result, missing); err == nil || !strings.Contains(err.Error(), "want 2") {
		t.Fatalf("missing criterion error = %v", err)
	}

	writeResultFile(t, filepath.Join(root, "work.txt"), "tampered\n")
	if err := ValidateResult(result, want); err == nil || !strings.Contains(err.Error(), "evidence") {
		t.Fatalf("tampered referenced file error = %v", err)
	}
}

func TestDispatchGenerationSeparatesAttemptsAndSessions(t *testing.T) {
	base := DispatchContext{RunID: "run", PhaseID: "phase", Attempt: 1, SessionID: "one", BaseSHA: "abc"}
	cases := []DispatchContext{
		{RunID: "other", PhaseID: "phase", Attempt: 1, SessionID: "one", BaseSHA: "abc"},
		{RunID: "run", PhaseID: "other", Attempt: 1, SessionID: "one", BaseSHA: "abc"},
		{RunID: "run", PhaseID: "phase", Attempt: 2, SessionID: "one", BaseSHA: "abc"},
		{RunID: "run", PhaseID: "phase", Attempt: 1, SessionID: "two", BaseSHA: "abc"},
		{RunID: "run", PhaseID: "phase", Attempt: 1, SessionID: "one", BaseSHA: "def"},
	}
	for _, other := range cases {
		if DispatchGeneration(base) == DispatchGeneration(other) {
			t.Fatalf("generation collision for %+v", other)
		}
	}
}

func runGitResult(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeResultFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
