// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package epicreview

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a shell script that prints body as its whole stdout.
// When KORYPH_TEST_EPICREVIEW_STDIN is set the script also captures stdin there.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		"if [ -n \"$KORYPH_TEST_EPICREVIEW_STDIN\" ]; then cat > \"$KORYPH_TEST_EPICREVIEW_STDIN\"; else cat > /dev/null; fi\n" +
		"cat <<'FAKE_EOF'\n" + body + "\nFAKE_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeClaudeFlaky writes a script that fails (exit 1) for its first failN
// invocations, then prints okBody. Invocation count persists across separate
// process spawns via a counter file.
func fakeClaudeFlaky(t *testing.T, failN int, okBody string) string {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	path := filepath.Join(dir, "fake-claude-flaky")
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		"n=0; [ -f '" + counter + "' ] && n=$(cat '" + counter + "')\n" +
		"n=$((n+1)); echo $n > '" + counter + "'\n" +
		"if [ \"$n\" -le " + strconv.Itoa(failN) + " ]; then echo 'transient: rate limit' >&2; exit 1; fi\n" +
		"cat <<'FAKE_EOF'\n" + okBody + "\nFAKE_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// baseOpts builds a minimal Opts for tests. outDir is placed in a temp dir so
// tests do not write to the repo.
func baseOpts(t *testing.T, claudeBin string) Opts {
	t.Helper()
	repo := t.TempDir()
	return Opts{
		EpicID:          "test-epic-001",
		EpicTitle:       "Test epic",
		EpicDescription: "Make things better.",
		DesignDocPath:   "docs/designs/test-epic.md",
		Children: []Child{
			{ID: "child-a", Title: "Implement A", MergeSHA: "abc1234", Labels: []string{"area:sched"}},
			{ID: "child-b", Title: "Implement B", CloseReason: "merged", Labels: []string{"area:quota"}},
		},
		Round:     1,
		RepoRoot:  repo,
		ClaudeBin: claudeBin,
		Persona:   "koryph-epic-validator",
		Model:     "opus",
		OutDir:    filepath.Join(t.TempDir(), "epic-reviews"),
	}
}

// cleanEnvelope wraps a raw verdict JSON in the claude CLI result envelope.
// The inner verdict JSON is properly encoded as a JSON string value so that
// multi-line or escaped content survives the shell script's heredoc round-trip.
func cleanEnvelope(verdictJSON string) string {
	// json.Marshal of a string produces a valid JSON string literal including
	// all necessary escape sequences (\n, \t, \", \\, …).
	inner, err := json.Marshal(verdictJSON)
	if err != nil {
		panic("cleanEnvelope: " + err.Error())
	}
	return `{"type":"result","is_error":false,"result":` + string(inner) + `}`
}

// fakeClaudeEnvDump dumps the validator's environment to envCapture (via
// `env`) before printing body.
func fakeClaudeEnvDump(t *testing.T, envCapture, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude-env")
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		"env > " + envCapture + "\n" +
		"cat <<'FAKE_EOF'\n" + body + "\nFAKE_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestValidateThreadsProxyAndSpawnKind is the koryph-3l1.1 acceptance test
// for this spawn site: o.ProxyBaseURL reaches the validator's actual child
// env as ANTHROPIC_BASE_URL, and attemptValidate unconditionally stamps
// KORYPH_SPAWN_KIND=epicreview (its ChildEnvSpec literal).
func TestValidateThreadsProxyAndSpawnKind(t *testing.T) {
	verdictJSON := `{"met":true,"summary":"clean","gaps":[]}`
	envCapture := filepath.Join(t.TempDir(), "env.txt")
	o := baseOpts(t, fakeClaudeEnvDump(t, envCapture, cleanEnvelope(verdictJSON)))
	o.ProxyBaseURL = "http://127.0.0.1:8091"

	v := Validate(context.Background(), o)
	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}

	env, err := os.ReadFile(envCapture)
	if err != nil {
		t.Fatalf("read captured env: %v", err)
	}
	if !strings.Contains(string(env), "ANTHROPIC_BASE_URL=http://127.0.0.1:8091\n") {
		t.Errorf("captured env missing ANTHROPIC_BASE_URL:\n%s", env)
	}
	if !strings.Contains(string(env), "KORYPH_SPAWN_KIND=epicreview\n") {
		t.Errorf("captured env missing KORYPH_SPAWN_KIND=epicreview:\n%s", env)
	}
}

// TestValidateClean checks a verdict where the epic is fully met with no gaps.
func TestValidateClean(t *testing.T) {
	verdictJSON := `{"met":true,"summary":"The epic landed cleanly.","gaps":[]}`
	o := baseOpts(t, fakeClaude(t, cleanEnvelope(verdictJSON)))

	v := Validate(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}
	if !v.Met {
		t.Errorf("Met = false, want true")
	}
	if v.Summary == "" {
		t.Errorf("Summary must not be empty")
	}
	if len(v.Gaps) != 0 {
		t.Errorf("Gaps = %+v, want empty", v.Gaps)
	}
	if len(v.Structural) != 0 {
		t.Errorf("Structural = %+v, want empty", v.Structural)
	}
	if v.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", v.Attempts)
	}

	// Verdict persisted to OutDir.
	dest := filepath.Join(o.OutDir, "test-epic-001-round1.json")
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("verdict not persisted at %s: %v", dest, err)
	}
	if !strings.Contains(string(raw), `"met":true`) {
		t.Errorf("persisted verdict = %q, want met:true", raw)
	}
}

// TestValidateGaps checks a verdict with completeness gaps (met=false).
func TestValidateGaps(t *testing.T) {
	verdictJSON := `{
		"met": false,
		"summary": "Design goal §4 was not implemented.",
		"gaps": [
			{
				"title": "Missing §4 integration",
				"why": "§4 of the design doc required X; no child delivered X",
				"acceptance": "X is callable from Y with no error",
				"type": "task",
				"labels": ["area:engine"],
				"depends_on": []
			}
		]
	}`
	o := baseOpts(t, fakeClaude(t, cleanEnvelope(verdictJSON)))

	v := Validate(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}
	if v.Met {
		t.Errorf("Met = true, want false (gaps present)")
	}
	if len(v.Gaps) != 1 {
		t.Fatalf("len(Gaps) = %d, want 1", len(v.Gaps))
	}
	g := v.Gaps[0]
	if g.Title != "Missing §4 integration" {
		t.Errorf("Gap.Title = %q", g.Title)
	}
	if g.Type != "task" {
		t.Errorf("Gap.Type = %q, want task", g.Type)
	}
	if len(g.Labels) == 0 || g.Labels[0] != "area:engine" {
		t.Errorf("Gap.Labels = %v", g.Labels)
	}
}

// TestValidateStructural checks a verdict with structural findings but met=true.
func TestValidateStructural(t *testing.T) {
	verdictJSON := `{
		"met": true,
		"summary": "Epic complete; one structural finding surfaced.",
		"structural": [
			{
				"category": "duplication",
				"title": "Duplicate parseX helper",
				"why": "internal/a/helper.go:12 and internal/b/helper.go:8 both define parseX",
				"acceptance": "parseX lives in internal/shared, both packages import it",
				"type": "chore",
				"labels": ["area:engine"]
			}
		]
	}`
	o := baseOpts(t, fakeClaude(t, cleanEnvelope(verdictJSON)))

	v := Validate(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}
	if !v.Met {
		t.Errorf("Met = false, want true (structural findings do not affect met)")
	}
	if len(v.Gaps) != 0 {
		t.Errorf("Gaps = %+v, want empty (structural-only verdict)", v.Gaps)
	}
	if len(v.Structural) != 1 {
		t.Fatalf("len(Structural) = %d, want 1", len(v.Structural))
	}
	s := v.Structural[0]
	if s.Category != "duplication" {
		t.Errorf("Structural.Category = %q, want duplication", s.Category)
	}
	if s.Type != "chore" {
		t.Errorf("Structural.Type = %q, want chore", s.Type)
	}
}

// TestValidateMalformedJSONRetry checks that a malformed-JSON response is
// retried and that the run eventually degrades when all attempts are exhausted.
func TestValidateMalformedJSONRetry(t *testing.T) {
	old := backoffUnit
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = old })

	o := baseOpts(t, fakeClaude(t, "I am not JSON at all, sorry."))
	o.Attempts = 2

	v := Validate(context.Background(), o)

	if !v.Degraded {
		t.Errorf("Degraded = false, want true for repeated malformed output")
	}
	if v.Reason == "" {
		t.Errorf("degraded verdict must carry a Reason, got empty")
	}
	if v.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (all attempts consumed)", v.Attempts)
	}
}

// TestValidateDegradation checks that a missing binary causes a degraded verdict.
func TestValidateDegradation(t *testing.T) {
	o := baseOpts(t, filepath.Join(t.TempDir(), "no-such-claude"))
	o.Attempts = 1

	v := Validate(context.Background(), o)

	if !v.Degraded {
		t.Errorf("Degraded = false, want true for missing binary")
	}
	if v.Met {
		t.Errorf("Met = true; degraded verdicts must never claim met")
	}
	if v.Reason == "" {
		t.Errorf("degraded verdict must carry a Reason, got empty")
	}
}

// TestValidateRetriesTransientThenSucceeds verifies retry + success path.
func TestValidateRetriesTransientThenSucceeds(t *testing.T) {
	old := backoffUnit
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = old })

	verdictJSON := `{"met":true,"summary":"All good after retry."}`
	o := baseOpts(t, fakeClaudeFlaky(t, 1, cleanEnvelope(verdictJSON)))
	o.Attempts = 3

	v := Validate(context.Background(), o)

	if v.Degraded {
		t.Fatalf("verdict degraded after a transient failure that should have been retried: %+v", v)
	}
	if !v.Met {
		t.Errorf("Met = false, want true")
	}
	if v.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (one transient + one success)", v.Attempts)
	}
}

// TestValidatePromptContent checks that the prompt contains required context.
func TestValidatePromptContent(t *testing.T) {
	verdictJSON := `{"met":true,"summary":"ok"}`
	capture := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("KORYPH_TEST_EPICREVIEW_STDIN", capture)

	o := baseOpts(t, fakeClaude(t, cleanEnvelope(verdictJSON)))
	o.EpicID = "epic-xyzzy"
	o.EpicTitle = "XYZ epic title"
	o.DesignDocPath = "docs/designs/xyzzy.md"
	o.PriorVerdicts = []string{`{"met":false,"summary":"round 1 miss"}`}
	o.Round = 2
	o.Children = []Child{
		{ID: "ch-1", Title: "Child one", MergeSHA: "deadbeef", CloseReason: "merged", Labels: []string{"area:sched"}},
	}

	Validate(context.Background(), o)

	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("captured stdin: %v", err)
	}
	ps := string(prompt)

	for _, want := range []string{
		"epic-xyzzy",
		"XYZ epic title",
		"docs/designs/xyzzy.md",
		"deadbeef",
		"area:sched",
		"round 1 miss", // prior verdict content
		"STRICT JSON",
		"Completeness",
		"Structural",
		"extract-common|architecture|duplication",
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// TestValidateProgressCallbackLaunchLine verifies that Progress is called
// exactly once on a clean single-attempt run, and that the launch line
// contains the key fields an operator needs (epic id, round, children count).
func TestValidateProgressCallbackLaunchLine(t *testing.T) {
	verdictJSON := `{"met":true,"summary":"Clean landing."}`
	o := baseOpts(t, fakeClaude(t, cleanEnvelope(verdictJSON)))

	var calls []string
	o.Progress = func(format string, args ...any) {
		calls = append(calls, fmt.Sprintf(format, args...))
	}

	v := Validate(context.Background(), o)
	if v.Degraded {
		t.Fatalf("verdict degraded: %+v", v)
	}

	if len(calls) != 1 {
		t.Fatalf("Progress calls = %d, want 1 (launch only); got: %v", len(calls), calls)
	}
	launch := calls[0]
	for _, want := range []string{"test-epic-001", "round 1", "opus", "koryph-epic-validator", "2 children", "420s"} {
		if !strings.Contains(launch, want) {
			t.Errorf("launch line missing %q: %q", want, launch)
		}
	}
}

// TestValidateProgressCallbackRetryLine verifies that Progress is called for
// the launch (attempt 1) and then again for each retry, with the retry line
// carrying the attempt number and the previous failure reason.
func TestValidateProgressCallbackRetryLine(t *testing.T) {
	old := backoffUnit
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = old })

	verdictJSON := `{"met":true,"summary":"All good after retry."}`
	// Fails once, then succeeds.
	o := baseOpts(t, fakeClaudeFlaky(t, 1, cleanEnvelope(verdictJSON)))
	o.Attempts = 3

	var calls []string
	o.Progress = func(format string, args ...any) {
		calls = append(calls, fmt.Sprintf(format, args...))
	}

	v := Validate(context.Background(), o)
	if v.Degraded {
		t.Fatalf("verdict degraded after retried transient: %+v", v)
	}

	// Expect: launch (attempt 1) + retry (attempt 2).
	if len(calls) != 2 {
		t.Fatalf("Progress calls = %d, want 2 (launch + 1 retry); got: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "test-epic-001") {
		t.Errorf("launch line missing epic id: %q", calls[0])
	}
	retry := calls[1]
	if !strings.Contains(retry, "attempt 2") {
		t.Errorf("retry line missing attempt number: %q", retry)
	}
	if !strings.Contains(retry, "reason") {
		t.Errorf("retry line missing reason field: %q", retry)
	}
}

// TestValidateProgressNilSafe verifies that Validate does not panic when
// Progress is nil (the zero value for a function field).
func TestValidateProgressNilSafe(t *testing.T) {
	verdictJSON := `{"met":true,"summary":"ok"}`
	o := baseOpts(t, fakeClaude(t, cleanEnvelope(verdictJSON)))
	// o.Progress is nil by default from baseOpts.

	v := Validate(context.Background(), o)
	if v.Degraded {
		t.Fatalf("verdict degraded with nil Progress: %+v", v)
	}
}

// TestBackoffForExponentialAndCapped mirrors the same test in the review
// package to ensure this package's copy behaves identically.
func TestBackoffForExponentialAndCapped(t *testing.T) {
	oldU, oldM := backoffUnit, maxBackoff
	backoffUnit, maxBackoff = 2*time.Second, 30*time.Second
	t.Cleanup(func() { backoffUnit, maxBackoff = oldU, oldM })

	cases := []struct {
		retry int
		want  time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second},   // capped
		{100, 30 * time.Second}, // shift overflow -> capped
	}
	for _, tc := range cases {
		if got := backoffFor(tc.retry); got != tc.want {
			t.Errorf("backoffFor(%d) = %s, want %s", tc.retry, got, tc.want)
		}
	}
}

// TestOutPathDefault checks the default verdict file path naming.
func TestOutPathDefault(t *testing.T) {
	p := outPath("/repo", "", "ep-001", 1)
	want := "/repo/.koryph/epic-reviews/ep-001-round1.json"
	if p != want {
		t.Errorf("outPath = %q, want %q", p, want)
	}

	p2 := outPath("/repo", "/custom/dir", "ep-002", 3)
	want2 := "/custom/dir/ep-002-round3.json"
	if p2 != want2 {
		t.Errorf("outPath = %q, want %q", p2, want2)
	}
}
