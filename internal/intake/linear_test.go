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
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// --- canned GraphQL fixtures ------------------------------------------------

// cannedLinearIssues is a Linear GraphQL response with three issues:
// ENG-12 (priority 3/Medium, no special labels)
// ENG-34 (priority 2/High, label "bug")
// ENG-56 (priority 4/Low, already ingested)
const cannedLinearIssues = `{
  "data": {
    "issues": {
      "nodes": [
        {
          "id": "uuid-12",
          "identifier": "ENG-12",
          "title": "Add dark mode",
          "description": "Please add a dark theme.",
          "priority": 3,
          "labels": {"nodes": [{"name": "triage"}]},
          "state": {"name": "Todo"},
          "creator": {"name": "Alice Smith", "email": "alice@acme.test"}
        },
        {
          "id": "uuid-34",
          "identifier": "ENG-34",
          "title": "Crash on login",
          "description": "",
          "priority": 2,
          "labels": {"nodes": [{"name": "bug"}, {"name": "triage"}]},
          "state": {"name": "Todo"},
          "creator": {"name": "Bob Jones", "email": "bob@acme.test"}
        },
        {
          "id": "uuid-56",
          "identifier": "ENG-56",
          "title": "Already tracked",
          "description": "",
          "priority": 4,
          "labels": {"nodes": []},
          "state": {"name": "Todo"},
          "creator": {"name": "Carol", "email": "carol@acme.test"}
        }
      ]
    }
  }
}`

// fakeBDForLinear matches on "linear-" prefix for dedup; returns an existing
// bead for ENG-56.
const fakeBDForLinear = `#!/bin/sh
if [ -n "$BD_ARGS_LOG" ]; then
  echo "$@" >> "$BD_ARGS_LOG"
fi
case "$1" in
  list)
    if printf '%s' "$*" | grep -q -- 'linear-.*ENG.*56'; then
      printf '[{"id":"cx-existing","title":"Already tracked","status":"open","priority":3,"labels":["linear-ENG#56","intake","no-dispatch"]}]'
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

// --- test harness -----------------------------------------------------------

// linearHarness wires a fake Linear GraphQL server and a fake bd binary.
type linearHarness struct {
	root      string
	server    *httptest.Server
	bdLog     string
	createSeq string

	// commentLog tracks issue IDs passed to commentCreate.
	commentLog []string
	// issueQueryCount tracks how many issue-list queries arrived.
	issueQueryCount int
}

func newLinearHarness(t *testing.T) *linearHarness {
	t.Helper()
	h := &linearHarness{}
	root := t.TempDir()
	h.root = root

	// Write fake bd.
	bdBin := filepath.Join(root, "bd")
	if err := os.WriteFile(bdBin, []byte(fakeBDForLinear), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	h.bdLog = filepath.Join(root, "bd-argv.log")
	h.createSeq = filepath.Join(root, "create.seq")
	t.Setenv("KORYPH_BD_BIN", bdBin)
	t.Setenv("BD_ARGS_LOG", h.bdLog)
	t.Setenv("BD_CREATE_SEQ", h.createSeq)

	// LINEAR_API_KEY (plain, no vault).
	t.Setenv("LINEAR_API_KEY", "lin_api_fake_key_abc")

	// Start fake Linear GraphQL server.
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Validate auth header.
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Decode the request body to distinguish query vs. mutation.
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		lower := strings.ToLower(req.Query)
		switch {
		case strings.Contains(lower, "commentcreate"):
			// Extract issueId from variables.
			if issID, ok := req.Variables["issueId"].(string); ok {
				h.commentLog = append(h.commentLog, issID)
			}
			_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":true}}}`))

		case strings.Contains(lower, "issue(id"):
			// UUID resolution query for comment-back.
			// Return a fixed UUID based on the identifier variable.
			if id, ok := req.Variables["identifier"].(string); ok {
				resp := map[string]any{
					"data": map[string]any{
						"issue": map[string]any{"id": "uuid-resolved-" + id},
					},
				}
				data, _ := json.Marshal(resp)
				_, _ = w.Write(data)
			} else {
				_, _ = w.Write([]byte(`{"data":{"issue":{"id":"uuid-resolved"}}}`))
			}

		default:
			// Issues list query.
			h.issueQueryCount++
			_, _ = w.Write([]byte(cannedLinearIssues))
		}
	})
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)

	return h
}

// record returns a registry Record for the harness root.
func (h *linearHarness) record() *registry.Record {
	return &registry.Record{
		ProjectID: "linear-test",
		Root:      h.root,
		Remote:    "https://github.com/placeholder/placeholder",
	}
}

// client builds a linearClient pointed at the fake server.
func (h *linearHarness) client(t *testing.T) *linearClient {
	t.Helper()
	c, err := newLinear()
	if err != nil {
		t.Fatalf("newLinear: %v", err)
	}
	c.baseURL = h.server.URL + "/graphql"
	return c
}

// readBDLog reads and returns the bd invocation log.
func (h *linearHarness) readBDLog(t *testing.T) string {
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

// --- unit tests: helpers ---------------------------------------------------

func TestLinearIssueNumber(t *testing.T) {
	cases := []struct {
		id      string
		want    int
		wantErr bool
	}{
		{"ENG-42", 42, false},
		{"PROJ-1", 1, false},
		{"ACME-100", 100, false},
		{"ENG-", 0, true},
		{"nodashinid", 0, true},
		{"ENG-xyz", 0, true},
	}
	for _, tc := range cases {
		n, err := linearIssueNumber(tc.id)
		if tc.wantErr {
			if err == nil {
				t.Errorf("linearIssueNumber(%q): expected error", tc.id)
			}
			continue
		}
		if err != nil {
			t.Errorf("linearIssueNumber(%q): unexpected error %v", tc.id, err)
			continue
		}
		if n != tc.want {
			t.Errorf("linearIssueNumber(%q) = %d, want %d", tc.id, n, tc.want)
		}
	}
}

func TestParseTrigger(t *testing.T) {
	cases := []struct {
		input     string
		wantKind  triggerKind
		wantValue string
	}{
		{"", triggerNone, ""},
		{"triage", triggerLabel, "triage"},
		{"label:triage", triggerLabel, "triage"},
		{"label:My Label", triggerLabel, "My Label"},
		{"LABEL:triage", triggerLabel, "triage"},
		{"state:Todo", triggerState, "Todo"},
		{"STATE:In Progress", triggerState, "In Progress"},
		{"  label: spaced  ", triggerLabel, "spaced"},
	}
	for _, tc := range cases {
		gotKind, gotValue := parseTrigger(tc.input)
		if gotKind != tc.wantKind || gotValue != tc.wantValue {
			t.Errorf("parseTrigger(%q) = (%v, %q), want (%v, %q)",
				tc.input, gotKind, gotValue, tc.wantKind, tc.wantValue)
		}
	}
}

func TestLinearLabels(t *testing.T) {
	cases := []struct {
		name        string
		iss         linearIssue
		wantPrio    string
		wantBug     bool
		extraLabels []string
	}{
		{"urgent (p1)", mkLinearIssue(1, nil), "p0", false, nil},
		{"high (p2)", mkLinearIssue(2, nil), "p1", false, nil},
		{"medium (p3)", mkLinearIssue(3, nil), "p2", false, nil},
		{"low (p4)", mkLinearIssue(4, nil), "p3", false, nil},
		{"no-priority (p0)", mkLinearIssue(0, nil), "p2", false, nil},
		{"bug label", mkLinearIssue(2, []string{"bug"}), "p1", true, nil},
		{"bug case-insensitive", mkLinearIssue(3, []string{"Bug"}), "p2", true, nil},
		{"native labels", mkLinearIssue(3, []string{"regression"}), "p2", false, []string{"regression"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := linearLabels(tc.iss)
			hasPrio := false
			hasBug := false
			for _, l := range labels {
				if l == tc.wantPrio {
					hasPrio = true
				}
				if strings.EqualFold(l, "bug") {
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
					t.Errorf("labels %v missing expected label %q", labels, extra)
				}
			}
		})
	}
}

func mkLinearIssue(priority int, labelNames []string) linearIssue {
	nodes := make([]linearLabel, 0, len(labelNames))
	for _, n := range labelNames {
		nodes = append(nodes, linearLabel{Name: n})
	}
	return linearIssue{
		Identifier: "ENG-1",
		Priority:   priority,
		Labels:     linearLabelConn{Nodes: nodes},
	}
}

func TestLinearProvenanceIsScoped(t *testing.T) {
	c := &linearClient{}
	k1 := c.Provenance("linear.app", "ENG", 1)
	k2 := c.Provenance("linear.app", "OPS", 1)
	k3 := c.Provenance("linear.app", "ENG", 2)
	if k1 == k2 || k1 == k3 || k2 == k3 {
		t.Errorf("provenance keys must be distinct: %q %q %q", k1, k2, k3)
	}
	if !strings.HasPrefix(k1, "linear-") {
		t.Errorf("provenance key must start with linear-: %q", k1)
	}
	if k1 != "linear-ENG#1" {
		t.Errorf("provenance = %q, want %q", k1, "linear-ENG#1")
	}
}

func TestBuildLinearDescription(t *testing.T) {
	iss := SourceIssue{Number: 42, Body: "the body", Author: "alice@acme.test"}
	got := buildLinearDescription("ENG", iss)
	want := "the body\n\n---\nSource: linear.app/team/ENG/issue/ENG-42, author alice@acme.test, ingested by koryph intake"
	if got != want {
		t.Fatalf("buildLinearDescription =\n%q\nwant\n%q", got, want)
	}
	// Empty body → footer only, no leading blank line.
	empty := buildLinearDescription("ENG", SourceIssue{Number: 1})
	if strings.HasPrefix(empty, "\n") {
		t.Fatalf("empty-body description should not lead with a blank line: %q", empty)
	}
}

// --- integration: RunLinear dry-run ----------------------------------------

func TestRunLinearDryRunMutatesNothing(t *testing.T) {
	h := newLinearHarness(t)
	res, err := RunLinear(context.Background(), LinearOptions{
		Project: h.record(),
		TeamKey: "ENG",
		Trigger: "label:triage",
		DryRun:  true,
		Client:  h.client(t),
	})
	if err != nil {
		t.Fatalf("RunLinear dry-run: %v", err)
	}
	// ENG-12 and ENG-34 would be ingested; ENG-56 skipped (already ingested).
	if len(res.Ingested) != 2 {
		t.Fatalf("dry-run ingested = %d, want 2: %+v", len(res.Ingested), res.Ingested)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("dry-run skipped = %d, want 1: %+v", len(res.Skipped), res.Skipped)
	}
	// No bd create must have been called.
	bdLog := h.readBDLog(t)
	if strings.Contains(bdLog, "create") {
		t.Fatalf("dry-run must not create beads:\n%s", bdLog)
	}
	// No comment must have been posted.
	if len(h.commentLog) != 0 {
		t.Fatalf("dry-run must not post comments: %v", h.commentLog)
	}
}

// --- integration: RunLinear live ingest ------------------------------------

func TestRunLinearIngestsDedupesAndMaps(t *testing.T) {
	h := newLinearHarness(t)
	res, err := RunLinear(context.Background(), LinearOptions{
		Project: h.record(),
		TeamKey: "ENG",
		Trigger: "label:triage",
		Client:  h.client(t),
	})
	if err != nil {
		t.Fatalf("RunLinear: %v", err)
	}
	if res.Owner == "" || res.Repo != "ENG" {
		t.Fatalf("Owner/Repo = %q/%q, want .../ENG", res.Owner, res.Repo)
	}
	if len(res.Ingested) != 2 {
		t.Fatalf("ingested = %d, want 2: %+v", len(res.Ingested), res.Ingested)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %d, want 1: %+v", len(res.Skipped), res.Skipped)
	}

	// ENG-56 skipped with existing bead id.
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
		if !strings.Contains(line, "linear-") {
			t.Fatalf("create missing linear- provenance key: %q", line)
		}
	}
	// ENG-12 → priority 2 (Medium/Linear-3), no --type.
	if !hasCreateWith(createLines, "ENG#12", "--priority 2") {
		t.Fatalf("ENG-12 should have priority 2:\n%s", bdLog)
	}
	if hasCreateWith(createLines, "ENG#12", "--type") {
		t.Fatalf("ENG-12 (no bug label) should not set --type:\n%s", bdLog)
	}
	// ENG-34 → priority 1 (High/Linear-2), type bug (has "bug" label).
	if !hasCreateWith(createLines, "ENG#34", "--priority 1") {
		t.Fatalf("ENG-34 should have priority 1:\n%s", bdLog)
	}
	if !hasCreateWith(createLines, "ENG#34", "--type bug") {
		t.Fatalf("ENG-34 (bug label) should have --type bug:\n%s", bdLog)
	}
}

// --- integration: RunLinear comment-back -----------------------------------

func TestRunLinearCommentBack(t *testing.T) {
	h := newLinearHarness(t)
	res, err := RunLinear(context.Background(), LinearOptions{
		Project:     h.record(),
		TeamKey:     "ENG",
		Trigger:     "label:triage",
		CommentBack: true,
		Client:      h.client(t),
	})
	if err != nil {
		t.Fatalf("RunLinear comment-back: %v", err)
	}
	// Comments posted for ENG-12 and ENG-34 (ingested), not ENG-56 (skipped).
	if len(h.commentLog) != 2 {
		t.Fatalf("comment count = %d, want 2: %v", len(h.commentLog), h.commentLog)
	}
	for _, it := range res.Ingested {
		if it.Reason != "commented" {
			t.Errorf("ingested #%d reason = %q, want commented", it.Number, it.Reason)
		}
	}
}

// --- integration: RunLinear state trigger ----------------------------------

func TestRunLinearStateTrigger(t *testing.T) {
	h := newLinearHarness(t)
	// State trigger — the fake server responds with the same canned issues
	// regardless of filter type, so we just verify no error occurs and that
	// the query was dispatched (at least one server call made).
	_, err := RunLinear(context.Background(), LinearOptions{
		Project: h.record(),
		TeamKey: "ENG",
		Trigger: "state:Todo",
		DryRun:  true,
		Client:  h.client(t),
	})
	if err != nil {
		t.Fatalf("RunLinear state trigger: %v", err)
	}
	if h.issueQueryCount == 0 {
		t.Fatalf("expected at least one issues query, got 0")
	}
}

// --- integration: RunLinear no issues --------------------------------------

func TestRunLinearNoIssues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[]}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	bdBin := filepath.Join(root, "bd")
	_ = os.WriteFile(bdBin, []byte(fakeBDForLinear), 0o755)
	t.Setenv("KORYPH_BD_BIN", bdBin)
	t.Setenv("LINEAR_API_KEY", "lin_api_test_key")

	c, _ := newLinear()
	c.baseURL = srv.URL + "/graphql"

	res, err := RunLinear(context.Background(), LinearOptions{
		Project: &registry.Record{ProjectID: "x", Root: root},
		TeamKey: "ENG",
		Client:  c,
	})
	if err != nil {
		t.Fatalf("RunLinear no-issues: %v", err)
	}
	if len(res.Ingested) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

// --- integration: RunLinear input validation --------------------------------

func TestRunLinearRejectsNilProject(t *testing.T) {
	_, err := RunLinear(context.Background(), LinearOptions{TeamKey: "ENG"})
	if err == nil {
		t.Fatal("expected error for nil project")
	}
}

func TestRunLinearRejectsEmptyTeamKey(t *testing.T) {
	_, err := RunLinear(context.Background(), LinearOptions{
		Project: &registry.Record{ProjectID: "x", Root: "/tmp"},
	})
	if err == nil {
		t.Fatal("expected error for empty team key")
	}
}

// --- integration: RunLinear HTTP errors ------------------------------------

func TestRunLinearHTTPError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":[{"message":"rate limited"}]}`, http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	bdBin := filepath.Join(root, "bd")
	_ = os.WriteFile(bdBin, []byte(fakeBDForLinear), 0o755)
	t.Setenv("KORYPH_BD_BIN", bdBin)
	t.Setenv("LINEAR_API_KEY", "lin_api_test_key")

	c, _ := newLinear()
	c.baseURL = srv.URL + "/graphql"

	_, err := RunLinear(context.Background(), LinearOptions{
		Project: &registry.Record{ProjectID: "x", Root: root},
		TeamKey: "ENG",
		Client:  c,
	})
	if err == nil {
		t.Fatal("expected error on HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention HTTP 429: %v", err)
	}
}

func TestRunLinearGraphQLError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"team ENG not found"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	bdBin := filepath.Join(root, "bd")
	_ = os.WriteFile(bdBin, []byte(fakeBDForLinear), 0o755)
	t.Setenv("KORYPH_BD_BIN", bdBin)
	t.Setenv("LINEAR_API_KEY", "lin_api_test_key")

	c, _ := newLinear()
	c.baseURL = srv.URL + "/graphql"

	_, err := RunLinear(context.Background(), LinearOptions{
		Project: &registry.Record{ProjectID: "x", Root: root},
		TeamKey: "ENG",
		Client:  c,
	})
	if err == nil {
		t.Fatal("expected error on GraphQL error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

// --- integration: newLinear credential checks ------------------------------

func TestNewLinearMissingKey(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	_, err := newLinear()
	if err == nil || !strings.Contains(err.Error(), "LINEAR_API_KEY") {
		t.Errorf("expected error mentioning LINEAR_API_KEY, got %v", err)
	}
}

// --- integration: project config validation --------------------------------

func TestLinearProviderValidation(t *testing.T) {
	cfg := &project.Config{
		SchemaVersion:   1,
		ProjectID:       "test",
		WorkSource:      "bd",
		Gate:            []string{"true"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
	}

	// "linear" provider is valid.
	cfg.Intake = []project.IntakeSource{
		{Provider: "linear", Source: "ENG", Trigger: "label:triage"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected linear provider to be valid, got: %v", err)
	}

	// State trigger is also valid.
	cfg.Intake = []project.IntakeSource{
		{Provider: "linear", Source: "ENG", Trigger: "state:Todo"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected linear provider with state trigger to be valid, got: %v", err)
	}

	// Unknown provider is rejected (regression: "newprovider" must not sneak through).
	cfg.Intake = []project.IntakeSource{
		{Provider: "newprovider", Source: "ENG"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for unknown provider")
	}

	// Empty source is rejected for linear.
	cfg.Intake = []project.IntakeSource{
		{Provider: "linear", Source: ""},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for empty source")
	}
}

// --- integration: RunMulti with linear provider ----------------------------

func TestRunMultiLinearProvider(t *testing.T) {
	h := newLinearHarness(t)
	rec := &registry.Record{
		ProjectID: "multi-linear-test",
		Root:      h.root,
		Remote:    "https://github.com/placeholder/placeholder",
	}

	results, err := RunMulti(context.Background(), MultiOptions{
		Project: rec,
		Sources: []project.IntakeSource{
			{
				Provider: "linear",
				Source:   "ENG",
				Trigger:  "label:triage",
			},
		},
		// RunMulti calls newLinear() internally which reads LINEAR_API_KEY from
		// env (already set by newLinearHarness). But the client it builds points
		// at linearGraphQLEndpoint, not our test server — so we expect a
		// connection error (not "provider not supported").
	})
	// The error should be a network failure, not "not supported".
	if err != nil && strings.Contains(err.Error(), "not supported") {
		t.Fatalf("linear provider should be dispatched, not rejected: %v", err)
	}
	_ = results
}

func TestRunMultiLinearProviderValidation(t *testing.T) {
	cfg := &project.Config{
		SchemaVersion:   1,
		ProjectID:       "test",
		WorkSource:      "bd",
		Gate:            []string{"true"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
	}

	// Valid: linear.
	cfg.Intake = []project.IntakeSource{
		{Provider: "linear", Source: "ENG", Trigger: "label:triage"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected valid config, got: %v", err)
	}

	// Rejected: unknown provider.
	cfg.Intake = []project.IntakeSource{
		{Provider: "linear-v2", Source: "ENG"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for unknown provider linear-v2")
	}
}
