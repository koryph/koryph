// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package execx runs external commands with explicit working directory and
// environment control. Koryph never mutates its own process environment to
// influence a child; children get a fully constructed env slice.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Result captures a completed command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// Cmd describes a command to run. Env == nil inherits the parent environment;
// otherwise Env is the complete child environment (use account.Env to build).
type Cmd struct {
	Dir     string
	Env     []string
	Name    string
	Args    []string
	Stdin   string
	Timeout time.Duration
}

// Run executes c and returns the result. A non-zero exit is not an error;
// callers inspect ExitCode. Errors are reserved for spawn/timeout failures.
func Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if c.Env != nil {
		cmd.Env = c.Env
	}
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	start := time.Now()
	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String(), Duration: time.Since(start)}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		if ctx.Err() == context.DeadlineExceeded {
			res.ExitCode = -1
			return res, fmt.Errorf("timeout after %s: %s %s", c.Timeout, c.Name, strings.Join(c.Args, " "))
		}
		res.ExitCode = -1
		return res, err
	}
	return res, nil
}

// MustSucceed runs c and returns an error when the exit code is non-zero,
// including trailing stderr for diagnostics.
func MustSucceed(ctx context.Context, c Cmd) (Result, error) {
	res, err := Run(ctx, c)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		tail := res.Stderr
		if len(tail) > 800 {
			tail = tail[len(tail)-800:]
		}
		return res, fmt.Errorf("%s %s: exit %d: %s", c.Name, strings.Join(c.Args, " "), res.ExitCode, strings.TrimSpace(tail))
	}
	return res, nil
}

// LookPath reports whether name resolves on PATH.
func LookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// BaseEnv returns a copy of the parent environment with the listed variables
// removed. Used to scrub e.g. ANTHROPIC_API_KEY or CLAUDE_CONFIG_DIR before
// explicit re-injection.
//
// BaseEnv is a DENYLIST: everything not named is forwarded. For untrusted
// children (dispatched agents) prefer AllowEnv, which forwards nothing except an
// explicit allowlist so credentials cannot leak by omission.
func BaseEnv(remove ...string) []string {
	drop := map[string]bool{}
	for _, k := range remove {
		drop[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if !drop[key] {
			env = append(env, kv)
		}
	}
	return env
}

// AllowEnv returns the parent environment filtered to an ALLOWLIST: a variable
// is forwarded only when its name is in allow OR begins with one of prefixes.
// It is the inverse of BaseEnv and the safe default for constructing an
// untrusted child's environment — a credential the caller forgot to name is
// dropped rather than leaked. Order follows os.Environ().
func AllowEnv(allow []string, prefixes []string) []string {
	keep := map[string]bool{}
	for _, k := range allow {
		keep[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if keep[key] || hasAnyPrefix(key, prefixes) {
			env = append(env, kv)
		}
	}
	return env
}

// hasAnyPrefix reports whether s starts with any of prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
