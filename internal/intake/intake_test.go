// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package intake

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/registry"
)

// fakeGH is a stand-in `gh` binary. It appends its full argv (one line) to
// $GH_ARGS_LOG and, for `issue list`, prints the JSON in $GH_ISSUES_JSON.
// `issue comment` succeeds silently.
const fakeGH = `#!/bin/sh
if [ -n "$GH_ARGS_LOG" ]; then
  echo "$@" >> "$GH_ARGS_LOG"
fi
case "$1 $2" in
  "issue list")
    cat "$GH_ISSUES_JSON"
    ;;
  "issue comment")
    : # succeed silently
    ;;
  *)
    echo "fake gh: unexpected args: $@" >&2
    exit 1
    ;;
esac
`

// fakeBD is a stand-in `bd` binary. It logs argv to $BD_ARGS_LOG. For `list`
// (the dedupe query) it reports an existing bead ONLY when the query carries
// gh-56 (via --external-ref or --label); otherwise it returns an empty array.
// `create --silent` emits a monotonically increasing fake id ($BD_CREATE_SEQ).
const fakeBD = `#!/bin/sh
if [ -n "$BD_ARGS_LOG" ]; then
  echo "$@" >> "$BD_ARGS_LOG"
fi
case "$1" in
  list)
    if printf '%s' "$*" | grep -q -- 'gh-56'; then
      printf '[{"id":"cx-existing","title":"Already tracked","status":"open","priority":2,"labels":["gh-56","intake","no-dispatch"]}]'
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
    : # comment/close/etc succeed silently
    ;;
esac
`

// canned gh issue list: #12 (plain), #34 (p1 + bug), #56 (already ingested).
const cannedIssues = `[
  {"number":12,"title":"Add dark mode","body":"Please add a dark theme.","labels":[{"name":"triage"}],"author":{"login":"alice"}},
  {"number":34,"title":"Crash on login","body":"It crashes hard.","labels":[{"name":"triage"},{"name":"p1"},{"name":"bug"}],"author":{"login":"bob"}},
  {"number":56,"title":"Already tracked","body":"dup","labels":[{"name":"triage"}],"author":{"login":"carol"}}
]`

// harness wires fake gh + fake bd into a temp project and returns their argv
// log paths.
type harness struct {
	root       string
	ghLog      string
	bdLog      string
	createSeq  string
	issuesJSON string
}

func newHarness(t *testing.T, issues string) *harness {
	t.Helper()
	root := t.TempDir()

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
		t.Fatalf("write canned issues: %v", err)
	}

	h := &harness{
		root:       root,
		ghLog:      filepath.Join(root, "gh-argv.log"),
		bdLog:      filepath.Join(root, "bd-argv.log"),
		createSeq:  filepath.Join(root, "create.seq"),
		issuesJSON: issuesJSON,
	}
	t.Setenv("KORYPH_GH_BIN", ghBin)
	t.Setenv("KORYPH_BD_BIN", bdBin)
	t.Setenv("GH_ARGS_LOG", h.ghLog)
	t.Setenv("BD_ARGS_LOG", h.bdLog)
	t.Setenv("GH_ISSUES_JSON", issuesJSON)
	t.Setenv("BD_CREATE_SEQ", h.createSeq)
	return h
}

func (h *harness) record(remote string) *registry.Record {
	return &registry.Record{ProjectID: "demo", Root: h.root, Remote: remote}
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read log %s: %v", path, err)
	}
	return string(data)
}

// --- remote parsing --------------------------------------------------------

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		remote      string
		owner, repo string
		wantErr     bool
	}{
		{"https://github.com/acme/widgets.git", "acme", "widgets", false},
		{"https://github.com/acme/widgets", "acme", "widgets", false},
		{"git@github.com:acme/widgets.git", "acme", "widgets", false},
		{"git@github.com:acme/widgets", "acme", "widgets", false},
		{"ssh://git@github.com/acme/widgets.git", "acme", "widgets", false},
		{"https://GitHub.com/Acme/Widgets.git", "Acme", "Widgets", false}, // host case-insensitive, path preserved
		{"", "", "", true},
		{"https://gitlab.com/acme/widgets.git", "", "", true},
		{"git@bitbucket.org:acme/widgets.git", "", "", true},
		{"not a url", "", "", true},
		{"https://github.com/acme", "", "", true}, // no repo segment
	}
	for _, tc := range cases {
		t.Run(tc.remote, func(t *testing.T) {
			owner, repo, err := ParseGitHubRemote(tc.remote)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %s/%s", tc.remote, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.remote, err)
			}
			if owner != tc.owner || repo != tc.repo {
				t.Fatalf("%q -> %s/%s, want %s/%s", tc.remote, owner, repo, tc.owner, tc.repo)
			}
		})
	}
}

func TestRunRejectsNonGitHubRemote(t *testing.T) {
	h := newHarness(t, cannedIssues)
	if _, err := Run(context.Background(), Options{Project: h.record("https://gitlab.com/acme/widgets.git")}); err == nil {
		t.Fatal("expected refusal for a non-GitHub remote")
	}
}

func TestRunRejectsNilProject(t *testing.T) {
	if _, err := Run(context.Background(), Options{}); err == nil {
		t.Fatal("expected error for nil project")
	}
}

// --- ingest / dedupe / mapping ---------------------------------------------

func TestRunIngestsDedupesAndMaps(t *testing.T) {
	h := newHarness(t, cannedIssues)
	res, err := Run(context.Background(), Options{Project: h.record("git@github.com:acme/widgets.git")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Owner != "acme" || res.Repo != "widgets" {
		t.Fatalf("owner/repo = %s/%s", res.Owner, res.Repo)
	}
	// #12 and #34 ingested; #56 skipped (dedupe).
	if len(res.Ingested) != 2 {
		t.Fatalf("ingested = %d, want 2: %+v", len(res.Ingested), res.Ingested)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %d, want 1: %+v", len(res.Skipped), res.Skipped)
	}
	if res.Skipped[0].Number != 56 || res.Skipped[0].BeadID != "cx-existing" {
		t.Fatalf("skip = %+v, want #56 -> cx-existing", res.Skipped[0])
	}
	if res.Skipped[0].Reason != "already ingested" {
		t.Fatalf("skip reason = %q", res.Skipped[0].Reason)
	}
	for _, it := range res.Ingested {
		if it.BeadID == "" {
			t.Fatalf("ingested item missing bead id: %+v", it)
		}
	}

	// gh issue list called once with the trigger label; the repo passed through.
	ghLog := readLog(t, h.ghLog)
	if !strings.Contains(ghLog, "issue list --repo acme/widgets --label triage --state open") {
		t.Fatalf("gh issue list argv unexpected:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "issue comment") {
		t.Fatalf("no comment expected without --comment:\n%s", ghLog)
	}

	// Dedupe — with acme/widgets remote the new qualified key is
	// "gh-acme/widgets#<n>". The backward-compat check also searches the old
	// "gh-<n>" key so beads created by older intake runs are not re-ingested.
	//
	// fakeBD's dedup heuristic only matches the old "gh-56" pattern. So the
	// sequence for #56 is:
	//   1. bd list --external-ref gh-acme/widgets#56 → []
	//   2. bd list --label gh-acme/widgets#56 → []
	//   3. bd list --external-ref gh-56 → [cx-existing] ← FOUND via backward compat
	// For #12/#34 all four steps return empty and a new bead is created.
	bdLog := readLog(t, h.bdLog)

	// Primary (new qualified key): ext-ref queried for all 3 issues.
	for _, n := range []string{"gh-acme/widgets#12", "gh-acme/widgets#34", "gh-acme/widgets#56"} {
		if !strings.Contains(bdLog, "list --external-ref "+n) {
			t.Fatalf("dedupe primary ext-ref query for %s missing:\n%s", n, bdLog)
		}
	}
	// Primary label fallback (runs for all 3 because new-key ext-ref returns empty).
	for _, n := range []string{"gh-acme/widgets#12", "gh-acme/widgets#34", "gh-acme/widgets#56"} {
		if !strings.Contains(bdLog, "list --label "+n) {
			t.Fatalf("dedupe primary label fallback for %s missing:\n%s", n, bdLog)
		}
	}
	// Backward-compat: old unqualified key searched for all 3.
	for _, n := range []string{"gh-12", "gh-34", "gh-56"} {
		if !strings.Contains(bdLog, "list --external-ref "+n) {
			t.Fatalf("dedupe backward-compat ext-ref query for %s missing:\n%s", n, bdLog)
		}
	}
	// Backward-compat label fallback for #12 and #34 only.
	// #56 is found by the backward-compat ext-ref step, so its label fallback
	// is never reached.
	for _, n := range []string{"gh-12", "gh-34"} {
		if !strings.Contains(bdLog, "list --label "+n) {
			t.Fatalf("dedupe backward-compat label fallback for %s missing:\n%s", n, bdLog)
		}
	}

	// The no-dispatch label is MANDATORY on every create.
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
	}
	// #12: new qualified key, default priority 2, no --type.
	if !hasCreateWith(createLines, "gh-acme/widgets#12,intake,no-dispatch", "--priority 2") {
		t.Fatalf("expected #12 create with default priority 2:\n%s", bdLog)
	}
	if hasCreateWith(createLines, "gh-acme/widgets#12", "--type") {
		t.Fatalf("plain issue #12 should not set --type:\n%s", bdLog)
	}
	if !hasCreateWith(createLines, "--external-ref", "gh-acme/widgets#12") {
		t.Fatalf("expected --external-ref gh-acme/widgets#12 on create:\n%s", bdLog)
	}
	// #34: new qualified key, priority 1, --type bug.
	if !hasCreateWith(createLines, "gh-acme/widgets#34,intake,no-dispatch", "--priority 1") || !hasCreateWith(createLines, "gh-acme/widgets#34", "--type bug") {
		t.Fatalf("expected #34 create with priority 1 + type bug:\n%s", bdLog)
	}
	if !hasCreateWith(createLines, "--external-ref", "gh-acme/widgets#34") {
		t.Fatalf("expected --external-ref gh-acme/widgets#34 on create:\n%s", bdLog)
	}
}

// --- dry-run: zero mutation ------------------------------------------------

func TestRunDryRunMutatesNothing(t *testing.T) {
	h := newHarness(t, cannedIssues)
	res, err := Run(context.Background(), Options{
		Project: h.record("https://github.com/acme/widgets.git"),
		DryRun:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ingested) != 2 || len(res.Skipped) != 1 {
		t.Fatalf("dry-run counts = %d/%d, want 2/1", len(res.Ingested), len(res.Skipped))
	}
	// The argv logs PROVE no mutation: no bd create, no gh comment.
	bdLog := readLog(t, h.bdLog)
	if strings.Contains(bdLog, "create") {
		t.Fatalf("dry-run must not create beads:\n%s", bdLog)
	}
	ghLog := readLog(t, h.ghLog)
	if strings.Contains(ghLog, "issue comment") {
		t.Fatalf("dry-run must not comment:\n%s", ghLog)
	}
	// The create-sequence counter file must not exist (no create ran).
	if _, err := os.Stat(h.createSeq); !os.IsNotExist(err) {
		t.Fatalf("create sequence file should not exist after dry-run")
	}
}

// --- comment-back opt-in ---------------------------------------------------

func TestRunCommentBack(t *testing.T) {
	h := newHarness(t, cannedIssues)
	res, err := Run(context.Background(), Options{
		Project:     h.record("https://github.com/acme/widgets.git"),
		CommentBack: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ghLog := readLog(t, h.ghLog)
	// Comment fired for both ingested issues, not the skipped one.
	for _, n := range []string{"12", "34"} {
		if !strings.Contains(ghLog, "issue comment "+n+" --repo acme/widgets") {
			t.Fatalf("expected comment on #%s:\n%s", n, ghLog)
		}
	}
	if strings.Contains(ghLog, "issue comment 56") {
		t.Fatalf("skipped issue #56 must not be commented:\n%s", ghLog)
	}
	for _, it := range res.Ingested {
		if it.Reason != "commented" {
			t.Fatalf("ingested #%d reason = %q, want commented", it.Number, it.Reason)
		}
	}
}

// --- empty frontier --------------------------------------------------------

func TestRunNoIssues(t *testing.T) {
	h := newHarness(t, `[]`)
	res, err := Run(context.Background(), Options{Project: h.record("https://github.com/acme/widgets.git")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ingested) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

// --- cross-repo key uniqueness ------------------------------------------------

// TestProvenanceIsRepoQualified asserts that issues with the same number in
// different repos produce distinct external-ref keys so they are never
// conflated by the bead deduplication logic.
func TestProvenanceIsRepoQualified(t *testing.T) {
	gh := newGH(t.TempDir())
	key1 := gh.Provenance("acme", "widgets", 1)
	key2 := gh.Provenance("acme", "other", 1)
	key3 := gh.Provenance("beta", "widgets", 1)

	if key1 == key2 {
		t.Errorf("same issue number in different repos must not share a key: %q == %q", key1, key2)
	}
	if key1 == key3 {
		t.Errorf("same issue number in different owners must not share a key: %q == %q", key1, key3)
	}
	if key2 == key3 {
		t.Errorf("same issue number, different owner/repo combos must not share a key: %q == %q", key2, key3)
	}

	// Keys must embed the owner and repo so they are human-readable.
	for _, k := range []string{key1, key2, key3} {
		if !strings.Contains(k, "acme") && !strings.Contains(k, "beta") {
			t.Errorf("key %q does not embed owner", k)
		}
	}
}

// TestLegacyProvenanceBackwardCompat verifies the backward-compat helper still
// produces the pre-v1 "gh-<number>" format (used ONLY for fallback dedup of
// old beads, never for creating new ones).
func TestLegacyProvenanceBackwardCompat(t *testing.T) {
	gh := newGH(t.TempDir())
	if got := gh.legacyProvenance(42); got != "gh-42" {
		t.Errorf("legacyProvenance(42) = %q, want gh-42", got)
	}
}

// --- provenance footer + type/priority helpers -----------------------------

func TestBuildDescriptionFooter(t *testing.T) {
	iss := SourceIssue{Number: 7, Body: "the body", Author: "dana"}
	got := buildDescription("acme", "widgets", iss)
	want := "the body\n\n---\nSource: github.com/acme/widgets/issues/7, author @dana, ingested by koryph intake"
	if got != want {
		t.Fatalf("description =\n%q\nwant\n%q", got, want)
	}
	// Empty body → footer only.
	empty := buildDescription("acme", "widgets", SourceIssue{Number: 8, Author: "e"})
	if strings.HasPrefix(empty, "\n") {
		t.Fatalf("empty-body description should not lead with a blank line: %q", empty)
	}
}

func TestPriorityAndType(t *testing.T) {
	if got := priorityFor(SourceIssue{Labels: []string{"triage"}}); got != 2 {
		t.Fatalf("default priority = %d, want 2", got)
	}
	for label, want := range map[string]int{"p0": 0, "p1": 1, "p2": 2, "p3": 3} {
		if got := priorityFor(SourceIssue{Labels: []string{label}}); got != want {
			t.Fatalf("priority for %q = %d, want %d", label, got, want)
		}
	}
	if got := issueTypeFor(SourceIssue{Labels: []string{"BUG"}}); got != "bug" {
		t.Fatalf("bug type = %q, want bug (case-insensitive)", got)
	}
	if got := issueTypeFor(SourceIssue{Labels: []string{"triage"}}); got != "" {
		t.Fatalf("non-bug type = %q, want empty", got)
	}
}

// --- small helpers ---------------------------------------------------------

func grepLines(log, needle string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}

// hasCreateWith reports whether some create line contains ALL the substrings.
func hasCreateWith(lines []string, subs ...string) bool {
	for _, line := range lines {
		ok := true
		for _, s := range subs {
			if !strings.Contains(line, s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
