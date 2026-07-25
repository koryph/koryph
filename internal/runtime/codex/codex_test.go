// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"os"
	"os/exec"
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
	want := []string{
		"codex", "--ask-for-approval", "never", "exec", "--json",
		"--ignore-user-config",
		"-c", `default_permissions="koryph_signing"`,
		"-c", signingFilesystemRule("/repo"),
		"-c", "permissions.koryph_signing.network.enabled=true",
		"-c", unixSocketRule(append([]string{"/run/koryph-signing/signing.sock"}, testAgentSockets("/phase")...)...),
		"--dangerously-bypass-hook-trust", "--add-dir", "/phase", "--add-dir", "/repo/.git",
		"--model", "gpt-5.6-terra", "-c", `model_reasoning_effort="high"`,
		"--output-last-message", "/phase/SUMMARY.md",
	}
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
	want := []string{
		"codex", "--ask-for-approval", "never", "exec",
		"--ignore-user-config",
		"-c", `default_permissions="koryph_signing"`,
		"-c", signingFilesystemRule("/repo"),
		"-c", "permissions.koryph_signing.network.enabled=true",
		"-c", unixSocketRule("/run/koryph-signing/signing.sock"),
		"--dangerously-bypass-hook-trust", "--add-dir", "/repo/.git",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %q\\nwant = %q", argv, want)
	}
}

func TestSigningFilesystemRuleKeepsWritesScopedAndToolchainsReadable(t *testing.T) {
	t.Setenv("PATH", "/nix/store/tool/bin:/opt/homebrew/bin:/usr/bin")
	t.Setenv("HOMEBREW_PREFIX", "/opt/homebrew")
	bundle := filepath.Join(t.TempDir(), "ca-bundle.crt")
	if err := os.WriteFile(bundle, []byte("test certificate bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NIX_SSL_CERT_FILE", bundle)
	t.Setenv("CURL_CA_BUNDLE", filepath.Join(t.TempDir(), "missing-ca.pem"))
	rule := signingFilesystemRule("/repo")
	for _, want := range []string{
		`":workspace_roots"={"."="write"}`,
		`":tmpdir"="write"`,
		`"/nix/store"="read"`,
		`"/opt/homebrew"="read"`,
		`"/usr/bin"="read"`,
		`"~/.gitconfig"="read"`,
		`"~/.cache/pre-commit"="write"`,
		`"/repo/.beads/hooks"="read"`,
		`"/repo/.allowed_signers"="read"`,
		tomlString(bundle) + `="read"`,
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("rule = %q, missing %q", rule, want)
		}
	}
	if strings.Contains(rule, `":root"="read"`) {
		t.Fatalf("rule = %q, must not grant broad root reads", rule)
	}
	if strings.Contains(rule, "missing-ca.pem") {
		t.Fatalf("rule = %q, must not grant nonexistent bundle paths", rule)
	}
}

func TestCommandWithoutSigningKeepsWorkspaceWriteSandbox(t *testing.T) {
	argv, _, err := (Codex{Bin: "codex"}).Command(runtime.DispatchSpec{
		RepoRoot: "/repo", PhaseDir: "/phase",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--sandbox workspace-write") {
		t.Fatalf("argv = %q, want ordinary workspace-write sandbox", argv)
	}
	if strings.Contains(joined, "koryph_signing") {
		t.Fatalf("argv = %q, signing profile must be absent without a scoped socket", argv)
	}
}

func TestCommandSigningCachesAreNarrowlyScoped(t *testing.T) {
	_, env, err := (Codex{Bin: "codex"}).Command(runtime.DispatchSpec{
		RepoRoot: "/repo", PhaseDir: "/phase", SSHAuthSock: "/signing/socket",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"PRE_COMMIT_HOME=" + filepath.Join(os.Getenv("HOME"), ".cache", "pre-commit"),
		"GOCACHE=/phase/go-cache",
		"GOMODCACHE=/phase/go-mod-cache",
		"TEST_TELEMETRY_DIR=/phase/go-telemetry",
		"XDG_CACHE_HOME=/phase/cache",
		"TMPDIR=/phase",
		testAgentSocketsEnv + "=" + strings.Join(testAgentSockets("/phase"), string(filepath.ListSeparator)),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q:\n%s", want, joined)
		}
	}
}

func TestSigningSocketRuleKeepsProductionAndTestSocketsExact(t *testing.T) {
	production := "/run/koryph-signing/signing.sock"
	rule := unixSocketRule(append([]string{production}, testAgentSockets("/phase")...)...)
	if !strings.Contains(rule, tomlString(production)+`="allow"`) {
		t.Fatalf("rule = %q, missing exact production socket", rule)
	}
	for _, socket := range testAgentSockets("/phase") {
		if !strings.Contains(rule, tomlString(socket)+`="allow"`) {
			t.Errorf("rule = %q, missing exact test socket %q", rule, socket)
		}
	}
	if strings.Contains(rule, "dangerously_allow_all_unix_sockets") {
		t.Fatalf("rule = %q, must not enable all Unix sockets", rule)
	}
}

func TestCommandKeepsTestSocketsShortForDeepPhaseDir(t *testing.T) {
	phaseDir := "/private/tmp/" + strings.Repeat("deep-phase/", 16)
	argv, _, err := (Codex{Bin: "codex"}).Command(runtime.DispatchSpec{
		RepoRoot: "/repo", PhaseDir: phaseDir, SSHAuthSock: "/run/koryph-signing/signing.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	var rule string
	for _, arg := range argv {
		if strings.HasPrefix(arg, "permissions.koryph_signing.network.unix_sockets=") {
			rule = arg
			break
		}
	}
	if rule == "" {
		t.Fatal("rendered dispatch lacks Unix-socket permission rule")
	}
	for _, socket := range testAgentSockets(phaseDir) {
		if len(socket) >= 104 {
			t.Errorf("test socket = %q, exceeds macOS socket path limit", socket)
		}
		if !strings.Contains(rule, tomlString(socket)+`="allow"`) {
			t.Errorf("rendered dispatch lacks exact socket allowlist entry %q", socket)
		}
	}
	if strings.Contains(rule, phaseDir) {
		t.Errorf("rendered dispatch leaked deep phase path into socket rule: %q", rule)
	}
}

func TestCommandWithoutSigningStillUsesPhaseLocalMutableCaches(t *testing.T) {
	_, env, err := (Codex{Bin: "codex"}).Command(runtime.DispatchSpec{RepoRoot: "/repo", PhaseDir: "/phase"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"GOCACHE=/phase/go-cache",
		"GOMODCACHE=/phase/go-mod-cache",
		"TEST_TELEMETRY_DIR=/phase/go-telemetry",
		"XDG_CACHE_HOME=/phase/cache",
		"TMPDIR=/phase",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "PRE_COMMIT_HOME=") {
		t.Errorf("ordinary workspace-write launch inherited signing-only PRE_COMMIT_HOME:\n%s", joined)
	}
}

func TestCommandJSONUsesScratchLocalMutableCaches(t *testing.T) {
	_, env, err := (Codex{Bin: "codex"}).CommandJSON(runtime.JSONSpec{RepoRoot: "/repo", ScratchDir: "/scratch"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"GOCACHE=/scratch/go-cache", "GOMODCACHE=/scratch/go-mod-cache", "TMPDIR=/scratch", "TEST_TELEMETRY_DIR=/scratch/go-telemetry"} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q:\n%s", want, joined)
		}
	}
}

func TestSandboxCacheEnvRedirectsGoTelemetryForActualSubprocess(t *testing.T) {
	scratch := t.TempDir()
	cmd := exec.Command("go", "env", "GOTELEMETRYDIR")
	// Go may launch a detached telemetry sidecar that outlives this command and
	// races TempDir cleanup. Mark this assertion subprocess as a sidecar child
	// so it reports the configured telemetry directory without launching one.
	cmd.Env = append(os.Environ(), "GO_TELEMETRY_CHILD=2")
	cmd.Env = append(cmd.Env, sandboxCacheEnv("", scratch)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go env GOTELEMETRYDIR: %v\n%s", err, output)
	}
	if got, want := strings.TrimSpace(string(output)), filepath.Join(scratch, "go-telemetry"); got != want {
		t.Errorf("go telemetry directory = %q, want %q", got, want)
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
