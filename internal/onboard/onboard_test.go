// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package onboard

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/registry"
)

// --- fixtures --------------------------------------------------------------

// initRepo creates a git repo on main with one commit and returns its root.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	writeFile(t, filepath.Join(dir, "README.md"), "# fixture\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeBD writes an executable bd stub that answers version/ready and returns
// its path, wiring KORYPH_BD_BIN for the test.
func fakeBD(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
case "$1" in
  version) echo "bd 0.9.9-test"; exit 0;;
  ready)   echo "[]"; exit 0;;
  *)       echo "[]"; exit 0;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_BD_BIN", path)
	return path
}

// fakeIdentityDir writes a .claude.json reporting email and returns the dir.
func fakeIdentityDir(t *testing.T, email string) string {
	t.Helper()
	dir := t.TempDir()
	body := `{"oauthAccount":{"emailAddress":"` + email + `","organizationName":"Test Org"}}`
	writeFile(t, filepath.Join(dir, ".claude.json"), body)
	return dir
}

const workEnvrc = `# >>> claude-account (managed) >>>
export CLAUDE_CONFIG_DIR="$HOME/.claude-work"
# <<< claude-account (managed) <<<
`

const personalUnsetEnvrc = `# >>> claude-account (managed) >>>
unset CLAUDE_CONFIG_DIR
# <<< claude-account (managed) <<<
`

const personalDeprecatedEnvrc = `# >>> claude-account (managed) >>>
export CLAUDE_CONFIG_DIR="$HOME/.claude"
# <<< claude-account (managed) <<<
`

// --- Inspect ---------------------------------------------------------------

func TestInspectFullFixture(t *testing.T) {
	fakeBD(t)
	root := initRepo(t)

	writeFile(t, filepath.Join(root, ".envrc"), workEnvrc)
	writeFile(t, filepath.Join(root, ".beads", ".gitignore"), "issues.jsonl\n*.lock\n")
	writeFile(t, filepath.Join(root, ".beads", "config.yaml"), "sync:\n  remote: origin\n  branch: refs/dolt/data\n")
	writeFile(t, filepath.Join(root, ".claude", "settings.json"), `{"hooks":{"SessionStart":[{"command":"bd prime"}]}}`)
	writeFile(t, filepath.Join(root, ".claude", "agents", "implementer.md"), "impl\n")
	writeFile(t, filepath.Join(root, ".claude", "agents", "security-reviewer.md"), "sec\n")
	writeFile(t, filepath.Join(root, "koryph", "scheduler.sh"), "#!/bin/sh\nexec foo --source bd\n")
	writeFile(t, filepath.Join(root, "docs", "plans", "README.md"), "plans\n")

	inv, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if !inv.IsGitRepo {
		t.Error("IsGitRepo = false")
	}
	if inv.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", inv.DefaultBranch)
	}
	if !inv.HasBeads || !inv.BeadsHardened {
		t.Errorf("beads: HasBeads=%v Hardened=%v, want both true", inv.HasBeads, inv.BeadsHardened)
	}
	if !inv.BDAvailable || inv.BDVersion == "" {
		t.Errorf("bd: available=%v version=%q, want available with version", inv.BDAvailable, inv.BDVersion)
	}
	if !inv.ClaudeSettings || !inv.BDPrimeHook {
		t.Errorf("claude: settings=%v bdPrime=%v, want both true", inv.ClaudeSettings, inv.BDPrimeHook)
	}
	if !containsAll(inv.Personas, "implementer", "security-reviewer") {
		t.Errorf("Personas = %v, want implementer+security-reviewer", inv.Personas)
	}
	if !inv.LegacyKoryph {
		t.Error("LegacyKoryph = false")
	}
	if !hintPresent(inv.LegacyHints, "scheduler.sh") || !hintPresent(inv.LegacyHints, "--source bd") {
		t.Errorf("LegacyHints = %v, want scheduler.sh + --source bd", inv.LegacyHints)
	}
	if inv.EnvrcProfile != "work" {
		t.Errorf("EnvrcProfile = %q, want work", inv.EnvrcProfile)
	}
	if inv.EnvrcDir == "" {
		t.Errorf("EnvrcDir empty for a work block")
	}
	if inv.AdapterPresent {
		t.Error("AdapterPresent = true, want false (no koryph.project.json)")
	}
	if inv.PlansDir != "docs/plans" {
		t.Errorf("PlansDir = %q, want docs/plans", inv.PlansDir)
	}
}

func TestInspectIsReadOnly(t *testing.T) {
	root := initRepo(t)
	writeFile(t, filepath.Join(root, ".envrc"), personalUnsetEnvrc)
	before := snapshotDir(t, root)
	if _, err := Inspect(context.Background(), root); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	after := snapshotDir(t, root)
	if before != after {
		t.Errorf("Inspect mutated the tree:\nbefore=%v\nafter =%v", before, after)
	}
}

func TestInspectEnvrcClassification(t *testing.T) {
	cases := []struct {
		name    string
		envrc   string
		profile string
	}{
		{"work", workEnvrc, "work"},
		{"personal-unset", personalUnsetEnvrc, "personal-unset"},
		{"personal-deprecated", personalDeprecatedEnvrc, "personal-explicit-deprecated"},
		{"none", "export FOO=bar\n", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := initRepo(t)
			writeFile(t, filepath.Join(root, ".envrc"), tc.envrc)
			inv, err := Inspect(context.Background(), root)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if inv.EnvrcProfile != tc.profile {
				t.Errorf("EnvrcProfile = %q, want %q", inv.EnvrcProfile, tc.profile)
			}
		})
	}
}

// --- extractEnvrcDir — variable-indirection unit tests ----------------------

func TestExtractEnvrcDirVariableIndirection(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  string
	}{
		{
			name:  "direct path unchanged",
			block: `export CLAUDE_CONFIG_DIR="$HOME/.claude-work"`,
			want:  `$HOME/.claude-work`,
		},
		{
			name:  "simple var ref resolves",
			block: "WORK_DIR=\"$HOME/.claude-work\"\nexport CLAUDE_CONFIG_DIR=\"$WORK_DIR\"",
			want:  `$HOME/.claude-work`,
		},
		{
			name:  "brace var ref resolves",
			block: "WORK_DIR=\"$HOME/.claude-work\"\nexport CLAUDE_CONFIG_DIR=\"${WORK_DIR}\"",
			want:  `$HOME/.claude-work`,
		},
		{
			name:  "export on definition line resolves",
			block: "export WORK_DIR=\"$HOME/.claude-work\"\nexport CLAUDE_CONFIG_DIR=\"$WORK_DIR\"",
			want:  `$HOME/.claude-work`,
		},
		{
			name:  "default expansion returned as-is (already contains full hint)",
			block: `export CLAUDE_CONFIG_DIR="${CLAUDE_WORK_DIR:-$HOME/.claude-work}"`,
			want:  `${CLAUDE_WORK_DIR:-$HOME/.claude-work}`,
		},
		{
			name:  "unresolvable ref returned as-is",
			block: `export CLAUDE_CONFIG_DIR="$EXTERNAL_DIR"`,
			want:  `$EXTERNAL_DIR`,
		},
		{
			name:  "no CLAUDE_CONFIG_DIR assignment returns empty",
			block: `unset CLAUDE_CONFIG_DIR`,
			want:  ``,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractEnvrcDir(tc.block)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInspectEnvrcDirVariableIndirection is an integration test that exercises
// the full Inspect path with an .envrc that sets CLAUDE_CONFIG_DIR via an
// intermediate variable.
func TestInspectEnvrcDirVariableIndirection(t *testing.T) {
	const indirectWorkEnvrc = `# >>> claude-account (managed) >>>
WORK_DIR="$HOME/.claude-work"
export CLAUDE_CONFIG_DIR="$WORK_DIR"
# <<< claude-account (managed) <<<
`
	root := initRepo(t)
	writeFile(t, filepath.Join(root, ".envrc"), indirectWorkEnvrc)
	inv, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inv.EnvrcProfile != "work" {
		t.Errorf("EnvrcProfile = %q, want work", inv.EnvrcProfile)
	}
	// Variable indirection must be resolved: the dir must be the actual path,
	// not the intermediate variable reference "$WORK_DIR".
	if inv.EnvrcDir != `$HOME/.claude-work` {
		t.Errorf("EnvrcDir = %q, want $HOME/.claude-work (resolved from $WORK_DIR)", inv.EnvrcDir)
	}
}

// --- Register --------------------------------------------------------------

func TestRegisterHappyPath(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := initRepo(t)
	store := newStore(t)

	inv, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	rec, err := Register(context.Background(), store, inv, RegisterOpts{
		ProjectID:        "demo",
		AccountProfile:   registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rec.ProjectID != "demo" || rec.MigrationStatus != registry.StatusRegistered {
		t.Errorf("record = %+v", rec)
	}
	if rec.PlannerModel != "opus" || rec.ImplModel != "sonnet" || rec.RecoveryModelPolicy != "upgrade-opus" {
		t.Errorf("model defaults = %q/%q/%q", rec.PlannerModel, rec.ImplModel, rec.RecoveryModelPolicy)
	}
	if rec.BatchPolicy != "explicit" || rec.APIFallback != "off" || rec.VisibilitySync != "off" {
		t.Errorf("billing/policy defaults = %+v", rec)
	}
	if strings.Join(rec.AllowedModels, ",") != "haiku,sonnet,opus" {
		t.Errorf("AllowedModels = %v", rec.AllowedModels)
	}
	if rec.WorktreeRoot == "" {
		t.Error("WorktreeRoot not defaulted")
	}
	// Adapter scaffold written.
	if _, err := os.Stat(filepath.Join(root, "koryph.project.json")); err != nil {
		t.Errorf("koryph.project.json not scaffolded: %v", err)
	}
	// Present in the store.
	if _, err := store.Get("demo"); err != nil {
		t.Errorf("record not in store: %v", err)
	}
}

func TestRegisterRefusesEnvrcDisagreement(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := initRepo(t)
	writeFile(t, filepath.Join(root, ".envrc"), workEnvrc)
	store := newStore(t)

	inv, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	_, err = Register(context.Background(), store, inv, RegisterOpts{
		ProjectID:        "demo",
		AccountProfile:   registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
	})
	if err == nil {
		t.Fatal("Register succeeded despite personal-vs-work .envrc disagreement")
	}
	if !strings.Contains(err.Error(), "force") && !strings.Contains(err.Error(), ".envrc") {
		t.Errorf("error should mention .envrc/force: %v", err)
	}
	// Nothing scaffolded, nothing stored.
	if _, statErr := os.Stat(filepath.Join(root, "koryph.project.json")); !os.IsNotExist(statErr) {
		t.Errorf("adapter scaffolded despite refusal (stat err %v)", statErr)
	}
}

func TestRegisterForceOverridesEnvrc(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := initRepo(t)
	writeFile(t, filepath.Join(root, ".envrc"), workEnvrc)
	store := newStore(t)

	inv, _ := Inspect(context.Background(), root)
	rec, err := Register(context.Background(), store, inv, RegisterOpts{
		ProjectID:        "demo",
		AccountProfile:   registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
		Force:            true,
	})
	if err != nil {
		t.Fatalf("Register with Force: %v", err)
	}
	if rec.AccountProfile != registry.ProfilePersonal {
		t.Errorf("AccountProfile = %q", rec.AccountProfile)
	}
}

func TestRegisterRefusesDuplicate(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := initRepo(t)
	store := newStore(t)

	inv, _ := Inspect(context.Background(), root)
	opts := RegisterOpts{ProjectID: "demo", AccountProfile: registry.ProfilePersonal, ExpectedIdentity: "me@example.com"}
	if _, err := Register(context.Background(), store, inv, opts); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if _, err := Register(context.Background(), store, inv, opts); err == nil {
		t.Fatal("second Register succeeded; duplicate must be refused")
	}
}

func TestRegisterValidatesOpts(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := initRepo(t)
	store := newStore(t)
	inv, _ := Inspect(context.Background(), root)

	// Missing account.
	if _, err := Register(context.Background(), store, inv, RegisterOpts{ExpectedIdentity: "me@example.com"}); err == nil {
		t.Error("expected refusal for empty account profile")
	}
	// Non-personal without config dir.
	if _, err := Register(context.Background(), store, inv, RegisterOpts{
		ProjectID: "w", AccountProfile: registry.ProfileWork, ExpectedIdentity: "me@example.com",
	}); err == nil {
		t.Error("expected refusal for work account without config dir")
	}
	// Bad identity.
	if _, err := Register(context.Background(), store, inv, RegisterOpts{
		ProjectID: "b", AccountProfile: registry.ProfilePersonal, ExpectedIdentity: "not-an-email",
	}); err == nil {
		t.Error("expected refusal for non-email identity")
	}
}

// --- Validate --------------------------------------------------------------

func TestValidateMinimalGreen(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	fakeBD(t)
	root := initRepo(t)
	idDir := fakeIdentityDir(t, "me@example.com")
	writeFile(t, filepath.Join(root, ".claude", "settings.json"), `{"hooks":{"SessionStart":[{"command":"bd prime"}]}}`)

	store := newStore(t)
	inv, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if _, err := Register(context.Background(), store, inv, RegisterOpts{
		ProjectID:        "demo",
		AccountProfile:   registry.ProfileWork,
		ClaudeConfigDir:  idDir,
		ExpectedIdentity: "me@example.com",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var out bytes.Buffer
	v, err := Validate(context.Background(), store, "demo", &out)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !v.OK {
		t.Fatalf("Validate not OK; checks=%+v", v.Checks)
	}
	if out.Len() == 0 {
		t.Error("Validate did not stream any check lines to out")
	}

	if !hasCheck(v, "governor", LevelWarn, "uncalibrat") {
		t.Errorf("expected an uncalibrated governor warn; checks=%+v", v.Checks)
	}
	if !hasCheck(v, "scheduler dry-run", LevelOK, "frontier empty") {
		t.Errorf("expected an OK frontier-empty scheduler check; checks=%+v", v.Checks)
	}
	if !hasCheck(v, "account identity", LevelOK, "me@example.com") {
		t.Errorf("expected an OK account-identity check; checks=%+v", v.Checks)
	}
}

func TestValidateFailsOnIdentityMismatch(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	fakeBD(t)
	root := initRepo(t)
	idDir := fakeIdentityDir(t, "someone@else.com")

	store := newStore(t)
	inv, _ := Inspect(context.Background(), root)
	if _, err := Register(context.Background(), store, inv, RegisterOpts{
		ProjectID:        "demo",
		AccountProfile:   registry.ProfileWork,
		ClaudeConfigDir:  idDir,
		ExpectedIdentity: "me@example.com",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	v, err := Validate(context.Background(), store, "demo", nil)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if v.OK {
		t.Fatal("Validate OK despite identity mismatch")
	}
	if !hasLevel(v, "account identity", LevelError) {
		t.Errorf("expected an error-level account-identity check; checks=%+v", v.Checks)
	}
}

// --- helpers ---------------------------------------------------------------

func newStore(t *testing.T) *registry.Store {
	t.Helper()
	s := registry.NewStore()
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	return s
}

func containsAll(hay []string, needles ...string) bool {
	set := map[string]bool{}
	for _, h := range hay {
		set[h] = true
	}
	for _, n := range needles {
		if !set[n] {
			return false
		}
	}
	return true
}

func hintPresent(hints []string, sub string) bool {
	for _, h := range hints {
		if strings.Contains(h, sub) {
			return true
		}
	}
	return false
}

func hasCheck(v *Validation, name, level, detailSub string) bool {
	for _, c := range v.Checks {
		if c.Name == name && c.Level == level && strings.Contains(c.Detail, detailSub) {
			return true
		}
	}
	return false
}

func hasLevel(v *Validation, name, level string) bool {
	for _, c := range v.Checks {
		if c.Name == name && c.Level == level {
			return true
		}
	}
	return false
}

// snapshotDir returns a stable string of the tree's file paths + sizes, used to
// prove Inspect performs no writes.
func snapshotDir(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip .git internals: read-only git commands can refresh index stat
		// caches without changing tracked content, which is not a mutation we
		// care about here.
		if strings.Contains(path, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if info.IsDir() {
			b.WriteString("d " + rel + "\n")
		} else {
			b.WriteString("f " + rel + " " + strconv.FormatInt(info.Size(), 10) + "\n")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
