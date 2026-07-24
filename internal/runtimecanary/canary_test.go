// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtimecanary

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

func canaryOpts(t *testing.T) Options {
	t.Helper()
	return Options{
		Runtime:    runtimetest.Stub{StubName: "fake", Caps: runtime.Capabilities{Personas: true, ModelSelect: true}},
		RepoRoot:   "/repo",
		Worktree:   "/worktree",
		ScratchDir: t.TempDir(),
		Model:      "standard-model",
		ProofNonce: "0123456789abcdef0123456789abcdef",
		Profile:    runtime.Profile{Name: "work", ConfigDir: "/target/profile"},
		Verify:     func(context.Context) (string, error) { return "identity", nil },
	}
}

func TestRunProjectsProfileAndFixedContract(t *testing.T) {
	o := canaryOpts(t)
	var spec runtime.JSONSpec
	var ex runtime.JSONExec
	o.Spawn = func(_ context.Context, _ runtime.Runtime, gotSpec runtime.JSONSpec, gotExec runtime.JSONExec) (execx.Result, error) {
		spec, ex = gotSpec, gotExec
		proof := filepath.Join(gotSpec.ScratchDir, "runtime-canary-proof-"+o.ProofNonce)
		if err := os.WriteFile(proof, []byte(o.ProofNonce+"\n"), 0o644); err != nil {
			return execx.Result{}, err
		}
		return execx.Result{Stdout: `{"type":"result","is_error":false,"result":"{\"ok\":true,\"token\":\"koryph-runtime-canary-v1\"}"}`}, nil
	}
	got := Run(context.Background(), o)
	if !got.OK || got.Identity != "identity" {
		t.Fatalf("result = %+v", got)
	}
	if spec.Profile.ConfigDir != "/target/profile" || spec.PermissionMode != "dontAsk" ||
		spec.Model != "standard-model" || spec.SpawnKind != "runtime-canary" {
		t.Fatalf("spec = %+v", spec)
	}
	if !strings.Contains(ex.Stdin, "printf '%s\\n'") ||
		!strings.Contains(ex.Stdin, o.ProofNonce) ||
		!strings.Contains(ex.Stdin, `{"ok":true,"token":"koryph-runtime-canary-v1"}`) {
		t.Fatalf("fixed prompt missing canary contract: %q", ex.Stdin)
	}
}

func TestRunFailsClosedBeforeSpawnOnIdentityError(t *testing.T) {
	o := canaryOpts(t)
	o.Verify = func(context.Context) (string, error) { return "", errors.New("secret provider output") }
	called := false
	o.Spawn = func(context.Context, runtime.Runtime, runtime.JSONSpec, runtime.JSONExec) (execx.Result, error) {
		called = true
		return execx.Result{}, nil
	}
	got := Run(context.Background(), o)
	if got.OK || got.Kind != "identity" || called || strings.Contains(got.Detail, "secret") {
		t.Fatalf("result=%+v spawnCalled=%v", got, called)
	}
}

func TestRunRejectsUnprovedToolAction(t *testing.T) {
	o := canaryOpts(t)
	o.Spawn = func(context.Context, runtime.Runtime, runtime.JSONSpec, runtime.JSONExec) (execx.Result, error) {
		return execx.Result{Stdout: `{"ok":true,"token":"invented"}`}, nil
	}
	got := Run(context.Background(), o)
	if got.OK || got.Kind != "tool" {
		t.Fatalf("result=%+v", got)
	}
}

func TestRunClassifiesTimeoutAsTransient(t *testing.T) {
	o := canaryOpts(t)
	o.Spawn = func(context.Context, runtime.Runtime, runtime.JSONSpec, runtime.JSONExec) (execx.Result, error) {
		return execx.Result{TimedOut: true}, execx.ErrTimeout
	}
	got := Run(context.Background(), o)
	if got.OK || got.Kind != "transient" {
		t.Fatalf("result=%+v", got)
	}
}
