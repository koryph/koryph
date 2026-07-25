// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/commandguard"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/resmon"
)

func init() {
	registerCmd(command{
		name:    "command",
		summary: "run a phase command through Koryph's worker guard",
		run:     cmdCommand,
		hidden:  true,
		subs: []command{{
			name:    "exec",
			summary: "execute a resolved worker tool with phase-local single-flight",
			run:     cmdCommandExec,
			hidden:  true,
		}},
	})
}

func cmdCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		fmt.Fprintln(stderr, "koryph command is an internal worker command")
		return engine.ExitUsage
	}
	if args[0] != "exec" {
		fmt.Fprintf(stderr, "koryph command: unknown subcommand %q\n", args[0])
		return engine.ExitUsage
	}
	return cmdCommandExec(args[1:], stdout, stderr)
}

// cmdCommandExec is deliberately worker-only. Role is a compile-time choice,
// not a flag or environment input, so a phase cannot forge validation
// authority. Generated runtime shims call:
//
//	koryph command exec --real /absolute/resolved/tool -- ARGS...
func cmdCommandExec(args []string, stdout, stderr io.Writer) int {
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		fmt.Fprintln(stderr, "koryph command exec: '--' is required before tool arguments")
		return engine.ExitUsage
	}
	fs := newFlagSet("command exec", stderr)
	realArg := fs.String("real", "", "absolute canonical executable path")
	if err := fs.Parse(args[:separator]); err != nil {
		return engine.ExitUsage
	}
	if fs.NArg() != 0 || strings.TrimSpace(*realArg) == "" {
		fmt.Fprintln(stderr, "koryph command exec: --real ABSOLUTE is required")
		return engine.ExitUsage
	}
	real, err := canonicalWorkerExecutable(*realArg)
	if err != nil {
		fmt.Fprintf(stderr, "koryph command exec: %v\n", err)
		return engine.ExitFatal
	}
	phaseDir, phaseID, err := currentPhase()
	if err != nil {
		fmt.Fprintf(stderr, "koryph command exec: %v\n", err)
		return engine.ExitFatal
	}
	if filepath.Base(filepath.Clean(phaseDir)) != phaseID {
		fmt.Fprintln(stderr, "koryph command exec: phase directory does not match phase identity")
		return engine.ExitFatal
	}
	identity, err := resmon.EnsureIndependentProcessGroup(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "koryph command exec: %v\n", err)
		return engine.ExitFatal
	}
	toolArgs := args[separator+1:]
	effective := append([]string{real}, toolArgs...)
	guard := commandguard.NewProcessGuard(phaseDir)
	decision, err := guard.Acquire(context.Background(), commandguard.ProcessGuardRequest{
		Argv: effective, Role: commandguard.CommandRoleWorker, Identity: identity,
	})
	if err != nil {
		fmt.Fprintf(stderr, "koryph command exec: command guard: %v\n", err)
		return engine.ExitFatal
	}
	switch decision.Action {
	case commandguard.ProcessGuardDeny:
		fmt.Fprintf(stderr, "koryph command exec: %s\n", decision.Reason)
		return 126
	case commandguard.ProcessGuardReuse:
		fmt.Fprintf(stdout, "koryph command reuse: pid=%d role=%s status=%s log=%s\n",
			decision.Existing.Identity.PID, decision.Existing.Role,
			decision.Existing.Status, decision.Existing.LogPath)
		result, err := guard.Wait(context.Background(), decision)
		if err != nil {
			fmt.Fprintf(stderr, "koryph command exec: wait for authoritative command: %v\n", err)
			return engine.ExitFatal
		}
		return result.ExitCode
	case commandguard.ProcessGuardStart:
		return runGuardedWorkerTool(guard, decision, identity, real, toolArgs, stdout, stderr)
	default:
		fmt.Fprintln(stderr, "koryph command exec: invalid guard decision")
		return engine.ExitFatal
	}
}

func canonicalWorkerExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("--real must be absolute")
	}
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("resolve --real: %w", err)
	}
	if resolved != clean {
		return "", errors.New("--real must already be canonical (no symlink components)")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", errors.New("--real must name an executable regular file")
	}
	return resolved, nil
}

func runGuardedWorkerTool(
	guard *commandguard.ProcessGuard,
	decision commandguard.ProcessGuardDecision,
	identity resmon.CommandIdentity,
	real string,
	args []string,
	stdout, stderr io.Writer,
) int {
	var logFile *os.File
	if decision.Class.Scope != resmon.CommandFocused {
		var err error
		logFile, err = guard.OpenLog(decision)
		if err != nil {
			fmt.Fprintf(stderr, "koryph command exec: create command log: %v\n", err)
			_, _ = guard.Complete(decision, "failed", engine.ExitFatal)
			return engine.ExitFatal
		}
		defer logFile.Close()
	}

	cmd := exec.Command(real, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if logFile != nil {
		cmd.Stdout = io.MultiWriter(stdout, logFile)
		cmd.Stderr = io.MultiWriter(stderr, logFile)
	}
	// Join the independently-created wrapper group explicitly. The kernel
	// performs this before exec, so the full descendant cohort is observable
	// from the authenticated wrapper identity.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: identity.ProcessGroup}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "koryph command exec: start %s: %v\n", real, err)
		if decision.Class.Scope != resmon.CommandFocused {
			_, _ = guard.Complete(decision, "failed", 127)
		}
		return 127
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var monitorErr error
	var waitErr error
	for {
		select {
		case waitErr = <-waitCh:
			goto finished
		case <-ticker.C:
			if decision.Class.Scope != resmon.CommandFocused {
				if _, err := guard.Observe(context.Background(), decision); err != nil && monitorErr == nil {
					monitorErr = err
				}
			}
		}
	}

finished:
	exitCode := processExitCode(waitErr)
	if decision.Class.Scope != resmon.CommandFocused {
		if _, err := guard.Observe(context.Background(), decision); err != nil && monitorErr == nil {
			monitorErr = err
		}
		status := "passed"
		if monitorErr != nil {
			exitCode = engine.ExitFatal
		}
		if exitCode != 0 {
			status = "failed"
		}
		if _, err := guard.Complete(decision, status, exitCode); err != nil {
			fmt.Fprintf(stderr, "koryph command exec: persist command result: %v\n", err)
			return engine.ExitFatal
		}
		if monitorErr != nil {
			fmt.Fprintf(stderr, "koryph command exec: resource monitor: %v\n", monitorErr)
		}
	}
	return exitCode
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
	}
	return 1
}
