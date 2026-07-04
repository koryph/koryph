// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/forge"
	_ "github.com/koryph/koryph/internal/forge/gitlab" // register provider
)

// ---------- test helpers ------------------------------------------------------

// glTestServer starts an httptest.Server and configures the GitLab PRService
// to use it via KORYPH_GITLAB_BASE_URL. Returns the server and a cleanup func.
func glTestServer(t *testing.T, mux *http.ServeMux) forge.PRService {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KORYPH_GITLAB_BASE_URL", srv.URL+"/api/v4")
	t.Setenv("KORYPH_GITLAB_TOKEN", "glpat-testtoken")
	gl, ok := forge.Default.Get("gitlab")
	if !ok {
		t.Fatal("gitlab provider not registered")
	}
	return gl.PRs()
}

// jsonMust encodes v to JSON and panics on error (test helper).
func jsonMust(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------- List -------------------------------------------------------------

func TestGitLabPRServiceList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("PRIVATE-TOKEN") != "glpat-testtoken" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{
			{
				"iid":           42,
				"title":         "Add feature",
				"web_url":       "https://gitlab.com/acme/proj/-/merge_requests/42",
				"state":         "opened",
				"labels":        []string{"area:cli"},
				"source_branch": "feat/add",
				"sha":           "abc123",
				"draft":         false,
				"author":        map[string]string{"username": "alice"},
			},
		}))
	})

	svc := glTestServer(t, mux)
	prs, err := svc.List(context.Background(), "acme", "proj", forge.ListPROptions{State: "open"})
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
		t.Errorf("State = %q, want open (mapped from 'opened')", pr.State)
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

// TestGitLabPRServiceList_StateMerged verifies "closed" state passes through
// unchanged and the API receives state=closed.
func TestGitLabPRServiceList_StateMerged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "closed" {
			http.Error(w, "expected state=closed", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{{
			"iid": 1, "title": "Old MR", "web_url": "https://gitlab.com/x", "state": "closed",
			"labels": []string{}, "source_branch": "old", "sha": "old", "draft": false,
			"author": map[string]string{"username": "bob"},
		}}))
	})
	svc := glTestServer(t, mux)
	prs, err := svc.List(context.Background(), "acme", "proj", forge.ListPROptions{State: "closed"})
	if err != nil {
		t.Fatalf("List closed: %v", err)
	}
	if len(prs) != 1 || prs[0].State != "closed" {
		t.Errorf("expected 1 closed MR, got %+v", prs)
	}
}

// ---------- Get --------------------------------------------------------------

func TestGitLabPRServiceGet(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"iid":           7,
			"title":         "Fix bug",
			"web_url":       "https://gitlab.com/acme/proj/-/merge_requests/7",
			"state":         "opened",
			"labels":        []string{},
			"source_branch": "fix/bug",
			"sha":           "def456",
			"draft":         false,
			"author":        map[string]string{"username": "bob"},
		}))
	})

	svc := glTestServer(t, mux)
	pr, err := svc.Get(context.Background(), "acme", "proj", 7)
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
	if pr.State != "open" {
		t.Errorf("State = %q, want open", pr.State)
	}
}

// ---------- Create -----------------------------------------------------------

func TestGitLabPRServiceCreate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body["source_branch"] != "feat/new" {
			http.Error(w, "bad source_branch", http.StatusBadRequest)
			return
		}
		if body["target_branch"] != "main" {
			http.Error(w, "bad target_branch", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(jsonMust(map[string]any{
			"iid":           99,
			"title":         body["title"],
			"web_url":       "https://gitlab.com/acme/proj/-/merge_requests/99",
			"state":         "opened",
			"labels":        []string{},
			"source_branch": "feat/new",
			"sha":           "newsha",
			"draft":         false,
			"author":        map[string]string{"username": "carol"},
		}))
	})

	svc := glTestServer(t, mux)
	pr, err := svc.Create(context.Background(), "acme", "proj", "feat/new", "main", "My MR", "description")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pr.Number != 99 {
		t.Errorf("Number = %d, want 99", pr.Number)
	}
	if !strings.Contains(pr.URL, "99") {
		t.Errorf("URL = %q, should contain '99'", pr.URL)
	}
	if pr.HeadBranch != "feat/new" {
		t.Errorf("HeadBranch = %q, want feat/new", pr.HeadBranch)
	}
}

// ---------- Close / Reopen ---------------------------------------------------

func TestGitLabPRServiceClose(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/5", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["state_event"] != "close" {
			http.Error(w, "expected state_event=close", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"iid": 5, "title": "T", "web_url": "u", "state": "closed",
			"labels": []string{}, "source_branch": "b", "sha": "s", "draft": false,
			"author": map[string]string{"username": "u"},
		}))
	})

	svc := glTestServer(t, mux)
	if err := svc.Close(context.Background(), "acme", "proj", 5); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestGitLabPRServiceReopen(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/5", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["state_event"] != "reopen" {
			http.Error(w, "expected state_event=reopen", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"iid": 5, "title": "T", "web_url": "u", "state": "opened",
			"labels": []string{}, "source_branch": "b", "sha": "s", "draft": false,
			"author": map[string]string{"username": "u"},
		}))
	})

	svc := glTestServer(t, mux)
	if err := svc.Reopen(context.Background(), "acme", "proj", 5); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
}

// ---------- ListChecks -------------------------------------------------------

func TestGitLabPRServiceListChecks(t *testing.T) {
	mux := http.NewServeMux()
	// Pipeline list endpoint.
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/42/pipelines", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{
			{"id": 101, "status": "success"},
		}))
	})
	// Jobs for pipeline 101.
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/pipelines/101/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{
			{"name": "lint", "status": "success"},
			{"name": "test", "status": "running"},
		}))
	})

	svc := glTestServer(t, mux)
	checks, err := svc.ListChecks(context.Background(), "acme", "proj", 42)
	if err != nil {
		t.Fatalf("ListChecks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("ListChecks: got %d checks, want 2", len(checks))
	}
	// lint: success → completed/success
	if checks[0].Name != "lint" || checks[0].Status != "completed" || checks[0].Conclusion != "success" {
		t.Errorf("checks[0] = %+v, want {lint completed success}", checks[0])
	}
	// test: running → in_progress/""
	if checks[1].Name != "test" || checks[1].Status != "in_progress" || checks[1].Conclusion != "" {
		t.Errorf("checks[1] = %+v, want {test in_progress ''}", checks[1])
	}
}

// TestGitLabPRServiceListChecks_NoPipeline verifies an empty pipeline list
// returns an empty (non-nil) CheckRun slice without error.
func TestGitLabPRServiceListChecks_NoPipeline(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/1/pipelines", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	})

	svc := glTestServer(t, mux)
	checks, err := svc.ListChecks(context.Background(), "acme", "proj", 1)
	if err != nil {
		t.Fatalf("ListChecks (no pipeline): %v", err)
	}
	if len(checks) != 0 {
		t.Errorf("expected 0 checks, got %d", len(checks))
	}
}

// TestGitLabPRServiceListChecks_StatusMapping exercises all GitLab job status
// → forge.CheckRun mappings.
func TestGitLabPRServiceListChecks_StatusMapping(t *testing.T) {
	cases := []struct {
		glStatus   string
		wantStatus string
		wantConc   string
	}{
		{"created", "queued", ""},
		{"waiting_for_resource", "queued", ""},
		{"preparing", "queued", ""},
		{"pending", "queued", ""},
		{"manual", "queued", ""},
		{"scheduled", "queued", ""},
		{"running", "in_progress", ""},
		{"success", "completed", "success"},
		{"failed", "completed", "failure"},
		{"canceled", "completed", "cancelled"},
		{"skipped", "completed", "skipped"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run("status="+tc.glStatus, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/1/pipelines", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(jsonMust([]map[string]any{{"id": 200, "status": tc.glStatus}}))
			})
			mux.HandleFunc("/api/v4/projects/acme%2Fproj/pipelines/200/jobs", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(jsonMust([]map[string]any{{"name": "job", "status": tc.glStatus}}))
			})

			svc := glTestServer(t, mux)
			checks, err := svc.ListChecks(context.Background(), "acme", "proj", 1)
			if err != nil {
				t.Fatalf("ListChecks: %v", err)
			}
			if len(checks) != 1 {
				t.Fatalf("expected 1 check, got %d", len(checks))
			}
			if checks[0].Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", checks[0].Status, tc.wantStatus)
			}
			if checks[0].Conclusion != tc.wantConc {
				t.Errorf("Conclusion = %q, want %q", checks[0].Conclusion, tc.wantConc)
			}
		})
	}
}

// ---------- Merge ------------------------------------------------------------

func mergeTestSetup(t *testing.T, assertBody func(body map[string]any) bool) forge.PRService {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/1/merge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if assertBody != nil && !assertBody(body) {
			http.Error(w, "assertion failed", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})
	return glTestServer(t, mux)
}

func TestGitLabPRServiceMerge_Default(t *testing.T) {
	svc := mergeTestSetup(t, func(b map[string]any) bool {
		squash, ok := b["squash"].(bool)
		return ok && !squash
	})
	if err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{}); err != nil {
		t.Fatalf("Merge default: %v", err)
	}
}

func TestGitLabPRServiceMerge_Merge(t *testing.T) {
	svc := mergeTestSetup(t, func(b map[string]any) bool {
		squash, ok := b["squash"].(bool)
		return ok && !squash
	})
	if err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{Method: "merge"}); err != nil {
		t.Fatalf("Merge merge: %v", err)
	}
}

func TestGitLabPRServiceMerge_Squash(t *testing.T) {
	svc := mergeTestSetup(t, func(b map[string]any) bool {
		squash, ok := b["squash"].(bool)
		return ok && squash
	})
	if err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{Method: "squash"}); err != nil {
		t.Fatalf("Merge squash: %v", err)
	}
}

func TestGitLabPRServiceMerge_MergeWhenPipelineSucceeds(t *testing.T) {
	svc := mergeTestSetup(t, func(b map[string]any) bool {
		mwps, ok := b["merge_when_pipeline_succeeds"].(bool)
		return ok && mwps
	})
	if err := svc.Merge(context.Background(), "acme", "proj", 1,
		forge.MergeOptions{Method: "merge_when_pipeline_succeeds"}); err != nil {
		t.Fatalf("Merge merge_when_pipeline_succeeds: %v", err)
	}
}

func TestGitLabPRServiceMerge_RebaseUnsupported(t *testing.T) {
	// No server needed — rebase error is returned before any HTTP call.
	mux := http.NewServeMux()
	svc := glTestServer(t, mux)
	err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{Method: "rebase"})
	if err == nil {
		t.Fatal("Merge rebase: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("Merge rebase: error = %q, want 'not supported'", err.Error())
	}
}

func TestGitLabPRServiceMerge_UnknownMethod(t *testing.T) {
	mux := http.NewServeMux()
	svc := glTestServer(t, mux)
	err := svc.Merge(context.Background(), "acme", "proj", 1, forge.MergeOptions{Method: "force"})
	if err == nil {
		t.Fatal("Merge unknown method: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("Merge unknown: error = %q, want 'unknown method'", err.Error())
	}
}

func TestGitLabPRServiceMerge_CommitMessage(t *testing.T) {
	svc := mergeTestSetup(t, func(b map[string]any) bool {
		msg, ok := b["merge_commit_message"].(string)
		return ok && msg == "custom commit msg"
	})
	if err := svc.Merge(context.Background(), "acme", "proj", 1,
		forge.MergeOptions{Method: "merge", CommitMessage: "custom commit msg"}); err != nil {
		t.Fatalf("Merge with commit message: %v", err)
	}
}

// ---------- Approve ----------------------------------------------------------

func TestGitLabPRServiceApprove(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/7/approve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	})

	svc := glTestServer(t, mux)
	if err := svc.Approve(context.Background(), "acme", "proj", 7, "LGTM"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
}

// ---------- AddLabels / RemoveLabels -----------------------------------------

func TestGitLabPRServiceAddLabels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(body["add_labels"], "bug") {
			http.Error(w, "missing bug label", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"iid": 1, "title": "T", "web_url": "u", "state": "opened",
			"labels": []string{"bug"}, "source_branch": "b", "sha": "s", "draft": false,
			"author": map[string]string{"username": "u"},
		}))
	})

	svc := glTestServer(t, mux)
	if err := svc.AddLabels(context.Background(), "acme", "proj", 1, []string{"bug", "priority:high"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
}

func TestGitLabPRServiceRemoveLabels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(body["remove_labels"], "wip") {
			http.Error(w, "missing wip in remove_labels", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"iid": 1, "title": "T", "web_url": "u", "state": "opened",
			"labels": []string{}, "source_branch": "b", "sha": "s", "draft": false,
			"author": map[string]string{"username": "u"},
		}))
	})

	svc := glTestServer(t, mux)
	if err := svc.RemoveLabels(context.Background(), "acme", "proj", 1, []string{"wip"}); err != nil {
		t.Fatalf("RemoveLabels: %v", err)
	}
}

// TestGitLabPRServiceAddLabels_Empty verifies no HTTP call is made for empty label slice.
func TestGitLabPRServiceAddLabels_Empty(t *testing.T) {
	// No server — if an HTTP call is made, it will fail (no handler).
	mux := http.NewServeMux()
	svc := glTestServer(t, mux)
	if err := svc.AddLabels(context.Background(), "acme", "proj", 1, nil); err != nil {
		t.Fatalf("AddLabels(nil): %v", err)
	}
}

func TestGitLabPRServiceRemoveLabels_Empty(t *testing.T) {
	mux := http.NewServeMux()
	svc := glTestServer(t, mux)
	if err := svc.RemoveLabels(context.Background(), "acme", "proj", 1, nil); err != nil {
		t.Fatalf("RemoveLabels(nil): %v", err)
	}
}

// ---------- failure paths ----------------------------------------------------

func TestGitLabPRServiceList_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	})
	svc := glTestServer(t, mux)
	_, err := svc.List(context.Background(), "acme", "proj", forge.ListPROptions{})
	if err == nil {
		t.Fatal("List with API error: want error, got nil")
	}
}

func TestGitLabPRServiceGet_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/merge_requests/999", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	svc := glTestServer(t, mux)
	_, err := svc.Get(context.Background(), "acme", "proj", 999)
	if err == nil {
		t.Fatal("Get 404: want error, got nil")
	}
}

// ---------- namespace handling -----------------------------------------------

// TestGitLabPRServiceNamespaced verifies that a project with slashes in the
// path ("group/subgroup/project") is correctly URL-path-encoded.
func TestGitLabPRServiceNamespaced(t *testing.T) {
	mux := http.NewServeMux()
	// URL path should be group%2Fsubgroup%2Fproject for "group/subgroup" + "project"
	// but our API encodes owner+"/"+repo, so "group" + "subgroup/project" → "group%2Fsubgroup%2Fproject"
	mux.HandleFunc("/api/v4/projects/group%2Fproj/merge_requests/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"iid": 1, "title": "T", "web_url": "u", "state": "opened",
			"labels": []string{}, "source_branch": "b", "sha": "s", "draft": false,
			"author": map[string]string{"username": "u"},
		}))
	})
	svc := glTestServer(t, mux)
	pr, err := svc.Get(context.Background(), "group", "proj", 1)
	if err != nil {
		t.Fatalf("Get (namespaced): %v", err)
	}
	if pr.Number != 1 {
		t.Errorf("Number = %d, want 1", pr.Number)
	}
}
