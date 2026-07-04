// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	githubforge "github.com/koryph/koryph/internal/forge/github"

	"github.com/koryph/koryph/internal/forge"
)

// ---------- fake gh helpers --------------------------------------------------

// fakeGhBin writes a shell script at dir/gh (executable) and returns the
// directory so the caller can prepend it to PATH.
func fakeGhBin(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	path := filepath.Join(bin, "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KORYPH_GH_BIN", path)
	return bin
}

// ---------- List -------------------------------------------------------------

// listGhScript answers `gh pr list --repo owner/repo ...` with a JSON array
// matching the fixture data used in TestPRServiceList.
const listGhScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
  printf '[{"number":42,"title":"Add feature","url":"https://github.com/acme/proj/pull/42","state":"OPEN","isDraft":false,"headRefName":"feat/add","headRefOid":"abc123","author":{"login":"alice"},"labels":[{"name":"area:cli"}]}]\n'
  exit 0
fi
exit 1
`

// TestPRServiceList fixture-locks the List method: a fake gh that emits a
// one-PR JSON array produces the correctly-mapped forge.PR slice.
func TestPRServiceList(t *testing.T) {
	fakeGhBin(t, listGhScript)
	ctx := context.Background()
	svc := githubforge.New().PRs()

	prs, err := svc.List(ctx, "acme", "proj", forge.ListPROptions{State: "open"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("List: got %d PRs, want 1", len(prs))
	}
	pr := prs[0]
	if pr.Number != 42 {
		t.Errorf("Number = %d, want 42", pr.Number)
	}
	if pr.Title != "Add feature" {
		t.Errorf("Title = %q, want 'Add feature'", pr.Title)
	}
	if pr.State != "open" {
		t.Errorf("State = %q, want open", pr.State)
	}
	if pr.Author != "alice" {
		t.Errorf("Author = %q, want alice", pr.Author)
	}
	if pr.HeadBranch != "feat/add" {
		t.Errorf("HeadBranch = %q, want feat/add", pr.HeadBranch)
	}
	if pr.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q, want abc123", pr.HeadSHA)
	}
	if pr.Draft {
		t.Error("Draft = true, want false")
	}
	if len(pr.Labels) != 1 || pr.Labels[0] != "area:cli" {
		t.Errorf("Labels = %v, want [area:cli]", pr.Labels)
	}
}

// ---------- Get --------------------------------------------------------------

const getGhScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ] && [ "$3" = "7" ]; then
  printf '{"number":7,"title":"Fix bug","url":"https://github.com/acme/proj/pull/7","state":"OPEN","isDraft":false,"headRefName":"fix/bug","headRefOid":"def456","author":{"login":"bob"},"labels":[]}\n'
  exit 0
fi
exit 1
`

func TestPRServiceGet(t *testing.T) {
	fakeGhBin(t, getGhScript)
	ctx := context.Background()
	svc := githubforge.New().PRs()

	pr, err := svc.Get(ctx, "acme", "proj", 7)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if pr.Number != 7 {
		t.Errorf("Number = %d, want 7", pr.Number)
	}
	if pr.Author != "bob" {
		t.Errorf("Author = %q, want bob", pr.Author)
	}
	if pr.HeadSHA != "def456" {
		t.Errorf("HeadSHA = %q, want def456", pr.HeadSHA)
	}
}

// ---------- Create -----------------------------------------------------------

const createGhScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  printf 'https://github.com/acme/proj/pull/99\n'
  exit 0
fi
exit 1
`

func TestPRServiceCreate(t *testing.T) {
	fakeGhBin(t, createGhScript)
	ctx := context.Background()
	svc := githubforge.New().PRs()

	pr, err := svc.Create(ctx, "acme", "proj", "feat/new", "main", "My PR", "description")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pr.Number != 99 {
		t.Errorf("Number = %d, want 99 (parsed from URL)", pr.Number)
	}
	if !strings.Contains(pr.URL, "pull/99") {
		t.Errorf("URL = %q, want it to contain pull/99", pr.URL)
	}
}

// ---------- Close / Reopen ---------------------------------------------------

// closeReopenGhScript records which subcommand was called and exits 0.
const closeReopenGhScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "close" ]; then exit 0; fi
if [ "$1" = "pr" ] && [ "$2" = "reopen" ]; then exit 0; fi
exit 1
`

func TestPRServiceClose(t *testing.T) {
	fakeGhBin(t, closeReopenGhScript)
	svc := githubforge.New().PRs()
	if err := svc.Close(context.Background(), "acme", "proj", 5); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestPRServiceReopen(t *testing.T) {
	fakeGhBin(t, closeReopenGhScript)
	svc := githubforge.New().PRs()
	if err := svc.Reopen(context.Background(), "acme", "proj", 5); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
}

// ---------- ListChecks -------------------------------------------------------

const checksGhScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "checks" ]; then
  printf '[{"name":"lint","status":"completed","conclusion":"success"},{"name":"test","status":"in_progress","conclusion":""}]\n'
  exit 0
fi
exit 1
`

func TestPRServiceListChecks(t *testing.T) {
	fakeGhBin(t, checksGhScript)
	ctx := context.Background()
	svc := githubforge.New().PRs()

	checks, err := svc.ListChecks(ctx, "acme", "proj", 42)
	if err != nil {
		t.Fatalf("ListChecks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("ListChecks: got %d checks, want 2", len(checks))
	}
	if checks[0].Name != "lint" || checks[0].Status != "completed" || checks[0].Conclusion != "success" {
		t.Errorf("checks[0] = %+v, want {lint completed success}", checks[0])
	}
	if checks[1].Name != "test" || checks[1].Status != "in_progress" || checks[1].Conclusion != "" {
		t.Errorf("checks[1] = %+v, want {test in_progress ''}", checks[1])
	}
}

// ---------- Merge ------------------------------------------------------------

// mergeGhScript records its arguments so tests can assert the merge strategy
// flag was forwarded correctly.
func mergeGhScriptForMethod(method, wantFlag string) string {
	return fmt.Sprintf(`#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "merge" ]; then
  # Verify the right strategy flag is present.
  found=0
  for arg in "$@"; do
    if [ "$arg" = "%s" ]; then found=1; fi
  done
  if [ "$found" = "1" ]; then exit 0; fi
  echo "strategy flag %s not found in: $*" >&2
  exit 1
fi
exit 1
`, wantFlag, wantFlag)
}

func TestPRServiceMerge_Squash(t *testing.T) {
	fakeGhBin(t, mergeGhScriptForMethod("squash", "--squash"))
	svc := githubforge.New().PRs()
	if err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{Method: "squash"}); err != nil {
		t.Fatalf("Merge squash: %v", err)
	}
}

func TestPRServiceMerge_Rebase(t *testing.T) {
	fakeGhBin(t, mergeGhScriptForMethod("rebase", "--rebase"))
	svc := githubforge.New().PRs()
	if err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{Method: "rebase"}); err != nil {
		t.Fatalf("Merge rebase: %v", err)
	}
}

func TestPRServiceMerge_Merge(t *testing.T) {
	fakeGhBin(t, mergeGhScriptForMethod("merge", "--merge"))
	svc := githubforge.New().PRs()
	if err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{Method: "merge"}); err != nil {
		t.Fatalf("Merge merge: %v", err)
	}
}

// TestPRServiceMerge_DefaultIssMerge verifies an empty method defaults to
// --merge (the explicit seam for koryph-ufy).
func TestPRServiceMerge_DefaultIsMerge(t *testing.T) {
	fakeGhBin(t, mergeGhScriptForMethod("", "--merge"))
	svc := githubforge.New().PRs()
	if err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{}); err != nil {
		t.Fatalf("Merge default: %v", err)
	}
}

// TestPRServiceMerge_UnknownMethodError verifies a bad method name is caught
// before calling gh (no subprocess spawned).
func TestPRServiceMerge_UnknownMethodError(t *testing.T) {
	svc := githubforge.New().PRs()
	err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{Method: "force"})
	if err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Fatalf("Merge bad method: want error containing 'unknown method', got %v", err)
	}
}

// ---------- Approve ----------------------------------------------------------

const approveGhScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "review" ] && [ "$4" = "--approve" ]; then exit 0; fi
exit 1
`

func TestPRServiceApprove(t *testing.T) {
	fakeGhBin(t, approveGhScript)
	svc := githubforge.New().PRs()
	if err := svc.Approve(context.Background(), "acme", "proj", 7, "LGTM"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
}

// ---------- AddLabels / RemoveLabels -----------------------------------------

// labelEditGhScript checks that `gh pr edit` is called with --add-label or
// --remove-label, then exits 0.
const addLabelGhScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "edit" ]; then
  for arg in "$@"; do
    if [ "$arg" = "--add-label" ]; then exit 0; fi
  done
fi
exit 1
`

const removeLabelGhScript = `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "edit" ]; then
  for arg in "$@"; do
    if [ "$arg" = "--remove-label" ]; then exit 0; fi
  done
fi
exit 1
`

func TestPRServiceAddLabels(t *testing.T) {
	fakeGhBin(t, addLabelGhScript)
	svc := githubforge.New().PRs()
	if err := svc.AddLabels(context.Background(), "acme", "proj", 1, []string{"bug", "priority:high"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
}

func TestPRServiceRemoveLabels(t *testing.T) {
	fakeGhBin(t, removeLabelGhScript)
	svc := githubforge.New().PRs()
	if err := svc.RemoveLabels(context.Background(), "acme", "proj", 1, []string{"wip"}); err != nil {
		t.Fatalf("RemoveLabels: %v", err)
	}
}

// TestPRServiceAddLabelsEmpty verifies no gh call is made when labels is empty.
func TestPRServiceAddLabelsEmpty(t *testing.T) {
	// No fake gh — if gh is called, the real binary will fail the test.
	svc := githubforge.New().PRs()
	if err := svc.AddLabels(context.Background(), "acme", "proj", 1, nil); err != nil {
		t.Fatalf("AddLabels(nil): %v", err)
	}
}

func TestPRServiceRemoveLabelsEmpty(t *testing.T) {
	svc := githubforge.New().PRs()
	if err := svc.RemoveLabels(context.Background(), "acme", "proj", 1, nil); err != nil {
		t.Fatalf("RemoveLabels(nil): %v", err)
	}
}

// ---------- parsePRNumberFromURL (internal, tested via Create) ---------------

// TestPRServiceCreate_NoNumber verifies that a gh response that doesn't look
// like a PR URL still yields a non-nil PR (with empty number) rather than an
// error, so the caller is not blocked.
func TestPRServiceCreate_NoNumber(t *testing.T) {
	fakeGhBin(t, `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  printf 'not-a-url\n'; exit 0
fi
exit 1
`)
	ctx := context.Background()
	svc := githubforge.New().PRs()
	pr, err := svc.Create(ctx, "acme", "proj", "feat", "main", "T", "B")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pr == nil {
		t.Fatal("Create returned nil PR")
	}
	// Number should be 0 when URL doesn't contain /pull/.
	if pr.Number != 0 {
		t.Errorf("Number = %d, want 0 for unparseable URL", pr.Number)
	}
}

// ---------- failure paths ----------------------------------------------------

// TestPRServiceList_GhError verifies that a non-zero exit from gh surfaces an
// informative error.
func TestPRServiceList_GhError(t *testing.T) {
	fakeGhBin(t, `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
  echo "not authenticated" >&2; exit 1
fi
exit 1
`)
	svc := githubforge.New().PRs()
	_, err := svc.List(context.Background(), "acme", "proj", forge.ListPROptions{})
	if err == nil {
		t.Fatal("List with gh failure: want error, got nil")
	}
}
