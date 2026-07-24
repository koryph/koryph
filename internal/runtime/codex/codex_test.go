// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

func TestCodexConformsToSharedRuntimeContract(t *testing.T) {
	runtimetest.AssertConforms(t, Codex{Bin: "codex"}, runtimetest.ConformanceFixture{
		Dispatch: runtime.DispatchSpec{
			RepoRoot: "/repo", PhaseDir: "/phase", Model: "gpt-5.6-terra", Effort: "high",
			Profile:     runtime.Profile{ConfigDir: "/profiles/work"},
			SSHAuthSock: "/run/koryph-signing/signing.sock",
		},
		JSON: runtime.JSONSpec{
			RepoRoot: "/repo", Model: "gpt-5.6-terra", Effort: "high",
			Profile:     runtime.Profile{ConfigDir: "/profiles/work"},
			SSHAuthSock: "/run/koryph-signing/signing.sock",
		},
		Stream: "{\"type\":\"thread.started\",\"thread_id\":\"t1\"}\n",
	})
}

func TestCommandRendersSafeCodexExec(t *testing.T) {
	argv, _, err := (Codex{Bin: "codex"}).Command(runtime.DispatchSpec{
		RepoRoot: "/repo", PhaseDir: "/phase", Model: "gpt-5.6-terra", Effort: "high",
		Profile: runtime.Profile{ConfigDir: "/profiles/work"}, Billing: runtime.BillingSubscription,
		SSHAuthSock: "/run/koryph-signing/signing.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "--ask-for-approval", "never", "exec", "--json", "--sandbox", "workspace-write", "--dangerously-bypass-hook-trust", "--add-dir", "/phase", "--add-dir", "/repo/.git", "--add-dir", "/run/koryph-signing", "--model", "gpt-5.6-terra", "-c", `model_reasoning_effort="high"`, "--output-last-message", "/phase/SUMMARY.md"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %q\nwant = %q", argv, want)
	}
}

func TestCommandJSONAllowsOnlySharedGitMetadata(t *testing.T) {
	argv, _, err := (Codex{Bin: "codex"}).CommandJSON(runtime.JSONSpec{
		RepoRoot: "/repo", SSHAuthSock: "/run/koryph-signing/signing.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "--ask-for-approval", "never", "exec", "--sandbox", "workspace-write", "--dangerously-bypass-hook-trust", "--add-dir", "/repo/.git", "--add-dir", "/run/koryph-signing"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %q\\nwant = %q", argv, want)
	}
}

func TestCommandRejectsUnsupportedSafetySemantics(t *testing.T) {
	for _, spec := range []runtime.DispatchSpec{{ResumeSessionID: "thread"}, {MaxBudgetUSD: 1}} {
		if _, _, err := (Codex{}).Command(spec); err == nil {
			t.Errorf("Command(%+v) succeeded; want unsupported-capability error", spec)
		}
	}
}

func TestParseEventsNormalizesThreadUsageAndRateLimit(t *testing.T) {
	es, err := (Codex{}).ParseEvents(strings.NewReader("{\"type\":\"thread.started\",\"thread_id\":\"t1\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":4,\"output_tokens\":3}}\n{\"type\":\"error\",\"message\":\"HTTP 429 rate limit\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	first, ok, _ := es.Next()
	if !ok || first.Kind != runtime.EventSession || first.SessionID != "t1" {
		t.Fatalf("first event = %+v", first)
	}
	second, ok, _ := es.Next()
	if !ok || second.Kind != runtime.EventResult || !second.HasUsage || second.InputTokens != 10 || second.CacheReadTokens != 4 || second.OutputTokens != 3 {
		t.Fatalf("second event = %+v", second)
	}
	third, ok, _ := es.Next()
	if !ok || third.Kind != runtime.EventError || !third.RateLimited {
		t.Fatalf("third event = %+v", third)
	}
}

func TestRenderPersonaAndPromptUseCanonicalSource(t *testing.T) {
	rendered, err := (Codex{}).RenderPersona("p", []byte("---\nmodel: sonnet\n---\nFollow the protocol."))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(rendered); !strings.Contains(got, "developer_instructions") || strings.Contains(got, "model: sonnet") {
		t.Errorf("unexpected rendered persona:\n%s", got)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "p.md"), []byte("Persona instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt, err := (Codex{}).PreparePrompt(root, "p", "Do the task")
	if err != nil || !strings.Contains(prompt, "Persona instructions") || !strings.Contains(prompt, "Do the task") {
		t.Fatalf("prompt=%q err=%v", prompt, err)
	}
}
