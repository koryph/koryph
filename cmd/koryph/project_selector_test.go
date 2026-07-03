// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/registry"
)

// registerProject is a helper that registers a bare project (no GitHub remote)
// in the isolated store and returns its id.
func registerProject(t *testing.T, id string) string {
	t.Helper()
	root := gitRepo(t)
	ctx := context.Background()
	store := registry.NewStore()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	rec := &registry.Record{
		ProjectID:        id,
		Name:             id,
		Root:             root,
		AccountProfile:   "personal",
		ExpectedIdentity: "me@example.com",
	}
	if err := store.Add(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return id
}

// --- resolveProjectID unit tests ---

func TestResolveProjectIDFlagWins(t *testing.T) {
	var errb bytes.Buffer
	id, code := resolveProjectID(&errb, "test", "", "from-flag")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, errb.String())
	}
	if id != "from-flag" {
		t.Errorf("id = %q, want from-flag", id)
	}
}

func TestResolveProjectIDPositionalWins(t *testing.T) {
	var errb bytes.Buffer
	id, code := resolveProjectID(&errb, "test", "from-pos", "")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, errb.String())
	}
	if id != "from-pos" {
		t.Errorf("id = %q, want from-pos", id)
	}
}

func TestResolveProjectIDSameValueBothAccepted(t *testing.T) {
	var errb bytes.Buffer
	id, code := resolveProjectID(&errb, "test", "myproj", "myproj")
	if code != 0 {
		t.Fatalf("code = %d, want 0 when both agree; stderr=%s", code, errb.String())
	}
	if id != "myproj" {
		t.Errorf("id = %q, want myproj", id)
	}
}

func TestResolveProjectIDConflictIsUsageError(t *testing.T) {
	var errb bytes.Buffer
	_, code := resolveProjectID(&errb, "cmd", "a", "b")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit on conflict", code)
	}
	if !strings.Contains(errb.String(), "conflict") {
		t.Errorf("stderr = %q, want 'conflict' message", errb.String())
	}
}

func TestResolveProjectIDNeitherIsUsageError(t *testing.T) {
	var errb bytes.Buffer
	_, code := resolveProjectID(&errb, "cmd", "", "")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit when neither provided", code)
	}
}

// --- project show: --project flag form ---

func TestProjectShowAcceptsProjectFlag(t *testing.T) {
	isolate(t)
	registerProject(t, "myproject")

	code, out, errb := runCmd("project", "show", "--project", "myproject")
	if code != 0 {
		t.Fatalf("project show --project code = %d; stderr=%s", code, errb)
	}
	var rec registry.Record
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	if rec.ProjectID != "myproject" {
		t.Errorf("ProjectID = %q, want myproject", rec.ProjectID)
	}
}

func TestProjectShowPositionalAndFlagAgree(t *testing.T) {
	isolate(t)
	registerProject(t, "myproject")

	code, out, errb := runCmd("project", "show", "myproject", "--project", "myproject")
	if code != 0 {
		t.Fatalf("code = %d; stderr=%s", code, errb)
	}
	var rec registry.Record
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	if rec.ProjectID != "myproject" {
		t.Errorf("ProjectID = %q, want myproject", rec.ProjectID)
	}
}

func TestProjectShowConflictIsUsageError(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("project", "show", "proj-a", "--project", "proj-b")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit on id conflict", code)
	}
	if !strings.Contains(errb, "conflict") {
		t.Errorf("stderr = %q, want conflict message", errb)
	}
}

func TestProjectShowMissingIDIsUsageError(t *testing.T) {
	isolate(t)
	code, _, _ := runCmd("project", "show")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit when no id given", code)
	}
}

// --- project set-account: --project flag form ---

func TestProjectSetAccountAcceptsProjectFlag(t *testing.T) {
	isolate(t)
	registerProject(t, "acct-proj")

	code, _, errb := runCmd("project", "set-account", "--project", "acct-proj",
		"--profile", "work", "--identity", "work@example.com", "--reason", "switched jobs")
	if code != 0 {
		t.Fatalf("project set-account --project code = %d; stderr=%s", code, errb)
	}
}

func TestProjectSetAccountConflictIsUsageError(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("project", "set-account", "pos-id", "--project", "flag-id",
		"--profile", "work", "--identity", "work@example.com", "--reason", "test")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit on id conflict", code)
	}
	if !strings.Contains(errb, "conflict") {
		t.Errorf("stderr = %q, want conflict message", errb)
	}
}

func TestProjectSetAccountMissingIDIsUsageError(t *testing.T) {
	isolate(t)
	code, _, _ := runCmd("project", "set-account",
		"--profile", "work", "--identity", "work@example.com", "--reason", "test")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit when no id given", code)
	}
}

// --- validate: --project flag form ---

func TestValidateAcceptsProjectFlag(t *testing.T) {
	isolate(t)
	// An unknown id produces a not-found fatal (not a usage error).
	code, _, errb := runCmd("validate", "--project", "nope")
	if code != engine.ExitFatal {
		t.Errorf("code = %d, want fatal for unknown project (got stderr: %s)", code, errb)
	}
}

func TestValidateConflictIsUsageError(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("validate", "pos-id", "--project", "flag-id")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit on id conflict", code)
	}
	if !strings.Contains(errb, "conflict") {
		t.Errorf("stderr = %q, want conflict message", errb)
	}
}

func TestValidateMissingIDIsUsageError(t *testing.T) {
	isolate(t)
	code, _, _ := runCmd("validate")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit when no id given", code)
	}
}

// --- registry not-found error includes remediation hint ---

func TestNotFoundErrorSuggestsProjectList(t *testing.T) {
	isolate(t)
	// project show on an unknown id hits registry.Get which must now carry
	// the 'run koryph project list' hint in the error message.
	_, _, errb := runCmd("project", "show", "--project", "no-such-project")
	if !strings.Contains(errb, "project list") {
		t.Errorf("stderr = %q, want 'project list' hint in not-found error", errb)
	}
}

func TestValidateNotFoundSuggestsProjectList(t *testing.T) {
	isolate(t)
	_, _, errb := runCmd("validate", "no-such-project")
	if !strings.Contains(errb, "project list") {
		t.Errorf("stderr = %q, want 'project list' hint in not-found error", errb)
	}
}
