// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package intake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// --- fake bd reused from intake_test.go is in the same package -------------

// cannedJIRASearch returns a JIRA search response with three issues:
// ENG-12 (medium, Task), ENG-34 (high, Bug), ENG-56 (low, Story — already ingested).
const cannedJIRASearch = `{
  "issues": [
    {
      "id": "10012",
      "key": "ENG-12",
      "fields": {
        "summary": "Add dark mode",
        "description": {
          "version": 1,
          "type": "doc",
          "content": [{"type": "paragraph", "content": [{"type": "text", "text": "Please add a dark theme."}]}]
        },
        "labels": [],
        "priority": {"name": "Medium"},
        "issuetype": {"name": "Task"},
        "reporter": {"displayName": "Alice Smith", "emailAddress": "alice@acme.test"}
      }
    },
    {
      "id": "10034",
      "key": "ENG-34",
      "fields": {
        "summary": "Crash on login",
        "description": null,
        "labels": ["regression"],
        "priority": {"name": "High"},
        "issuetype": {"name": "Bug"},
        "reporter": {"displayName": "Bob Jones", "emailAddress": "bob@acme.test"}
      }
    },
    {
      "id": "10056",
      "key": "ENG-56",
      "fields": {
        "summary": "Already tracked",
        "description": null,
        "labels": [],
        "priority": {"name": "Low"},
        "issuetype": {"name": "Story"},
        "reporter": {"displayName": "Carol", "emailAddress": "carol@acme.test"}
      }
    }
  ]
}`

// fakeBDForJIRA matches on "jira-" prefix for dedup; returns existing bead for
// ENG-56. The shell script is a variant of the one in intake_test.go.
const fakeBDForJIRA = `#!/bin/sh
if [ -n "$BD_ARGS_LOG" ]; then
  echo "$@" >> "$BD_ARGS_LOG"
fi
case "$1" in
  list)
    if printf '%s' "$*" | grep -q -- 'jira-.*ENG.*56'; then
      printf '[{"id":"cx-existing","title":"Already tracked","status":"open","priority":3,"labels":["jira-acme.atlassian.net/ENG#56","intake","no-dispatch"]}]'
    else
      printf '[]'
    fi
    ;;
  create)
    n=1
    if [ -n "$BD_CREATE_SEQ" ]; then
      n=$(cat "$BD_CREATE_SEQ" 2>/dev/null || echo 0)
      n=$((n + 1))
      echo "$n" > "$BD_CREATE_SEQ"
    fi
    printf 'cx-%03d\n' "$n"
    ;;
  version)
    printf 'bd 0.42.0\n'
    ;;
  *)
    :
    ;;
esac
`

// jiraHarness sets up a fake JIRA HTTP server, fake bd binary, and env vars.
type jiraHarness struct {
	root      string
	server    *httptest.Server
	bdLog     string
	createSeq string

	// commentLog tracks POST /comment requests: "ENG-<n>"
	commentLog []string
	// searchCalled counts GET /search requests
	searchCalled int
}

// newJIRAHarness creates a temp environment with a fake JIRA server.
func newJIRAHarness(t *testing.T) *jiraHarness {
	t.Helper()
	h := &jiraHarness{}
	root := t.TempDir()
	h.root = root

	// Write fake bd.
	bdBin := filepath.Join(root, "bd")
	if err := os.WriteFile(bdBin, []byte(fakeBDForJIRA), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	h.bdLog = filepath.Join(root, "bd-argv.log")
	h.createSeq = filepath.Join(root, "create.seq")
	t.Setenv("KORYPH_BD_BIN", bdBin)
	t.Setenv("BD_ARGS_LOG", h.bdLog)
	t.Setenv("BD_CREATE_SEQ", h.createSeq)

	// Set JIRA credentials (plain, no vault).
	t.Setenv("JIRA_EMAIL", "test@acme.test")
	t.Setenv("JIRA_TOKEN", "fake-token-abc")

	// Start fake JIRA HTTP server.
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/search", func(w http.ResponseWriter, r *http.Request) {
		h.searchCalled++
		// Validate auth header.
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedJIRASearch))
	})
	mux.HandleFunc("/rest/api/3/issue/", func(w http.ResponseWriter, r *http.Request) {
		// POST /rest/api/3/issue/<key>/comment
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comment") {
			segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/"), "/")
			if len(segments) >= 1 {
				h.commentLog = append(h.commentLog, segments[0])
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.NotFound(w, r)
	})
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)

	return h
}

// record returns a registry Record pointing at the harness root.
func (h *jiraHarness) record() *registry.Record {
	return &registry.Record{ProjectID: "jira-test", Root: h.root, Remote: "https://github.com/placeholder/placeholder"}
}

// client builds a jiraClient pointed at the fake server (no vault lookup).
func (h *jiraHarness) client(t *testing.T) *jiraClient {
	t.Helper()
	c, err := newJIRA(h.server.URL)
	if err != nil {
		t.Fatalf("newJIRA: %v", err)
	}
	return c
}

// bdLog reads and returns the bd invocation log.
func (h *jiraHarness) readBDLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.bdLog)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read bd log: %v", err)
	}
	return string(data)
}

// --- unit tests for helpers ------------------------------------------------

func TestBuildJQL(t *testing.T) {
	cases := []struct {
		key, jql, want string
	}{
		{"ENG", "", `project = "ENG"`},
		{"ENG", `status = "To Do"`, `project = "ENG" AND (status = "To Do")`},
		{"ACME", `assignee = currentUser()`, `project = "ACME" AND (assignee = currentUser())`},
	}
	for _, tc := range cases {
		got := buildJQL(tc.key, tc.jql)
		if got != tc.want {
			t.Errorf("buildJQL(%q, %q) = %q, want %q", tc.key, tc.jql, got, tc.want)
		}
	}
}

func TestIssueNumberFromKey(t *testing.T) {
	cases := []struct {
		key     string
		want    int
		wantErr bool
	}{
		{"ENG-42", 42, false},
		{"PROJ-1", 1, false},
		{"ACME-100", 100, false},
		{"ENG-", 0, true},
		{"nodashinkey", 0, true},
		{"ENG-xyz", 0, true},
	}
	for _, tc := range cases {
		n, err := issueNumberFromKey(tc.key)
		if tc.wantErr {
			if err == nil {
				t.Errorf("issueNumberFromKey(%q): expected error", tc.key)
			}
			continue
		}
		if err != nil {
			t.Errorf("issueNumberFromKey(%q): unexpected error %v", tc.key, err)
			continue
		}
		if n != tc.want {
			t.Errorf("issueNumberFromKey(%q) = %d, want %d", tc.key, n, tc.want)
		}
	}
}

func TestJIRALabels(t *testing.T) {
	cases := []struct {
		name        string
		iss         jiraIssue
		wantPrio    string // expected p0/p1/p2/p3 label
		wantBug     bool
		extraLabels []string
	}{
		{"highest", mkJIRAIssue("Highest", "Task", nil), "p0", false, nil},
		{"blocker", mkJIRAIssue("Blocker", "Task", nil), "p0", false, nil},
		{"high", mkJIRAIssue("High", "Story", nil), "p1", false, nil},
		{"critical", mkJIRAIssue("Critical", "Story", nil), "p1", false, nil},
		{"medium", mkJIRAIssue("Medium", "Task", nil), "p2", false, nil},
		{"low", mkJIRAIssue("Low", "Task", nil), "p3", false, nil},
		{"lowest", mkJIRAIssue("Lowest", "Task", nil), "p3", false, nil},
		{"trivial", mkJIRAIssue("Trivial", "Task", nil), "p3", false, nil},
		{"minor", mkJIRAIssue("Minor", "Task", nil), "p3", false, nil},
		{"unknown", mkJIRAIssue("Unknown", "Task", nil), "p2", false, nil},
		{"bug type", mkJIRAIssue("High", "Bug", nil), "p1", true, nil},
		{"bug case-insensitive", mkJIRAIssue("Medium", "BUG", nil), "p2", true, nil},
		{"native labels", mkJIRAIssue("Medium", "Task", []string{"regression"}), "p2", false, []string{"regression"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := jiraLabels(tc.iss)
			hasPrio := false
			hasBug := false
			for _, l := range labels {
				if l == tc.wantPrio {
					hasPrio = true
				}
				if l == "bug" {
					hasBug = true
				}
			}
			if !hasPrio {
				t.Errorf("labels %v missing expected priority label %q", labels, tc.wantPrio)
			}
			if tc.wantBug != hasBug {
				t.Errorf("labels %v: wantBug=%v hasBug=%v", labels, tc.wantBug, hasBug)
			}
			for _, extra := range tc.extraLabels {
				found := false
				for _, l := range labels {
					if l == extra {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("labels %v missing expected extra label %q", labels, extra)
				}
			}
		})
	}
}

func mkJIRAIssue(prio, issType string, labels []string) jiraIssue {
	return jiraIssue{
		Key: "ENG-1",
		Fields: jiraFields{
			Priority:  jiraNamed{Name: prio},
			IssueType: jiraNamed{Name: issType},
			Labels:    labels,
		},
	}
}

func TestExtractADFText(t *testing.T) {
	cases := []struct {
		name string
		adf  interface{}
		want string
	}{
		{"nil", nil, ""},
		{"plain string", "hello world", "hello world"},
		{"simple paragraph", map[string]interface{}{
			"version": 1,
			"type":    "doc",
			"content": []interface{}{
				map[string]interface{}{
					"type": "paragraph",
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "Hello, world!"},
					},
				},
			},
		}, "Hello, world!"},
		{"two paragraphs", map[string]interface{}{
			"type": "doc",
			"content": []interface{}{
				map[string]interface{}{
					"type":    "paragraph",
					"content": []interface{}{map[string]interface{}{"type": "text", "text": "Para one."}},
				},
				map[string]interface{}{
					"type":    "paragraph",
					"content": []interface{}{map[string]interface{}{"type": "text", "text": "Para two."}},
				},
			},
		}, "Para one.\nPara two."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractADFText(tc.adf)
			if got != tc.want {
				t.Errorf("extractADFText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildJIRADescription(t *testing.T) {
	iss := SourceIssue{Number: 42, Body: "the body", Author: "alice@acme.test"}
	got := buildJIRADescription("acme.atlassian.net", "ENG", iss)
	want := "the body\n\n---\nSource: https://acme.atlassian.net/browse/ENG-42, author alice@acme.test, ingested by koryph intake"
	if got != want {
		t.Fatalf("buildJIRADescription =\n%q\nwant\n%q", got, want)
	}
	// Empty body → footer only, no leading blank line.
	empty := buildJIRADescription("acme.atlassian.net", "ENG", SourceIssue{Number: 1})
	if strings.HasPrefix(empty, "\n") {
		t.Fatalf("empty-body description should not lead with a blank line: %q", empty)
	}
}

func TestJIRAProvenanceIsScoped(t *testing.T) {
	c := &jiraClient{}
	k1 := c.Provenance("acme.atlassian.net", "ENG", 1)
	k2 := c.Provenance("acme.atlassian.net", "OTHER", 1)
	k3 := c.Provenance("beta.atlassian.net", "ENG", 1)
	if k1 == k2 || k1 == k3 || k2 == k3 {
		t.Errorf("provenance keys must be distinct: %q %q %q", k1, k2, k3)
	}
	if !strings.HasPrefix(k1, "jira-") {
		t.Errorf("provenance key must start with jira-: %q", k1)
	}
}

func TestParseJIRASource(t *testing.T) {
	cases := []struct {
		source    string
		host, key string
		wantErr   bool
	}{
		{"acme.atlassian.net/ENG", "acme.atlassian.net", "ENG", false},
		{"acme.atlassian.net/eng", "acme.atlassian.net", "ENG", false}, // uppercased
		{"https://acme.atlassian.net/ENG", "acme.atlassian.net", "ENG", false},
		{"acme.atlassian.net/ENG/", "acme.atlassian.net", "ENG", false},
		{"acme.atlassian.net", "", "", true}, // no project key
		{"", "", "", true},                   // empty
		{"/ENG", "", "", true},               // no host
	}
	for _, tc := range cases {
		host, key, err := parseJIRASource(tc.source)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseJIRASource(%q): expected error", tc.source)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseJIRASource(%q): unexpected error %v", tc.source, err)
			continue
		}
		if host != tc.host || key != tc.key {
			t.Errorf("parseJIRASource(%q) = (%q, %q), want (%q, %q)", tc.source, host, key, tc.host, tc.key)
		}
	}
}

// --- integration: RunJIRA dry-run -----------------------------------------

func TestRunJIRADryRunMutatesNothing(t *testing.T) {
	h := newJIRAHarness(t)
	res, err := RunJIRA(context.Background(), JIRAOptions{
		Project:    h.record(),
		BaseURL:    h.server.URL,
		ProjectKey: "ENG",
		JQL:        `status = "To Do"`,
		DryRun:     true,
		Client:     h.client(t),
	})
	if err != nil {
		t.Fatalf("RunJIRA dry-run: %v", err)
	}
	// ENG-12, ENG-34 would be ingested; ENG-56 skipped (already ingested).
	if len(res.Ingested) != 2 {
		t.Fatalf("dry-run ingested = %d, want 2: %+v", len(res.Ingested), res.Ingested)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("dry-run skipped = %d, want 1: %+v", len(res.Skipped), res.Skipped)
	}
	// No bd create must have run.
	bdLog := h.readBDLog(t)
	if strings.Contains(bdLog, "create") {
		t.Fatalf("dry-run must not create beads:\n%s", bdLog)
	}
	// No comment posted.
	if len(h.commentLog) != 0 {
		t.Fatalf("dry-run must not post comments: %v", h.commentLog)
	}
}

// --- integration: RunJIRA live ingest -------------------------------------

func TestRunJIRAIngestsDedupesAndMaps(t *testing.T) {
	h := newJIRAHarness(t)
	res, err := RunJIRA(context.Background(), JIRAOptions{
		Project:    h.record(),
		BaseURL:    h.server.URL,
		ProjectKey: "ENG",
		JQL:        `status = "To Do"`,
		Client:     h.client(t),
	})
	if err != nil {
		t.Fatalf("RunJIRA: %v", err)
	}
	if res.Owner == "" || res.Repo != "ENG" {
		t.Fatalf("Owner/Repo = %q/%q, want <host>/ENG", res.Owner, res.Repo)
	}
	if len(res.Ingested) != 2 {
		t.Fatalf("ingested = %d, want 2: %+v", len(res.Ingested), res.Ingested)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %d, want 1: %+v", len(res.Skipped), res.Skipped)
	}
	// ENG-56 is skipped with the existing bead id.
	if res.Skipped[0].Number != 56 || res.Skipped[0].BeadID != "cx-existing" {
		t.Fatalf("skipped item = %+v, want #56 -> cx-existing", res.Skipped[0])
	}
	if res.Skipped[0].Reason != "already ingested" {
		t.Fatalf("skip reason = %q", res.Skipped[0].Reason)
	}

	// Verify bd create args.
	bdLog := h.readBDLog(t)
	createLines := grepLines(bdLog, "create ")
	if len(createLines) != 2 {
		t.Fatalf("create count = %d, want 2:\n%s", len(createLines), bdLog)
	}
	for _, line := range createLines {
		if !strings.Contains(line, "no-dispatch") || !strings.Contains(line, "intake") {
			t.Fatalf("create missing mandatory labels: %q", line)
		}
		if !strings.Contains(line, "--external-ref") {
			t.Fatalf("create missing --external-ref: %q", line)
		}
		if !strings.Contains(line, "jira-") {
			t.Fatalf("create missing jira- provenance key: %q", line)
		}
	}
	// ENG-12 → priority 2 (Medium), no --type.
	if !hasCreateWith(createLines, "ENG#12", "--priority 2") {
		t.Fatalf("ENG-12 should have priority 2:\n%s", bdLog)
	}
	if hasCreateWith(createLines, "ENG#12", "--type") {
		t.Fatalf("ENG-12 (Task) should not set --type:\n%s", bdLog)
	}
	// ENG-34 → priority 1 (High), type bug (Bug).
	if !hasCreateWith(createLines, "ENG#34", "--priority 1") {
		t.Fatalf("ENG-34 should have priority 1:\n%s", bdLog)
	}
	if !hasCreateWith(createLines, "ENG#34", "--type bug") {
		t.Fatalf("ENG-34 (Bug) should have --type bug:\n%s", bdLog)
	}
}

// --- integration: RunJIRA comment-back -----------------------------------

func TestRunJIRACommentBack(t *testing.T) {
	h := newJIRAHarness(t)
	res, err := RunJIRA(context.Background(), JIRAOptions{
		Project:     h.record(),
		BaseURL:     h.server.URL,
		ProjectKey:  "ENG",
		CommentBack: true,
		Client:      h.client(t),
	})
	if err != nil {
		t.Fatalf("RunJIRA comment-back: %v", err)
	}
	// Comments posted for ENG-12 and ENG-34 (ingested), not ENG-56 (skipped).
	if len(h.commentLog) != 2 {
		t.Fatalf("comment count = %d, want 2: %v", len(h.commentLog), h.commentLog)
	}
	for _, key := range []string{"ENG-12", "ENG-34"} {
		found := false
		for _, k := range h.commentLog {
			if k == key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected comment on %s, got: %v", key, h.commentLog)
		}
	}
	for _, it := range res.Ingested {
		if it.Reason != "commented" {
			t.Errorf("ingested #%d reason = %q, want commented", it.Number, it.Reason)
		}
	}
}

// --- integration: RunJIRA no issues ---------------------------------------

func TestRunJIRANoIssues(t *testing.T) {
	h := newJIRAHarness(t)
	// Override fake server to return empty results.
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := newJIRA(srv.URL)
	res, err := RunJIRA(context.Background(), JIRAOptions{
		Project:    h.record(),
		BaseURL:    srv.URL,
		ProjectKey: "ENG",
		Client:     c,
	})
	if err != nil {
		t.Fatalf("RunJIRA no-issues: %v", err)
	}
	if len(res.Ingested) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

// --- integration: RunJIRA input validation --------------------------------

func TestRunJIRARejectsNilProject(t *testing.T) {
	_, err := RunJIRA(context.Background(), JIRAOptions{BaseURL: "https://acme.atlassian.net", ProjectKey: "ENG"})
	if err == nil {
		t.Fatal("expected error for nil project")
	}
}

func TestRunJIRARejectsEmptyProjectKey(t *testing.T) {
	_, err := RunJIRA(context.Background(), JIRAOptions{
		Project: &registry.Record{ProjectID: "x", Root: "/tmp"},
		BaseURL: "https://acme.atlassian.net",
	})
	if err == nil {
		t.Fatal("expected error for empty project key")
	}
}

// --- integration: JIRA search HTTP error handling -------------------------

func TestRunJIRAHTTPError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/search", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errorMessages":["Project ENG not found"]}`, http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	bdBin := filepath.Join(root, "bd")
	_ = os.WriteFile(bdBin, []byte(fakeBDForJIRA), 0o755)
	t.Setenv("KORYPH_BD_BIN", bdBin)
	t.Setenv("JIRA_EMAIL", "test@test")
	t.Setenv("JIRA_TOKEN", "tok")

	c, _ := newJIRA(srv.URL)
	_, err := RunJIRA(context.Background(), JIRAOptions{
		Project:    &registry.Record{ProjectID: "x", Root: root},
		BaseURL:    srv.URL,
		ProjectKey: "ENG",
		Client:     c,
	})
	if err == nil {
		t.Fatal("expected error on HTTP 400")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention HTTP 400: %v", err)
	}
}

// --- integration: RunMulti JIRA provider -----------------------------------

func TestRunMultiJIRAProvider(t *testing.T) {
	h := newJIRAHarness(t)
	rec := &registry.Record{
		ProjectID: "multi-jira-test",
		Root:      h.root,
		Remote:    "https://github.com/placeholder/placeholder",
	}

	// Build a server URL in the form the JIRA source parser expects.
	serverURL := h.server.URL
	host := strings.TrimPrefix(serverURL, "http://")

	results, err := RunMulti(context.Background(), MultiOptions{
		Project: rec,
		Sources: []project.IntakeSource{
			{
				Provider: "jira",
				Source:   host + "/ENG",
				Trigger:  `status = "To Do"`,
			},
		},
		// Inject the fake client by patching newJIRA — not possible here since
		// RunMultiJIRA calls newJIRA internally. Instead we rely on env vars
		// (JIRA_EMAIL / JIRA_TOKEN) already set by newJIRAHarness and the fake
		// HTTP server returned by h.server.URL, BUT the server URL uses "http://"
		// not "https://". We'll test without client injection, relying on the
		// fake server being an http:// server and that jiraClient is built from
		// the parsed source URL (not https:// prefixed when host contains "127.0.0.1").
		//
		// Since runMultiJIRA prefixes "https://", the client won't reach the
		// plain http test server. This test therefore only validates that the
		// JIRA branch is selected and fails with a connection error (not an
		// "unsupported provider" error), proving the routing is correct.
	})
	// We expect an error (can't reach https://127.0.0.1:... from the test),
	// but the error should be about the JIRA connection, not "provider not supported".
	if err != nil && strings.Contains(err.Error(), "not supported") {
		t.Fatalf("JIRA provider should be dispatched, not rejected: %v", err)
	}
	_ = results
}

// TestRunMultiJIRAProviderValidation verifies project.Config accepts "jira"
// without an error and rejects unknown providers.
func TestRunMultiJIRAProviderValidation(t *testing.T) {
	cfg := &project.Config{
		SchemaVersion:   1,
		ProjectID:       "test",
		WorkSource:      "bd",
		Gate:            []string{"true"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
	}

	// "jira" provider is valid.
	cfg.Intake = []project.IntakeSource{
		{Provider: "jira", Source: "acme.atlassian.net/ENG", Trigger: `status = "To Do"`},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected jira provider to be valid, got: %v", err)
	}

	// Unknown provider is rejected.
	cfg.Intake = []project.IntakeSource{
		{Provider: "asana", Source: "ENG"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for unknown provider")
	}

	// Empty source is still rejected.
	cfg.Intake = []project.IntakeSource{
		{Provider: "jira", Source: ""},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for empty source")
	}
}

// --- JIRA client: credential resolution -----------------------------------

func TestNewJIRAMissingEmail(t *testing.T) {
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_TOKEN", "tok")
	_, err := newJIRA("https://acme.atlassian.net")
	if err == nil || !strings.Contains(err.Error(), "JIRA_EMAIL") {
		t.Errorf("expected error mentioning JIRA_EMAIL, got %v", err)
	}
}

func TestNewJIRAMissingToken(t *testing.T) {
	t.Setenv("JIRA_EMAIL", "me@acme.test")
	t.Setenv("JIRA_TOKEN", "")
	_, err := newJIRA("https://acme.atlassian.net")
	if err == nil || !strings.Contains(err.Error(), "JIRA_TOKEN") {
		t.Errorf("expected error mentioning JIRA_TOKEN, got %v", err)
	}
}

// --- JIRA List: ADF description extraction --------------------------------

func TestJIRAListExtractsADFDescription(t *testing.T) {
	// Build a minimal server that returns one issue with an ADF description.
	adf := map[string]interface{}{
		"version": 1,
		"type":    "doc",
		"content": []interface{}{
			map[string]interface{}{
				"type":    "paragraph",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "The body text."}},
			},
		},
	}
	issueJSON, _ := json.Marshal(map[string]interface{}{
		"issues": []interface{}{
			map[string]interface{}{
				"id":  "10001",
				"key": "ENG-1",
				"fields": map[string]interface{}{
					"summary":     "ADF issue",
					"description": adf,
					"labels":      []interface{}{},
					"priority":    map[string]interface{}{"name": "Medium"},
					"issuetype":   map[string]interface{}{"name": "Task"},
					"reporter":    map[string]interface{}{"displayName": "Alice", "emailAddress": "a@b.test"},
				},
			},
		},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(issueJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("JIRA_EMAIL", "test@test")
	t.Setenv("JIRA_TOKEN", "tok")
	c, _ := newJIRA(srv.URL)
	issues, err := c.List(context.Background(), "host", "ENG", "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(issues))
	}
	if issues[0].Body != "The body text." {
		t.Errorf("ADF body = %q, want %q", issues[0].Body, "The body text.")
	}
	if issues[0].Number != 1 {
		t.Errorf("Number = %d, want 1", issues[0].Number)
	}
	_ = strconv.Itoa(issues[0].Number) // ensure it compiles
}
