// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
)

// goldenSpec mirrors internal/dispatch/cli_test.go's baseSpec fixture
// field-for-field (converted to runtime.DispatchSpec), so this package's
// Command output can be checked against the EXACT pre-koryph-v8u.2 argv
// dispatch/cli_test.go's TestDispatchLaunchesDetachedAgent captured from a
// real (fake) claude invocation — the golden data proving the extraction is
// byte-identical.
func goldenSpec() runtime.DispatchSpec {
	return runtime.DispatchSpec{
		ProjectID:        "proj",
		RepoRoot:         "/repo",
		RunID:            "run-20260702-0001",
		PhaseID:          "bead-42",
		PhaseDir:         "/repo/run/bead-42",
		Worktree:         "/repo/run/bead-42/worktree",
		Branch:           "feat/bead-42",
		Persona:          "implementer",
		Model:            "sonnet",
		Effort:           "high",
		Profile:          runtime.Profile{Name: "work", ConfigDir: "/cfg/work"},
		ExpectedIdentity: "agent@example.com",
		Billing:          runtime.BillingSubscription,
		MaxBudgetUSD:     25,
		Prompt:           "# Task\n\nDo the thing.\n",
		SessionID:        "0f6a2f5e-1111-4222-8333-944444444444",
		SessionName:      "koryph/proj/bead-42/a1",
		BeadsDir:         "/repo/.beads",
		Attempt:          1,
	}
}

// TestCommandArgvMatchesPreRefactorGolden pins Command's argv to the EXACT
// flag sequence internal/dispatch/cli_test.go's TestDispatchLaunchesDetachedAgent
// observed from CLIBackend.Dispatch before this extraction (koryph-v8u.2's
// acceptance criterion: "a dispatched run produces identical launch.sh argv
// to pre-refactor").
func TestCommandArgvMatchesPreRefactorGolden(t *testing.T) {
	rt := Claude{Bin: "claude"}
	argv, _, err := rt.Command(goldenSpec())
	if err != nil {
		t.Fatalf("Command: %v", err)
	}

	want := []string{
		"claude",
		"-p",
		"--agent", "implementer",
		"--session-id", "0f6a2f5e-1111-4222-8333-944444444444",
		"--permission-mode", "dontAsk",
		"--model", "sonnet",
		"--effort", "high",
		"--max-budget-usd", "25",
		"--fallback-model", "sonnet",
		"--name", "koryph/proj/bead-42/a1",
		"--add-dir", "/repo/run/bead-42",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv =\n%q\nwant\n%q", argv, want)
	}
}

// TestCommandArgvResumeAndFallbackBin mirrors
// dispatch/cli_test.go's TestDispatchResumeAndAPIKeyEnv argv assertions:
// --resume/--fork-session sits between --name and --add-dir, and an empty
// Bin resolves to the "claude" default.
func TestCommandArgvResumeAndFallbackBin(t *testing.T) {
	spec := goldenSpec()
	spec.ResumeSessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	rt := Claude{} // zero value: Bin defaults to "claude"
	argv, _, err := rt.Command(spec)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if argv[0] != "claude" {
		t.Errorf("argv[0] = %q, want default bin %q", argv[0], "claude")
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--resume "+spec.ResumeSessionID+" --fork-session") {
		t.Errorf("argv missing resume+fork-session: %q", joined)
	}
	if !strings.Contains(joined, "--name "+spec.SessionName+" --resume") {
		t.Errorf("argv resume not immediately after --name: %q", joined)
	}
}

// TestCommandArgvStrictMCP pins koryph-kwv: StrictMCP=true adds
// --strict-mcp-config immediately before --add-dir so the dispatched agent
// loads no ambient MCP servers; the default (false) leaves argv byte-identical.
func TestCommandArgvStrictMCP(t *testing.T) {
	rt := Claude{Bin: "claude"}

	base, _, err := rt.Command(goldenSpec())
	if err != nil {
		t.Fatalf("Command (default): %v", err)
	}
	if strings.Contains(strings.Join(base, " "), "--strict-mcp-config") {
		t.Errorf("default argv unexpectedly contains --strict-mcp-config: %q", base)
	}

	spec := goldenSpec()
	spec.StrictMCP = true
	argv, _, err := rt.Command(spec)
	if err != nil {
		t.Fatalf("Command (strict): %v", err)
	}
	if joined := strings.Join(argv, " "); !strings.Contains(joined, "--strict-mcp-config --add-dir "+spec.PhaseDir) {
		t.Errorf("strict argv missing --strict-mcp-config before --add-dir: %q", joined)
	}
}

// TestCommandArgvOmitsOptionalFlags mirrors
// dispatch/cli_test.go's TestDispatchOmitsOptionalFlags.
func TestCommandArgvOmitsOptionalFlags(t *testing.T) {
	spec := goldenSpec()
	spec.Effort = ""
	spec.MaxBudgetUSD = 0
	spec.SessionName = ""

	rt := Claude{Bin: "claude"}
	argv, _, err := rt.Command(spec)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(argv, " ")
	for _, absent := range []string{"--effort", "--max-budget-usd", "--name", "--resume", "--fork-session"} {
		if strings.Contains(joined, absent) {
			t.Errorf("argv contains %q which should be omitted: %q", absent, joined)
		}
	}
}

// TestCommandJSONReviewerArgvGolden pins CommandJSON's argv for a read-only
// reviewer/epic-validator spawn to the EXACT flag sequence internal/review and
// internal/epicreview hand-built before the koryph-fiv seam: plan mode, an
// --effort hint, no fallback-model, no max-budget, --output-format json.
func TestCommandJSONReviewerArgvGolden(t *testing.T) {
	rt := Claude{Bin: "claude"}
	argv, _, err := rt.CommandJSON(runtime.JSONSpec{
		Persona:        "koryph-security-reviewer",
		Model:          "opus",
		Effort:         "high",
		PermissionMode: "plan",
		SpawnKind:      "review",
		Billing:        runtime.BillingSubscription,
	})
	if err != nil {
		t.Fatalf("CommandJSON: %v", err)
	}
	want := []string{
		"claude",
		"-p",
		"--agent", "koryph-security-reviewer",
		"--permission-mode", "plan",
		"--model", "opus",
		"--effort", "high",
		"--output-format", "json",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv =\n%q\nwant\n%q", argv, want)
	}
}

// TestCommandJSONStageArgvGolden pins CommandJSON's argv for a mutating stage
// spawn to the EXACT flag sequence internal/stage hand-built before the seam:
// dontAsk mode, --max-budget-usd, --fallback-model sonnet, --output-format
// json.
func TestCommandJSONStageArgvGolden(t *testing.T) {
	rt := Claude{Bin: "claude"}
	argv, _, err := rt.CommandJSON(runtime.JSONSpec{
		Persona:        "koryph-test-engineer",
		Model:          "sonnet",
		MaxBudgetUSD:   25,
		PermissionMode: "dontAsk",
		Fallback:       true,
		SpawnKind:      "stage",
		Billing:        runtime.BillingSubscription,
	})
	if err != nil {
		t.Fatalf("CommandJSON: %v", err)
	}
	want := []string{
		"claude",
		"-p",
		"--agent", "koryph-test-engineer",
		"--permission-mode", "dontAsk",
		"--model", "sonnet",
		"--max-budget-usd", "25",
		"--fallback-model", "sonnet",
		"--output-format", "json",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv =\n%q\nwant\n%q", argv, want)
	}
}

// TestCommandJSONDefaultsPermissionModeToPlan confirms an unset PermissionMode
// falls back to the safe read-only "plan" posture, never a mutating mode.
func TestCommandJSONDefaultsPermissionModeToPlan(t *testing.T) {
	rt := Claude{Bin: "claude"}
	argv, _, err := rt.CommandJSON(runtime.JSONSpec{Persona: "p", Model: "opus"})
	if err != nil {
		t.Fatalf("CommandJSON: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--permission-mode plan") {
		t.Errorf("empty PermissionMode did not default to plan: %q", joined)
	}
	for _, absent := range []string{"--effort", "--max-budget-usd", "--fallback-model"} {
		if strings.Contains(joined, absent) {
			t.Errorf("argv contains %q which should be omitted: %q", absent, joined)
		}
	}
}

// TestCommandEnvSubscriptionOmitsAPIKey mirrors the env assertions from
// dispatch/cli_test.go's TestDispatchLaunchesDetachedAgent: subscription
// billing never carries ANTHROPIC_API_KEY, and a work profile's ConfigDir
// surfaces as CLAUDE_CONFIG_DIR.
func TestCommandEnvSubscriptionOmitsAPIKey(t *testing.T) {
	rt := Claude{Bin: "claude"}
	_, env, err := rt.Command(goldenSpec())
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ANTHROPIC_API_KEY=") {
		t.Errorf("env leaks ANTHROPIC_API_KEY under subscription billing:\n%s", joined)
	}
	if !strings.Contains(joined, "CLAUDE_CONFIG_DIR=/cfg/work") {
		t.Errorf("env missing work CLAUDE_CONFIG_DIR:\n%s", joined)
	}
}

// TestCommandEnvAPIKeyBilling mirrors
// dispatch/cli_test.go's TestDispatchResumeAndAPIKeyEnv env assertions.
func TestCommandEnvAPIKeyBilling(t *testing.T) {
	spec := goldenSpec()
	spec.Billing = runtime.BillingAPIKey
	spec.APIKey = "sk-explicit-batch-key"

	rt := Claude{Bin: "claude"}
	_, env, err := rt.Command(spec)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=sk-explicit-batch-key") {
		t.Errorf("env missing explicit API key under api-key billing:\n%s", joined)
	}
}

// TestCommandEnvPersonalProfileOmitsConfigDir confirms the personal/default
// profile (ConfigDir=="") never sets CLAUDE_CONFIG_DIR — pointing it at
// ~/.claude explicitly would pick a different keychain entry than leaving it
// unset (see account.ChildEnv's doc).
func TestCommandEnvPersonalProfileOmitsConfigDir(t *testing.T) {
	spec := goldenSpec()
	spec.Profile = runtime.Profile{}

	rt := Claude{Bin: "claude"}
	_, env, err := rt.Command(spec)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			t.Errorf("personal profile must not set CLAUDE_CONFIG_DIR, got %q", kv)
		}
	}
}

// TestCapabilitiesAllTrueExceptSandbox pins the capability matrix so a future
// change to this adapter must consciously edit this test. UsageSource was
// added true (koryph-v8u.5): claude has a real ccusage/transcript usage
// source, so the governor's fail-closed enforcement stays unaffected by the
// new capability-gated advisory path (which is for a future runtime without
// one).
func TestCapabilitiesAllTrueExceptSandbox(t *testing.T) {
	got := (Claude{}).Capabilities()
	want := runtime.Capabilities{
		JSONStream:  true,
		Personas:    true,
		Hooks:       true,
		Resume:      true,
		EffortFlag:  true,
		BudgetFlag:  true,
		Sandbox:     false,
		ModelSelect: true,
		UsageSource: true,
	}
	if got != want {
		t.Errorf("Capabilities() = %+v, want %+v", got, want)
	}
}

func TestNameProviderInstructionFileModelMap(t *testing.T) {
	c := Claude{}
	if c.Name() != "claude" {
		t.Errorf("Name() = %q, want claude", c.Name())
	}
	if c.Provider() != "anthropic" {
		t.Errorf("Provider() = %q, want anthropic", c.Provider())
	}
	if c.InstructionFile() != "CLAUDE.md" {
		t.Errorf("InstructionFile() = %q, want CLAUDE.md", c.InstructionFile())
	}
	if !reflect.DeepEqual(c.ModelMap(), runtime.ClaudeModelMap) {
		t.Errorf("ModelMap() = %v, want runtime.ClaudeModelMap", c.ModelMap())
	}
}

// TestDefaultRegistryHasClaude confirms this package's init registered the
// adapter into runtime.Default (koryph-v8u.2's "first real entry").
func TestDefaultRegistryHasClaude(t *testing.T) {
	rt, ok := runtime.Default.Get("claude")
	if !ok {
		t.Fatal("runtime.Default.Get(\"claude\"): not found")
	}
	if rt.Name() != "claude" {
		t.Errorf("registered runtime Name() = %q, want claude", rt.Name())
	}
}

// writeClaudeConfig writes a minimal .claude.json fixture and returns a
// runtime.Profile pointing ConfigDir at it (koryph-v8u.5's VerifyIdentity
// tests mirror internal/account's own TestVerify/TestVerifyExpected fixture
// shape, just through the Runtime seam).
func writeClaudeConfig(t *testing.T, email string) runtime.Profile {
	t.Helper()
	dir := t.TempDir()
	body := `{"oauthAccount":{"emailAddress":"` + email + `","organizationName":"Test Org"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return runtime.Profile{Name: "work", ConfigDir: dir}
}

// TestVerifyIdentity proves VerifyIdentity is a real, fail-closed identity
// gate reachable through the Runtime seam (koryph-v8u.5) — not a stub that
// always succeeds — by exercising every account.VerifyExpected failure shape
// (mismatch, missing file, empty expected) through Claude{}.VerifyIdentity
// instead of calling account directly, plus the success path returning the
// confirmed identity.
func TestVerifyIdentity(t *testing.T) {
	ctx := context.Background()
	rt := Claude{}

	t.Run("match, case-insensitive", func(t *testing.T) {
		p := writeClaudeConfig(t, "Owner@Example.Com")
		got, err := rt.VerifyIdentity(ctx, p, "owner@example.com")
		if err != nil {
			t.Fatalf("VerifyIdentity: %v", err)
		}
		if got != "Owner@Example.Com" {
			t.Errorf("got = %q, want the logged-in email verbatim", got)
		}
	})

	t.Run("mismatch fails closed", func(t *testing.T) {
		p := writeClaudeConfig(t, "owner@example.com")
		if _, err := rt.VerifyIdentity(ctx, p, "someone-else@example.com"); err == nil {
			t.Fatal("VerifyIdentity succeeded on mismatched identity; must fail closed")
		} else if !strings.Contains(err.Error(), "account mismatch") {
			t.Errorf("err = %v, want account mismatch", err)
		}
	})

	t.Run("missing config fails closed", func(t *testing.T) {
		p := runtime.Profile{Name: "work", ConfigDir: filepath.Join(t.TempDir(), "nope")}
		if _, err := rt.VerifyIdentity(ctx, p, "owner@example.com"); err == nil {
			t.Fatal("VerifyIdentity succeeded on a missing config dir; must fail closed")
		}
	})

	t.Run("empty expected fails closed", func(t *testing.T) {
		p := writeClaudeConfig(t, "owner@example.com")
		if _, err := rt.VerifyIdentity(ctx, p, ""); err == nil {
			t.Fatal("VerifyIdentity succeeded with an empty expected identity; must fail closed")
		}
	})
}

// TestCommandEnvAllowlistAndSigningSocket proves the koryph-3vp.2 env
// allowlist and scoped SSH_AUTH_SOCK survive through the Runtime.Command path
// unweakened (koryph-v8u.5's compatibility requirement): a polluted parent
// process env leaks nothing except an explicitly declared EnvPassthrough
// var, and only the koryph-managed signing socket (never the operator's
// ambient one) is injected.
func TestCommandEnvAllowlistAndSigningSocket(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_should_not_leak_through_command")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/operator-ambient-agent.sock")
	t.Setenv("KORYPH_ALLOWED_VAR", "forwarded-by-prefix")
	t.Setenv("MY_PROJECT_VAR", "forwarded-by-passthrough")

	spec := goldenSpec()
	spec.SSHAuthSock = "/koryph/signing/agent.sock"
	spec.EnvPassthrough = []string{"MY_PROJECT_VAR"}

	rt := Claude{Bin: "claude"}
	_, env, err := rt.Command(spec)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(env, "\n")

	if strings.Contains(joined, "ghp_should_not_leak_through_command") {
		t.Errorf("GH_TOKEN leaked through Command's env:\n%s", joined)
	}
	if strings.Contains(joined, "/tmp/operator-ambient-agent.sock") {
		t.Errorf("operator's ambient SSH_AUTH_SOCK leaked through Command's env:\n%s", joined)
	}
	if !strings.Contains(joined, "SSH_AUTH_SOCK=/koryph/signing/agent.sock") {
		t.Errorf("scoped signing socket missing from Command's env:\n%s", joined)
	}
	if !strings.Contains(joined, "KORYPH_ALLOWED_VAR=forwarded-by-prefix") {
		t.Errorf("KORYPH_ prefix passthrough missing from Command's env:\n%s", joined)
	}
	if !strings.Contains(joined, "MY_PROJECT_VAR=forwarded-by-passthrough") {
		t.Errorf("registry EnvPassthrough entry missing from Command's env:\n%s", joined)
	}
}

// TestCommandEnvProxyBaseURL is the koryph-3l1.1 main-dispatch acceptance
// test: DispatchSpec.ProxyBaseURL (threaded from registry.Record.
// ProxyBaseURL() via dispatch.Spec/toRuntimeSpec) reaches Command's env as
// ANTHROPIC_BASE_URL; unset leaves it absent (main dispatch never sets
// KORYPH_SPAWN_KIND either — that marker is main dispatch's default empty
// value, unlike the three secondary spawn sites).
func TestCommandEnvProxyBaseURL(t *testing.T) {
	rt := Claude{Bin: "claude"}

	spec := goldenSpec()
	_, env, err := rt.Command(spec)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ANTHROPIC_BASE_URL=") {
		t.Errorf("ANTHROPIC_BASE_URL present with ProxyBaseURL unset:\n%s", joined)
	}
	if strings.Contains(joined, "KORYPH_SPAWN_KIND=") {
		t.Errorf("KORYPH_SPAWN_KIND present for main dispatch, want absent:\n%s", joined)
	}

	spec.ProxyBaseURL = "http://127.0.0.1:8091"
	_, env, err = rt.Command(spec)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined = strings.Join(env, "\n")
	if !strings.Contains(joined, "ANTHROPIC_BASE_URL=http://127.0.0.1:8091") {
		t.Errorf("ANTHROPIC_BASE_URL missing from Command's env:\n%s", joined)
	}
}
