// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/phasecontrol"
)

func TestPhaseLabelAddRequiresDispatchEnvironment(t *testing.T) {
	t.Setenv("KORYPH_PHASE_DIR", "")
	t.Setenv("KORYPH_DIR", "")
	t.Setenv("KORYPH_PHASE_ID", "")
	code, _, errb := runCmd("phase", "request", "label-add", "--label", "area:docs", "--timeout", "1ms")
	if code != engine.ExitFatal || errb == "" {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
}

func TestPhaseLabelAddRejectsControlLabel(t *testing.T) {
	code, _, _ := runCmd("phase", "request", "label-add", "--label", "model:frontier")
	if code != engine.ExitUsage {
		t.Fatalf("code=%d, want usage", code)
	}
}

func TestPhaseBlockWritesStructuredStatus(t *testing.T) {
	dir := t.TempDir()
	status := filepath.Join(dir, "status.json")
	if err := os.WriteFile(status, []byte(`{"state":"running","step":"work","pct":25}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_DIR", dir)
	t.Setenv("KORYPH_PHASE_ID", "bead-1")
	t.Setenv("KORYPH_STATUS_PATH", status)
	code, out, errb := runCmd("phase", "block", "--capability", "runtime-canary", "--detail", "profile unavailable")
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errb)
	}
	data, err := os.ReadFile(status)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || !containsAll(string(data), `"block_kind": "capability"`, `"capability": "runtime-canary"`) {
		t.Fatalf("status=%s", data)
	}
}

func TestPhaseCompleteDerivesTerminalManifestFromDispatchAndGit(t *testing.T) {
	repo := t.TempDir()
	phaseDir := t.TempDir()
	phaseGit(t, repo, "init", "-b", "main")
	phaseGit(t, repo, "config", "user.email", "test@example.com")
	phaseGit(t, repo, "config", "user.name", "Test")
	phaseWrite(t, filepath.Join(repo, "base.txt"), "base\n")
	phaseGit(t, repo, "add", "base.txt")
	phaseGit(t, repo, "commit", "-m", "chore: base")
	base := phaseGit(t, repo, "rev-parse", "HEAD")
	phaseWrite(t, filepath.Join(repo, "work.txt"), "work\n")
	phaseGit(t, repo, "add", "work.txt")
	phaseGit(t, repo, "commit", "-m", "feat: work")

	dispatch := ledger.Manifest{
		BeadID: "bead-1", WorktreePath: repo, BaseCommit: base,
		Attempt: 2, SessionID: "session-2",
	}
	dispatch.DispatchGeneration = phasecontrol.DispatchGeneration(phasecontrol.DispatchContext{
		RunID: "run-1", PhaseID: dispatch.BeadID, Attempt: dispatch.Attempt,
		SessionID: dispatch.SessionID, BaseSHA: dispatch.BaseCommit,
	})
	dispatchJSON, err := json.Marshal(dispatch)
	if err != nil {
		t.Fatal(err)
	}
	phaseWrite(t, filepath.Join(phaseDir, "manifest.json"), string(dispatchJSON))
	summary := filepath.Join(phaseDir, "SUMMARY.md")
	phaseWrite(t, summary, "done\n")
	testLog := filepath.Join(phaseDir, "focused.log")
	phaseWrite(t, testLog, "ok\n")
	evidencePath := filepath.Join(phaseDir, "evidence.json")
	evidenceJSON, _ := json.Marshal(phasecontrol.Evidence{
		FocusedTests: []phasecontrol.FocusedTestEvidence{{
			Command: "go test ./internal/example", ExitStatus: 0, LogPath: testLog,
		}},
		Acceptance: []phasecontrol.AcceptanceEvidence{{
			CriterionID: "AC1",
			References: []phasecontrol.EvidenceReference{{
				Kind: "file", Path: filepath.Join(repo, "work.txt"),
			}},
		}},
	})
	phaseWrite(t, evidencePath, string(evidenceJSON))

	t.Setenv("KORYPH_PHASE_DIR", phaseDir)
	t.Setenv("KORYPH_PHASE_ID", "bead-1")
	t.Setenv("KORYPH_RUN_ID", "run-1")
	t.Setenv("KORYPH_SUMMARY_PATH", summary)
	code, out, errb := runCmd("phase", "complete", "--evidence", evidencePath)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errb)
	}
	result, err := phasecontrol.LoadResult(phaseDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID != "run-1" || result.PhaseID != "bead-1" || result.Attempt != 2 ||
		result.Generation == "" || result.CandidateSHA == base {
		t.Fatalf("result = %+v", result)
	}
}

func TestPhaseCompleteRejectsDirtyCandidateWithoutResult(t *testing.T) {
	repo := t.TempDir()
	phaseDir := t.TempDir()
	phaseGit(t, repo, "init", "-b", "main")
	phaseGit(t, repo, "config", "user.email", "test@example.com")
	phaseGit(t, repo, "config", "user.name", "Test")
	phaseWrite(t, filepath.Join(repo, "base.txt"), "base\n")
	phaseGit(t, repo, "add", "base.txt")
	phaseGit(t, repo, "commit", "-m", "chore: base")
	base := phaseGit(t, repo, "rev-parse", "HEAD")
	phaseWrite(t, filepath.Join(repo, "work.txt"), "work\n")
	phaseGit(t, repo, "add", "work.txt")
	phaseGit(t, repo, "commit", "-m", "feat: work")
	phaseWrite(t, filepath.Join(repo, "dirty.txt"), "dirty\n")

	dispatch := ledger.Manifest{
		BeadID: "bead-1", WorktreePath: repo, BaseCommit: base, Attempt: 1, SessionID: "session-1",
	}
	dispatch.DispatchGeneration = phasecontrol.DispatchGeneration(phasecontrol.DispatchContext{
		RunID: "run-1", PhaseID: dispatch.BeadID, Attempt: dispatch.Attempt,
		SessionID: dispatch.SessionID, BaseSHA: dispatch.BaseCommit,
	})
	dispatchJSON, _ := json.Marshal(dispatch)
	phaseWrite(t, filepath.Join(phaseDir, "manifest.json"), string(dispatchJSON))
	summary := filepath.Join(phaseDir, "SUMMARY.md")
	phaseWrite(t, summary, "done\n")
	logPath := filepath.Join(phaseDir, "focused.log")
	phaseWrite(t, logPath, "ok\n")
	evidencePath := filepath.Join(phaseDir, "evidence.json")
	evidenceJSON, _ := json.Marshal(phasecontrol.Evidence{
		FocusedTests: []phasecontrol.FocusedTestEvidence{{Command: "go test ./x", LogPath: logPath}},
		Acceptance: []phasecontrol.AcceptanceEvidence{{
			CriterionID: "AC1",
			References:  []phasecontrol.EvidenceReference{{Kind: "file", Path: filepath.Join(repo, "work.txt")}},
		}},
	})
	phaseWrite(t, evidencePath, string(evidenceJSON))
	t.Setenv("KORYPH_PHASE_DIR", phaseDir)
	t.Setenv("KORYPH_PHASE_ID", "bead-1")
	t.Setenv("KORYPH_RUN_ID", "run-1")
	t.Setenv("KORYPH_SUMMARY_PATH", summary)

	code, _, errb := runCmd("phase", "complete", "--evidence", evidencePath)
	if code != engine.ExitFatal || !strings.Contains(errb, "not clean") {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
	if _, err := os.Stat(phasecontrol.ResultPath(phaseDir)); !os.IsNotExist(err) {
		t.Fatalf("result exists for dirty candidate: %v", err)
	}
}

func containsAll(s string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(s, value) {
			return false
		}
	}
	return true
}

func phaseGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func phaseWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
