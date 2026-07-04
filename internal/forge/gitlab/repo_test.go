// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/koryph/koryph/internal/forge"
	_ "github.com/koryph/koryph/internal/forge/gitlab" // register provider
)

// glRepoTestServer starts an httptest.Server and configures the GitLab
// RepoService to use it via KORYPH_GITLAB_BASE_URL.
func glRepoTestServer(t *testing.T, mux *http.ServeMux) forge.RepoService {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KORYPH_GITLAB_BASE_URL", srv.URL+"/api/v4")
	t.Setenv("KORYPH_GITLAB_TOKEN", "glpat-testtoken")
	gl, ok := forge.Default.Get("gitlab")
	if !ok {
		t.Fatal("gitlab provider not registered")
	}
	return gl.Repo()
}

// ---------- Get ---------------------------------------------------------------

func TestGitLabRepoServiceGet(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj", func(w http.ResponseWriter, r *http.Request) {
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
		_, _ = w.Write(jsonMust(map[string]any{
			"id":                               42,
			"name":                             "proj",
			"merge_method":                     "merge",
			"squash_option":                    "default_on",
			"remove_source_branch_after_merge": true,
		}))
	})

	svc := glRepoTestServer(t, mux)
	settings, err := svc.Get(context.Background(), "acme", "proj")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !settings.AllowMergeCommit {
		t.Error("AllowMergeCommit: want true (merge_method=merge)")
	}
	if !settings.AllowSquashMerge {
		t.Error("AllowSquashMerge: want true (squash_option=default_on)")
	}
	if settings.AllowRebaseMerge {
		t.Error("AllowRebaseMerge: want false (merge_method=merge)")
	}
	if settings.RawFull == nil {
		t.Error("RawFull: want non-nil")
	}
}

func TestGitLabRepoServiceGetRebaseMerge(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"merge_method":  "rebase_merge",
			"squash_option": "never",
		}))
	})

	svc := glRepoTestServer(t, mux)
	settings, err := svc.Get(context.Background(), "acme", "proj")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !settings.AllowRebaseMerge {
		t.Error("AllowRebaseMerge: want true (merge_method=rebase_merge)")
	}
	if settings.AllowMergeCommit {
		t.Error("AllowMergeCommit: want false (merge_method=rebase_merge)")
	}
	if settings.AllowSquashMerge {
		t.Error("AllowSquashMerge: want false (squash_option=never)")
	}
}

func TestGitLabRepoServiceGetFastForward(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"merge_method":  "ff",
			"squash_option": "always",
		}))
	})

	svc := glRepoTestServer(t, mux)
	settings, err := svc.Get(context.Background(), "acme", "proj")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !settings.AllowRebaseMerge {
		t.Error("AllowRebaseMerge: want true (merge_method=ff)")
	}
	if !settings.AllowSquashMerge {
		t.Error("AllowSquashMerge: want true (squash_option=always)")
	}
}

// ---------- GetRaw ------------------------------------------------------------

func TestGitLabRepoServiceGetRaw(t *testing.T) {
	projectJSON := jsonMust(map[string]any{
		"id":          99,
		"name":        "proj",
		"extra_field": "preserved",
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/ns%2Fproj", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(projectJSON)
	})

	svc := glRepoTestServer(t, mux)
	raw, err := svc.GetRaw(context.Background(), "ns", "proj")
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("GetRaw: unmarshal response: %v", err)
	}
	if m["extra_field"] != "preserved" {
		t.Errorf("GetRaw: extra_field lost; got %v", m["extra_field"])
	}
}

// ---------- Update / PatchRaw ------------------------------------------------

func TestGitLabRepoServiceUpdate(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(jsonMust(map[string]any{"merge_method": "merge", "squash_option": "never"}))
			return
		}
		if r.Method == http.MethodPut {
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(jsonMust(gotBody))
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})

	svc := glRepoTestServer(t, mux)
	err := svc.Update(context.Background(), "acme", "proj", &forge.RepoSettings{
		AllowSquashMerge: true,
		AllowMergeCommit: true,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	// AllowMergeCommit=true, AllowRebaseMerge=false → merge_method should be "merge"
	if gotBody["merge_method"] != "merge" {
		t.Errorf("Update: merge_method = %v, want merge", gotBody["merge_method"])
	}
	if gotBody["squash_option"] != "default_on" {
		t.Errorf("Update: squash_option = %v, want default_on", gotBody["squash_option"])
	}
}

func TestGitLabRepoServiceUpdateRebasePreferred(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(jsonMust(gotBody))
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})

	svc := glRepoTestServer(t, mux)
	// When both AllowRebaseMerge and AllowMergeCommit are true, rebase wins.
	err := svc.Update(context.Background(), "acme", "proj", &forge.RepoSettings{
		AllowRebaseMerge: true,
		AllowMergeCommit: true,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if gotBody["merge_method"] != "rebase_merge" {
		t.Errorf("Update: merge_method = %v, want rebase_merge", gotBody["merge_method"])
	}
}

// ---------- Unsupported operations -------------------------------------------

func TestGitLabRepoServiceUnsupported(t *testing.T) {
	gl, _ := forge.Default.Get("gitlab")
	svc := gl.Repo()

	if _, err := svc.VulnAlerts(context.Background(), "ns", "proj"); err != forge.ErrUnsupported {
		t.Errorf("VulnAlerts: want ErrUnsupported, got %v", err)
	}
	if err := svc.SetVulnAlerts(context.Background(), "ns", "proj", true); err != forge.ErrUnsupported {
		t.Errorf("SetVulnAlerts: want ErrUnsupported, got %v", err)
	}
	if _, err := svc.ActionsWorkflow(context.Background(), "ns", "proj"); err != forge.ErrUnsupported {
		t.Errorf("ActionsWorkflow: want ErrUnsupported, got %v", err)
	}
	if err := svc.SetActionsWorkflow(context.Background(), "ns", "proj", nil); err != forge.ErrUnsupported {
		t.Errorf("SetActionsWorkflow: want ErrUnsupported, got %v", err)
	}
}
