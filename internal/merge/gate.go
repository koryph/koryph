// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/commandguard"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/resmon"
)

// RunGate runs cmds sequentially in dir via `sh -c` (wrapped with
// `direnv exec <dir>` when direnv is on PATH and the dir's .envrc is
// allowed), accumulating combined output. It stops at the first non-zero
// exit; ok is true only when every command exits 0.
func RunGate(ctx context.Context, dir string, cmds []string) (ok bool, output string) {
	ok, output, _ = runGate(ctx, dir, "", cmds)
	return ok, output
}

// runGate is RunGate with an optional trusted product-owned phase context.
// When validationPhaseDir is set, known broad commands run as an authenticated
// validation cohort under the same phase-local guard used by worker shims.
func runGate(
	ctx context.Context,
	dir, validationPhaseDir string,
	cmds []string,
) (ok bool, output string, infraErr error) {
	// The gate compiles and runs agent-authored code (test files, Makefile
	// targets are not protected paths). Give it an allowlisted environment so a
	// planted test cannot read the orchestrator's ambient secrets (GH_TOKEN,
	// COSIGN_PASSWORD, KORYPH_PASSPHRASE, ANTHROPIC_API_KEY, cloud creds).
	// Project-legitimate env is re-supplied by `direnv exec` from the worktree's
	// own .envrc, layered on top of this baseline.
	env := execx.GateEnv()
	var b strings.Builder
	for _, c := range cmds {
		name, args := shellCmd(dir, c)
		classArgv := []string{"sh", "-c", c}
		b.WriteString("$ " + c + "\n")
		res, err := runGateCommand(ctx, validationPhaseDir, classArgv, execx.Cmd{
			Dir: dir, Name: name, Args: args, Env: env,
		})
		if err == nil && res.ExitCode != 0 && name == "direnv" && strings.Contains(res.Stderr, "is blocked") {
			// Fresh agent worktrees carry a never-approved .envrc; direnv
			// refuses to exec there. Fall back to a plain shell — the gate
			// must not fail on environment ceremony.
			b.WriteString("(direnv blocked; running without direnv)\n")
			res, err = runGateCommand(ctx, validationPhaseDir, classArgv, execx.Cmd{
				Dir: dir, Name: "sh", Args: []string{"-c", c}, Env: env,
			})
		}
		b.WriteString(res.Stdout)
		b.WriteString(res.Stderr)
		if err != nil {
			b.WriteString("\nerror: " + err.Error() + "\n")
			return false, b.String(), err
		}
		if res.ExitCode != 0 {
			return false, b.String(), nil
		}
	}
	return true, b.String(), nil
}

const guardedValidationInternalExit = 125

type validationTiming struct {
	poll        time.Duration
	cancelGrace time.Duration
	forceWait   time.Duration
}

var defaultValidationTiming = validationTiming{
	poll:        100 * time.Millisecond,
	cancelGrace: 5 * time.Second,
	forceWait:   2 * time.Second,
}

// runGateCommand starts a broad validation child in its own process group but
// holds it behind an inherited pipe until its exact kernel identity has been
// published by the trusted merge caller. No worker-controlled role token or
// environment variable can create a validation owner.
func runGateCommand(
	ctx context.Context,
	validationPhaseDir string,
	classArgv []string,
	spec execx.Cmd,
) (execx.Result, error) {
	return runGateCommandWithTiming(ctx, validationPhaseDir, classArgv, spec, defaultValidationTiming)
}

func runGateCommandWithTiming(
	ctx context.Context,
	validationPhaseDir string,
	classArgv []string,
	spec execx.Cmd,
	timing validationTiming,
) (execx.Result, error) {
	if validationPhaseDir == "" || resmon.ClassifyCommand(classArgv).Scope == resmon.CommandFocused {
		return execx.Run(ctx, spec)
	}

	guard := commandguard.NewProcessGuard(validationPhaseDir)
	ownerLease, err := guard.PrepareOwnerLease(classArgv)
	if err != nil {
		return execx.Result{ExitCode: -1}, fmt.Errorf("prepare validation owner lease: %w", err)
	}
	leaseFile, err := ownerLease.ExtraFile()
	if err != nil {
		_ = ownerLease.Close()
		return execx.Result{ExitCode: -1}, fmt.Errorf("open validation owner lease: %w", err)
	}
	supervisor, err := startValidationSupervisor(spec, leaseFile)
	if err != nil {
		_ = ownerLease.Close()
		return execx.Result{ExitCode: -1}, err
	}

	identity, err := resmon.CurrentCommandIdentity(ctx, supervisor.cmd.Process.Pid)
	if err != nil {
		_ = supervisor.stopSuspended(validationTiming{forceWait: 2 * time.Second})
		_ = ownerLease.Close()
		return execx.Result{ExitCode: -1}, fmt.Errorf("identify validation command: %w", err)
	}
	supervisor.identity = identity
	request := commandguard.ProcessGuardRequest{
		Argv: classArgv, Role: commandguard.CommandRoleValidation, Identity: identity,
		OwnerLease: ownerLease,
	}
	decision, err := guard.Acquire(ctx, request)
	if err != nil {
		_ = supervisor.stopSuspended(timing)
		_ = ownerLease.Close()
		return execx.Result{ExitCode: -1}, fmt.Errorf("publish validation command: %w", err)
	}

	// A worker-owned broad command may already be live. Wait for its exact
	// generation solely to prevent overlap, then acquire a new validation-owned
	// generation. A worker result is never accepted as post-rebase evidence.
	for decision.Action == commandguard.ProcessGuardReuse &&
		decision.Existing.Role == commandguard.CommandRoleWorker {
		if _, err := guard.Wait(ctx, decision); err != nil {
			_ = supervisor.stopSuspended(timing)
			_ = ownerLease.Close()
			return execx.Result{ExitCode: -1},
				fmt.Errorf("wait for worker command before validation: %w", err)
		}
		decision, err = guard.Acquire(ctx, request)
		if err != nil {
			_ = supervisor.stopSuspended(timing)
			_ = ownerLease.Close()
			return execx.Result{ExitCode: -1},
				fmt.Errorf("publish validation command after worker completion: %w", err)
		}
	}
	if decision.Action == commandguard.ProcessGuardReuse {
		if decision.Existing.Role != commandguard.CommandRoleValidation {
			_ = supervisor.stopSuspended(timing)
			_ = ownerLease.Close()
			return execx.Result{ExitCode: guardedValidationInternalExit},
				errors.New("trusted validation command reused a non-validation owner")
		}
		// This suspended helper never reached the real gate. The already-live
		// validation owner remains untouched and supplies the authoritative
		// result.
		if err := supervisor.stopSuspended(timing); err != nil {
			_ = ownerLease.Close()
			return execx.Result{ExitCode: guardedValidationInternalExit},
				fmt.Errorf("stop duplicate validation supervisor: %w", err)
		}
		_ = ownerLease.Close()
		result, err := guard.Wait(ctx, decision)
		return execx.Result{
			ExitCode: result.ExitCode, Duration: time.Since(supervisor.started),
			Stdout: "koryph validation reuse: pid=" + fmt.Sprint(decision.Existing.Identity.PID) +
				" role=" + string(decision.Existing.Role) +
				" status=" + decision.Existing.Status + " log=" + decision.Existing.LogPath + "\n",
		}, err
	}
	if decision.Action != commandguard.ProcessGuardStart {
		_ = supervisor.stopSuspended(timing)
		_ = ownerLease.Close()
		return execx.Result{ExitCode: guardedValidationInternalExit},
			errors.New("trusted validation command received a non-start decision")
	}

	return runAuthoritativeValidation(ctx, guard, decision, supervisor, timing)
}

type validationChildResult struct {
	exitCode int
	err      error
}

type validationSupervisor struct {
	cmd           *exec.Cmd
	identity      resmon.CommandIdentity
	releaseWrite  *os.File
	finalizeWrite *os.File
	waitCh        chan error
	childCh       chan validationChildResult
	stdout        bytes.Buffer
	stderr        bytes.Buffer
	log           lateWriter
	started       time.Time
	released      bool
	reaped        bool
}

func startValidationSupervisor(spec execx.Cmd, ownerLease *os.File) (*validationSupervisor, error) {
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		releaseRead.Close()
		releaseWrite.Close()
		return nil, err
	}
	finalizeRead, finalizeWrite, err := os.Pipe()
	if err != nil {
		releaseRead.Close()
		releaseWrite.Close()
		statusRead.Close()
		statusWrite.Close()
		return nil, err
	}
	closeAll := func() {
		releaseRead.Close()
		releaseWrite.Close()
		statusRead.Close()
		statusWrite.Close()
		finalizeRead.Close()
		finalizeWrite.Close()
	}

	// fd 3 releases the real child, fd 4 reports that direct child's exit,
	// fd 5 lets the trusted parent release the supervisor only after the
	// complete process group has drained, and fd 6 carries the owner lease
	// through the complete validation cohort. The supervisor ignores TERM,
	// while an explicit subshell resets TERM to default before exec'ing the
	// real child. The leader therefore remains authenticatable through the
	// graceful-stop window without teaching the gate to ignore cancellation.
	const trampoline = `trap '' TERM
IFS= read -r koryph_release <&3 || exit 125
[ "$koryph_release" = run ] || exit 125
(
	trap - TERM
	exec "$@"
) 3<&- 4>&- 5<&- &
koryph_child=$!
wait "$koryph_child"
koryph_status=$?
printf '%s\n' "$koryph_status" >&4 || exit 125
IFS= read -r koryph_finalize <&5 || exit 125
[ "$koryph_finalize" = done ] || exit 125
exit "$koryph_status"`
	args := []string{"-c", trampoline, "koryph-validation", spec.Name}
	args = append(args, spec.Args...)
	s := &validationSupervisor{
		releaseWrite: releaseWrite, finalizeWrite: finalizeWrite,
		waitCh:  make(chan error, 1),
		childCh: make(chan validationChildResult, 1), started: time.Now(),
	}
	s.cmd = exec.Command("/bin/sh", args...)
	s.cmd.Dir = spec.Dir
	s.cmd.Env = spec.Env
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd.ExtraFiles = []*os.File{releaseRead, statusWrite, finalizeRead, ownerLease}
	s.cmd.Stdout = io.MultiWriter(&s.stdout, &s.log)
	s.cmd.Stderr = io.MultiWriter(&s.stderr, &s.log)
	if err := s.cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	releaseRead.Close()
	statusWrite.Close()
	finalizeRead.Close()
	go func() { s.waitCh <- s.cmd.Wait() }()
	go func() {
		defer statusRead.Close()
		line, err := bufio.NewReader(statusRead).ReadString('\n')
		if err != nil {
			s.childCh <- validationChildResult{err: fmt.Errorf("read validation child status: %w", err)}
			return
		}
		value := strings.TrimSpace(line)
		exitCode, err := strconv.Atoi(value)
		if err != nil || exitCode < 0 || exitCode > 255 {
			s.childCh <- validationChildResult{err: fmt.Errorf("invalid validation child status %q", value)}
			return
		}
		s.childCh <- validationChildResult{exitCode: exitCode}
	}()
	return s, nil
}

func runAuthoritativeValidation(
	ctx context.Context,
	guard *commandguard.ProcessGuard,
	decision commandguard.ProcessGuardDecision,
	supervisor *validationSupervisor,
	timing validationTiming,
) (result execx.Result, retErr error) {
	exitCode := guardedValidationInternalExit
	var logFile *os.File

	// Once a validation owner is published, every return path first drains and
	// reaps its exact process group, then publishes a terminal result. This
	// prevents a retry from starting against a stale "running" owner while an
	// old gate descendant still touches the worktree.
	defer func() {
		if !supervisor.reaped {
			if err := supervisor.terminateAndReap(timing); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("teardown validation supervisor: %w", err))
			}
		}
		if logFile != nil {
			if err := logFile.Sync(); err != nil {
				exitCode = guardedValidationInternalExit
				retErr = errors.Join(retErr, fmt.Errorf("sync validation log: %w", err))
			}
			supervisor.log.set(io.Discard)
			if err := logFile.Close(); err != nil {
				exitCode = guardedValidationInternalExit
				retErr = errors.Join(retErr, fmt.Errorf("close validation log: %w", err))
			}
		}
		if _, err := guard.Observe(context.Background(), decision); err != nil {
			exitCode = guardedValidationInternalExit
			retErr = errors.Join(retErr, fmt.Errorf("final validation observation: %w", err))
		}
		status := "passed"
		if exitCode != 0 {
			status = "failed"
		}
		if _, err := guard.Complete(decision, status, exitCode); err != nil {
			exitCode = guardedValidationInternalExit
			retErr = errors.Join(retErr, fmt.Errorf("persist validation result: %w", err))
		}
		result = execx.Result{
			Stdout: supervisor.stdout.String(), Stderr: supervisor.stderr.String(),
			ExitCode: exitCode, Duration: time.Since(supervisor.started),
		}
	}()

	logFile, err := guard.OpenLog(decision)
	if err != nil {
		return result, fmt.Errorf("open validation log: %w", err)
	}
	supervisor.log.set(logFile)
	if _, err := guard.Observe(context.Background(), decision); err != nil {
		return result, fmt.Errorf("observe suspended validation command: %w", err)
	}
	if err := supervisor.release(); err != nil {
		return result, fmt.Errorf("release validation command: %w", err)
	}

	ticker := time.NewTicker(timing.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := supervisor.terminateAndReap(timing); err != nil {
				return result, errors.Join(ctx.Err(), err)
			}
			return result, ctx.Err()
		case child := <-supervisor.childCh:
			if child.err != nil {
				return result, child.err
			}
			drained, err := validationCohortDrained(supervisor.identity)
			if err != nil {
				return result, err
			}
			if !drained {
				if err := supervisor.terminateAndReap(timing); err != nil {
					return result, errors.Join(
						errors.New("validation child exited with a live descendant cohort"), err)
				}
				return result, errors.New("validation child exited with a live descendant cohort")
			}
			if err := supervisor.finishAndReap(child.exitCode, timing); err != nil {
				return result, err
			}
			exitCode = child.exitCode
			return result, nil
		case waitErr := <-supervisor.waitCh:
			supervisor.reaped = true
			return result, fmt.Errorf("validation supervisor exited before cohort drain: %w", waitErr)
		case <-ticker.C:
			if _, err := guard.Observe(context.Background(), decision); err != nil {
				return result, fmt.Errorf("observe validation command: %w", err)
			}
		}
	}
}

func (s *validationSupervisor) release() error {
	if _, err := io.WriteString(s.releaseWrite, "run\n"); err != nil {
		s.releaseWrite.Close()
		return err
	}
	if err := s.releaseWrite.Close(); err != nil {
		return err
	}
	s.released = true
	return nil
}

func (s *validationSupervisor) stopSuspended(timing validationTiming) error {
	_ = s.releaseWrite.Close()
	_ = s.finalizeWrite.Close()
	timer := time.NewTimer(timing.forceWait)
	defer timer.Stop()
	select {
	case <-s.waitCh:
		s.reaped = true
		return nil
	case <-timer.C:
	}
	if s.identity.PID <= 0 {
		return errors.New("suspended validation supervisor did not exit")
	}
	if err := signalValidationCommand(s.identity, syscall.SIGKILL); err != nil {
		return err
	}
	return s.awaitReap(timing.forceWait)
}

func validationCohortDrained(identity resmon.CommandIdentity) (bool, error) {
	probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sample, live, err := resmon.InspectCommand(probeCtx, identity)
	if err != nil {
		return false, fmt.Errorf("inspect validation cohort: %w", err)
	}
	if !live {
		return false, errors.New("validation supervisor identity is no longer live")
	}
	return sample.PIDs == 1, nil
}

func (s *validationSupervisor) finishAndReap(childExit int, timing validationTiming) error {
	if _, err := io.WriteString(s.finalizeWrite, "done\n"); err != nil {
		_ = s.finalizeWrite.Close()
		return fmt.Errorf("release drained validation supervisor: %w", err)
	}
	if err := s.finalizeWrite.Close(); err != nil {
		return err
	}
	timer := time.NewTimer(timing.forceWait)
	defer timer.Stop()
	select {
	case waitErr := <-s.waitCh:
		s.reaped = true
		if got := guardedProcessExitCode(waitErr); got != childExit {
			return fmt.Errorf("validation supervisor exit %d does not match child exit %d", got, childExit)
		}
		return nil
	case <-timer.C:
	}
	if err := signalValidationCommand(s.identity, syscall.SIGKILL); err != nil {
		return fmt.Errorf("stop unresponsive drained validation supervisor: %w", err)
	}
	if err := s.awaitReap(timing.forceWait); err != nil {
		return err
	}
	return errors.New("drained validation supervisor required force-stop")
}

func (s *validationSupervisor) terminateAndReap(timing validationTiming) error {
	if s.reaped {
		return nil
	}
	if !s.released {
		return s.stopSuspended(timing)
	}
	drained, err := validationCohortDrained(s.identity)
	if err == nil && drained {
		return s.finishAndReap(guardedValidationInternalExit, timing)
	}
	if err := signalValidationCommand(s.identity, syscall.SIGTERM); err != nil {
		return fmt.Errorf("gracefully stop validation cohort: %w", err)
	}
	deadline := time.NewTimer(timing.cancelGrace)
	ticker := time.NewTicker(timing.poll)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case waitErr := <-s.waitCh:
			s.reaped = true
			return fmt.Errorf("validation supervisor exited before group drain: %w", waitErr)
		case <-ticker.C:
			drained, err := validationCohortDrained(s.identity)
			if err != nil {
				return err
			}
			if drained {
				return s.finishAndReap(guardedValidationInternalExit, timing)
			}
		case <-deadline.C:
			if err := signalValidationCommand(s.identity, syscall.SIGKILL); err != nil {
				return fmt.Errorf("force-stop validation cohort after grace period: %w", err)
			}
			return s.awaitReap(timing.forceWait)
		}
	}
}

func (s *validationSupervisor) awaitReap(timeout time.Duration) error {
	defer s.releaseWrite.Close()
	defer s.finalizeWrite.Close()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.waitCh:
		s.reaped = true
		return nil
	case <-timer.C:
		return errors.New("validation supervisor did not reap after exact-group force-stop")
	}
}

func signalValidationCommand(identity resmon.CommandIdentity, signal syscall.Signal) error {
	probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, live, err := resmon.InspectCommand(probeCtx, identity)
	if err != nil {
		return fmt.Errorf("authenticate validation command: %w", err)
	}
	if !live {
		return errors.New("validation command identity is no longer live")
	}
	pgid, err := syscall.Getpgid(identity.PID)
	if err != nil {
		return fmt.Errorf("read validation process group: %w", err)
	}
	if pgid != identity.ProcessGroup || pgid != identity.PID {
		return fmt.Errorf("validation process-group identity changed: pid=%d recorded=%d live=%d",
			identity.PID, identity.ProcessGroup, pgid)
	}
	if err := syscall.Kill(-pgid, signal); err != nil {
		return fmt.Errorf("signal validation process group %d with %s: %w", pgid, signal, err)
	}
	return nil
}

type lateWriter struct {
	mu     sync.Mutex
	target io.Writer
}

func (w *lateWriter) set(target io.Writer) {
	w.mu.Lock()
	w.target = target
	w.mu.Unlock()
}

func (w *lateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.target == nil {
		return len(p), nil
	}
	return w.target.Write(p)
}

func guardedProcessExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
	}
	return guardedValidationInternalExit
}

// shellCmd builds a `sh -c` invocation, wrapped with `direnv exec <dir>` when
// direnv is available so project env is loaded before the gate command runs.
func shellCmd(dir, command string) (string, []string) {
	if execx.LookPath("direnv") {
		return "direnv", []string{"exec", dir, "sh", "-c", command}
	}
	return "sh", []string{"-c", command}
}
