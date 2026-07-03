// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// --- fixture ---------------------------------------------------------------

// fix is one fully-mocked engine fixture: a registry home, a git project, a
// fake identity, and fake bd + claude binaries wired through the env.
type fix struct {
	home   string // KORYPH_HOME
	repo   string // project root (git, koryph.project.json)
	wtRoot string // worktree root
	idDir  string // CLAUDE_CONFIG_DIR with .claude.json
	bdDir  string // fake-bd state dir (bd.log, ready.json, counter)
}

type fixOpts struct {
	expectedIdentity string // registry ExpectedIdentity (default test@example.com)
	migrationStatus  string // default validated
	workSource       string // default bd
	mergePolicy      string // default auto
	commitStyle      string // default "" (conventional enforcement on)
	// pipeline, when set, is written to the project config AND swaps in a
	// persona-aware fake claude that commits a file named after its --agent
	// (so implementer vs stage commits are distinguishable).
	pipeline []project.PipelineStage
	// bdScript overrides the fake-bd script (default: one ready bead then empty).
	bdScript string
}

const fakeIdentityEmail = "test@example.com"

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
}

// fakeBD emits one ready task on the first `ready` call and an empty frontier
// afterwards; `show` is not-found; mutations are logged and succeed.
const fakeBDScript = `#!/bin/sh
dir="$FAKE_BD_DIR"
printf '%s\n' "$*" >> "$dir/bd.log"
case "$1" in
  ready)
    if [ -f "$dir/ready_served" ]; then
      echo '[]'
    else
      touch "$dir/ready_served"
      cat "$dir/ready.json"
    fi
    ;;
  version) echo "bd version 1.0.5" ;;
  update|close|comment) exit 0 ;;
  show) exit 1 ;;
  *) exit 1 ;;
esac
`

// fakeClaudeScript acts as a well-behaved implementer: consume the prompt,
// commit one file in the worktree ($PWD), write SUMMARY.md, report cost.
const fakeClaudeScript = `#!/bin/sh
cat > /dev/null
echo "work" > agent-work.txt
git add agent-work.txt
git commit -q --no-verify -m "feat(tb1): work"
printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
printf '{"type":"result","total_cost_usd":0.42}\n'
exit 0
`

// personaClaudeScript commits a file named after its --agent persona (so an
// implementer commit and each pipeline stage commit are distinguishable),
// writes SUMMARY.md only for the implementer, and reports cost.
const personaClaudeScript = `#!/bin/sh
cat > /dev/null
persona=unknown
while [ $# -gt 0 ]; do
  case "$1" in
    --agent) persona="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$KORYPH_TEST_FAIL_PERSONA" ] && [ "$persona" = "$KORYPH_TEST_FAIL_PERSONA" ]; then
  printf '{"type":"result","total_cost_usd":0.05}\n'
  exit 7
fi
echo "work by $persona" > "$persona.txt"
git add "$persona.txt"
git commit -q --no-verify -m "chore($persona): work"
if [ "$persona" = "koryph-implementer" ]; then
  printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
fi
printf '{"type":"result","total_cost_usd":0.10}\n'
exit 0
`

const readyJSON = `[{"id":"tb1","title":"Test bead one","description":"do the work","status":"open","priority":1,"issue_type":"task","labels":["fp:core"]}]`

// newFixture assembles the whole mock world and points the engine at it.
func newFixture(t *testing.T, o fixOpts) *fix {
	t.Helper()
	if o.expectedIdentity == "" {
		o.expectedIdentity = fakeIdentityEmail
	}
	if o.migrationStatus == "" {
		o.migrationStatus = registry.StatusValidated
	}
	if o.workSource == "" {
		o.workSource = "bd"
	}
	if o.mergePolicy == "" {
		o.mergePolicy = "auto"
	}

	tmp := t.TempDir()
	f := &fix{
		home:   filepath.Join(tmp, "koryph-home"),
		repo:   filepath.Join(tmp, "proj"),
		wtRoot: filepath.Join(tmp, "worktrees"),
		idDir:  filepath.Join(tmp, "claude-work"),
		bdDir:  filepath.Join(tmp, "fake-bd"),
	}
	for _, d := range []string{f.home, f.repo, f.idDir, f.bdDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Fake identity.
	writeFile(t, filepath.Join(f.idDir, ".claude.json"),
		`{"oauthAccount":{"emailAddress":"`+fakeIdentityEmail+`","organizationName":"Test Org"}}`, 0o644)

	// Fake binaries + state. A configured pipeline swaps in the persona-aware
	// claude so implementer vs stage commits are distinguishable.
	claudeScript := fakeClaudeScript
	if len(o.pipeline) > 0 {
		claudeScript = personaClaudeScript
	}
	bdScript := fakeBDScript
	if o.bdScript != "" {
		bdScript = o.bdScript
	}
	bdBin := filepath.Join(tmp, "bin", "fake-bd")
	claudeBin := filepath.Join(tmp, "bin", "fake-claude")
	writeFile(t, bdBin, bdScript, 0o755)
	writeFile(t, claudeBin, claudeScript, 0o755)
	writeFile(t, filepath.Join(f.bdDir, "ready.json"), readyJSON, 0o644)

	// Env wiring (also isolates the test from the real ~/.koryph).
	t.Setenv("KORYPH_HOME", f.home)
	t.Setenv("KORYPH_BD_BIN", bdBin)
	t.Setenv("KORYPH_CLAUDE_BIN", claudeBin)
	t.Setenv("FAKE_BD_DIR", f.bdDir)
	t.Setenv("KORYPH_NO_NPX", "1")
	t.Setenv("KORYPH_BACKOFF_SEC", "0")

	// Project repo.
	runGit(t, f.repo, "init", "-b", "main")
	runGit(t, f.repo, "config", "user.name", "fixture")
	runGit(t, f.repo, "config", "user.email", "fixture@example.com")
	runGit(t, f.repo, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(f.repo, "README.md"), "seed\n", 0o644)
	writeFile(t, filepath.Join(f.repo, ".claude", "agents", "koryph-implementer.md"),
		"---\nmodel: sonnet\neffort: high\n---\n\n# implementer\n", 0o644)
	cfg := &project.Config{
		SchemaVersion:      1,
		ProjectID:          "proj",
		WorkSource:         o.workSource,
		PlansDir:           map[bool]string{true: "docs/plans", false: ""}[o.workSource == "markdown"],
		Gate:               []string{"true"},
		MergePolicy:        project.Policy(o.mergePolicy),
		CommitStyle:        o.commitStyle,
		RiskTierDefault:    1,
		MaxConcurrentSlots: 2,
		Pipeline:           o.pipeline,
	}
	if err := cfg.Save(f.repo); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.repo, "add", "-A")
	runGit(t, f.repo, "commit", "--no-verify", "-m", "chore: seed fixture")

	// Registry record.
	ctx := context.Background()
	st := registry.NewStoreAt(f.home)
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	rec := &registry.Record{
		ProjectID:        "proj",
		Name:             "Proj Fixture",
		Root:             f.repo,
		DefaultBranch:    "main",
		BeadsStatus:      "initialized",
		BeadsHooksStatus: "wired",
		MigrationStatus:  o.migrationStatus,
		AccountProfile:   "work",
		ClaudeConfigDir:  f.idDir,
		ExpectedIdentity: o.expectedIdentity,
		AllowedModels:    []string{"haiku", "sonnet", "opus"},
		WorktreeRoot:     f.wtRoot,
	}
	if err := st.Add(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fix) bdLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.bdDir, "bd.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

func baseOptions(out *bytes.Buffer) Options {
	return Options{
		ProjectID: "proj",
		Once:      true,
		AutoMerge: true,
		PollSec:   1,
		StuckSec:  60,
		Max:       2,
		Out:       out,
	}
}

// branchExists reports whether the repo has a local branch of that name.
func branchExists(repo, branch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repo
	return cmd.Run() == nil
}

// --- tests -------------------------------------------------------------------

func TestRunOnceMergesAndDrains(t *testing.T) {
	f := newFixture(t, fixOpts{})
	var out bytes.Buffer
	ctx := context.Background()

	got, err := Run(ctx, baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Code != ExitOK {
		t.Errorf("Code = %d, want %d", got.Code, ExitOK)
	}
	if got.Dispatched != 1 || got.Merged != 1 || got.Failed != 0 || got.Blocked != 0 {
		t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged", got)
	}

	// The agent commit landed on main.
	if log := runGit(t, f.repo, "log", "--format=%s", "main"); !strings.Contains(log, "feat(tb1): work") {
		t.Errorf("main log missing agent commit:\n%s", log)
	}

	// Worktree and branch were cleaned up by the merge.
	if _, err := os.Stat(filepath.Join(f.wtRoot, "agent-tb1")); !os.IsNotExist(err) {
		t.Errorf("worktree not cleaned: stat err = %v", err)
	}
	if branchExists(f.repo, "agent/tb1") {
		t.Error("branch agent/tb1 still exists after merge")
	}

	// bd saw the claim and the close.
	log := f.bdLog(t)
	if !strings.Contains(log, "update tb1 --claim") {
		t.Errorf("bd.log missing claim:\n%s", log)
	}
	if !strings.Contains(log, "close tb1") {
		t.Errorf("bd.log missing close tb1:\n%s", log)
	}

	// Ledger: latest run finalized as done, slot merged, cost recorded.
	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if run.RunID != got.RunID {
		t.Errorf("latest run %q != outcome run %q", run.RunID, got.RunID)
	}
	if run.Status != ledger.RunDone {
		t.Errorf("run status = %q, want %q", run.Status, ledger.RunDone)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatalf("no slot tb1 in run: %+v", run.Slots)
	}
	if sl.Status != ledger.SlotMerged {
		t.Errorf("slot status = %q, want merged", sl.Status)
	}
	if sl.CostUSD != 0.42 {
		t.Errorf("slot cost = %v, want 0.42", sl.CostUSD)
	}
	if sl.MergedAt == "" {
		t.Error("slot MergedAt empty")
	}
	if sl.VerifiedIdentity != fakeIdentityEmail {
		t.Errorf("slot verified identity = %q", sl.VerifiedIdentity)
	}
	if sl.Model != "sonnet" || !strings.Contains(sl.ModelWhy, "stage default") {
		t.Errorf("slot model = %q (%q), want sonnet via stage default", sl.Model, sl.ModelWhy)
	}

	// Manifest v2 with billing + account fields.
	m, err := store.LoadManifest(run.RunID, "tb1")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.SchemaVersion != 2 {
		t.Errorf("manifest schema = %d, want 2", m.SchemaVersion)
	}
	if m.BillingMode != "subscription" {
		t.Errorf("manifest billing = %q, want subscription", m.BillingMode)
	}
	if m.AccountProfile != "work" || m.ClaudeConfigDir != f.idDir {
		t.Errorf("manifest account fields = %q / %q", m.AccountProfile, m.ClaudeConfigDir)
	}
	if m.BaseCommit == "" || m.SessionID == "" || m.Branch != "agent/tb1" {
		t.Errorf("manifest incomplete: base=%q session=%q branch=%q", m.BaseCommit, m.SessionID, m.Branch)
	}

	// Second run: the frontier is now empty → drained with the CLI contract code.
	var out2 bytes.Buffer
	got2, err := Run(ctx, baseOptions(&out2))
	t.Logf("engine output (run 2):\n%s", out2.String())
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if !got2.Drained || got2.Code != ExitDrained {
		t.Errorf("run 2 = %+v, want Drained with code %d", got2, ExitDrained)
	}
}

// TestRunPipelineStageCommitsAndMerges proves koryph-a14: a configured
// post-implement stage runs in the worktree after the implementer and its
// commit is carried through the merge.
func TestRunPipelineStageCommitsAndMerges(t *testing.T) {
	f := newFixture(t, fixOpts{pipeline: []project.PipelineStage{{Name: "docs"}}})
	var out bytes.Buffer
	ctx := context.Background()

	got, err := Run(ctx, baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Code != ExitOK || got.Merged != 1 {
		t.Fatalf("Outcome = %+v, want 1 merged", got)
	}

	// Both the implementer and the docs-stage commits landed on main.
	log := runGit(t, f.repo, "log", "--format=%s", "main")
	for _, want := range []string{"chore(koryph-implementer): work", "chore(koryph-feature-docs-author): work"} {
		if !strings.Contains(log, want) {
			t.Errorf("main log missing %q:\n%s", want, log)
		}
	}
	// The stage's file exists in the merged main tree.
	runGit(t, f.repo, "cat-file", "-e", "main:koryph-feature-docs-author.txt")
	// The engine logged the stage running.
	if !strings.Contains(out.String(), `stage "docs" running`) {
		t.Errorf("engine did not log the docs stage:\n%s", out.String())
	}
}

// TestRunRequiredStageFailureBlocks proves a failing REQUIRED stage blocks the
// slot (no auto-merge past incomplete pipeline work).
func TestRunRequiredStageFailureBlocks(t *testing.T) {
	f := newFixture(t, fixOpts{pipeline: []project.PipelineStage{{Name: "docs"}}})
	t.Setenv("KORYPH_TEST_FAIL_PERSONA", "koryph-feature-docs-author")
	var out bytes.Buffer
	ctx := context.Background()

	got, err := Run(ctx, baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Merged != 0 || got.Blocked != 1 {
		t.Errorf("Outcome = %+v, want 0 merged / 1 blocked", got)
	}

	// The implementer commit did NOT reach main (merge was refused).
	if log := runGit(t, f.repo, "log", "--format=%s", "main"); strings.Contains(log, "chore(koryph-implementer): work") {
		t.Errorf("implementer work merged despite a failed required stage:\n%s", log)
	}
	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil || sl.Status != ledger.SlotBlocked {
		t.Fatalf("slot = %+v, want blocked", sl)
	}
	if !strings.Contains(sl.Note, "docs") {
		t.Errorf("slot note = %q, want it to name the failed stage", sl.Note)
	}
}

func TestRunAccountMismatchFailsClosed(t *testing.T) {
	f := newFixture(t, fixOpts{expectedIdentity: "other@example.com"})
	var out bytes.Buffer

	got, err := Run(context.Background(), baseOptions(&out))
	if err == nil {
		t.Fatal("Run succeeded despite account mismatch; must fail closed")
	}
	// The error names both the logged-in and the expected identity.
	for _, want := range []string{fakeIdentityEmail, "other@example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if got.Code != ExitFatal {
		t.Errorf("Code = %d, want %d", got.Code, ExitFatal)
	}
	if got.Dispatched != 0 {
		t.Errorf("Dispatched = %d, want 0", got.Dispatched)
	}

	// Nothing was touched: no run dirs, no worktrees, no bd mutations.
	store := ledger.NewStore(f.repo)
	runs, _ := store.ListRuns()
	if len(runs) != 0 {
		t.Errorf("run dirs created despite refusal: %v", runs)
	}
	if _, err := os.Stat(f.wtRoot); !os.IsNotExist(err) {
		t.Errorf("worktree root created despite refusal (stat err %v)", err)
	}
	if log := f.bdLog(t); log != "" {
		t.Errorf("bd was invoked despite refusal:\n%s", log)
	}
}

func TestRunMergePendingWithoutAutoMerge(t *testing.T) {
	f := newFixture(t, fixOpts{})
	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.AutoMerge = false

	got, err := Run(context.Background(), opts)
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Merged != 0 || got.Dispatched != 1 {
		t.Errorf("Outcome = %+v, want 0 merged / 1 dispatched", got)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil || sl.Status != ledger.SlotMergePending {
		t.Fatalf("slot = %+v, want merge-pending", sl)
	}
	// Merge-pending is terminal, so the run still finalizes.
	if run.Status != ledger.RunDone {
		t.Errorf("run status = %q, want done", run.Status)
	}

	// The bead was NOT closed; the operator was pinged instead.
	log := f.bdLog(t)
	if strings.Contains(log, "close tb1") {
		t.Errorf("bead closed despite merge-pending:\n%s", log)
	}
	if !strings.Contains(log, "comment tb1 ready for merge: branch agent/tb1") {
		t.Errorf("bd.log missing ready-for-merge comment:\n%s", log)
	}

	// Branch and worktree are preserved for the operator merge.
	if !branchExists(f.repo, "agent/tb1") {
		t.Error("branch agent/tb1 missing; must be preserved for manual merge")
	}
	if _, err := os.Stat(filepath.Join(f.wtRoot, "agent-tb1")); err != nil {
		t.Errorf("worktree missing; must be preserved for manual merge: %v", err)
	}
	// The agent's work is NOT on main.
	if log := runGit(t, f.repo, "log", "--format=%s", "main"); strings.Contains(log, "feat(tb1): work") {
		t.Errorf("agent commit landed on main despite merge-pending:\n%s", log)
	}
}

func TestRunRefusesUnvalidatedProject(t *testing.T) {
	newFixture(t, fixOpts{migrationStatus: registry.StatusRegistered})
	_, err := Run(context.Background(), baseOptions(nil))
	if err == nil {
		t.Fatal("Run succeeded on an unvalidated project")
	}
	if !strings.Contains(err.Error(), registry.StatusRegistered) {
		t.Errorf("error %q does not name the migration status", err)
	}
}

func TestRunRefusesMarkdownWorkSource(t *testing.T) {
	newFixture(t, fixOpts{workSource: "markdown"})
	_, err := Run(context.Background(), baseOptions(nil))
	if err == nil {
		t.Fatal("Run succeeded on a markdown work source")
	}
	if !strings.Contains(err.Error(), "legacy markdown projects run their project-local fork until migrated") {
		t.Errorf("error %q missing the markdown refusal message", err)
	}
}
