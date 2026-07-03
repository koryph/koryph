// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package registry

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// gitProject creates a fresh directory that is a valid git repository, usable
// as a Record.Root.
func gitProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func commitCount(t *testing.T, home string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(gitOut(t, home, "rev-list", "--count", "HEAD")))
	if err != nil {
		t.Fatalf("commit count: %v", err)
	}
	return n
}

// newInitStore returns an initialized Store rooted at a fresh temp home.
func newInitStore(t *testing.T) *Store {
	t.Helper()
	s := NewStoreAt(t.TempDir())
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	return s
}

func sampleRecord(id, root string) *Record {
	return &Record{
		ProjectID:        id,
		Name:             id,
		Root:             root,
		DefaultBranch:    "main",
		AccountProfile:   ProfilePersonal,
		ExpectedIdentity: "personal@example.com",
	}
}

func TestInitIdempotent(t *testing.T) {
	home := t.TempDir()
	s := NewStoreAt(home)
	ctx := context.Background()

	if err := s.Init(ctx); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("second init: %v", err)
	}

	for _, d := range []string{".git", "registry.d", "quota"} {
		if _, err := os.Stat(filepath.Join(home, d)); err != nil {
			t.Fatalf("expected %s to exist: %v", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "README.md")); err != nil {
		t.Fatalf("expected README.md: %v", err)
	}

	// Idempotent: the second Init must not add a commit.
	if got := commitCount(t, home); got != 1 {
		t.Fatalf("expected exactly 1 commit after idempotent init, got %d", got)
	}
}

func TestAddGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	rec := sampleRecord("my-proj", root)
	if err := s.Add(ctx, rec); err != nil {
		t.Fatalf("add: %v", err)
	}

	got, err := s.Get("my-proj")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProjectID != "my-proj" || got.Root != root {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("schema_version=%d, want 1", got.SchemaVersion)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("timestamps not set: created=%q updated=%q", got.CreatedAt, got.UpdatedAt)
	}
	if got.MigrationStatus != StatusRegistered {
		t.Fatalf("migration_status=%q, want %q", got.MigrationStatus, StatusRegistered)
	}

	if log := gitOut(t, s.Home, "log", "--oneline"); !strings.Contains(log, "register my-proj") {
		t.Fatalf("expected register commit in log:\n%s", log)
	}
}

func TestAddDuplicateRefused(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	rec := sampleRecord("dup", root)
	if err := s.Add(ctx, rec); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := s.Add(ctx, sampleRecord("dup", root)); err == nil {
		t.Fatal("expected duplicate add to be refused")
	}
}

func TestAddValidation(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	// Non-slug id.
	if err := s.Add(ctx, sampleRecord("Bad_ID", root)); err == nil {
		t.Fatal("expected invalid slug to be refused")
	}
	// Root that is not a git repo.
	notGit := sampleRecord("plain", t.TempDir())
	if err := s.Add(ctx, notGit); err == nil {
		t.Fatal("expected non-git root to be refused")
	}
	// Bad identity.
	badEmail := sampleRecord("bad-email", root)
	badEmail.ExpectedIdentity = "not-an-email"
	if err := s.Add(ctx, badEmail); err == nil {
		t.Fatal("expected non-email identity to be refused")
	}
}

func TestSaveRefusesAccountDrift(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	if err := s.Add(ctx, sampleRecord("drift", root)); err != nil {
		t.Fatalf("add: %v", err)
	}
	rec, err := s.Get("drift")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// A non-account edit is allowed.
	rec.Name = "renamed"
	if err := s.Save(ctx, rec); err != nil {
		t.Fatalf("save non-account edit: %v", err)
	}

	// Mutating an account field via Save must be refused.
	rec.AccountProfile = ProfileWork
	err = s.Save(ctx, rec)
	if err == nil {
		t.Fatal("expected account drift via Save to be refused")
	}
	if !strings.Contains(err.Error(), "SetAccount") {
		t.Fatalf("error should point at SetAccount, got: %v", err)
	}
}

func TestSetAccountHappyPath(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	if err := s.Add(ctx, sampleRecord("acct", root)); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Advance the migration status so we can prove SetAccount resets it.
	rec, err := s.Get("acct")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	rec.MigrationStatus = StatusValidated
	if err := s.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Empty reason is refused.
	if err := s.SetAccount(ctx, "acct", ProfileWork, "/cfg/work", "work@example.com", ""); err == nil {
		t.Fatal("expected empty reason to be refused")
	}

	if err := s.SetAccount(ctx, "acct", ProfileWork, "/cfg/work", "work@example.com", "moving to work account"); err != nil {
		t.Fatalf("set-account: %v", err)
	}

	got, err := s.Get("acct")
	if err != nil {
		t.Fatalf("get after set-account: %v", err)
	}
	if got.AccountProfile != ProfileWork {
		t.Fatalf("account_profile=%q, want %q", got.AccountProfile, ProfileWork)
	}
	if got.ClaudeConfigDir != "/cfg/work" {
		t.Fatalf("claude_config_dir=%q, want /cfg/work", got.ClaudeConfigDir)
	}
	if got.ExpectedIdentity != "work@example.com" {
		t.Fatalf("expected_identity=%q", got.ExpectedIdentity)
	}
	if got.MigrationStatus != StatusRegistered {
		t.Fatalf("migration_status=%q, want reset to %q", got.MigrationStatus, StatusRegistered)
	}

	// Audit line appended.
	audit, err := os.ReadFile(filepath.Join(s.Home, "audit.jsonl"))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(audit), `"kind":"set-account"`) {
		t.Fatalf("audit missing set-account line:\n%s", audit)
	}
	if !strings.Contains(string(audit), "moving to work account") {
		t.Fatalf("audit missing reason:\n%s", audit)
	}

	// Commit landed.
	if log := gitOut(t, s.Home, "log", "--oneline"); !strings.Contains(log, "set-account acct personal->work") {
		t.Fatalf("expected set-account commit in log:\n%s", log)
	}
}

func TestListSorted(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	for _, id := range []string{"zeta", "alpha", "mango"} {
		if err := s.Add(ctx, sampleRecord(id, root)); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}

	recs, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, len(recs))
	for i, r := range recs {
		got[i] = r.ProjectID
	}
	want := []string{"alpha", "mango", "zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("list order = %v, want %v", got, want)
	}
}
