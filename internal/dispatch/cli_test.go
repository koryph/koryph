// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/account"
)

// fakeClaude writes a shell script that records its argv and env into
// $KORYPH_DIR, emits one stream-json result line, and lingers briefly.
//
// Each capture file (argv.txt, env.txt, stdin.txt) is written atomically via a
// temp-file + mv so that waitForFile never reads a partial write under load.
// Without the atomic rename, printf(1) may emit each argument as a separate
// write(2) syscall, letting the Go reader observe a truncated file as soon as
// len(data) > 0 is satisfied by the very first line.
func fakeClaude(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$KORYPH_DIR/argv.txt.tmp" && mv "$KORYPH_DIR/argv.txt.tmp" "$KORYPH_DIR/argv.txt"
env > "$KORYPH_DIR/env.txt.tmp" && mv "$KORYPH_DIR/env.txt.tmp" "$KORYPH_DIR/env.txt"
cat > "$KORYPH_DIR/stdin.txt.tmp" && mv "$KORYPH_DIR/stdin.txt.tmp" "$KORYPH_DIR/stdin.txt"
printf '{"type":"result","total_cost_usd":1.23}\n'
sleep 0.2
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// claudeConfigDir writes a .claude.json reporting email and returns the dir.
func claudeConfigDir(t *testing.T, email string) string {
	t.Helper()
	dir := t.TempDir()
	body := `{"oauthAccount":{"emailAddress":"` + email + `","organizationName":"Test Org"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// baseSpec builds a dispatchable Spec rooted in fresh temp dirs.
func baseSpec(t *testing.T) Spec {
	t.Helper()
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := Spec{
		ProjectID:        "proj",
		RepoRoot:         root,
		RunID:            "run-20260702-0001",
		PhaseID:          "bead-42",
		PhaseDir:         filepath.Join(root, "run", "bead-42"),
		Worktree:         worktree,
		Branch:           "feat/bead-42",
		Persona:          "implementer",
		Model:            "sonnet",
		Effort:           "high",
		Profile:          account.Profile{Name: "work", ConfigDir: claudeConfigDir(t, "agent@example.com")},
		ExpectedIdentity: "agent@example.com",
		Billing:          account.BillingSubscription,
		MaxBudgetUSD:     25,
		Prompt:           "# Task\n\nDo the thing.\n",
		SessionID:        "0f6a2f5e-1111-4222-8333-944444444444",
		SessionName:      "koryph/proj/bead-42/a1",
		BeadsDir:         filepath.Join(root, ".beads"),
		Attempt:          1,
	}
	// Dispatch detaches (Setsid) and Release()s the fake claude, so it lingers —
	// still writing under PhaseDir — after the test body returns. Reap it before
	// this test's t.TempDir() cleanup runs (registered later ⇒ runs first, LIFO)
	// so RemoveAll cannot race the process and fail with "directory not empty".
	reapDetachedChildren(t)
	return spec
}

// reapDetachedChildren waits (bounded) for the test's detached child processes
// to exit and reaps them, mirroring the engine's own Wait4-based liveness probe
// for released agents. Runs as a cleanup so it precedes TempDir removal.
func reapDetachedChildren(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var ws syscall.WaitStatus
			wpid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if err == syscall.ECHILD {
				return // no children remain
			}
			if wpid == 0 {
				time.Sleep(5 * time.Millisecond) // alive, not yet exited
				continue
			}
			// reaped one; loop for any others
		}
	})
}

// waitForFile polls until path exists and is non-empty (deadline 5s).
func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return nil
}

func argvLines(data []byte) []string {
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func TestDispatchLaunchesDetachedAgent(t *testing.T) {
	// Polluted parent env must be scrubbed for the child.
	t.Setenv("ANTHROPIC_API_KEY", "sk-polluted-parent")
	t.Setenv("CLAUDE_CONFIG_DIR", "/polluted/dir")

	spec := baseSpec(t)
	b := CLIBackend{ClaudeBin: fakeClaude(t)}
	h, err := b.Dispatch(context.Background(), spec)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if h.PID <= 0 {
		t.Errorf("Handle.PID = %d", h.PID)
	}
	if h.SessionID != spec.SessionID {
		t.Errorf("Handle.SessionID = %q", h.SessionID)
	}
	if h.VerifiedIdentity != "agent@example.com" {
		t.Errorf("Handle.VerifiedIdentity = %q", h.VerifiedIdentity)
	}

	// launch.sh: exists, 0755, prescribed shape.
	info, err := os.Stat(h.LaunchPath)
	if err != nil {
		t.Fatalf("launch.sh: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("launch.sh mode = %o, want 755", perm)
	}
	launch := string(waitForFile(t, h.LaunchPath))
	if !strings.HasPrefix(launch, "#!/bin/sh\n") {
		t.Errorf("launch.sh missing shebang:\n%s", launch)
	}
	if !strings.Contains(launch, "cd '"+spec.Worktree+"' || exit 97") {
		t.Errorf("launch.sh missing cd guard:\n%s", launch)
	}
	for _, want := range []string{
		"KORYPH_RUN_ID='" + spec.RunID + "'",
		"KORYPH_PHASE_ID='" + spec.PhaseID + "'",
		"KORYPH_DIR='" + spec.PhaseDir + "'",
		"KORYPH_LOG_PATH='" + filepath.Join(spec.PhaseDir, "session.log") + "'",
		"KORYPH_STATUS_PATH='" + filepath.Join(spec.PhaseDir, "status.json") + "'",
		"KORYPH_SUMMARY_PATH='" + filepath.Join(spec.PhaseDir, "SUMMARY.md") + "'",
		"KORYPH_SESSION_ID='" + spec.SessionID + "'",
		"BEADS_DIR='" + spec.BeadsDir + "'",
	} {
		if !strings.Contains(launch, want) {
			t.Errorf("launch.sh missing %q:\n%s", want, launch)
		}
	}

	// prompt.md + seeded status.json + INBOX.md.
	prompt, err := os.ReadFile(filepath.Join(spec.PhaseDir, "prompt.md"))
	if err != nil || string(prompt) != spec.Prompt {
		t.Errorf("prompt.md = %q, %v", prompt, err)
	}
	var status struct {
		PhaseID   string `json:"phase_id"`
		State     string `json:"state"`
		Step      string `json:"step"`
		Pct       int    `json:"pct"`
		UpdatedAt string `json:"updated_at"`
	}
	raw, err := os.ReadFile(h.StatusPath)
	if err != nil {
		t.Fatalf("status.json: %v", err)
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("status.json parse: %v", err)
	}
	if status.PhaseID != spec.PhaseID || status.State != "queued" || status.Step != "dispatched" || status.Pct != 0 {
		t.Errorf("status.json seed = %+v", status)
	}
	if _, err := time.Parse(time.RFC3339, status.UpdatedAt); err != nil {
		t.Errorf("status.json updated_at %q not RFC3339: %v", status.UpdatedAt, err)
	}
	inbox, err := os.ReadFile(filepath.Join(spec.PhaseDir, "INBOX.md"))
	if err != nil || string(inbox) != "(operator nudges appear here)\n" {
		t.Errorf("INBOX.md = %q, %v", inbox, err)
	}

	// argv: exact flag sequence.
	argv := argvLines(waitForFile(t, filepath.Join(spec.PhaseDir, "argv.txt")))
	want := []string{
		"-p",
		"--agent", "implementer",
		"--session-id", spec.SessionID,
		"--permission-mode", "dontAsk",
		"--model", "sonnet",
		"--effort", "high",
		"--max-budget-usd", "25",
		"--fallback-model", "sonnet",
		"--name", spec.SessionName,
		"--add-dir", spec.PhaseDir,
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv =\n%q\nwant\n%q", argv, want)
	}

	// stdin: the prompt is fed on stdin.
	stdin := waitForFile(t, filepath.Join(spec.PhaseDir, "stdin.txt"))
	if string(stdin) != spec.Prompt {
		t.Errorf("stdin = %q, want prompt", stdin)
	}

	// env: subscription billing → ANTHROPIC_API_KEY scrubbed even though the
	// parent is polluted; CLAUDE_CONFIG_DIR points at the work profile.
	env := string(waitForFile(t, filepath.Join(spec.PhaseDir, "env.txt")))
	if strings.Contains(env, "ANTHROPIC_API_KEY=") {
		t.Errorf("child env leaks ANTHROPIC_API_KEY under subscription billing:\n%s", env)
	}
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR="+spec.Profile.ConfigDir+"\n") {
		t.Errorf("child env missing work CLAUDE_CONFIG_DIR:\n%s", env)
	}
	if strings.Contains(env, "CLAUDE_CONFIG_DIR=/polluted/dir") {
		t.Errorf("polluted CLAUDE_CONFIG_DIR leaked:\n%s", env)
	}
	for _, kv := range []string{
		"KORYPH_RUN_ID=" + spec.RunID,
		"KORYPH_PHASE_ID=" + spec.PhaseID,
		"KORYPH_SESSION_ID=" + spec.SessionID,
		"BEADS_DIR=" + spec.BeadsDir,
	} {
		if !strings.Contains(env, kv+"\n") {
			t.Errorf("child env missing %q", kv)
		}
	}

	// ParseResultCost reads the fake's result line off stream.jsonl.
	waitForFile(t, h.StreamPath)
	cost, ok := ParseResultCost(h.StreamPath)
	if !ok || cost != 1.23 {
		t.Errorf("ParseResultCost = %v, %v; want 1.23, true", cost, ok)
	}

	if _, err := os.Stat(h.StderrPath); err != nil {
		t.Errorf("stderr.log: %v", err)
	}
}

// TestDispatchThreadsProxyBaseURL is the koryph-3l1.1 main-dispatch
// end-to-end acceptance test: Spec.ProxyBaseURL flows through toRuntimeSpec
// and the claude adapter's Command into the actually-spawned agent's real
// child env as ANTHROPIC_BASE_URL. Main dispatch never sets SpawnKind, so
// KORYPH_SPAWN_KIND must stay absent regardless.
func TestDispatchThreadsProxyBaseURL(t *testing.T) {
	spec := baseSpec(t)
	spec.ProxyBaseURL = "http://127.0.0.1:8091"
	b := CLIBackend{ClaudeBin: fakeClaude(t)}
	if _, err := b.Dispatch(context.Background(), spec); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	env := string(waitForFile(t, filepath.Join(spec.PhaseDir, "env.txt")))
	if !strings.Contains(env, "ANTHROPIC_BASE_URL=http://127.0.0.1:8091\n") {
		t.Errorf("child env missing ANTHROPIC_BASE_URL:\n%s", env)
	}
	if strings.Contains(env, "KORYPH_SPAWN_KIND=") {
		t.Errorf("child env has KORYPH_SPAWN_KIND set for main dispatch, want absent:\n%s", env)
	}
}

// TestDispatchOmitsProxyBaseURLByDefault is the I6 zero-residue guarantee at
// the main-dispatch integration level: a Spec that never touches
// ProxyBaseURL produces a child env with no ANTHROPIC_BASE_URL at all.
func TestDispatchOmitsProxyBaseURLByDefault(t *testing.T) {
	spec := baseSpec(t)
	b := CLIBackend{ClaudeBin: fakeClaude(t)}
	if _, err := b.Dispatch(context.Background(), spec); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	env := string(waitForFile(t, filepath.Join(spec.PhaseDir, "env.txt")))
	if strings.Contains(env, "ANTHROPIC_BASE_URL=") {
		t.Errorf("child env has ANTHROPIC_BASE_URL with ProxyBaseURL unset, want absent:\n%s", env)
	}
}

func TestDispatchResumeAndAPIKeyEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-polluted-parent")

	spec := baseSpec(t)
	spec.Billing = account.BillingAPIKey
	spec.APIKey = "sk-explicit-batch-key"
	spec.ResumeSessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	b := CLIBackend{ClaudeBin: fakeClaude(t)}
	if _, err := b.Dispatch(context.Background(), spec); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	argv := argvLines(waitForFile(t, filepath.Join(spec.PhaseDir, "argv.txt")))
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--resume "+spec.ResumeSessionID+" --fork-session") {
		t.Errorf("argv missing resume+fork-session: %q", joined)
	}
	// --resume sits between --name and --add-dir per the prescribed layout.
	if !strings.Contains(joined, "--name "+spec.SessionName+" --resume") {
		t.Errorf("argv resume not after --name: %q", joined)
	}

	env := string(waitForFile(t, filepath.Join(spec.PhaseDir, "env.txt")))
	if !strings.Contains(env, "ANTHROPIC_API_KEY=sk-explicit-batch-key\n") {
		t.Errorf("child env missing explicit API key under api-key billing:\n%s", env)
	}
	if strings.Contains(env, "sk-polluted-parent") {
		t.Errorf("polluted parent key leaked:\n%s", env)
	}
}

func TestDispatchOmitsOptionalFlags(t *testing.T) {
	spec := baseSpec(t)
	spec.Effort = ""
	spec.MaxBudgetUSD = 0
	spec.SessionName = ""

	b := CLIBackend{ClaudeBin: fakeClaude(t)}
	if _, err := b.Dispatch(context.Background(), spec); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	argv := argvLines(waitForFile(t, filepath.Join(spec.PhaseDir, "argv.txt")))
	joined := strings.Join(argv, " ")
	for _, absent := range []string{"--effort", "--max-budget-usd", "--name", "--resume", "--fork-session"} {
		if strings.Contains(joined, absent) {
			t.Errorf("argv contains %q which should be omitted: %q", absent, joined)
		}
	}
}

func TestDispatchAPIKeyBillingRequiresKey(t *testing.T) {
	spec := baseSpec(t)
	spec.Billing = account.BillingAPIKey
	spec.APIKey = ""
	b := CLIBackend{ClaudeBin: fakeClaude(t)}
	if _, err := b.Dispatch(context.Background(), spec); err == nil {
		t.Fatal("Dispatch succeeded with api-key billing and empty key; must fail closed")
	}
	if _, err := os.Stat(spec.PhaseDir); !os.IsNotExist(err) {
		t.Errorf("PhaseDir created despite billing refusal: %v", err)
	}
}

func TestDispatchInvalidBillingMode(t *testing.T) {
	spec := baseSpec(t)
	spec.Billing = ""
	b := CLIBackend{ClaudeBin: fakeClaude(t)}
	if _, err := b.Dispatch(context.Background(), spec); err == nil {
		t.Fatal("Dispatch succeeded with empty billing mode; must fail closed")
	}
}

func TestDispatchRejectsSingleQuotePaths(t *testing.T) {
	b := CLIBackend{ClaudeBin: fakeClaude(t)}

	spec := baseSpec(t)
	spec.PhaseDir = filepath.Join(filepath.Dir(spec.PhaseDir), "it's-a-phase")
	if _, err := b.Dispatch(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "single quote") {
		t.Fatalf("Dispatch with quoted phase dir: err = %v, want single-quote rejection", err)
	}
	if _, err := os.Stat(spec.PhaseDir); !os.IsNotExist(err) {
		t.Errorf("PhaseDir created despite quote rejection: %v", err)
	}

	spec2 := baseSpec(t)
	spec2.Worktree = spec2.Worktree + "-o'clock"
	if _, err := b.Dispatch(context.Background(), spec2); err == nil || !strings.Contains(err.Error(), "single quote") {
		t.Fatalf("Dispatch with quoted worktree: err = %v, want single-quote rejection", err)
	}
}

func TestDispatchIdentityMismatchWritesNothing(t *testing.T) {
	spec := baseSpec(t)
	spec.ExpectedIdentity = "someone-else@example.com"
	b := CLIBackend{ClaudeBin: fakeClaude(t)}
	_, err := b.Dispatch(context.Background(), spec)
	if err == nil {
		t.Fatal("Dispatch succeeded despite identity mismatch; must fail closed")
	}
	if !strings.Contains(err.Error(), "account mismatch") {
		t.Errorf("err = %v, want account mismatch", err)
	}
	if _, statErr := os.Stat(spec.PhaseDir); !os.IsNotExist(statErr) {
		t.Errorf("PhaseDir created despite identity refusal (stat err %v)", statErr)
	}
}

func TestDispatchPreservesExistingInbox(t *testing.T) {
	spec := baseSpec(t)
	if err := os.MkdirAll(spec.PhaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := "operator note: keep going\n"
	if err := os.WriteFile(filepath.Join(spec.PhaseDir, "INBOX.md"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	b := CLIBackend{ClaudeBin: fakeClaude(t)}
	if _, err := b.Dispatch(context.Background(), spec); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(spec.PhaseDir, "INBOX.md"))
	if err != nil || string(got) != custom {
		t.Errorf("INBOX.md overwritten: %q, %v", got, err)
	}
}

func TestParseResultCost(t *testing.T) {
	t.Run("last result line wins, is_error irrelevant", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stream.jsonl")
		lines := strings.Join([]string{
			`{"type":"system","subtype":"init"}`,
			`{"type":"result","total_cost_usd":0.5,"is_error":false}`,
			`not json at all`,
			`{"type":"assistant","message":{}}`,
			`{"type":"result","total_cost_usd":2.5,"is_error":true}`,
		}, "\n") + "\n"
		if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
			t.Fatal(err)
		}
		cost, ok := ParseResultCost(path)
		if !ok || cost != 2.5 {
			t.Errorf("ParseResultCost = %v, %v; want 2.5, true", cost, ok)
		}
	})

	t.Run("no result line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stream.jsonl")
		if err := os.WriteFile(path, []byte(`{"type":"system"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if cost, ok := ParseResultCost(path); ok || cost != 0 {
			t.Errorf("ParseResultCost = %v, %v; want 0, false", cost, ok)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if cost, ok := ParseResultCost(filepath.Join(t.TempDir(), "nope.jsonl")); ok || cost != 0 {
			t.Errorf("ParseResultCost = %v, %v; want 0, false", cost, ok)
		}
	})
}

func TestParseResultUsage(t *testing.T) {
	t.Run("usage present", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stream.jsonl")
		lines := strings.Join([]string{
			`{"type":"system","subtype":"init"}`,
			`{"type":"result","total_cost_usd":0.124317,"is_error":false,` +
				`"usage":{"input_tokens":3861,"output_tokens":17,"cache_read_input_tokens":15837,"cache_creation_input_tokens":3451}}`,
		}, "\n") + "\n"
		if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
			t.Fatal(err)
		}
		usage, ok := ParseResultUsage(path)
		want := TokenUsage{InputTokens: 3861, OutputTokens: 17, CacheReadTokens: 15837, CacheCreationTokens: 3451}
		if !ok || usage != want {
			t.Errorf("ParseResultUsage = %+v, %v; want %+v, true", usage, ok, want)
		}
	})

	t.Run("usage absent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stream.jsonl")
		if err := os.WriteFile(path, []byte(`{"type":"result","total_cost_usd":1.23}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if usage, ok := ParseResultUsage(path); ok || usage != (TokenUsage{}) {
			t.Errorf("ParseResultUsage = %+v, %v; want zero value, false", usage, ok)
		}
	})

	t.Run("is_error result still carries usage", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stream.jsonl")
		line := `{"type":"result","is_error":true,"total_cost_usd":0.05,` +
			`"usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":50,"cache_creation_input_tokens":10}}`
		if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		usage, ok := ParseResultUsage(path)
		want := TokenUsage{InputTokens: 100, OutputTokens: 5, CacheReadTokens: 50, CacheCreationTokens: 10}
		if !ok || usage != want {
			t.Errorf("ParseResultUsage = %+v, %v; want %+v, true", usage, ok, want)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if usage, ok := ParseResultUsage(filepath.Join(t.TempDir(), "nope.jsonl")); ok || usage != (TokenUsage{}) {
			t.Errorf("ParseResultUsage = %+v, %v; want zero value, false", usage, ok)
		}
	})
}

func TestParseRateLimited(t *testing.T) {
	writeStream := func(t *testing.T, lines ...string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "stream.jsonl")
		body := strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	positive := map[string]string{
		"top-level error event, rate_limit_error":               `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your rate limit."}}`,
		"top-level error event, 429 in message":                 `{"type":"error","message":"HTTP 429 Too Many Requests"}`,
		"result is_error true, embedded error object":           `{"type":"result","is_error":true,"subtype":"error_during_execution","result":"API Error: 429 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}"}`,
		"result is_error true, overloaded_error in result text": `{"type":"result","is_error":true,"subtype":"error","result":"overloaded_error: the service is temporarily overloaded"}`,
	}
	for name, line := range positive {
		t.Run("positive/"+name, func(t *testing.T) {
			path := writeStream(t,
				`{"type":"system","subtype":"init"}`,
				line,
			)
			if !ParseRateLimited(path) {
				t.Errorf("ParseRateLimited(%q) = false, want true", line)
			}
		})
	}

	negative := map[string][]string{
		"ordinary max-turns error": {
			`{"type":"result","is_error":true,"subtype":"error_max_turns","result":"Max turns reached"}`,
		},
		"clean success result": {
			`{"type":"result","total_cost_usd":1.23,"is_error":false}`,
		},
		"429 mentioned in ordinary (non-error) assistant text": {
			`{"type":"assistant","message":{"content":[{"type":"text","text":"the API returned 429 once but retried fine"}]}}`,
			`{"type":"result","total_cost_usd":0.10,"is_error":false}`,
		},
		"garbage lines and no result": {
			`not json at all`,
			`{"type":"system"}`,
		},
	}
	for name, lines := range negative {
		t.Run("negative/"+name, func(t *testing.T) {
			path := writeStream(t, lines...)
			if ParseRateLimited(path) {
				t.Errorf("ParseRateLimited() = true for %v, want false", lines)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if ParseRateLimited(filepath.Join(t.TempDir(), "nope.jsonl")) {
			t.Error("ParseRateLimited(missing file) = true, want false")
		}
	})
}

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("Alive(self) = false")
	}
	if Alive(0) || Alive(-5) {
		t.Error("Alive of non-positive pid must be false")
	}

	// Spawn and fully reap a child; its pid is then dead.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if Alive(pid) {
		t.Errorf("Alive(reaped child %d) = true, want false", pid)
	}
}

func TestStopGraceful(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := StopGraceful(pid); err != nil {
		t.Fatalf("StopGraceful: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("Wait: %v (want SIGTERM exit)", err)
		}
		ws, ok := ee.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
			t.Errorf("child not terminated by SIGTERM: %v", ee)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child did not exit after StopGraceful (SIGTERM)")
	}
}

func TestStopForce(t *testing.T) {
	// A child that ignores SIGTERM must still die under StopForce (SIGKILL).
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := StopForce(pid); err != nil {
		t.Fatalf("StopForce: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("Wait: %v (want SIGKILL exit)", err)
		}
		ws, ok := ee.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Errorf("child not killed by SIGKILL: %v", ee)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child survived StopForce (SIGKILL)")
	}
}

func TestStopForceInvalidPID(t *testing.T) {
	if err := StopForce(0); err == nil {
		t.Error("StopForce(0) should error")
	}
}
