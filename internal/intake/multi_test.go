// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package intake

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// makeHarness wires fake gh + fake bd binaries into a temp root and sets the
// required env vars. The fake gh reads issue JSON from the file whose path is
// in GH_ISSUES_JSON; the fake bd creates beads with sequential IDs and dedupes
// on gh-56.
func makeHarness(t *testing.T, issues string) (root string, ghLog string, bdLog string) {
	t.Helper()
	root = t.TempDir()

	ghBin := filepath.Join(root, "gh")
	if err := os.WriteFile(ghBin, []byte(fakeGH), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	bdBin := filepath.Join(root, "bd")
	if err := os.WriteFile(bdBin, []byte(fakeBD), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	issuesJSON := filepath.Join(root, "issues.json")
	if err := os.WriteFile(issuesJSON, []byte(issues), 0o644); err != nil {
		t.Fatalf("write issues.json: %v", err)
	}

	createSeq := filepath.Join(root, "create.seq")
	ghLog = filepath.Join(root, "gh-argv.log")
	bdLog = filepath.Join(root, "bd-argv.log")

	t.Setenv("KORYPH_GH_BIN", ghBin)
	t.Setenv("KORYPH_BD_BIN", bdBin)
	t.Setenv("GH_ARGS_LOG", ghLog)
	t.Setenv("BD_ARGS_LOG", bdLog)
	t.Setenv("GH_ISSUES_JSON", issuesJSON)
	t.Setenv("BD_CREATE_SEQ", createSeq)
	return root, ghLog, bdLog
}

// multiRecord returns a registry Record pointing at root. Remote is unused by
// RunMulti (source-level remotes are synthetic) but required to be non-empty.
func multiRecord(root string) *registry.Record {
	return &registry.Record{ProjectID: "multi-test", Root: root, Remote: "https://github.com/placeholder/placeholder"}
}

// --- RunMulti: two sources, both ingest independently ---------------------------

func TestRunMultiTwoSources(t *testing.T) {
	// Two canned issues in repo A; none in repo B.
	repoAIssues := `[
		{"number":1,"title":"Issue A1","body":"body a1","labels":[{"name":"triage"}],"author":{"login":"alice"}},
		{"number":2,"title":"Issue A2","body":"body a2","labels":[{"name":"triage"},{"name":"p1"}],"author":{"login":"bob"}}
	]`
	repoBIssues := `[
		{"number":10,"title":"Issue B1","body":"body b1","labels":[{"name":"triage"}],"author":{"login":"carol"}}
	]`

	// Both sources share the same fake gh binary, which always serves the same
	// issues.json. We test them independently with two RunMulti calls, each
	// with a separate harness.
	root1, ghLog1, bdLog1 := makeHarness(t, repoAIssues)
	rec1 := multiRecord(root1)

	// First source: acme/widget
	results1, err := RunMulti(context.Background(), MultiOptions{
		Project: rec1,
		Sources: []project.IntakeSource{
			{Provider: "github", Source: "acme/widget", Trigger: "triage"},
		},
	})
	if err != nil {
		t.Fatalf("source 1 error: %v", err)
	}
	if len(results1) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results1))
	}
	if results1[0].Provider != "github" || results1[0].Source != "acme/widget" {
		t.Fatalf("result metadata wrong: %+v", results1[0])
	}
	if len(results1[0].Result.Ingested) != 2 {
		t.Fatalf("source1: ingested %d, want 2", len(results1[0].Result.Ingested))
	}
	_ = ghLog1
	_ = bdLog1

	// Second source: acme/other
	root2, _, _ := makeHarness(t, repoBIssues)
	rec2 := multiRecord(root2)

	results2, err := RunMulti(context.Background(), MultiOptions{
		Project: rec2,
		Sources: []project.IntakeSource{
			{Provider: "github", Source: "acme/other", Trigger: "triage"},
		},
	})
	if err != nil {
		t.Fatalf("source 2 error: %v", err)
	}
	if len(results2[0].Result.Ingested) != 1 {
		t.Fatalf("source2: ingested %d, want 1", len(results2[0].Result.Ingested))
	}
}

// --- RunMulti: two sources in one call -----------------------------------------

func TestRunMultiTwoSourcesOneCall(t *testing.T) {
	// Single harness serves issues for both; gh is called once per source with
	// their respective --repo flags.
	issues := `[
		{"number":1,"title":"Issue 1","body":"body","labels":[{"name":"triage"}],"author":{"login":"alice"}},
		{"number":2,"title":"Issue 2","body":"body","labels":[{"name":"triage"}],"author":{"login":"bob"}}
	]`
	root, ghLog, _ := makeHarness(t, issues)
	rec := multiRecord(root)

	results, err := RunMulti(context.Background(), MultiOptions{
		Project: rec,
		Sources: []project.IntakeSource{
			{Source: "acme/repo-one"},
			{Source: "acme/repo-two", Trigger: "bug"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Both results should have the provider populated.
	for _, r := range results {
		if r.Provider != "github" {
			t.Errorf("provider = %q, want github", r.Provider)
		}
	}

	// gh called twice — once per repo, each with the correct --repo flag.
	ghLogData, err := os.ReadFile(ghLog)
	if err != nil {
		t.Fatalf("read gh log: %v", err)
	}
	ghLines := string(ghLogData)
	if !strings.Contains(ghLines, "--repo acme/repo-one") {
		t.Errorf("gh not called for acme/repo-one:\n%s", ghLines)
	}
	if !strings.Contains(ghLines, "--repo acme/repo-two") {
		t.Errorf("gh not called for acme/repo-two:\n%s", ghLines)
	}
	// Second source uses "bug" trigger.
	if !strings.Contains(ghLines, "--label bug") {
		t.Errorf("second source should use trigger=bug:\n%s", ghLines)
	}
}

// --- RunMulti: global overrides apply to all sources ---------------------------

func TestRunMultiLabelOverrideAppliesGlobally(t *testing.T) {
	issues := `[{"number":1,"title":"T","body":"","labels":[{"name":"triage"}],"author":{"login":"x"}}]`
	root, ghLog, _ := makeHarness(t, issues)
	rec := multiRecord(root)

	_, err := RunMulti(context.Background(), MultiOptions{
		Project:       rec,
		Sources:       []project.IntakeSource{{Source: "a/r1"}, {Source: "a/r2", Trigger: "bug"}},
		OverrideLabel: "emergency",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ghLogData, err := os.ReadFile(ghLog)
	if err != nil {
		t.Fatalf("read gh log: %v", err)
	}
	// Both sources must use the override label, not their own trigger.
	lines := strings.Split(strings.TrimSpace(string(ghLogData)), "\n")
	for _, line := range lines {
		if strings.Contains(line, "issue list") {
			if !strings.Contains(line, "--label emergency") {
				t.Errorf("expected --label emergency on line: %q", line)
			}
		}
	}
}

// --- RunMulti: dry-run does not create beads -----------------------------------

func TestRunMultiDryRunMutatesNothing(t *testing.T) {
	issues := `[{"number":1,"title":"X","body":"","labels":[{"name":"triage"}],"author":{"login":"x"}}]`
	root, _, bdLog := makeHarness(t, issues)
	rec := multiRecord(root)

	_, err := RunMulti(context.Background(), MultiOptions{
		Project: rec,
		Sources: []project.IntakeSource{{Source: "a/r"}, {Source: "b/s"}},
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data, rerr := os.ReadFile(bdLog); rerr == nil {
		if strings.Contains(string(data), "create") {
			t.Fatalf("dry-run must not create beads:\n%s", data)
		}
	}
}

// --- RunMulti: nil project returns error ---------------------------------------

func TestRunMultiNilProject(t *testing.T) {
	_, err := RunMulti(context.Background(), MultiOptions{
		Sources: []project.IntakeSource{{Source: "a/r"}},
	})
	if err == nil {
		t.Fatal("expected error for nil project")
	}
}

// --- RunMulti: empty sources returns error -------------------------------------

func TestRunMultiNoSources(t *testing.T) {
	_, err := RunMulti(context.Background(), MultiOptions{
		Project: &registry.Record{ProjectID: "x", Root: "/tmp", Remote: "https://github.com/a/b"},
	})
	if err == nil {
		t.Fatal("expected error for empty sources")
	}
}

// --- RunMulti: unsupported provider is skipped with error ----------------------

func TestRunMultiUnsupportedProvider(t *testing.T) {
	issues := `[]`
	root, _, _ := makeHarness(t, issues)
	rec := multiRecord(root)

	results, err := RunMulti(context.Background(), MultiOptions{
		Project: rec,
		Sources: []project.IntakeSource{
			{Provider: "asana", Source: "team/ENG"},
		},
	})
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
	if !strings.Contains(err.Error(), "asana") {
		t.Errorf("error should mention provider name, got: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("unsupported provider should produce 0 results, got %d", len(results))
	}
}

// --- IntakeSource: empty source field is invalid -------------------------------

func TestIntakeSourceValidation(t *testing.T) {
	cases := []struct {
		src     project.IntakeSource
		wantErr bool
	}{
		{project.IntakeSource{Source: "owner/repo"}, false},
		{project.IntakeSource{Provider: "github", Source: "owner/repo", Trigger: "bug", Limit: 5}, false},
		{project.IntakeSource{Provider: "github", Source: ""}, true},             // missing source
		{project.IntakeSource{Provider: "asana", Source: "ENG"}, true},           // unsupported provider
		{project.IntakeSource{Provider: "github", Source: "x", Limit: -1}, true}, // negative limit
	}

	cfg := &project.Config{
		SchemaVersion:   1,
		ProjectID:       "test",
		WorkSource:      "bd",
		Gate:            []string{"true"},
		MergePolicy:     "manual",
		RiskTierDefault: 2,
	}

	for _, tc := range cases {
		cfg.Intake = []project.IntakeSource{tc.src}
		err := cfg.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("expected error for %+v, got nil", tc.src)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("unexpected error for %+v: %v", tc.src, err)
		}
	}
}
