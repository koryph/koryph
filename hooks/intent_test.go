// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package hooks

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runIntentOpts controls one invocation of koryph-intent.sh under test.
type runIntentOpts struct {
	stdin     string // raw stdin body (the UserPromptSubmit hook JSON)
	phaseID   string // KORYPH_PHASE_ID, empty = unset
	spawnKind string // KORYPH_SPAWN_KIND, empty = unset
	pathDirs  string // full PATH override; empty = inherit os.Environ's PATH
}

// runIntent invokes hooks/koryph-intent.sh and returns its stdout, stderr,
// and exit code.
func runIntent(t *testing.T, opts runIntentOpts) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command("bash", "koryph-intent.sh")
	env := os.Environ()
	if opts.pathDirs != "" {
		env = append(env, "PATH="+opts.pathDirs)
	}
	if opts.phaseID != "" {
		env = append(env, "KORYPH_PHASE_ID="+opts.phaseID)
	}
	if opts.spawnKind != "" {
		env = append(env, "KORYPH_SPAWN_KIND="+opts.spawnKind)
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(opts.stdin)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("running koryph-intent.sh: %v\nstderr:\n%s", err, errBuf.String())
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// promptJSON builds a minimal UserPromptSubmit hook payload for prompt.
func promptJSON(t *testing.T, prompt string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// pathWithoutJq returns the current PATH with every directory that would
// resolve a real `jq` binary stripped out, so tests can exercise the
// missing-jq fail-open path without breaking bash's other PATH-resolved
// dependencies (tr, printf is a builtin).
func pathWithoutJq(t *testing.T) string {
	t.Helper()
	parts := filepath.SplitList(os.Getenv("PATH"))
	out := make([]string, 0, len(parts))
	for _, dir := range parts {
		if _, err := os.Stat(filepath.Join(dir, "jq")); err == nil {
			continue // this dir would resolve a real jq — drop it
		}
		out = append(out, dir)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

// TestInjectsRubricOnWorkIntent is the golden path: an operator prompt
// describing work to build produces a valid, hook-json-shaped rubric that
// names every planning command, under the 1KB byte budget (I3).
func TestInjectsRubricOnWorkIntent(t *testing.T) {
	prompts := []string{
		"We need to add rate limiting to the API server so a noisy client cannot starve the rest.",
		"fix the crash when the config file is missing on first startup",
		"I want the exporter to support CSV as well as JSON output",
		"the search page is broken when the query contains a unicode emoji",
	}
	for _, p := range prompts {
		t.Run(p[:20], func(t *testing.T) {
			out, _, code := runIntent(t, runIntentOpts{stdin: promptJSON(t, p)})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if len(out) == 0 {
				t.Fatal("no rubric injected for a work-intent prompt")
			}
			if len(out) > 1024 {
				t.Errorf("rubric is %d bytes, want <= 1024 (I3 byte budget)", len(out))
			}
			var payload struct {
				HookSpecificOutput struct {
					HookEventName     string `json:"hookEventName"`
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("output is not valid hook JSON: %v\n%s", err, out)
			}
			if payload.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
				t.Errorf("hookEventName = %q, want UserPromptSubmit", payload.HookSpecificOutput.HookEventName)
			}
			for _, cmd := range []string{"/koryph-design", "/koryph-plan", "/koryph-import", "/koryph-issue"} {
				if !strings.Contains(payload.HookSpecificOutput.AdditionalContext, cmd) {
					t.Errorf("rubric doesn't name %s:\n%s", cmd, payload.HookSpecificOutput.AdditionalContext)
				}
			}
		})
	}
}

// TestSilentOnQuestions proves pure questions — no build/change/fix verbs —
// inject nothing: the common conversational case must stay byte-free (I3).
func TestSilentOnQuestions(t *testing.T) {
	prompts := []string{
		"why does the scheduler treat readers differently in the conflict check?",
		"what is the difference between footprints and resources here?",
		"where does the governor persist its per-account ceiling between runs?",
		// Intent vocabulary as NOUNS inside a question — the question-opener
		// suppressor must win, or every "the build"/"the bug" question pays
		// the rubric bytes.
		"why is the build slow on the ci runners this week, any ideas?",
		"how does the update path handle a missing config file today?",
		"does the feature flag system support percentage rollouts at all?",
	}
	for _, p := range prompts {
		out, _, code := runIntent(t, runIntentOpts{stdin: promptJSON(t, p)})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 for %q", code, p)
		}
		if out != "" {
			t.Errorf("question prompt %q injected output:\n%s", p, out)
		}
	}
}

// TestSilentOnRoutedPrefixes proves prompts that already route explicitly —
// slash commands, ! shell, # memory — never get the rubric, even when their
// text would otherwise match the intent heuristic.
func TestSilentOnRoutedPrefixes(t *testing.T) {
	prompts := []string{
		"/koryph-plan docs/designs/2026-07-example.md and then add the beads",
		"!make gate-agent && git status --short",
		"# remember that we build the exporter with the vendored toolchain",
	}
	for _, p := range prompts {
		out, _, code := runIntent(t, runIntentOpts{stdin: promptJSON(t, p)})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 for %q", code, p)
		}
		if out != "" {
			t.Errorf("prefixed prompt %q injected output:\n%s", p, out)
		}
	}
}

// TestSilentOnShortPrompts proves sub-24-char prompts inject nothing even
// when they carry an intent verb — too small to be a work description.
func TestSilentOnShortPrompts(t *testing.T) {
	out, _, code := runIntent(t, runIntentOpts{stdin: promptJSON(t, "fix the bug")})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("short prompt injected output:\n%s", out)
	}
}

// TestSilentInsideDispatch proves the rubric never fires for
// koryph-dispatched or secondary-spawn sessions (I2): their work already IS
// a bead, and the rubric would invite recursive planning.
func TestSilentInsideDispatch(t *testing.T) {
	intentPrompt := promptJSON(t, "add a retry loop around the flaky network fetch in the exporter")
	cases := []struct {
		name string
		opts runIntentOpts
	}{
		{"main dispatch (phase id)", runIntentOpts{stdin: intentPrompt, phaseID: "koryph-abc.1"}},
		{"secondary spawn (review)", runIntentOpts{stdin: intentPrompt, spawnKind: "review"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, code := runIntent(t, tc.opts)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if out != "" {
				t.Errorf("dispatch-session prompt injected output:\n%s", out)
			}
		})
	}
}

// TestFailOpenOnMalformedStdin proves unparsable hook input degrades to
// silence with exit 0 (I1) — never a blocked prompt.
func TestFailOpenOnMalformedStdin(t *testing.T) {
	for _, stdin := range []string{"not json at all {{{", ""} {
		out, _, code := runIntent(t, runIntentOpts{stdin: stdin})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (fail-open on stdin %q)", code, stdin)
		}
		if out != "" {
			t.Errorf("malformed stdin %q injected output:\n%s", stdin, out)
		}
	}
}

// TestFailOpenWhenJqMissing proves a machine without jq degrades to silence
// with exit 0 (I1) rather than a hook error on every prompt.
func TestFailOpenWhenJqMissing(t *testing.T) {
	out, _, code := runIntent(t, runIntentOpts{
		stdin:    promptJSON(t, "add a retry loop around the flaky network fetch in the exporter"),
		pathDirs: pathWithoutJq(t),
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (fail-open on missing jq)", code)
	}
	if out != "" {
		t.Errorf("missing-jq run injected output:\n%s", out)
	}
}
