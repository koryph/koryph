// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/forge"
	_ "github.com/koryph/koryph/internal/forge/gitlab" // register provider
)

// glProtTestServer starts an httptest.Server and configures the GitLab
// ProtectionService to use it via KORYPH_GITLAB_BASE_URL.
func glProtTestServer(t *testing.T, mux *http.ServeMux) forge.ProtectionService {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KORYPH_GITLAB_BASE_URL", srv.URL+"/api/v4")
	t.Setenv("KORYPH_GITLAB_TOKEN", "glpat-testtoken")
	gl, ok := forge.Default.Get("gitlab")
	if !ok {
		t.Fatal("gitlab provider not registered")
	}
	return gl.Protection()
}

// ---------- List --------------------------------------------------------------

func TestGitLabProtectionServiceList(t *testing.T) {
	mux := http.NewServeMux()

	// Protected branches.
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/protected_branches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{
			{"name": "main", "allow_force_push": false},
			{"name": "develop", "allow_force_push": false},
		}))
	})

	// Push rule singleton.
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/push_rule", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"prevent_secrets":         true,
			"reject_unsigned_commits": false,
		}))
	})

	// Approval rules.
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/approval_rules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{
			{"id": 10, "name": "code-review", "approvals_required": 1},
		}))
	})

	svc := glProtTestServer(t, mux)
	rulesets, err := svc.List(context.Background(), "acme/proj")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Expect: 2 protected branches + 1 push rule + 1 approval rule = 4.
	if len(rulesets) != 4 {
		t.Fatalf("List: got %d rulesets, want 4 (2 pb + 1 push-rules + 1 ar)", len(rulesets))
	}

	// Check IDs.
	ids := make(map[string]bool)
	for _, rs := range rulesets {
		ids[rs.ID] = true
	}
	for _, wantID := range []string{"pb:main", "pb:develop", "push-rules", "ar:10"} {
		if !ids[wantID] {
			t.Errorf("List: missing ID %q in result set", wantID)
		}
	}
}

func TestGitLabProtectionServiceListNoPushRule(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/protected_branches", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{}))
	})
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/push_rule", func(w http.ResponseWriter, r *http.Request) {
		// 404 means no push rule configured.
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/approval_rules", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust([]map[string]any{}))
	})

	svc := glProtTestServer(t, mux)
	rulesets, err := svc.List(context.Background(), "acme/proj")
	if err != nil {
		t.Fatalf("List (no push rule): %v", err)
	}
	if len(rulesets) != 0 {
		t.Errorf("List (no push rule): got %d rulesets, want 0", len(rulesets))
	}
}

func TestGitLabProtectionServiceListGroupUnsupported(t *testing.T) {
	gl, _ := forge.Default.Get("gitlab")
	svc := gl.Protection()
	_, err := svc.List(context.Background(), "mygroup")
	if !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("List group: want ErrUnsupported, got %v", err)
	}
}

// ---------- Get ---------------------------------------------------------------

func TestGitLabProtectionServiceGetProtectedBranch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/protected_branches/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"name":             "main",
			"allow_force_push": false,
		}))
	})

	svc := glProtTestServer(t, mux)
	rs, err := svc.Get(context.Background(), "acme/proj", "pb:main")
	if err != nil {
		t.Fatalf("Get pb:main: %v", err)
	}
	if rs.ID != "pb:main" {
		t.Errorf("Get pb:main: ID = %q, want pb:main", rs.ID)
	}
	if rs.Raw == nil {
		t.Error("Get pb:main: Raw should be populated")
	}
}

func TestGitLabProtectionServiceGetPushRule(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/push_rule", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"prevent_secrets": true,
		}))
	})

	svc := glProtTestServer(t, mux)
	rs, err := svc.Get(context.Background(), "acme/proj", "push-rules")
	if err != nil {
		t.Fatalf("Get push-rules: %v", err)
	}
	if rs.ID != "push-rules" {
		t.Errorf("Get push-rules: ID = %q, want push-rules", rs.ID)
	}
}

func TestGitLabProtectionServiceGetApprovalRule(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/approval_rules/10", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(map[string]any{
			"id":                 10,
			"name":               "code-review",
			"approvals_required": 2,
		}))
	})

	svc := glProtTestServer(t, mux)
	rs, err := svc.Get(context.Background(), "acme/proj", "ar:10")
	if err != nil {
		t.Fatalf("Get ar:10: %v", err)
	}
	if rs.ID != "ar:10" {
		t.Errorf("Get ar:10: ID = %q, want ar:10", rs.ID)
	}
	if rs.Name != "code-review" {
		t.Errorf("Get ar:10: Name = %q, want code-review", rs.Name)
	}
}

func TestGitLabProtectionServiceGetUnknownID(t *testing.T) {
	mux := http.NewServeMux()
	svc := glProtTestServer(t, mux)
	_, err := svc.Get(context.Background(), "acme/proj", "unknown-format")
	if err == nil {
		t.Error("Get unknown ID: expected error, got nil")
	}
}

// ---------- Create -----------------------------------------------------------

func TestGitLabProtectionServiceCreateProtectedBranch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/protected_branches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		// Echo back with the branch name from the request.
		name, _ := body["name"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(jsonMust(map[string]any{
			"name":             name,
			"allow_force_push": false,
		}))
	})

	svc := glProtTestServer(t, mux)
	rs, err := svc.Create(context.Background(), "acme/proj", &forge.Ruleset{
		Name: "pb:release/*",
		Raw:  jsonMust(map[string]any{"name": "release/*", "push_access_level": 0}),
	})
	if err != nil {
		t.Fatalf("Create pb: %v", err)
	}
	if !strings.HasPrefix(rs.ID, "pb:") {
		t.Errorf("Create pb: ID = %q, want pb: prefix", rs.ID)
	}
}

func TestGitLabProtectionServiceCreatePushRule(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/push_rule", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(jsonMust(body))
	})

	svc := glProtTestServer(t, mux)
	rs, err := svc.Create(context.Background(), "acme/proj", &forge.Ruleset{
		Name: "push-rules",
		Raw:  jsonMust(map[string]any{"prevent_secrets": true}),
	})
	if err != nil {
		t.Fatalf("Create push-rules: %v", err)
	}
	if rs.ID != "push-rules" {
		t.Errorf("Create push-rules: ID = %q, want push-rules", rs.ID)
	}
}

func TestGitLabProtectionServiceCreateApprovalRule(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/approval_rules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["id"] = 42
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(jsonMust(body))
	})

	svc := glProtTestServer(t, mux)
	rs, err := svc.Create(context.Background(), "acme/proj", &forge.Ruleset{
		Name: "sec-review",
		Raw:  jsonMust(map[string]any{"name": "sec-review", "approvals_required": 2}),
	})
	if err != nil {
		t.Fatalf("Create approval-rule: %v", err)
	}
	if rs.ID != "ar:42" {
		t.Errorf("Create approval-rule: ID = %q, want ar:42", rs.ID)
	}
	if rs.Name != "sec-review" {
		t.Errorf("Create approval-rule: Name = %q, want sec-review", rs.Name)
	}
}

// ---------- Update -----------------------------------------------------------

func TestGitLabProtectionServiceUpdatePushRule(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/push_rule", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(gotBody))
	})

	svc := glProtTestServer(t, mux)
	err := svc.Update(context.Background(), "acme/proj", &forge.Ruleset{
		ID:  "push-rules",
		Raw: jsonMust(map[string]any{"prevent_secrets": true, "reject_unsigned_commits": true}),
	})
	if err != nil {
		t.Fatalf("Update push-rules: %v", err)
	}
	if gotBody["reject_unsigned_commits"] != true {
		t.Errorf("Update push-rules: reject_unsigned_commits not set in body")
	}
}

func TestGitLabProtectionServiceUpdateApprovalRule(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/approval_rules/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jsonMust(gotBody))
	})

	svc := glProtTestServer(t, mux)
	err := svc.Update(context.Background(), "acme/proj", &forge.Ruleset{
		ID:   "ar:7",
		Name: "sec-review",
		Raw:  jsonMust(map[string]any{"name": "sec-review", "approvals_required": 3}),
	})
	if err != nil {
		t.Fatalf("Update ar:7: %v", err)
	}
}

// ---------- Delete -----------------------------------------------------------

func TestGitLabProtectionServiceDeleteProtectedBranch(t *testing.T) {
	deleted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/protected_branches/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "want DELETE", http.StatusMethodNotAllowed)
			return
		}
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	})

	svc := glProtTestServer(t, mux)
	if err := svc.Delete(context.Background(), "acme/proj", "pb:main"); err != nil {
		t.Fatalf("Delete pb:main: %v", err)
	}
	if !deleted {
		t.Error("Delete pb:main: DELETE request not received")
	}
}

func TestGitLabProtectionServiceDeletePushRule(t *testing.T) {
	deleted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/push_rule", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "want DELETE", http.StatusMethodNotAllowed)
			return
		}
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	})

	svc := glProtTestServer(t, mux)
	if err := svc.Delete(context.Background(), "acme/proj", "push-rules"); err != nil {
		t.Fatalf("Delete push-rules: %v", err)
	}
	if !deleted {
		t.Error("Delete push-rules: DELETE request not received")
	}
}

func TestGitLabProtectionServiceDeleteApprovalRule(t *testing.T) {
	deleted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/approval_rules/5", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "want DELETE", http.StatusMethodNotAllowed)
			return
		}
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	})

	svc := glProtTestServer(t, mux)
	if err := svc.Delete(context.Background(), "acme/proj", "ar:5"); err != nil {
		t.Fatalf("Delete ar:5: %v", err)
	}
	if !deleted {
		t.Error("Delete ar:5: DELETE request not received")
	}
}

// ---------- Update protected branch (delete + re-create) ---------------------

func TestGitLabProtectionServiceUpdateProtectedBranch(t *testing.T) {
	deleteCalled := false
	createCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/protected_branches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			createCalled = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			name, _ := body["name"].(string)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(jsonMust(map[string]any{"name": name}))
			return
		}
		http.Error(w, "unexpected method on collection", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/v4/projects/acme%2Fproj/protected_branches/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "unexpected method on item", http.StatusMethodNotAllowed)
	})

	svc := glProtTestServer(t, mux)
	err := svc.Update(context.Background(), "acme/proj", &forge.Ruleset{
		ID:   "pb:main",
		Name: "pb:main",
		Raw:  jsonMust(map[string]any{"name": "main", "push_access_level": 40}),
	})
	if err != nil {
		t.Fatalf("Update pb:main: %v", err)
	}
	if !deleteCalled {
		t.Error("Update pb:main: DELETE not called")
	}
	if !createCalled {
		t.Error("Update pb:main: POST not called")
	}
}
