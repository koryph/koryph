// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/epicreview"
	"github.com/koryph/koryph/internal/registry"
)

// --- fake helpers -----------------------------------------------------------

// epicFakeBD is a stand-in `bd` binary for epic validate tests. It:
//   - logs its argv to $BD_ARGS_LOG (one invocation per line)
//   - returns a canned epic for `show`
//   - returns $BD_SHOW_TYPE as issue_type when set (for "not an epic" tests)
//   - returns open children for `list` when $BD_OPEN_CHILDREN=1
//   - emits a unique bead id for `create` (base id + monotonic counter in file)
//   - succeeds silently for `update`, `close`, `comment`
const epicFakeBD = `#!/bin/sh
if [ -n "$BD_ARGS_LOG" ]; then
  echo "$@" >> "$BD_ARGS_LOG"
fi
case "$1" in
  show)
    TYPE="${BD_SHOW_TYPE:-epic}"
    printf '{"id":"%s","title":"The Epic","status":"open","priority":0,"issue_type":"%s","labels":["area:engine"],"description":"Make things better."}' "$2" "$TYPE"
    ;;
  list)
    if [ "${BD_OPEN_CHILDREN}" = "1" ]; then
      printf '[{"id":"child-1","title":"Open Child","status":"open","priority":1,"issue_type":"task","labels":[]}]'
    else
      printf '[{"id":"child-a","title":"Child A","status":"closed","priority":1,"issue_type":"task","labels":["area:engine"]},{"id":"child-b","title":"Child B","status":"closed","priority":2,"issue_type":"task","labels":["area:quota"]}]'
    fi
    ;;
  create)
    # Return a unique bead id using a counter file so multiple creates are distinguishable.
    COUNT_FILE="${TMPDIR:-/tmp}/epic_fake_bd_count_$$"
    n=1
    if [ -f "$COUNT_FILE" ]; then n=$(( $(cat "$COUNT_FILE") + 1 )); fi
    echo $n > "$COUNT_FILE"
    printf 'epic-created-%d\n' "$n"
    ;;
  update|close|comment)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`

// epicFakeClaude writes a script that emits body as its stdout.
func epicFakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	// Escape the body for the heredoc by using a quoted string approach.
	// We write the body to a file and cat it, avoiding shell interpolation issues.
	bodyFile := filepath.Join(t.TempDir(), "verdict.json")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncat > /dev/null\ncat %q\n", bodyFile)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// wrapVerdict wraps a raw verdict JSON in the claude CLI result envelope.
func wrapVerdict(verdictJSON string) string {
	inner, err := json.Marshal(verdictJSON)
	if err != nil {
		panic("wrapVerdict: " + err.Error())
	}
	return `{"type":"result","is_error":false,"result":` + string(inner) + `}`
}

// installEpicFakeBD writes the epicFakeBD script to a temp dir, sets
// KORYPH_BD_BIN to it, and returns the argv-log path.
func installEpicFakeBD(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	if err := os.WriteFile(bin, []byte(epicFakeBD), 0o755); err != nil {
		t.Fatalf("write epic fake bd: %v", err)
	}
	log := filepath.Join(dir, "argv.log")
	t.Setenv("KORYPH_BD_BIN", bin)
	t.Setenv("BD_ARGS_LOG", log)
	return log
}

// registerEpicProject registers a minimal project and returns its record.
func registerEpicProject(t *testing.T, id string) *registry.Record {
	t.Helper()
	root := gitRepo(t)
	ctx := context.Background()
	store := registry.NewStore()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	rec := &registry.Record{
		ProjectID:        id,
		Name:             id,
		Root:             root,
		AccountProfile:   "personal",
		ExpectedIdentity: "me@example.com",
	}
	if err := store.Add(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// --- flag / input validation ------------------------------------------------

func TestEpicValidateRequiresEpicID(t *testing.T) {
	isolate(t)
	registerEpicProject(t, "proj1")
	code, _, errb := runCmd("epic", "validate", "--project", "proj1")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit", code)
	}
	if !strings.Contains(errb, "epic-id") {
		t.Errorf("stderr should mention epic-id; got: %s", errb)
	}
}

func TestEpicValidateRequiresProject(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("epic", "validate", "my-epic-1")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit", code)
	}
	if !strings.Contains(errb, "--project") {
		t.Errorf("stderr should mention --project; got: %s", errb)
	}
}

func TestEpicValidateUnknownProject(t *testing.T) {
	isolate(t)
	code, _, _ := runCmd("epic", "validate", "my-epic-1", "--project", "no-such-project")
	if code != engine.ExitFatal {
		t.Errorf("code = %d, want fatal for unknown project", code)
	}
}

func TestEpicSubcommandHelp(t *testing.T) {
	code, out, _ := runCmd("epic", "-h")
	if code != 0 {
		t.Errorf("code = %d, want 0 for help", code)
	}
	if !strings.Contains(out, "validate") {
		t.Errorf("epic help should list 'validate'; got: %s", out)
	}
}

func TestEpicUnknownSubcommand(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("epic", "frobnicate")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit", code)
	}
	if !strings.Contains(errb, "unknown epic subcommand") {
		t.Errorf("stderr should mention unknown subcommand; got: %s", errb)
	}
}

// --- bd-layer validation checks ---------------------------------------------

func TestEpicValidateMustBeEpicType(t *testing.T) {
	isolate(t)
	installEpicFakeBD(t)
	t.Setenv("BD_SHOW_TYPE", "task") // fake bd returns issue_type=task
	rec := registerEpicProject(t, "proj-task")

	code, _, errb := runCmd("epic", "validate", "my-task-1", "--project", rec.ProjectID)
	if code != engine.ExitFatal {
		t.Errorf("code = %d, want fatal when target is not an epic", code)
	}
	if !strings.Contains(errb, "not epic") {
		t.Errorf("stderr should explain type mismatch; got: %s", errb)
	}
}

func TestEpicValidateOpenChildrenBlocked(t *testing.T) {
	isolate(t)
	installEpicFakeBD(t)
	t.Setenv("BD_OPEN_CHILDREN", "1")
	rec := registerEpicProject(t, "proj-open")

	code, _, errb := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID)
	if code != engine.ExitFatal {
		t.Errorf("code = %d, want fatal when children are open", code)
	}
	if !strings.Contains(errb, "unclosed children") {
		t.Errorf("stderr should mention unclosed children; got: %s", errb)
	}
}

// --- verdict handling -------------------------------------------------------

func TestEpicValidateMetClosesEpic(t *testing.T) {
	isolate(t)
	argsLog := installEpicFakeBD(t)

	verdictJSON := `{"met":true,"summary":"The epic landed cleanly — all goals met."}`
	claudeBin := epicFakeClaude(t, wrapVerdict(verdictJSON))
	t.Setenv("KORYPH_CLAUDE_BIN", claudeBin)

	rec := registerEpicProject(t, "proj-met")
	// Disable the docs-update stage: this test covers the direct-close path
	// (docs_update disabled -> met closes per auto_close default true).
	docsOff := false
	writeProjConfig(t, rec.Root, nil, &docsOff)

	code, out, _ := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID)
	if code != 0 {
		t.Errorf("code = %d, want 0 for met verdict", code)
	}
	if !strings.Contains(out, "validated") {
		t.Errorf("stdout should confirm validation; got: %s", out)
	}

	// Verify bd close was called.
	argv, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal("read argv log:", err)
	}
	calls := string(argv)
	if !strings.Contains(calls, "close my-epic-1") {
		t.Errorf("bd close not called; argv log: %s", calls)
	}
	if !strings.Contains(calls, "update") {
		t.Errorf("bd update (for notes) not called; argv log: %s", calls)
	}
}

func TestEpicValidateMetAutoCloseFalse(t *testing.T) {
	isolate(t)
	argsLog := installEpicFakeBD(t)

	verdictJSON := `{"met":true,"summary":"All goals met."}`
	claudeBin := epicFakeClaude(t, wrapVerdict(verdictJSON))
	t.Setenv("KORYPH_CLAUDE_BIN", claudeBin)

	rec := registerEpicProject(t, "proj-autoclose-false")
	// Write a project config with auto_close=false.
	autoCloseFalse := false
	docsOff2 := false
	writeProjConfig(t, rec.Root, &autoCloseFalse, &docsOff2)

	code, out, _ := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID)
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	// Should label, not close.
	argv, _ := os.ReadFile(argsLog)
	calls := string(argv)
	if strings.Contains(calls, "close my-epic-1") {
		t.Errorf("bd close should NOT be called when auto_close=false; argv log: %s", calls)
	}
	if !strings.Contains(calls, "add-label") {
		t.Errorf("bd add-label (validation:passed) should be called; argv log: %s", calls)
	}
	if !strings.Contains(out, "auto_close=false") {
		t.Errorf("stdout should mention auto_close=false; got: %s", out)
	}
}

func TestEpicValidateMetFilesDocsBead(t *testing.T) {
	isolate(t)
	argsLog := installEpicFakeBD(t)

	verdictJSON := `{"met":true,"summary":"All goals met."}`
	claudeBin := epicFakeClaude(t, wrapVerdict(verdictJSON))
	t.Setenv("KORYPH_CLAUDE_BIN", claudeBin)

	rec := registerEpicProject(t, "proj-met-docs")
	// No project config: docs_update defaults to enabled (design 4b) — met
	// labels validation:passed and files the docs bead; the epic is NOT
	// closed until the docs bead merges.
	code, out, _ := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID)
	if code != 0 {
		t.Errorf("code = %d, want 0 for met verdict", code)
	}
	argv, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal("read argv log:", err)
	}
	calls := string(argv)
	if strings.Contains(calls, "close my-epic-1") {
		t.Errorf("epic must NOT close while the docs bead is open; argv log: %s", calls)
	}
	if !strings.Contains(calls, "add-label validation:passed") {
		t.Errorf("validation:passed label missing; argv log: %s", calls)
	}
	if !strings.Contains(calls, "validation:docs") {
		t.Errorf("docs bead with validation:docs label not created; argv log: %s", calls)
	}
	if !strings.Contains(out, "docs bead") {
		t.Errorf("stdout should mention the docs bead; got: %s", out)
	}
}

func TestEpicValidateGapsFilesBeads(t *testing.T) {
	isolate(t)
	argsLog := installEpicFakeBD(t)

	verdictJSON := `{
		"met": false,
		"summary": "Design goal §4 was not implemented.",
		"gaps": [
			{
				"title": "Missing §4 integration",
				"why": "§4 requires X but no child delivered X",
				"acceptance": "X is callable from Y",
				"type": "task",
				"labels": ["area:engine"],
				"depends_on": []
			},
			{
				"title": "Config wiring absent",
				"why": "epic_validation config block not read",
				"acceptance": "config block is consumed",
				"type": "chore",
				"labels": ["area:engine"],
				"depends_on": ["0"]
			}
		]
	}`
	claudeBin := epicFakeClaude(t, wrapVerdict(verdictJSON))
	t.Setenv("KORYPH_CLAUDE_BIN", claudeBin)

	rec := registerEpicProject(t, "proj-gaps")

	code, out, _ := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID)
	if code != 0 {
		t.Errorf("code = %d, want 0 for gaps verdict (beads filed, no error)", code)
	}
	if !strings.Contains(out, "2 gap(s)") {
		t.Errorf("stdout should mention 2 gaps; got: %s", out)
	}

	argv, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal("read argv log:", err)
	}
	calls := string(argv)
	// Two child beads created.
	createCount := strings.Count(calls, "create")
	if createCount < 2 {
		t.Errorf("expected >=2 bd create calls for 2 gaps, got %d; argv log: %s", createCount, calls)
	}
	// Notes updated on epic.
	if !strings.Contains(calls, "append-notes") {
		t.Errorf("bd update --append-notes not called; argv log: %s", calls)
	}
	// Parent set on child beads.
	if !strings.Contains(calls, "--parent") {
		t.Errorf("bd create should carry --parent for gap beads; argv log: %s", calls)
	}
}

func TestEpicValidateStructuralBeadsStandalone(t *testing.T) {
	isolate(t)
	argsLog := installEpicFakeBD(t)

	verdictJSON := `{
		"met": true,
		"summary": "Epic complete; one structural finding.",
		"structural": [
			{
				"category": "duplication",
				"title": "Duplicate parseX helper",
				"why": "internal/a/helper.go:12 and internal/b/helper.go:8 define parseX",
				"acceptance": "parseX lives in internal/shared",
				"type": "chore",
				"labels": ["area:engine"]
			}
		]
	}`
	claudeBin := epicFakeClaude(t, wrapVerdict(verdictJSON))
	t.Setenv("KORYPH_CLAUDE_BIN", claudeBin)

	rec := registerEpicProject(t, "proj-structural")

	code, _, _ := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID)
	if code != 0 {
		t.Errorf("code = %d, want 0 (structural findings do not block met)", code)
	}

	argv, _ := os.ReadFile(argsLog)
	calls := string(argv)
	// One structural bead created (standalone: no --parent flag for it).
	if !strings.Contains(calls, "create") {
		t.Errorf("structural bead not created; argv log: %s", calls)
	}
	if !strings.Contains(calls, "validation:structural") {
		t.Errorf("structural bead should carry validation:structural label; argv log: %s", calls)
	}
	// Epic still closed (met=true).
	if !strings.Contains(calls, "close") {
		t.Errorf("epic should be closed (met=true); argv log: %s", calls)
	}
}

func TestEpicValidateDegradedExitsNonZero(t *testing.T) {
	isolate(t)
	argsLog := installEpicFakeBD(t)

	// Use a non-existent claude so the validator degrades immediately.
	t.Setenv("KORYPH_CLAUDE_BIN", filepath.Join(t.TempDir(), "no-such-claude"))

	rec := registerEpicProject(t, "proj-degraded")

	code, _, _ := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID)
	if code == 0 {
		t.Errorf("code = 0, want nonzero for degraded verdict")
	}

	argv, _ := os.ReadFile(argsLog)
	calls := string(argv)
	// validation:degraded label added.
	if !strings.Contains(calls, "add-label") {
		t.Errorf("bd add-label not called for degraded verdict; argv log: %s", calls)
	}
	if !strings.Contains(calls, "validation:degraded") {
		t.Errorf("validation:degraded label not passed; argv log: %s", calls)
	}
}

func TestEpicValidateJSONEmitsVerdict(t *testing.T) {
	isolate(t)
	installEpicFakeBD(t)

	verdictJSON := `{"met":true,"summary":"Clean landing."}`
	claudeBin := epicFakeClaude(t, wrapVerdict(verdictJSON))
	t.Setenv("KORYPH_CLAUDE_BIN", claudeBin)

	rec := registerEpicProject(t, "proj-json")

	code, out, _ := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID, "--json")
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	// stdout should contain the raw verdict JSON fields.
	if !strings.Contains(out, `"met"`) {
		t.Errorf("--json output missing 'met' field; got: %s", out)
	}
	if !strings.Contains(out, "true") {
		t.Errorf("--json output missing met=true; got: %s", out)
	}
}

func TestEpicValidateDegradedJSONFallback(t *testing.T) {
	isolate(t)
	installEpicFakeBD(t)

	// Non-existent claude → degraded with empty Raw.
	t.Setenv("KORYPH_CLAUDE_BIN", filepath.Join(t.TempDir(), "no-such-claude"))

	rec := registerEpicProject(t, "proj-degraded-json")

	code, out, _ := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID, "--json")
	if code == 0 {
		t.Errorf("code = 0, want nonzero for degraded verdict")
	}
	// Should emit some JSON even when degraded.
	if !strings.Contains(out, `"degraded"`) {
		t.Errorf("--json output missing 'degraded' field; got: %s", out)
	}
}

func TestEpicValidateRoundFlag(t *testing.T) {
	isolate(t)
	argsLog := installEpicFakeBD(t)

	verdictJSON := `{"met":true,"summary":"Round 2 clean."}`
	claudeBin := epicFakeClaude(t, wrapVerdict(verdictJSON))
	t.Setenv("KORYPH_CLAUDE_BIN", claudeBin)

	rec := registerEpicProject(t, "proj-round")

	code, _, _ := runCmd("epic", "validate", "my-epic-1", "--project", rec.ProjectID, "--round", "2")
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	argv, _ := os.ReadFile(argsLog)
	// close is called with reason containing "round 2"
	if !strings.Contains(string(argv), "round 2") {
		t.Errorf("close reason should mention round 2; argv log: %s", string(argv))
	}
}

// --- helper unit tests -------------------------------------------------------

func TestDetectNextRoundEmpty(t *testing.T) {
	dir := t.TempDir()
	got := epicreview.DetectNextRound(dir, "my-epic")
	if got != 1 {
		t.Errorf("detectNextRound empty dir = %d, want 1", got)
	}
}

func TestDetectNextRoundWithPriorFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"my-epic-round1.json",
		"my-epic-round2.json",
		"other-epic-round5.json", // different epic — must be ignored
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := epicreview.DetectNextRound(dir, "my-epic")
	if got != 3 {
		t.Errorf("detectNextRound = %d, want 3", got)
	}
}

func TestDetectNextRoundMissingDir(t *testing.T) {
	got := epicreview.DetectNextRound("/nonexistent/dir", "my-epic")
	if got != 1 {
		t.Errorf("detectNextRound missing dir = %d, want 1", got)
	}
}

func TestLoadPriorVerdictsEmpty(t *testing.T) {
	dir := t.TempDir()
	got := epicreview.LoadPriorVerdicts(dir, "my-epic", 1)
	if len(got) != 0 {
		t.Errorf("loadPriorVerdicts round 1 = %v, want empty", got)
	}
}

func TestLoadPriorVerdictsReadsRounds(t *testing.T) {
	dir := t.TempDir()
	// Write rounds 1 and 2.
	for r := 1; r <= 2; r++ {
		path := filepath.Join(dir, fmt.Sprintf("my-epic-round%d.json", r))
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"round":%d}`, r)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := epicreview.LoadPriorVerdicts(dir, "my-epic", 3) // asking for round 3 → reads 1+2
	if len(got) != 2 {
		t.Fatalf("loadPriorVerdicts = %v, want 2 entries", got)
	}
	if !strings.Contains(got[0], `"round":1`) {
		t.Errorf("prior[0] = %q, want round 1 content", got[0])
	}
	if !strings.Contains(got[1], `"round":2`) {
		t.Errorf("prior[1] = %q, want round 2 content", got[1])
	}
}

func TestLoadPriorVerdictsSkipsMissingRound(t *testing.T) {
	dir := t.TempDir()
	// Only round 2 file (round 1 missing).
	path := filepath.Join(dir, "my-epic-round2.json")
	if err := os.WriteFile(path, []byte(`{"round":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := epicreview.LoadPriorVerdicts(dir, "my-epic", 3)
	if len(got) != 1 {
		t.Fatalf("loadPriorVerdicts = %v, want 1 entry (round 2 only)", got)
	}
}

// --- helpers ----------------------------------------------------------------

// writeProjConfig writes a minimal but valid koryph.project.json with the
// given epic_validation.auto_close setting.
func writeProjConfig(t *testing.T, root string, autoClose, docsEnabled *bool) {
	t.Helper()
	type docsCfg struct {
		Enabled *bool `json:"enabled,omitempty"`
	}
	type evCfg struct {
		AutoClose  *bool    `json:"auto_close,omitempty"`
		DocsUpdate *docsCfg `json:"docs_update,omitempty"`
	}
	type cfg struct {
		SchemaVersion  int      `json:"schema_version"`
		ProjectID      string   `json:"project_id"`
		WorkSource     string   `json:"work_source"`
		Gate           []string `json:"gate"`
		MergePolicy    string   `json:"merge_policy"`
		EpicValidation evCfg    `json:"epic_validation,omitempty"`
	}
	c := cfg{
		SchemaVersion:  1,
		ProjectID:      "test",
		WorkSource:     "bd",
		Gate:           []string{"true"}, // at least one gate command required
		MergePolicy:    "manual",
		EpicValidation: evCfg{AutoClose: autoClose},
	}
	if docsEnabled != nil {
		c.EpicValidation.DocsUpdate = &docsCfg{Enabled: docsEnabled}
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "koryph.project.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
