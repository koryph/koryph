// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/promptc"
	"github.com/koryph/koryph/internal/quota"
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
	billingGuard     string
	workSource       string // default bd
	mergePolicy      string // default auto
	commitStyle      string // default "" (conventional enforcement on)
	// pipeline, when set, is written to the project config AND swaps in a
	// persona-aware fake claude that commits a file named after its --agent
	// (so implementer vs stage commits are distinguishable).
	pipeline []project.PipelineStage
	// bdScript overrides the fake-bd script (default: one ready bead then empty).
	bdScript string
	// claudeScript overrides the fake-claude script entirely (default:
	// fakeClaudeScript, or personaClaudeScript when pipeline is set). Used by
	// the rolling-dispatch tests (koryph-2im.3) to distinguish behavior by
	// $KORYPH_PHASE_ID (e.g. one bead sleeps to hold a slot open).
	claudeScript string
	// agentProxy, when set, is written onto the registry record
	// (koryph-3l1.3) so dispatch-level holdout-arm assignment tests can
	// exercise dispatchBead's registry.AgentProxy.ArmFor wiring end-to-end.
	agentProxy *registry.AgentProxy
}

const fakeIdentityEmail = "test@example.com"

var testKoryphBuild struct {
	once   sync.Once
	path   string
	err    error
	output []byte
}

// testKoryphBinary builds the real CLI once for fake workers. Completion is an
// imperative CLI contract, so fixtures invoke the production command instead
// of synthesizing result.json.
func testKoryphBinary(t *testing.T) string {
	t.Helper()
	testKoryphBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "koryph-engine-test-bin-")
		if err != nil {
			testKoryphBuild.err = err
			return
		}
		testKoryphBuild.path = filepath.Join(dir, "koryph")
		wd, err := os.Getwd()
		if err != nil {
			testKoryphBuild.err = err
			return
		}
		cmd := exec.Command("go", "build", "-o", testKoryphBuild.path, "./cmd/koryph")
		cmd.Dir = filepath.Clean(filepath.Join(wd, "..", ".."))
		testKoryphBuild.output, testKoryphBuild.err = cmd.CombinedOutput()
	})
	if testKoryphBuild.err != nil {
		t.Fatalf("build test koryph CLI: %v\n%s", testKoryphBuild.err, testKoryphBuild.output)
	}
	return testKoryphBuild.path
}

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

const fakeCompletionFunction = `
koryph_test_complete() {
  changed_file="$1"
  focused_log="$KORYPH_PHASE_DIR/focused-test.log"
  git diff --check HEAD^ > "$focused_log" 2>&1
  focused_status=$?
  printf 'exit_status=%d\n' "$focused_status" >> "$focused_log"
  evidence="$KORYPH_PHASE_DIR/completion-evidence.json"
  printf '{"focused_tests":[{"command":"git diff --check HEAD^","exit_status":%d,"log_path":"%s"}],"acceptance":[{"criterion_id":"AC1","references":[{"kind":"file","path":"%s"},{"kind":"focused-test","command":"git diff --check HEAD^"}]}]}\n' \
    "$focused_status" "$focused_log" "$PWD/$changed_file" > "$evidence"
  completion_log="$KORYPH_PHASE_DIR/phase-complete.log"
  "$KORYPH_TEST_KORYPH_BIN" phase complete --evidence "$evidence" > "$completion_log" 2>&1
  completion_status=$?
  cat "$completion_log"
  return "$completion_status"
}
`

// fakeClaudeScript acts as a well-behaved implementer: consume the prompt,
// commit one file in the worktree ($PWD), write SUMMARY.md, complete through
// the production CLI, and report cost.
const fakeClaudeScript = `#!/bin/sh
` + fakeCompletionFunction + `
cat > /dev/null
echo "work" > agent-work.txt
git add agent-work.txt
git commit -q --no-verify -m "feat(tb1): work"
printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
koryph_test_complete agent-work.txt || exit $?
printf '{"type":"result","total_cost_usd":0.42}\n'
exit 0
`

// personaClaudeScript commits a file named after its --agent persona (so an
// implementer commit and each pipeline stage commit are distinguishable),
// writes SUMMARY.md only for the implementer, and reports cost.
const personaClaudeScript = `#!/bin/sh
` + fakeCompletionFunction + `
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
  koryph_test_complete "$persona.txt" || exit $?
fi
printf '{"type":"result","total_cost_usd":0.10}\n'
exit 0
`

const readyJSON = `[{"id":"tb1","title":"Test bead one","description":"do the work","acceptance_criteria":"AC1: agent work is committed","status":"open","priority":1,"issue_type":"task","labels":["fp:core"]}]`

// fixtureAccount is the account profile every engine-test fixture runner
// resolves to (the rec built in newFixture). The concurrency governor pool is
// keyed on the account (koryph-1o2.1: runner.poolKey() == quotaName()), so a
// test that pre-seeds a cap, holds a foreign lease, or reads pool state for the
// fixture runner MUST target this pool key — not "" (govern.DefaultPool).
const fixtureAccount = "work"

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
	if o.claudeScript != "" {
		claudeScript = o.claudeScript
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
	t.Setenv("KORYPH_TEST_KORYPH_BIN", testKoryphBinary(t))
	t.Setenv("FAKE_BD_DIR", f.bdDir)
	t.Setenv("KORYPH_NO_NPX", "1")
	t.Setenv("KORYPH_BACKOFF_SEC", "0")
	// Disable the dispatch stagger for full-run fixtures: koryph-4rk6.3 made
	// an unset/omitted dispatch_stagger_seconds fall back to a 10s
	// anti-stampede floor (previously 0 = no stagger), which is correct for
	// real dispatch but would make every multi-bead fixture pay a real 10s
	// sleep per launch and starve short waitForCondition deadlines. Tests
	// that exercise stagger pacing itself set KORYPH_STAGGER_SEC explicitly.
	t.Setenv("KORYPH_STAGGER_SEC", "0")
	// Disable per-slot resource sampling in full-run fixtures: it forks `ps` /
	// scans /proc on the poll loop, and that subprocess overhead perturbs the
	// timing-sensitive wave/pacing tests under load. Sampling has its own
	// targeted tests (resource_sample_test.go) that re-enable it.
	t.Setenv("KORYPH_RESMON", "off")
	// Disable the memory admission gate (koryph-930) for full-run fixtures: it
	// is ON by default and reads the REAL host's memory, which would make
	// dispatch-dependent tests flaky on a loaded or small runner. Tests that
	// exercise the gate itself inject r.memProbe / set this env explicitly.
	t.Setenv("KORYPH_MIN_FREE_MEMORY_MB", "-1")

	// Project repo.
	runGit(t, f.repo, "init", "-b", "main")
	runGit(t, f.repo, "config", "user.name", "fixture")
	runGit(t, f.repo, "config", "user.email", "fixture@example.com")
	runGit(t, f.repo, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(f.repo, "README.md"), "seed\n", 0o644)
	writeFile(t, filepath.Join(f.repo, "AGENTS.md"),
		promptc.RepositoryClauseMarker+"\nFixture repository contract.\n", 0o644)
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
		AccountProfile:   fixtureAccount,
		ClaudeConfigDir:  f.idDir,
		ExpectedIdentity: o.expectedIdentity,
		AllowedModels:    []string{"haiku", "sonnet", "opus"},
		WorktreeRoot:     f.wtRoot,
		AgentProxy:       o.agentProxy,
		BillingGuard:     o.billingGuard,
		EnvPassthrough:   []string{"KORYPH_TEST_KORYPH_BIN"},
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

type observingBackend struct {
	observe func(dispatch.Spec) error
}

type spawnedSleepBackend struct {
	pid   int
	calls int
}

func (b *spawnedSleepBackend) Dispatch(_ context.Context, spec dispatch.Spec) (dispatch.Handle, error) {
	b.calls++
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return dispatch.Handle{}, err
	}
	b.pid = cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		_ = dispatch.StopForceAndReap(b.pid)
		return dispatch.Handle{}, err
	}
	return dispatch.Handle{
		PID: b.pid, SessionID: spec.SessionID, VerifiedIdentity: "agent@example.com",
		StreamPath: filepath.Join(spec.PhaseDir, "stream.jsonl"),
		StatusPath: filepath.Join(spec.PhaseDir, "status.json"),
	}, nil
}

func (b observingBackend) Dispatch(_ context.Context, spec dispatch.Spec) (dispatch.Handle, error) {
	if b.observe != nil {
		if err := b.observe(spec); err != nil {
			return dispatch.Handle{}, err
		}
	}
	return dispatch.Handle{
		PID: 1, SessionID: spec.SessionID,
		StreamPath: filepath.Join(spec.PhaseDir, "stream.jsonl"),
		StatusPath: filepath.Join(spec.PhaseDir, "status.json"),
	}, nil
}

func dispatchIdentityRunner(t *testing.T, f *fix, issue beads.Issue, observe func(dispatch.Spec) error) *runner {
	t.Helper()
	r := runnerFromFixture(t, f)
	r.adapter = &fakeSource{}
	r.quotaCfg = &quota.Config{}
	r.backend = observingBackend{observe: observe}
	r.issues[issue.ID] = issue
	return r
}

func TestDispatchPersistsIdentityBeforeFastWorkerCompletes(t *testing.T) {
	f := newFixture(t, fixOpts{})
	issue := beads.Issue{
		ID: "fast", Title: "fast worker",
		AcceptanceCriteria: "AC1: fast worker commit is present",
	}
	var r *runner
	r = dispatchIdentityRunner(t, f, issue, func(spec dispatch.Spec) error {
		persistedRun, err := r.store.LoadRun(r.run.RunID)
		if err != nil {
			return fmt.Errorf("slot was not persisted before backend launch: %w", err)
		}
		prelaunch := persistedRun.Slots[issue.ID]
		if prelaunch == nil || prelaunch.DispatchGeneration == "" ||
			prelaunch.DispatchBaseSHA == "" || prelaunch.PID != 0 ||
			prelaunch.Status != ledger.SlotQueued {
			return fmt.Errorf("prelaunch slot is incomplete or already live: %+v", prelaunch)
		}
		manifest, err := r.store.LoadManifest(r.run.RunID, issue.ID)
		if err != nil {
			return fmt.Errorf("manifest was not persisted before backend launch: %w", err)
		}
		if manifest.DispatchGeneration == "" || manifest.BaseCommit == "" ||
			manifest.SessionID != spec.SessionID || manifest.ExecutionState != "dispatching" {
			return fmt.Errorf("prelaunch manifest is incomplete: %+v", manifest)
		}
		writeFile(t, filepath.Join(spec.Worktree, "fast.txt"), "fast\n", 0o644)
		runGit(t, spec.Worktree, "add", "fast.txt")
		runGit(t, spec.Worktree, "commit", "--no-verify", "-m", "feat(fast): work")
		summary := filepath.Join(spec.PhaseDir, "SUMMARY.md")
		writeFile(t, summary, "fast completion\n", 0o644)
		logPath := filepath.Join(spec.PhaseDir, "focused.log")
		writeFile(t, logPath, "ok\n", 0o644)
		evidence := filepath.Join(spec.PhaseDir, "evidence.json")
		writeFile(t, evidence, fmt.Sprintf(
			`{"focused_tests":[{"command":"git diff --check HEAD^","exit_status":0,"log_path":%q}],"acceptance":[{"criterion_id":"AC1","references":[{"kind":"file","path":%q}]}]}`,
			logPath, filepath.Join(spec.Worktree, "fast.txt"),
		), 0o644)
		_, err = phasecontrol.Complete(t.Context(), phasecontrol.CompleteOptions{
			PhaseDir: spec.PhaseDir, Worktree: spec.Worktree,
			SummaryPath: summary, EvidencePath: evidence,
			Dispatch: phasecontrol.DispatchContext{
				RunID: r.run.RunID, PhaseID: issue.ID, Attempt: 1,
				SessionID: manifest.SessionID, BaseSHA: manifest.BaseCommit,
			},
		})
		return err
	})

	r.dispatchBead(t.Context(), dispatchReq{issue: issue, attempt: 1})
	sl := r.run.Slots[issue.ID]
	if sl == nil || sl.DispatchBaseSHA == "" || sl.DispatchGeneration == "" {
		t.Fatalf("slot missing trusted identity after fast completion: %+v", sl)
	}
	if ok, reason := r.candidateEligible(t.Context(), sl); !ok {
		t.Fatalf("fast worker result was not accepted: %s", reason)
	}
}

func TestDispatchLaunchedSlotPersistenceFailureStopsAndReapsWorker(t *testing.T) {
	f := newFixture(t, fixOpts{})
	issue := beads.Issue{ID: "rollback-clean", Title: "rollback clean"}
	r := dispatchIdentityRunner(t, f, issue, nil)
	backend := &spawnedSleepBackend{}
	r.backend = backend
	r.processIdentityProbe = func(context.Context, int) string { return "stable-process" }
	r.launchedSlotPersist = func(*ledger.Slot) error {
		return errors.New("injected launched-slot persistence failure")
	}

	r.dispatchBead(t.Context(), dispatchReq{issue: issue, attempt: 1})

	if backend.pid <= 0 {
		t.Fatal("backend did not start a worker")
	}
	if dispatch.Alive(backend.pid) {
		t.Fatalf("worker pid %d survived launched-slot rollback", backend.pid)
	}
	if got := r.run.Slots[issue.ID]; got == nil || got.Status != ledger.SlotBlocked ||
		!strings.Contains(got.Note, "persist launched slot") {
		t.Fatalf("rollback slot = %+v, want terminal persisted failure", got)
	}
	manifest, err := r.store.LoadManifest(r.run.RunID, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ExecutionState != "launch-untracked-stopped" {
		t.Errorf("manifest state = %q, want launch-untracked-stopped", manifest.ExecutionState)
	}
	if manifest.ExecutionError == "" {
		t.Error("manifest did not record the launched-slot persistence failure")
	}
}

func TestDispatchRollbackStopFailureRetainsLiveTrackedHandle(t *testing.T) {
	f := newFixture(t, fixOpts{})
	issue := beads.Issue{ID: "rollback-live", Title: "rollback live"}
	r := dispatchIdentityRunner(t, f, issue, nil)
	backend := &spawnedSleepBackend{}
	r.backend = backend
	r.processIdentityProbe = func(context.Context, int) string { return "stable-process" }
	r.launchedSlotPersist = func(*ledger.Slot) error {
		return errors.New("injected launched-slot persistence failure")
	}
	r.rollbackStop = func(int) error {
		return errors.New("injected stop/reap failure")
	}
	t.Cleanup(func() {
		if backend.pid > 0 {
			_ = dispatch.StopForceAndReap(backend.pid)
			r.releaseGlobalSlot(issue.ID)
		}
	})

	r.dispatchBead(t.Context(), dispatchReq{issue: issue, attempt: 1})

	got := r.run.Slots[issue.ID]
	if got == nil || got.Status != ledger.SlotRunning || got.PID != backend.pid ||
		got.VerifiedIdentity == "" || got.ProcessIdentity == "" ||
		!strings.Contains(got.Note, "authenticated live worker retained") {
		t.Fatalf("rollback slot = %+v, want live authenticated handle retained", got)
	}
	persisted, err := r.store.LoadRun(r.run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if saved := persisted.Slots[issue.ID]; saved == nil || saved.PID != backend.pid ||
		saved.Status != ledger.SlotRunning {
		t.Fatalf("persisted rollback slot = %+v, want live worker recorded", saved)
	}
	if !dispatch.Alive(backend.pid) {
		t.Fatalf("worker pid %d is not live; test did not exercise stop failure fallback", backend.pid)
	}
	manifest, err := r.store.LoadManifest(r.run.RunID, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ExecutionState != "launch-untracked-stop-failed" {
		t.Errorf("manifest state = %q, want launch-untracked-stop-failed", manifest.ExecutionState)
	}
	if manifest.PID != backend.pid || manifest.ExecutionError == "" {
		t.Errorf("manifest rollback evidence = pid %d / error %q, want pid %d / non-empty",
			manifest.PID, manifest.ExecutionError, backend.pid)
	}
}

func TestDispatchRollbackIdentityUnavailableOpensManualInvariantCircuit(t *testing.T) {
	f := newFixture(t, fixOpts{})
	issue := beads.Issue{ID: "rollback-manual", Title: "rollback manual"}
	r := dispatchIdentityRunner(t, f, issue, nil)
	backend := &spawnedSleepBackend{}
	r.backend = backend
	r.processIdentityProbe = func(context.Context, int) string { return "" }
	r.launchedSlotPersist = func(*ledger.Slot) error {
		return errors.New("injected launched-slot persistence failure")
	}
	stopCalls := 0
	r.rollbackStop = func(int) error {
		stopCalls++
		return nil
	}
	t.Cleanup(func() {
		if backend.pid > 0 {
			_ = dispatch.StopForceAndReap(backend.pid)
			r.releaseGlobalSlot(issue.ID)
		}
	})

	r.dispatchBead(t.Context(), dispatchReq{issue: issue, attempt: 1})

	got := r.run.Slots[issue.ID]
	if got == nil || got.Status != ledger.SlotBlocked || got.PID != backend.pid ||
		got.OutcomeClass != string(OutcomeEngineInvariant) ||
		!strings.Contains(got.Note, "manual hold") ||
		!strings.Contains(got.Note, "no automatic signal permitted") {
		t.Fatalf("manual invariant slot = %+v", got)
	}
	if stopCalls != 0 {
		t.Fatalf("PID-only rollback stop called %d time(s), want 0", stopCalls)
	}
	if r.dispatchCircuitReason == "" {
		t.Fatal("dispatch circuit was not opened")
	}
	if !dispatch.Alive(backend.pid) {
		t.Fatalf("manual-hold pid %d is not live; test did not exercise unsignalled fallback", backend.pid)
	}
	manifest, err := r.store.LoadManifest(r.run.RunID, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ExecutionState != "launch-untracked-identity-unavailable" ||
		manifest.PID != backend.pid || manifest.ExecutionError == "" {
		t.Fatalf("manual-hold manifest = %+v", manifest)
	}

	second := beads.Issue{ID: "circuit-blocked", Title: "circuit blocked"}
	r.issues[second.ID] = second
	r.dispatchBead(t.Context(), dispatchReq{issue: second, attempt: 1})
	if backend.calls != 1 {
		t.Fatalf("backend calls = %d, want 1 after circuit opens", backend.calls)
	}
	if blocked := r.run.Slots[second.ID]; blocked == nil ||
		blocked.OutcomeClass != string(OutcomeEngineInvariant) {
		t.Fatalf("post-circuit slot = %+v, want engine-invariant block", blocked)
	}
}

func TestDispatchBaseDoesNotChangeWhenMainAdvancesDuringLaunch(t *testing.T) {
	f := newFixture(t, fixOpts{})
	before := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main"))
	issue := beads.Issue{
		ID: "base-race", Title: "base race",
		AcceptanceCriteria: "AC1: dispatch identity remains stable",
	}
	var r *runner
	r = dispatchIdentityRunner(t, f, issue, func(dispatch.Spec) error {
		writeFile(t, filepath.Join(f.repo, "main-race.txt"), "advanced\n", 0o644)
		runGit(t, f.repo, "add", "main-race.txt")
		runGit(t, f.repo, "commit", "--no-verify", "-m", "chore: advance main during launch")
		return nil
	})

	r.dispatchBead(t.Context(), dispatchReq{issue: issue, attempt: 1})
	after := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main"))
	if after == before {
		t.Fatal("test setup did not advance main")
	}
	sl := r.run.Slots[issue.ID]
	manifest, err := r.store.LoadManifest(r.run.RunID, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sl.DispatchBaseSHA != before || manifest.BaseCommit != before ||
		sl.DispatchGeneration != manifest.DispatchGeneration {
		t.Fatalf("dispatch identity raced main: slot=%+v manifest=%+v before=%s after=%s",
			sl, manifest, before, after)
	}
	if head := strings.TrimSpace(runGit(t, sl.Worktree, "rev-parse", "HEAD")); head != before {
		t.Fatalf("worktree HEAD=%s, want captured base %s", head, before)
	}
}

func TestEngineMergeJobsCarryTrustedValidationPhaseContext(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.cfg = &project.Config{}
	r.adapter = &fakeSource{}
	candidate := strings.TrimSpace(runGit(t, sl.Worktree, "rev-parse", sl.Branch))
	base := strings.TrimSpace(runGit(t, r.rec.Root, "rev-parse", r.rec.DefaultBranch))
	buildIdentity, err := exactBuildIdentity()
	if err != nil {
		t.Fatal(err)
	}
	evidence := merge.GateEvidence{
		Schema: merge.GateEvidenceSchema, CandidateSHA: candidate, BaseSHA: base,
		DiffDigest:       "sha256:" + strings.Repeat("1", 64),
		GateConfigDigest: "sha256:" + strings.Repeat("2", 64),
		CommandDigest:    "sha256:" + strings.Repeat("3", 64), EngineVersion: EngineVersion,
		BuildIdentity: buildIdentity, CompletedAt: time.Now().UTC(),
	}
	installTestGateEvidence(t, r, sl, evidence)
	lane := &finalizationLane{}
	lane.merges.ready = sync.NewCond(&lane.merges.mu)
	r.finalizer = lane

	r.mergeSlot(t.Context(), sl)
	r.openPRSlot(t.Context(), sl)

	if len(lane.merges.queue) != 2 {
		t.Fatalf("queued finalization jobs = %d, want merge and PR", len(lane.merges.queue))
	}
	want := r.store.PhaseDir(r.run.RunID, sl.PhaseID)
	for i, job := range lane.merges.queue {
		if job.mergeOpts.ValidationPhaseDir != want {
			t.Errorf("job %d validation phase dir = %q, want %q", i, job.mergeOpts.ValidationPhaseDir, want)
		}
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
		if run, loadErr := ledger.NewStore(f.repo).LoadLatest(); loadErr == nil {
			if failed := run.Slots["tb1"]; failed != nil {
				if stream, readErr := os.ReadFile(failed.Stream); readErr == nil {
					t.Logf("failed worker stream:\n%s", stream)
				}
			}
		}
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
	// koryph-v8u.10: dispatchBead now passes RepoRoot into modelroute.Resolve,
	// so the fixture's .claude/agents/koryph-implementer.md legacy `model:
	// sonnet` pin (it carries no `tier:`) is honored ahead of the hardcoded
	// stage default, per the new persona-tier/model precedence step.
	if sl.Model != "sonnet" || !strings.Contains(sl.ModelWhy, "persona koryph-implementer legacy model pin") {
		t.Errorf("slot model = %q (%q), want sonnet via persona legacy model pin", sl.Model, sl.ModelWhy)
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
	if m.AccountProfile != fixtureAccount || m.ClaudeConfigDir != f.idDir {
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

func TestRunNativeCanaryAdmitsMigratedFixedCohort(t *testing.T) {
	newFixture(t, fixOpts{migrationStatus: registry.StatusMigrated})
	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.Once = true
	opts.Max = 2
	opts.AllowedIDs = []string{"tb1", "future"}
	opts.AuthoritativeWidth = true
	opts.NativeCanary = true
	outcome, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Dispatched != 1 {
		t.Fatalf("native canary dispatched %d beads, want 1", outcome.Dispatched)
	}
}

func TestRunNativeCanaryRefusesRegisteredProject(t *testing.T) {
	newFixture(t, fixOpts{migrationStatus: registry.StatusRegistered})
	opts := baseOptions(nil)
	opts.Once = true
	opts.Max = 2
	opts.AllowedIDs = []string{"tb1", "future"}
	opts.AuthoritativeWidth = true
	opts.NativeCanary = true
	_, err := Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "onboarding-complete") {
		t.Fatalf("registered native canary error = %v", err)
	}
}

func TestRunNativeCanaryRequiresFixedCohortForValidatedProject(t *testing.T) {
	err := validateMigrationPosture(
		&registry.Record{MigrationStatus: registry.StatusValidated},
		Options{ProjectID: "proj", NativeCanary: true},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "immutable fixed cohort") {
		t.Fatalf("validated native canary error = %v", err)
	}
}

func TestRunMigratedSteadyModeIsAdmitted(t *testing.T) {
	newFixture(t, fixOpts{migrationStatus: registry.StatusMigrated})
	var out bytes.Buffer
	outcome, err := Run(context.Background(), baseOptions(&out))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Dispatched != 1 {
		t.Fatalf("migrated steady mode dispatched %d beads, want 1", outcome.Dispatched)
	}
}

func TestRunOrdinaryModeRefusesIncompleteAndUnknownPostures(t *testing.T) {
	for _, status := range []string{
		registry.StatusRegistered,
		registry.StatusInventoried,
		"future",
		"",
	} {
		t.Run(status, func(t *testing.T) {
			err := validateMigrationPosture(
				&registry.Record{MigrationStatus: status},
				Options{ProjectID: "proj"},
				nil,
			)
			if err == nil {
				t.Fatalf("migration status %q was admitted", status)
			}
		})
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
