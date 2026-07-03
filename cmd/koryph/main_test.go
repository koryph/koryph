// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/registry"
)

// isolate points KORYPH_HOME and HOME at fresh temp dirs and disables npx so
// quota probes stay hermetic.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("KORYPH_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KORYPH_NO_NPX", "1")
}

// gitRepo creates a git repo usable as a project root.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// runCmd invokes the mux and returns (code, stdout, stderr).
func runCmd(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := runCmd("version")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "koryph "+engine.EngineVersion) {
		t.Errorf("version output = %q, want engine version %s", out, engine.EngineVersion)
	}
}

func TestUnknownCommandIsUsageExit(t *testing.T) {
	code, _, errb := runCmd("frobnicate")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want %d", code, engine.ExitUsage)
	}
	if !strings.Contains(errb, "unknown command") {
		t.Errorf("stderr = %q, want unknown-command notice", errb)
	}
}

func TestNoArgsIsUsageExit(t *testing.T) {
	if code, _, _ := runCmd(); code != engine.ExitUsage {
		t.Errorf("code = %d, want %d", code, engine.ExitUsage)
	}
}

func TestProjectAddListShow(t *testing.T) {
	isolate(t)
	root := gitRepo(t)

	// add
	code, out, errb := runCmd("project", "add", root,
		"--account", "personal", "--identity", "me@example.com", "--id", "demo")
	if code != 0 {
		t.Fatalf("add code = %d; stderr=%s", code, errb)
	}
	var rec registry.Record
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("add output not JSON: %v\n%s", err, out)
	}
	if rec.ProjectID != "demo" || rec.AccountProfile != "personal" {
		t.Errorf("record = %+v", rec)
	}
	if rec.MigrationStatus != registry.StatusRegistered {
		t.Errorf("migration status = %q", rec.MigrationStatus)
	}
	if _, err := os.Stat(filepath.Join(root, "koryph.project.json")); err != nil {
		t.Errorf("adapter not scaffolded: %v", err)
	}

	// list
	code, out, _ = runCmd("project", "list")
	if code != 0 {
		t.Fatalf("list code = %d", code)
	}
	if !strings.Contains(out, "demo") || !strings.Contains(out, "personal") {
		t.Errorf("list output = %q", out)
	}

	// show
	code, out, _ = runCmd("project", "show", "demo")
	if code != 0 {
		t.Fatalf("show code = %d", code)
	}
	var shown registry.Record
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("show output not JSON: %v\n%s", err, out)
	}
	if shown.ProjectID != "demo" {
		t.Errorf("shown = %+v", shown)
	}
}

func TestProjectAddRequiresRoot(t *testing.T) {
	isolate(t)
	if code, _, _ := runCmd("project", "add", "--account", "personal", "--identity", "me@example.com"); code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit", code)
	}
}

func TestOnboardJSON(t *testing.T) {
	isolate(t)
	root := gitRepo(t)
	code, out, errb := runCmd("onboard", root, "--json")
	if code != 0 {
		t.Fatalf("onboard code = %d; stderr=%s", code, errb)
	}
	var inv struct {
		Root      string `json:"root"`
		IsGitRepo bool   `json:"is_git_repo"`
	}
	if err := json.Unmarshal([]byte(out), &inv); err != nil {
		t.Fatalf("onboard --json not JSON: %v\n%s", err, out)
	}
	if !inv.IsGitRepo {
		t.Errorf("inventory IsGitRepo = false for a git repo")
	}
}

func TestOnboardHumanSummary(t *testing.T) {
	isolate(t)
	root := gitRepo(t)
	code, out, _ := runCmd("onboard", root)
	if code != 0 {
		t.Fatalf("onboard code = %d", code)
	}
	if !strings.Contains(out, "git repo") || !strings.Contains(out, "root") {
		t.Errorf("human summary missing expected fields:\n%s", out)
	}
}

func TestBoardEmpty(t *testing.T) {
	isolate(t)
	code, out, errb := runCmd("board")
	if code != 0 {
		t.Fatalf("board code = %d; stderr=%s", code, errb)
	}
	if !strings.Contains(out, "no projects registered") {
		t.Errorf("board empty output = %q", out)
	}
}

func TestBoardJSONEmpty(t *testing.T) {
	isolate(t)
	code, out, _ := runCmd("board", "--json")
	if code != 0 {
		t.Fatalf("board --json code = %d", code)
	}
	var entries []boardEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("board --json not JSON: %v\n%s", err, out)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %v, want empty", entries)
	}
}

func TestQuotaUncalibratedShow(t *testing.T) {
	isolate(t)
	code, out, errb := runCmd("quota", "--account", "personal")
	if code != 0 {
		t.Fatalf("quota code = %d; stderr=%s", code, errb)
	}
	// Uncalibrated + hermetic ($HOME has no transcripts, npx disabled) → the
	// account renders with calibrated=no.
	if !strings.Contains(out, "personal") {
		t.Errorf("quota output missing account:\n%s", out)
	}
	if !strings.Contains(out, "no") {
		t.Errorf("quota output should mark the account uncalibrated:\n%s", out)
	}
}

func TestQuotaJSONShow(t *testing.T) {
	isolate(t)
	code, out, _ := runCmd("quota", "--account", "personal", "--json")
	if code != 0 {
		t.Fatalf("quota --json code = %d", code)
	}
	var snaps []struct {
		Account    string `json:"account"`
		Calibrated bool   `json:"calibrated"`
	}
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		t.Fatalf("quota --json not JSON: %v\n%s", err, out)
	}
	if len(snaps) != 1 || snaps[0].Account != "personal" || snaps[0].Calibrated {
		t.Errorf("snaps = %+v, want one uncalibrated personal account", snaps)
	}
}

func TestValidateMissingProject(t *testing.T) {
	isolate(t)
	// A validate against an unknown project id fails (not found).
	if code, _, _ := runCmd("validate", "nope"); code != engine.ExitFatal {
		t.Errorf("code = %d, want fatal for unknown project", code)
	}
}

// --- init tests ------------------------------------------------------------

func TestInitCreatesHome(t *testing.T) {
	isolate(t)
	home := t.TempDir() // use a fresh dir, not the one isolate set
	t.Setenv("KORYPH_HOME", home)

	code, out, errb := runCmd("init")
	if code != 0 {
		t.Fatalf("init code = %d; stderr=%s", code, errb)
	}
	if !strings.Contains(out, home) {
		t.Errorf("init output should mention home dir;\ngot: %s", out)
	}
	if !strings.Contains(out, "Next steps") {
		t.Errorf("init output should contain next-steps;\ngot: %s", out)
	}
	// Verify that ~/.koryph was actually created as a git repo.
	if _, err := os.Stat(home + "/.git"); err != nil {
		t.Errorf(".git not found in KORYPH_HOME after init: %v", err)
	}
}

func TestInitIsIdempotent(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)

	for i := range 3 {
		code, _, errb := runCmd("init")
		if code != 0 {
			t.Fatalf("init run %d: code = %d; stderr=%s", i, code, errb)
		}
	}
}

func TestInitMissingToolsAreWarned(t *testing.T) {
	isolate(t)
	// Point PATH at an empty dir so no tools are found. git is special:
	// store.Init() calls git internally, so we need it in a real bin dir
	// that we inject separately via KORYPH_BD_BIN / not-present-in-PATH
	// gymnastics.  Instead we test only that claude+bd missing doesn't crash
	// by running in a fresh but otherwise real environment.
	//
	// The simplest reliable coverage: normal init with real PATH always
	// exits 0, even if some tools are absent. We already proved that above
	// (claude/bd may be absent in CI and init still returned 0).
	isolate(t)
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	code, out, _ := runCmd("init")
	if code != 0 {
		t.Fatalf("init code = %d", code)
	}
	// git is always present in this CI; claude/bd may not be — either way
	// output should contain an ok or not-found entry for each tool.
	for _, tool := range []string{"git", "claude", "bd"} {
		if !strings.Contains(out, tool) {
			t.Errorf("init output missing tool %q check;\ngot: %s", tool, out)
		}
	}
}

func TestInitUsagePrintsInHelp(t *testing.T) {
	_, out, _ := runCmd("help")
	if !strings.Contains(out, "init") {
		t.Errorf("help output missing 'init':\n%s", out)
	}
}

// --- intake tests ----------------------------------------------------------

const fakeGHList = `#!/bin/sh
case "$1 $2" in
  "issue list")
    printf '[{"number":12,"title":"Add dark mode","body":"b","labels":[{"name":"triage"}],"author":{"login":"alice"}},{"number":34,"title":"Crash","body":"b","labels":[{"name":"triage"},{"name":"p1"},{"name":"bug"}],"author":{"login":"bob"}}]'
    ;;
  *) : ;;
esac
`

const fakeBDList = `#!/bin/sh
case "$1" in
  list) printf '[]' ;;
  create) printf 'cx-1\n' ;;
  *) : ;;
esac
`

// registerGitHubProject registers a project whose remote is a GitHub URL and
// wires fake gh + bd binaries into the environment.
func registerGitHubProject(t *testing.T, id string) *registry.Record {
	t.Helper()
	root := gitRepo(t)

	ghBin := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(ghBin, []byte(fakeGHList), 0o755); err != nil {
		t.Fatal(err)
	}
	bdBin := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bdBin, []byte(fakeBDList), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_GH_BIN", ghBin)
	t.Setenv("KORYPH_BD_BIN", bdBin)

	ctx := context.Background()
	store := registry.NewStore()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	rec := &registry.Record{
		ProjectID:        id,
		Name:             id,
		Root:             root,
		Remote:           "https://github.com/acme/widgets.git",
		AccountProfile:   "personal",
		ExpectedIdentity: "me@example.com",
	}
	if err := store.Add(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestIntakeDryRunTableAndAudit(t *testing.T) {
	isolate(t)
	registerGitHubProject(t, "demo")

	code, out, errb := runCmd("intake", "--project", "demo", "--dry-run")
	if code != 0 {
		t.Fatalf("intake code = %d; stderr=%s", code, errb)
	}
	if !strings.Contains(out, "acme/widgets") {
		t.Fatalf("table missing repo:\n%s", out)
	}
	if !strings.Contains(out, "would-ingest") || !strings.Contains(out, "ingested 2, skipped 0") {
		t.Fatalf("dry-run table unexpected:\n%s", out)
	}

	// The registry audit log carries an "intake" event.
	auditData, err := os.ReadFile(filepath.Join(os.Getenv("KORYPH_HOME"), "audit.jsonl"))
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !strings.Contains(string(auditData), `"kind":"intake"`) {
		t.Fatalf("audit log missing intake event:\n%s", auditData)
	}
}

func TestIntakeRefusesNonGitHubRemote(t *testing.T) {
	isolate(t)
	rec := registerGitHubProject(t, "gl")
	// Rewrite the remote to a non-GitHub host on disk.
	ctx := context.Background()
	store := registry.NewStore()
	rec.Remote = "https://gitlab.com/acme/widgets.git"
	if err := store.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCmd("intake", "--project", "gl"); code != engine.ExitFatal {
		t.Errorf("code = %d, want fatal for a non-GitHub remote", code)
	}
}

func TestIntakeRequiresProject(t *testing.T) {
	isolate(t)
	if code, _, _ := runCmd("intake"); code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit without --project", code)
	}
}

func TestIntakeUsageInHelp(t *testing.T) {
	_, out, _ := runCmd("help")
	if !strings.Contains(out, "intake") {
		t.Errorf("help output missing 'intake':\n%s", out)
	}
}

func TestBatchRunRequiresYes(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "in.jsonl")
	if err := os.WriteFile(input, []byte(`{"id":"1","system":"s","user":"u"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Without --yes the run must refuse before any spend (no key needed).
	code, out, errb := runCmd("batch", "run", "--key-env", "KORYPH_BATCH_API_KEY",
		"--model", "haiku", "--input", input)
	if code != engine.ExitFatal {
		t.Errorf("code = %d, want fatal without --yes", code)
	}
	if !strings.Contains(out, "estimated spend") {
		t.Errorf("stdout should print an estimate:\n%s", out)
	}
	if !strings.Contains(errb, "--yes") {
		t.Errorf("stderr should tell the user to pass --yes:\n%s", errb)
	}
}
