// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/koryph/koryph/internal/forge"
	_ "github.com/koryph/koryph/internal/forge/gitlab" // register provider
)

// glSecretsTestServer starts an httptest.Server and configures the GitLab
// SecretsService to use it via KORYPH_GITLAB_BASE_URL.
func glSecretsTestServer(t *testing.T, mux *http.ServeMux) forge.SecretsService {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KORYPH_GITLAB_BASE_URL", srv.URL+"/api/v4")
	t.Setenv("KORYPH_GITLAB_TOKEN", "glpat-testtoken")
	gl, ok := forge.Default.Get("gitlab")
	if !ok {
		t.Fatal("gitlab provider not registered")
	}
	return gl.Secrets()
}

// ---------- ListRepo ----------------------------------------------------------

func TestGitLabSecretsServiceListRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/variables", func(w http.ResponseWriter, r *http.Request) {
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
			{"key": "DATABASE_URL", "variable_type": "env_var", "masked": false},
			{"key": "SECRET_KEY", "variable_type": "env_var", "masked": true},
		}))
	})

	svc := glSecretsTestServer(t, mux)
	names, err := svc.ListRepo(context.Background(), "acme", "proj")
	if err != nil {
		t.Fatalf("ListRepo: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("ListRepo: got %d names, want 2", len(names))
	}
	sort.Strings(names)
	if names[0] != "DATABASE_URL" || names[1] != "SECRET_KEY" {
		t.Errorf("ListRepo: got %v, want [DATABASE_URL SECRET_KEY]", names)
	}
}

func TestGitLabSecretsServiceListRepoEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/variables", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{}))
	})

	svc := glSecretsTestServer(t, mux)
	names, err := svc.ListRepo(context.Background(), "acme", "proj")
	if err != nil {
		t.Fatalf("ListRepo empty: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("ListRepo empty: got %d names, want 0", len(names))
	}
}

// ---------- ListOrg -----------------------------------------------------------

func TestGitLabSecretsServiceListOrg(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/mygroup/variables", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{
			{"key": "GROUP_TOKEN", "variable_type": "env_var", "masked": true},
		}))
	})

	svc := glSecretsTestServer(t, mux)
	names, err := svc.ListOrg(context.Background(), "mygroup")
	if err != nil {
		t.Fatalf("ListOrg: %v", err)
	}
	if len(names) != 1 || names[0] != "GROUP_TOKEN" {
		t.Errorf("ListOrg: got %v, want [GROUP_TOKEN]", names)
	}
}

// ---------- SetRepo (create) -------------------------------------------------

func TestGitLabSecretsServiceSetRepoCreate(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/variables", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(jsonMust(gotBody))
	})

	svc := glSecretsTestServer(t, mux)
	if err := svc.SetRepo(context.Background(), "acme", "proj", "MY_VAR", "secret123"); err != nil {
		t.Fatalf("SetRepo (create): %v", err)
	}
	if gotBody["key"] != "MY_VAR" {
		t.Errorf("SetRepo (create): key = %q, want MY_VAR", gotBody["key"])
	}
	if gotBody["value"] != "secret123" {
		t.Errorf("SetRepo (create): value = %q, want secret123", gotBody["value"])
	}
	// Verify protected=true is sent (security requirement from review).
	if gotBody["protected"] != true {
		t.Errorf("SetRepo (create): protected = %v, want true", gotBody["protected"])
	}
}

// ---------- SetRepo (upsert — variable already exists) -----------------------

func TestGitLabSecretsServiceSetRepoUpdate(t *testing.T) {
	// Simulate: POST returns 400 with "already been taken" → PUT should be called.
	putCalled := false
	var putBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/variables", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":{"key":["has already been taken"]}}`))
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/variables/EXISTING_VAR", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		putCalled = true
		if err := json.NewDecoder(r.Body).Decode(&putBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(putBody))
	})

	svc := glSecretsTestServer(t, mux)
	if err := svc.SetRepo(context.Background(), "acme", "proj", "EXISTING_VAR", "newvalue"); err != nil {
		t.Fatalf("SetRepo (update): %v", err)
	}
	if !putCalled {
		t.Error("SetRepo (update): PUT not called on fallback")
	}
	if putBody["value"] != "newvalue" {
		t.Errorf("SetRepo (update): PUT body value = %q, want newvalue", putBody["value"])
	}
	// Verify protected=true is preserved in the update payload.
	if putBody["protected"] != true {
		t.Errorf("SetRepo (update): protected = %v, want true", putBody["protected"])
	}
}

// ---------- SetOrg -----------------------------------------------------------

func TestGitLabSecretsServiceSetOrg(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/mygroup/variables", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(jsonMust(gotBody))
	})

	svc := glSecretsTestServer(t, mux)
	// repos param is ignored for GitLab (group vars apply to all projects).
	if err := svc.SetOrg(context.Background(), "mygroup", "DEPLOY_KEY", "keyval", nil); err != nil {
		t.Fatalf("SetOrg: %v", err)
	}
	if gotBody["key"] != "DEPLOY_KEY" {
		t.Errorf("SetOrg: key = %q, want DEPLOY_KEY", gotBody["key"])
	}
}

// ---------- Not-yet-registered-provider guard --------------------------------

func TestGitLabSecretsNotNil(t *testing.T) {
	gl, ok := forge.Default.Get("gitlab")
	if !ok {
		t.Fatal("gitlab not registered")
	}
	if gl.Secrets() == nil {
		t.Error("Secrets() returned nil")
	}
}
