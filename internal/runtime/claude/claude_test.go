// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import (
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
// change to this adapter must consciously edit this test.
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
