// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/commandguard"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/resmon"
)

func TestMergeValidationPublishesTrustedOwnerAndWorkerReusesIt(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	wt := worktreeOn(t, repo, "agent/guarded")
	commitIn(t, wt.Path, "guarded.txt", "guarded\n", "feat(test): guarded merge")

	phase, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeBin, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	starts := filepath.Join(t.TempDir(), "starts")
	makePath := filepath.Join(fakeBin, "make")
	script := "#!/bin/sh\necho start >>" + shellTestQuote(starts) + "\nsleep 1\nexit 0\n"
	if err := os.WriteFile(makePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(filepath.ListSeparator)+os.Getenv("PATH"))

	type mergeOutcome struct {
		result Result
		err    error
	}
	done := make(chan mergeOutcome, 1)
	go func() {
		result, err := Merge(context.Background(), Opts{
			RepoRoot: repo, Branch: "agent/guarded", DefaultBranch: "main",
			Gate: []string{makePath + " gate-agent"}, ValidationPhaseDir: phase,
			KeepWorktree: true,
		})
		done <- mergeOutcome{result: result, err: err}
	}()

	eventsPath := filepath.Join(phase, ".koryph-command", "events.jsonl")
	// Wait for the real tool marker, not merely owner publication, so this
	// assertion also proves the suspended child was released after publication.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(starts); err == nil && strings.Contains(string(data), "start") {
			break
		}
		select {
		case outcome := <-done:
			t.Fatalf("Merge finished before real gate start: (%+v, %v)", outcome.result, outcome.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for real gate start in %s", starts)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Use a real independent process identity to model the worker wrapper that
	// reached the same full-gate lane while trusted validation was live.
	workerProcess := exec.Command("/bin/sleep", "5")
	workerProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := workerProcess.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := resmon.CurrentCommandIdentity(context.Background(), workerProcess.Process.Pid)
	if err != nil {
		_ = workerProcess.Process.Signal(syscall.SIGTERM)
		_ = workerProcess.Wait()
		t.Fatal(err)
	}
	guard := commandguard.NewProcessGuard(phase)
	reuse, err := guard.Acquire(context.Background(), commandguard.ProcessGuardRequest{
		Argv: []string{"make", "gate"}, Role: commandguard.CommandRoleWorker, Identity: identity,
	})
	_ = workerProcess.Process.Signal(syscall.SIGTERM)
	_ = workerProcess.Wait()
	if err != nil || reuse.Action != commandguard.ProcessGuardReuse {
		t.Fatalf("worker gate Acquire = (%+v, %v), want reuse", reuse, err)
	}
	if reuse.Existing.Role != commandguard.CommandRoleValidation {
		t.Fatalf("worker reused role %q, want validation", reuse.Existing.Role)
	}

	result, err := guard.Wait(context.Background(), reuse)
	if err != nil || result.ExitCode != 0 || result.Status != "passed" {
		t.Fatalf("worker reused result = (%+v, %v)", result, err)
	}
	outcome := <-done
	if outcome.err != nil || outcome.result.Status != StatusMerged {
		t.Fatalf("Merge = (%+v, %v)", outcome.result, outcome.err)
	}
	data, err := os.ReadFile(starts)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "start"); got != 1 {
		t.Fatalf("real gate starts = %d, want 1:\n%s", got, data)
	}
	events, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"start"`, `"event":"reuse"`, `"event":"complete"`, `"class":"full-gate"`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("validation evidence missing %s:\n%s", want, events)
		}
	}
}

func TestRunGateChoosesBlockedDirenvFallbackBeforeBroadGuard(t *testing.T) {
	phase, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".envrc"), []byte("export TEST_ENV=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	direnvCalls := filepath.Join(t.TempDir(), "direnv-calls")
	realStarts := filepath.Join(t.TempDir(), "real-starts")
	writeExecutable(t, filepath.Join(fakeBin, "direnv"),
		"#!/bin/sh\necho call >>"+shellTestQuote(direnvCalls)+"\necho 'direnv: error .envrc is blocked' >&2\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "go"),
		"#!/bin/sh\necho start >>"+shellTestQuote(realStarts)+"\nexit 0\n")
	t.Setenv("PATH", fakeBin+string(filepath.ListSeparator)+"/usr/bin:/bin")

	ok, out, infraErr := runGate(
		context.Background(), worktree, phase, []string{"go test ./..."},
	)
	if infraErr != nil || !ok {
		t.Fatalf("runGate = (%t, %q, %v), want pass", ok, out, infraErr)
	}
	if !strings.Contains(out, "(direnv blocked; running without direnv)") {
		t.Fatalf("gate output lacks fallback explanation:\n%s", out)
	}
	assertFileLineCount(t, direnvCalls, 1)
	assertFileLineCount(t, realStarts, 1)
	assertGateEventCount(t, phase, `"event":"start"`, 1)
	assertGateEventCount(t, phase, `"event":"complete"`, 1)
}

func TestRunGateAppliesAllowedDirenvWithoutRetryingRealFailure(t *testing.T) {
	phase, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	fakeBin := t.TempDir()
	direnvCalls := filepath.Join(t.TempDir(), "direnv-calls")
	realStarts := filepath.Join(t.TempDir(), "real-starts")
	writeExecutable(t, filepath.Join(fakeBin, "direnv"),
		"#!/bin/sh\necho call >>"+shellTestQuote(direnvCalls)+"\nshift 2\nTEST_DIRENV_APPLIED=1 exec \"$@\"\n")
	writeExecutable(t, filepath.Join(fakeBin, "go"),
		"#!/bin/sh\n[ \"$TEST_DIRENV_APPLIED\" = 1 ] || exit 41\necho start >>"+shellTestQuote(realStarts)+"\necho 'tool says is blocked' >&2\nexit 7\n")
	t.Setenv("PATH", fakeBin+string(filepath.ListSeparator)+"/usr/bin:/bin")

	ok, out, infraErr := runGate(
		context.Background(), worktree, phase, []string{"go test ./..."},
	)
	if infraErr != nil || ok {
		t.Fatalf("runGate = (%t, %q, %v), want ordinary gate failure", ok, out, infraErr)
	}
	if strings.Contains(out, "(direnv blocked; running without direnv)") {
		t.Fatalf("real command stderr triggered a fallback:\n%s", out)
	}
	assertFileLineCount(t, direnvCalls, 2) // availability probe plus real gate
	assertFileLineCount(t, realStarts, 1)
	assertGateEventCount(t, phase, `"event":"start"`, 1)
	assertGateEventCount(t, phase, `"event":"complete"`, 1)
}

func TestRunGateCommandWaitsOutWorkerThenPublishesValidationOwner(t *testing.T) {
	phase, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "validation-start")
	tool := filepath.Join(t.TempDir(), "validation-tool")
	script := "#!/bin/sh\necho validation >>" + shellTestQuote(marker) + "\nexit 0\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workerProcess := exec.Command("/bin/sleep", "10")
	workerProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := workerProcess.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = workerProcess.Process.Signal(syscall.SIGTERM)
		_ = workerProcess.Wait()
	}()
	workerIdentity, err := resmon.CurrentCommandIdentity(context.Background(), workerProcess.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	classArgv := []string{"make", "build"}
	guard := commandguard.NewProcessGuard(phase)
	worker, err := guard.Acquire(context.Background(), commandguard.ProcessGuardRequest{
		Argv: classArgv, Role: commandguard.CommandRoleWorker, Identity: workerIdentity,
	})
	if err != nil || worker.Action != commandguard.ProcessGuardStart {
		t.Fatalf("worker Acquire = (%+v, %v), want start", worker, err)
	}

	type gateOutcome struct {
		result execx.Result
		err    error
	}
	done := make(chan gateOutcome, 1)
	go func() {
		result, err := runGateCommand(context.Background(), phase, classArgv, execx.Cmd{Name: tool})
		done <- gateOutcome{result: result, err: err}
	}()

	eventsPath := filepath.Join(phase, ".koryph-command", "events.jsonl")
	waitForGateEvent(t, eventsPath, `"existing_role":"worker"`)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("validation tool ran before worker completion: %v", err)
	}
	if _, err := guard.Complete(worker, "passed", 0); err != nil {
		t.Fatal(err)
	}

	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.result.ExitCode != 0 {
			t.Fatalf("runGateCommand = (%+v, %v)", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for validation command")
	}
	data, err := os.ReadFile(marker)
	if err != nil || strings.Count(string(data), "validation") != 1 {
		t.Fatalf("validation starts = %q, %v", data, err)
	}
	events, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"role":"validation"`,
		`"existing_role":"worker"`,
		`"event":"complete"`,
	} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("validation provenance missing %s:\n%s", want, events)
		}
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertFileLineCount(t *testing.T, path string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; got != want {
		t.Fatalf("%s lines = %d, want %d:\n%s", path, got, want, data)
	}
}

func assertGateEventCount(t *testing.T, phase, needle string, want int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(phase, ".koryph-command", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), needle); got != want {
		t.Fatalf("gate events containing %s = %d, want %d:\n%s", needle, got, want, data)
	}
}

func TestRunGateCommandCancellationStopsExactValidationCohort(t *testing.T) {
	phase, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "validation-cancel")
	tool := filepath.Join(t.TempDir(), "validation-tool")
	script := "#!/bin/sh\n" +
		"trap 'echo terminated >>" + shellTestQuote(marker) + "; exit 42' TERM\n" +
		"echo started >>" + shellTestQuote(marker) + "\n" +
		"while :; do sleep 1; done\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	type gateOutcome struct {
		result execx.Result
		err    error
	}
	done := make(chan gateOutcome, 1)
	go func() {
		result, err := runGateCommandWithTiming(
			ctx, phase, []string{"make", "build"}, execx.Cmd{Name: tool},
			validationTiming{poll: 20 * time.Millisecond, cancelGrace: 200 * time.Millisecond, forceWait: 2 * time.Second},
		)
		done <- gateOutcome{result: result, err: err}
	}()
	waitForGateEvent(t, marker, "started")

	cancelledAt := time.Now()
	cancel()
	var outcome gateOutcome
	select {
	case outcome = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("validation command blocked finalization after cancellation")
	}
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context.Canceled", outcome.err)
	}
	if elapsed := time.Since(cancelledAt); elapsed >= 3*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
	data, err := os.ReadFile(marker)
	if err != nil || !strings.Contains(string(data), "terminated") {
		t.Fatalf("validation cohort did not handle SIGTERM: %q, %v", data, err)
	}

	eventsPath := filepath.Join(phase, ".koryph-command", "events.jsonl")
	events, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	var completed resmon.CommandEvent
	for _, line := range strings.Split(strings.TrimSpace(string(events)), "\n") {
		var event resmon.CommandEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "complete" {
			completed = event
		}
	}
	if completed.Status != "failed" || completed.Role != string(commandguard.CommandRoleValidation) {
		t.Fatalf("cancellation completion = %+v", completed)
	}
	if _, live, err := resmon.InspectCommand(context.Background(), completed.Identity); err != nil || live {
		t.Fatalf("validation identity remained live after return: live=%v err=%v", live, err)
	}
}

func TestRunGateCommandDrainsDescendantAfterDirectLeaderExit(t *testing.T) {
	phase, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	starts := filepath.Join(t.TempDir(), "starts")
	descendantPID := filepath.Join(t.TempDir(), "descendant-pid")
	tool := filepath.Join(t.TempDir(), "orphaning-gate")
	script := "#!/bin/sh\n" +
		"echo start >>" + shellTestQuote(starts) + "\n" +
		"/bin/sh -c 'trap \"\" TERM; echo $$ >" + shellTestQuote(descendantPID) +
		"; while :; do sleep 1; done' &\n" +
		"exit 0\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	timing := validationTiming{
		poll: 20 * time.Millisecond, cancelGrace: 500 * time.Millisecond, forceWait: 2 * time.Second,
	}
	type gateOutcome struct {
		result execx.Result
		err    error
	}
	firstDone := make(chan gateOutcome, 1)
	go func() {
		result, err := runGateCommandWithTiming(
			context.Background(), phase, []string{"make", "build"},
			execx.Cmd{Name: tool}, timing,
		)
		firstDone <- gateOutcome{result: result, err: err}
	}()
	waitForGateEvent(t, descendantPID, "")

	// Reach the same lane while the direct gate child is gone but its
	// TERM-ignoring, stdout-holding descendant remains. The duplicate must
	// reuse the still-running validation owner, never launch a second tool.
	secondDone := make(chan gateOutcome, 1)
	go func() {
		result, err := runGateCommandWithTiming(
			context.Background(), phase, []string{"make", "build"},
			execx.Cmd{Name: tool}, timing,
		)
		secondDone <- gateOutcome{result: result, err: err}
	}()
	waitForGateEvent(t, filepath.Join(phase, ".koryph-command", "events.jsonl"),
		`"existing_role":"validation"`)

	var first, second gateOutcome
	select {
	case first = <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("orphaning gate did not return after bounded teardown")
	}
	select {
	case second = <-secondDone:
	case <-time.After(3 * time.Second):
		t.Fatal("duplicate gate did not receive terminal authoritative result")
	}
	if first.result.ExitCode != guardedValidationInternalExit ||
		first.err == nil || !strings.Contains(first.err.Error(), "live descendant cohort") {
		t.Fatalf("first gate = (%+v, %v), want failed orphan cleanup", first.result, first.err)
	}
	if second.err != nil || second.result.ExitCode != guardedValidationInternalExit {
		t.Fatalf("duplicate gate = (%+v, %v), want reused failed result", second.result, second.err)
	}
	if !strings.Contains(second.result.Stdout, " role=validation status=") {
		t.Fatalf("validation reuse omitted owner role: %q", second.result.Stdout)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "start") != 1 {
		t.Fatalf("real gate starts = %q, %v; want exactly one", data, err)
	}
	pidBytes, err := os.ReadFile(descendantPID)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("gate descendant %d survived teardown", pid)
	}

	owners, err := filepath.Glob(filepath.Join(phase, ".koryph-command", "owner-*.json"))
	if err != nil || len(owners) != 1 {
		t.Fatalf("owner records = %v, %v", owners, err)
	}
	var owner commandguard.CommandOwner
	raw, err := os.ReadFile(owners[0])
	if err != nil || json.Unmarshal(raw, &owner) != nil {
		t.Fatalf("read terminal owner: %v", err)
	}
	if owner.Status != "failed" || owner.Role != commandguard.CommandRoleValidation {
		t.Fatalf("terminal owner = %+v", owner)
	}
	results, err := filepath.Glob(filepath.Join(phase, ".koryph-command", "result-*.json"))
	if err != nil || len(results) != 1 {
		t.Fatalf("terminal result records = %v, %v", results, err)
	}
}

func TestSignalValidationCommandRejectsChangedStartIdentity(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "10")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = cmd.Wait()
	}()
	identity, err := resmon.CurrentCommandIdentity(context.Background(), cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	forged := identity
	forged.StartID += ":reused"
	if err := signalValidationCommand(forged, syscall.SIGTERM); err == nil {
		t.Fatal("changed start identity authorized a process-group signal")
	}
	if _, live, err := resmon.InspectCommand(context.Background(), identity); err != nil || !live {
		t.Fatalf("real validation identity was disrupted: live=%v err=%v", live, err)
	}
}

func waitForGateEvent(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s in %s", want, path)
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func processExists(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
