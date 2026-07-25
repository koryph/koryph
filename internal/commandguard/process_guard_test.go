// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package commandguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/resmon"
)

func realTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func testProcessGuard(t *testing.T) (*ProcessGuard, map[string]bool) {
	t.Helper()
	live := map[string]bool{}
	g := NewProcessGuard(realTempDir(t))
	g.now = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }
	g.inspect = func(_ context.Context, identity resmon.CommandIdentity) (resmon.Sample, bool, error) {
		return resmon.Sample{RSSKB: 512, CPUSeconds: 3}, live[identity.StartID], nil
	}
	var generation atomic.Int64
	g.newGeneration = func() (string, error) {
		return fmt.Sprintf("%032x", generation.Add(1)), nil
	}
	g.waitPoll = time.Millisecond
	g.waitTimeout = time.Second
	g.resultGrace = 20 * time.Millisecond
	return g, live
}

func broadRequest(pid int, start string) ProcessGuardRequest {
	return ProcessGuardRequest{
		Argv: []string{"go", "test", "./..."}, Role: CommandRoleWorker,
		Identity: resmon.CommandIdentity{PID: pid, StartID: start, ProcessGroup: pid},
	}
}

func releaseTestOwnerLease(t *testing.T, decision *ProcessGuardDecision) {
	t.Helper()
	if decision.lease == nil {
		t.Fatal("start decision has no owner lease")
	}
	if err := decision.lease.Close(); err != nil {
		t.Fatal(err)
	}
	decision.lease = nil
}

func TestProcessGuardSingleFlightReusesLiveCommandAndResult(t *testing.T) {
	g, live := testProcessGuard(t)
	ownerReq := broadRequest(101, "birth:owner")
	live[ownerReq.Identity.StartID] = true
	first, err := g.Acquire(context.Background(), ownerReq)
	if err != nil || first.Action != ProcessGuardStart {
		t.Fatalf("first Acquire = (%+v, %v), want start", first, err)
	}
	duplicateReq := broadRequest(202, "birth:duplicate")
	second, err := g.Acquire(context.Background(), duplicateReq)
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != ProcessGuardReuse {
		t.Fatalf("second action = %q, want reuse", second.Action)
	}
	if second.Existing.Identity != ownerReq.Identity || second.Existing.Status != "running" ||
		second.Existing.Role != CommandRoleWorker ||
		second.Existing.LogPath == "" || second.Existing.Generation == "" {
		t.Fatalf("reuse evidence = %+v", second.Existing)
	}

	g.now = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 2, 0, time.UTC) }
	if ok, err := g.Observe(context.Background(), first); err != nil || !ok {
		t.Fatalf("Observe = (%v, %v)", ok, err)
	}
	g.now = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 5, 0, time.UTC) }
	if _, err := g.Complete(first, "failed", 7); err != nil {
		t.Fatal(err)
	}
	got, err := g.Wait(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 7 || got.Generation != first.Existing.Generation || got.Identity != first.Existing.Identity ||
		got.PeakRSSKB != 512 || got.CPUSeconds != 3 || got.DurationMS != 5000 {
		t.Fatalf("reused result = %+v", got)
	}
}

func TestProcessGuardConcurrentFlockPublishesExactlyOneOwner(t *testing.T) {
	g, live := testProcessGuard(t)
	const claimers = 16
	for i := range claimers {
		live["birth:"+strconv.Itoa(i)] = true
	}
	decisions := make(chan ProcessGuardDecision, claimers)
	errs := make(chan error, claimers)
	var wg sync.WaitGroup
	for i := range claimers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			decision, err := g.Acquire(context.Background(), broadRequest(100+i, "birth:"+strconv.Itoa(i)))
			if err != nil {
				errs <- err
				return
			}
			decisions <- decision
		}(i)
	}
	wg.Wait()
	close(decisions)
	close(errs)
	for err := range errs {
		t.Errorf("Acquire: %v", err)
	}
	starts, reuses := 0, 0
	var owner ProcessGuardDecision
	for decision := range decisions {
		switch decision.Action {
		case ProcessGuardStart:
			starts++
			owner = decision
		case ProcessGuardReuse:
			reuses++
		}
	}
	if starts != 1 || reuses != claimers-1 {
		t.Fatalf("starts/reuses = %d/%d, want 1/%d", starts, reuses, claimers-1)
	}
	releaseTestOwnerLease(t, &owner)
}

func TestProcessGuardRolesFailClosedAndWorkerGateDenied(t *testing.T) {
	g, _ := testProcessGuard(t)
	identity := resmon.CommandIdentity{PID: 101, StartID: "birth", ProcessGroup: 101}
	if _, err := g.Acquire(context.Background(), ProcessGuardRequest{
		Argv: []string{"go", "test", "./internal/resmon"}, Identity: identity,
	}); err == nil {
		t.Fatal("missing role was accepted")
	}
	if _, err := g.Acquire(context.Background(), ProcessGuardRequest{
		Argv: []string{"go", "test", "./internal/resmon"}, Role: "forged", Identity: identity,
	}); err == nil {
		t.Fatal("unknown role was accepted")
	}
	denied, err := g.Acquire(context.Background(), ProcessGuardRequest{
		Argv: []string{"make", "gate-agent"}, Role: CommandRoleWorker, Identity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if denied.Action != ProcessGuardDeny {
		t.Fatalf("gate action = %q, want deny", denied.Action)
	}
	focused, err := g.Acquire(context.Background(), ProcessGuardRequest{
		Argv: []string{"go", "test", "./internal/resmon"}, Role: CommandRoleWorker, Identity: identity,
	})
	if err != nil || focused.Action != ProcessGuardStart || focused.Class.Scope != resmon.CommandFocused {
		t.Fatalf("focused Acquire = (%+v, %v)", focused, err)
	}
}

func TestProcessGuardWorkerReusesTrustedValidationGate(t *testing.T) {
	g, live := testProcessGuard(t)
	validation := ProcessGuardRequest{
		Argv: []string{"sh", "-c", "make gate-agent"}, Role: CommandRoleValidation,
		Identity: resmon.CommandIdentity{PID: 101, StartID: "validation", ProcessGroup: 101},
	}
	live["validation"] = true
	owner, err := g.Acquire(context.Background(), validation)
	if err != nil || owner.Action != ProcessGuardStart {
		t.Fatalf("validation Acquire = (%+v, %v)", owner, err)
	}
	worker := ProcessGuardRequest{
		Argv: []string{"make", "gate"}, Role: CommandRoleWorker,
		Identity: resmon.CommandIdentity{PID: 202, StartID: "worker", ProcessGroup: 202},
	}
	reuse, err := g.Acquire(context.Background(), worker)
	if err != nil || reuse.Action != ProcessGuardReuse {
		t.Fatalf("worker Acquire = (%+v, %v), want reuse", reuse, err)
	}
	if reuse.Existing.Identity != validation.Identity {
		t.Fatalf("worker reused %+v, want validation owner %+v", reuse.Existing.Identity, validation.Identity)
	}
	if reuse.Existing.Role != CommandRoleValidation {
		t.Fatalf("worker reused role %q, want validation", reuse.Existing.Role)
	}
	if _, err := g.Complete(owner, "failed", 7); err != nil {
		t.Fatal(err)
	}
	result, err := g.Wait(context.Background(), reuse)
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("worker result = (%+v, %v), want authoritative exit 7", result, err)
	}
}

func TestProcessGuardReplacesOnlyAuthenticatedStaleOwner(t *testing.T) {
	g, live := testProcessGuard(t)
	old := broadRequest(101, "birth:old")
	live[old.Identity.StartID] = true
	first, err := g.Acquire(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	live[old.Identity.StartID] = false
	releaseTestOwnerLease(t, &first)
	replacement := broadRequest(101, "birth:new")
	live[replacement.Identity.StartID] = true
	got, err := g.Acquire(context.Background(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != ProcessGuardStart || got.Existing.Identity != replacement.Identity ||
		got.Existing.Generation == first.Existing.Generation {
		t.Fatalf("replacement = %+v", got)
	}
}

func TestProcessGuardResultRemainsKeyedToOldGeneration(t *testing.T) {
	g, live := testProcessGuard(t)
	firstReq := broadRequest(101, "birth:first")
	live[firstReq.Identity.StartID] = true
	first, err := g.Acquire(context.Background(), firstReq)
	if err != nil {
		t.Fatal(err)
	}
	firstReuseReq := broadRequest(102, "birth:waiter")
	reuse, err := g.Acquire(context.Background(), firstReuseReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Complete(first, "passed", 0); err != nil {
		t.Fatal(err)
	}
	secondReq := broadRequest(103, "birth:second")
	live[secondReq.Identity.StartID] = true
	second, err := g.Acquire(context.Background(), secondReq)
	if err != nil || second.Action != ProcessGuardStart {
		t.Fatalf("second generation = (%+v, %v)", second, err)
	}
	oldResult, err := g.Wait(context.Background(), reuse)
	if err != nil {
		t.Fatal(err)
	}
	if oldResult.Generation != first.Existing.Generation || oldResult.Generation == second.Existing.Generation {
		t.Fatalf("old result generation = %q; first=%q second=%q",
			oldResult.Generation, first.Existing.Generation, second.Existing.Generation)
	}
}

func TestProcessGuardWaitIsBoundedWhenOwnerExitsWithoutResult(t *testing.T) {
	g, live := testProcessGuard(t)
	g.waitTimeout = 40 * time.Millisecond
	ownerReq := broadRequest(101, "birth:owner")
	live[ownerReq.Identity.StartID] = true
	owner, err := g.Acquire(context.Background(), ownerReq)
	if err != nil {
		t.Fatal(err)
	}
	reuse, err := g.Acquire(context.Background(), broadRequest(102, "birth:duplicate"))
	if err != nil {
		t.Fatal(err)
	}
	live[ownerReq.Identity.StartID] = false
	// Simulate an owner process crash: the kernel releases its flock even
	// though no generation-bound result was published.
	releaseTestOwnerLease(t, &owner)
	if _, err := g.Wait(context.Background(), reuse); err == nil ||
		!strings.Contains(err.Error(), "without its generation-bound result") {
		t.Fatalf("Wait error = %v", err)
	}
}

func TestProcessGuardWaitAllowsTerminalPublicationAfterOwnerExit(t *testing.T) {
	g, live := testProcessGuard(t)
	g.resultGrace = 200 * time.Millisecond
	ownerReq := broadRequest(101, "birth:owner")
	live[ownerReq.Identity.StartID] = true
	owner, err := g.Acquire(context.Background(), ownerReq)
	if err != nil {
		t.Fatal(err)
	}
	reuse, err := g.Acquire(context.Background(), broadRequest(102, "birth:duplicate"))
	if err != nil {
		t.Fatal(err)
	}
	live[ownerReq.Identity.StartID] = false
	completed := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, err := g.Complete(owner, "failed", 125)
		completed <- err
	}()
	result, err := g.Wait(context.Background(), reuse)
	if err != nil || result.Status != "failed" || result.ExitCode != 125 {
		t.Fatalf("Wait = (%+v, %v), want delayed terminal result", result, err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}

func TestProcessGuardRejectsSymlinkAndNonRegularEvidenceWithoutOutsideWrite(t *testing.T) {
	tests := []struct {
		name   string
		attack func(t *testing.T, g *ProcessGuard, root, outside string, class resmon.CommandClass)
		act    func(g *ProcessGuard, req ProcessGuardRequest) error
	}{
		{
			name: "events symlink",
			attack: func(t *testing.T, _ *ProcessGuard, root, outside string, _ resmon.CommandClass) {
				t.Helper()
				mustSymlink(t, outside, filepath.Join(root, commandEventsName))
			},
			act: func(g *ProcessGuard, req ProcessGuardRequest) error {
				_, err := g.Acquire(context.Background(), req)
				return err
			},
		},
		{
			name: "owner symlink",
			attack: func(t *testing.T, _ *ProcessGuard, root, outside string, class resmon.CommandClass) {
				t.Helper()
				mustSymlink(t, outside, filepath.Join(root, ownerFileName(class.Signature)))
			},
			act: func(g *ProcessGuard, req ProcessGuardRequest) error {
				_, err := g.Acquire(context.Background(), req)
				return err
			},
		},
		{
			name: "result symlink",
			attack: func(t *testing.T, _ *ProcessGuard, root, outside string, class resmon.CommandClass) {
				t.Helper()
				mustSymlink(t, outside, filepath.Join(root, resultFileName(class.Signature, fmt.Sprintf("%032x", 1))))
			},
			act: func(g *ProcessGuard, req ProcessGuardRequest) error {
				_, err := g.Acquire(context.Background(), req)
				return err
			},
		},
		{
			name: "lock symlink",
			attack: func(t *testing.T, _ *ProcessGuard, root, outside string, class resmon.CommandClass) {
				t.Helper()
				mustSymlink(t, outside, filepath.Join(root, lockFileName(class.Signature)))
			},
			act: func(g *ProcessGuard, req ProcessGuardRequest) error {
				_, err := g.Acquire(context.Background(), req)
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, live := testProcessGuard(t)
			live["birth"] = true
			root := filepath.Join(g.phaseDir, commandGuardDirName)
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(realTempDir(t), "outside")
			const sentinel = "do-not-change"
			if err := os.WriteFile(outside, []byte(sentinel), 0o600); err != nil {
				t.Fatal(err)
			}
			class := resmon.ClassifyCommand([]string{"go", "test", "./..."})
			tc.attack(t, g, root, outside, class)
			err := tc.act(g, broadRequest(101, "birth"))
			if err == nil {
				t.Fatal("symlink attack was accepted")
			}
			data, readErr := os.ReadFile(outside)
			if readErr != nil || string(data) != sentinel {
				t.Fatalf("outside file changed: %q, %v", data, readErr)
			}
		})
	}
}

func TestProcessGuardRejectsLogSymlinkForValidDecision(t *testing.T) {
	g, live := testProcessGuard(t)
	live["birth"] = true
	decision, err := g.Acquire(context.Background(), broadRequest(101, "birth"))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(realTempDir(t), "outside")
	const sentinel = "do-not-change"
	if err := os.WriteFile(outside, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, decision.Existing.LogPath)
	if _, err := g.OpenLog(decision); err == nil {
		t.Fatal("log symlink was accepted")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != sentinel {
		t.Fatalf("outside file changed: %q, %v", data, err)
	}
}

func TestProcessGuardRejectsSymlinkedPhaseAndRoot(t *testing.T) {
	for _, which := range []string{"phase", "root"} {
		t.Run(which, func(t *testing.T) {
			realPhase := realTempDir(t)
			phase := realPhase
			if which == "phase" {
				phase = filepath.Join(realTempDir(t), "phase-link")
				mustSymlink(t, realPhase, phase)
			} else {
				mustSymlink(t, realTempDir(t), filepath.Join(realPhase, commandGuardDirName))
			}
			g := NewProcessGuard(phase)
			_, err := g.Acquire(context.Background(), broadRequest(101, "birth"))
			if err == nil {
				t.Fatalf("%s symlink was accepted", which)
			}
		})
	}
}

func TestProcessGuardPersistsStructuredEvents(t *testing.T) {
	g, live := testProcessGuard(t)
	req := broadRequest(101, "birth")
	live["birth"] = true
	decision, err := g.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Complete(decision, "passed", 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(g.phaseDir, commandGuardDirName, commandEventsName))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event resmon.CommandEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Schema != "koryph.command-event/v1" {
			t.Fatalf("event schema = %q", event.Schema)
		}
		if event.Generation != decision.Existing.Generation {
			t.Fatalf("event generation = %q, want %q", event.Generation, decision.Existing.Generation)
		}
		if event.Role != string(CommandRoleWorker) {
			t.Fatalf("event role = %q, want worker", event.Role)
		}
		kinds = append(kinds, event.Event)
	}
	if strings.Join(kinds, ",") != "start,complete" {
		t.Fatalf("events = %v", kinds)
	}
}

func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatal(err)
	}
}

func TestCommandGuardFSRejectsDirectoryInPlaceOfRecord(t *testing.T) {
	g, _ := testProcessGuard(t)
	class := resmon.ClassifyCommand([]string{"go", "test", "./..."})
	root := filepath.Join(g.phaseDir, commandGuardDirName)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ownerFileName(class.Signature)), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Acquire(context.Background(), broadRequest(101, "birth")); err == nil {
		t.Fatal("directory owner record was accepted")
	}
}

func TestProcessGuardWaitRejectsMismatchedResult(t *testing.T) {
	g, live := testProcessGuard(t)
	req := broadRequest(101, "birth")
	live["birth"] = true
	first, err := g.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	reuse, err := g.Acquire(context.Background(), broadRequest(102, "other"))
	if err != nil {
		t.Fatal(err)
	}
	err = g.withSignatureLock(first.Class.Signature, func(fs *commandGuardFS) error {
		bad := CommandResult{
			Schema: commandResultSchema, Generation: first.Existing.Generation,
			Signature: first.Existing.Signature,
			Identity:  resmon.CommandIdentity{PID: 999, StartID: "wrong", ProcessGroup: 999},
		}
		return fs.writeJSON(resultFileName(first.Class.Signature, first.Existing.Generation), bad)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Wait(context.Background(), reuse); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Wait error = %v", err)
	}
}

func TestProcessGuardOpenLogRequiresAuthoritativeBroadDecision(t *testing.T) {
	g, _ := testProcessGuard(t)
	if _, err := g.OpenLog(ProcessGuardDecision{Action: ProcessGuardReuse}); err == nil {
		t.Fatal("reuse decision opened an authoritative log")
	}
}

func TestProcessGuardWaitRequiresReuse(t *testing.T) {
	g, _ := testProcessGuard(t)
	if _, err := g.Wait(context.Background(), ProcessGuardDecision{Action: ProcessGuardStart}); err == nil {
		t.Fatal("start decision was accepted by Wait")
	}
}

func TestProcessGuardCompleteRequiresStart(t *testing.T) {
	g, _ := testProcessGuard(t)
	if _, err := g.Complete(ProcessGuardDecision{Action: ProcessGuardReuse}, "passed", 0); err == nil {
		t.Fatal("reuse decision completed an owner")
	}
}

func TestProcessGuardOwnerLeaseSurvivesResourceProbeFailure(t *testing.T) {
	g, live := testProcessGuard(t)
	req := broadRequest(101, "birth")
	live["birth"] = true
	owner, err := g.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	g.inspect = func(context.Context, resmon.CommandIdentity) (resmon.Sample, bool, error) {
		return resmon.Sample{}, false, errors.New("probe failed")
	}
	reuse, err := g.Acquire(context.Background(), broadRequest(102, "other"))
	if err != nil || reuse.Action != ProcessGuardReuse {
		t.Fatalf("owner lease was not authoritative after probe failure: (%+v, %v)", reuse, err)
	}
	if _, err := g.Observe(context.Background(), owner); err == nil {
		t.Fatal("resource probe failure was hidden")
	}
}

func TestProcessGuardStrictlyRejectsMalformedAndForgedOwnerState(t *testing.T) {
	tests := []struct {
		name    string
		rewrite func([]byte, *CommandOwner) []byte
	}{
		{
			name: "unknown field",
			rewrite: func(raw []byte, _ *CommandOwner) []byte {
				return append(bytes.TrimSpace(raw)[:len(bytes.TrimSpace(raw))-1], []byte(`,"forged":true}`)...)
			},
		},
		{name: "trailing JSON", rewrite: func(raw []byte, _ *CommandOwner) []byte {
			return append(raw, []byte("{}\n")...)
		}},
		{name: "oversized", rewrite: func([]byte, *CommandOwner) []byte {
			return bytes.Repeat([]byte(" "), commandStateMaxBytes+1)
		}},
		{name: "bad generation", rewrite: mutateOwner(func(o *CommandOwner) { o.Generation = strings.Repeat("g", commandGenerationLen) })},
		{name: "bad status", rewrite: mutateOwner(func(o *CommandOwner) { o.Status = "complete" })},
		{name: "bad role", rewrite: mutateOwner(func(o *CommandOwner) { o.Role = "forged" })},
		{name: "wrong class", rewrite: mutateOwner(func(o *CommandOwner) { o.Class = resmon.CommandFocused })},
		{name: "wrong signature", rewrite: mutateOwner(func(o *CommandOwner) { o.Signature = strings.Repeat("1", commandSignatureLen) })},
		{name: "outside log", rewrite: mutateOwner(func(o *CommandOwner) { o.LogPath = "/tmp/forged-command.log" })},
		{name: "non-independent identity", rewrite: mutateOwner(func(o *CommandOwner) { o.Identity.ProcessGroup++ })},
		{name: "inconsistent duration", rewrite: mutateOwner(func(o *CommandOwner) { o.DurationMS++ })},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, live := testProcessGuard(t)
			live["birth"] = true
			first, err := g.Acquire(context.Background(), broadRequest(101, "birth"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(g.phaseDir, commandGuardDirName, ownerFileName(first.Class.Signature))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var owner CommandOwner
			if err := json.Unmarshal(raw, &owner); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.rewrite(raw, &owner), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Acquire(context.Background(), broadRequest(202, "other")); err == nil {
				t.Fatal("malformed/forged owner was accepted")
			}
		})
	}
}

func mutateOwner(mutate func(*CommandOwner)) func([]byte, *CommandOwner) []byte {
	return func(_ []byte, owner *CommandOwner) []byte {
		mutate(owner)
		raw, _ := json.Marshal(owner)
		return raw
	}
}

func TestProcessGuardStrictlyRejectsMalformedAndForgedResultState(t *testing.T) {
	tests := []struct {
		name    string
		rewrite func([]byte, *CommandResult) []byte
	}{
		{
			name: "unknown field",
			rewrite: func(raw []byte, _ *CommandResult) []byte {
				return append(bytes.TrimSpace(raw)[:len(bytes.TrimSpace(raw))-1], []byte(`,"forged":true}`)...)
			},
		},
		{name: "trailing JSON", rewrite: func(raw []byte, _ *CommandResult) []byte {
			return append(raw, []byte("{}\n")...)
		}},
		{name: "oversized", rewrite: func([]byte, *CommandResult) []byte {
			return bytes.Repeat([]byte(" "), commandStateMaxBytes+1)
		}},
		{name: "bad generation", rewrite: mutateResult(func(r *CommandResult) { r.Generation = strings.Repeat("g", commandGenerationLen) })},
		{name: "bad status", rewrite: mutateResult(func(r *CommandResult) { r.Status = "running" })},
		{name: "wrong role", rewrite: mutateResult(func(r *CommandResult) { r.Role = CommandRoleValidation })},
		{name: "outside log", rewrite: mutateResult(func(r *CommandResult) { r.LogPath = "/tmp/forged-command.log" })},
		{name: "wrong identity", rewrite: mutateResult(func(r *CommandResult) { r.Identity.StartID = "forged" })},
		{name: "inconsistent duration", rewrite: mutateResult(func(r *CommandResult) { r.DurationMS++ })},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, live := testProcessGuard(t)
			live["birth"] = true
			first, err := g.Acquire(context.Background(), broadRequest(101, "birth"))
			if err != nil {
				t.Fatal(err)
			}
			reuse, err := g.Acquire(context.Background(), broadRequest(202, "other"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := g.Complete(first, "passed", 0); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(g.phaseDir, commandGuardDirName,
				resultFileName(first.Class.Signature, first.Existing.Generation))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var result CommandResult
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.rewrite(raw, &result), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Wait(context.Background(), reuse); err == nil {
				t.Fatal("malformed/forged result was accepted")
			}
		})
	}
}

func mutateResult(mutate func(*CommandResult)) func([]byte, *CommandResult) []byte {
	return func(_ []byte, result *CommandResult) []byte {
		mutate(result)
		raw, _ := json.Marshal(result)
		return raw
	}
}

func TestProcessGuardRejectsInvalidCompletionTuple(t *testing.T) {
	g, live := testProcessGuard(t)
	live["birth"] = true
	first, err := g.Acquire(context.Background(), broadRequest(101, "birth"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tuple := range []struct {
		status string
		exit   int
	}{{"running", 0}, {"passed", 1}, {"failed", 0}, {"failed", 256}} {
		if _, err := g.Complete(first, tuple.status, tuple.exit); err == nil {
			t.Fatalf("Complete(%q, %d) succeeded", tuple.status, tuple.exit)
		}
	}
}
