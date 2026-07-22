// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package beads

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeInitBD is a stand-in `bd` binary for EnsureDB/Harden coverage. It logs
// argv (one line per invocation) to $BD_ARGS_LOG, and its `version`/`doctor`/
// `hooks` behavior is controlled by env vars so a single script serves every
// test case without per-test rewriting.
const fakeInitBD = `#!/bin/sh
if [ -n "$BD_ARGS_LOG" ]; then
  echo "$@" >> "$BD_ARGS_LOG"
fi
case "$1" in
  version)
    printf '%s\n' "${BD_VERSION_LINE:-bd version 1.0.5 (fake)}"
    ;;
  init)
    exit "${BD_INIT_EXIT:-0}"
    ;;
  doctor)
    if [ -n "$BD_DOCTOR_OUT" ]; then printf '%s' "$BD_DOCTOR_OUT"; fi
    if [ -n "$BD_DOCTOR_ERR" ]; then printf '%s' "$BD_DOCTOR_ERR" >&2; fi
    exit "${BD_DOCTOR_EXIT:-0}"
    ;;
  hooks)
    exit "${BD_HOOKS_EXIT:-0}"
    ;;
  *)
    exit 0
    ;;
esac
`

// newFakeInitAdapter writes fakeInitBD into a temp repo and returns an
// adapter pointed at it, plus the argv-log path. It also sets KORYPH_BD_BIN
// to the same binary: EnsureDB's version probe goes through the package-level
// ProbeVersion (which resolves via KORYPH_BD_BIN / ResolveBin, not a.Bin), so
// tests must keep both pointed at the same fake for consistent results — the
// same invariant New() maintains in production (Bin: ResolveBin()).
func newFakeInitAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	repo := t.TempDir()
	bin := filepath.Join(repo, "bd")
	if err := os.WriteFile(bin, []byte(fakeInitBD), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	log := filepath.Join(repo, "argv.log")
	t.Setenv("BD_ARGS_LOG", log)
	t.Setenv("KORYPH_BD_BIN", bin)
	return &Adapter{RepoRoot: repo, BeadsDir: filepath.Join(repo, ".beads"), Bin: bin}, log
}

func readLoggedArgs(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read argv log: %v", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// --- EnsureDB ---------------------------------------------------------------

func TestEnsureDB_AbsentDirInitsLocalOnly(t *testing.T) {
	a, log := newFakeInitAdapter(t)

	res, err := a.EnsureDB(context.Background(), EnsureOpts{Prefix: "myrepo"})
	if err != nil {
		t.Fatalf("EnsureDB: %v", err)
	}
	if !res.Initialized {
		t.Fatal("Initialized = false, want true for an absent .beads dir")
	}
	wantArgv := []string{"init", "--non-interactive", "--init-if-missing", "--prefix", "myrepo"}
	if strings.Join(res.InitArgv, " ") != strings.Join(wantArgv, " ") {
		t.Fatalf("InitArgv = %v, want %v (no --remote for local-only)", res.InitArgv, wantArgv)
	}

	args := readLoggedArgs(t, log)
	if len(args) < 1 || args[0] != strings.Join(wantArgv, " ") {
		t.Fatalf("logged argv = %v, want first line %q", args, strings.Join(wantArgv, " "))
	}

	if !res.VersionOK {
		t.Errorf("VersionOK = false, want true (fake bd reports %s)", "1.0.5")
	}
	if res.Remediation != "" {
		t.Errorf("Remediation = %q, want empty when version is OK", res.Remediation)
	}
	if res.SnapshotPath != "" {
		t.Errorf("SnapshotPath = %q, want empty on the init-from-absent path", res.SnapshotPath)
	}
	if res.DoctorRan {
		t.Error("DoctorRan = true, want false on the init-from-absent path")
	}
}

func TestEnsureDB_AbsentDirWithRemote(t *testing.T) {
	a, log := newFakeInitAdapter(t)

	res, err := a.EnsureDB(context.Background(), EnsureOpts{
		Prefix: "myrepo",
		Remote: "git+https://github.com/me/myrepo.git",
	})
	if err != nil {
		t.Fatalf("EnsureDB: %v", err)
	}
	if !res.Initialized {
		t.Fatal("Initialized = false, want true")
	}
	argv := strings.Join(res.InitArgv, " ")
	if !strings.Contains(argv, "--remote git+https://github.com/me/myrepo.git") {
		t.Fatalf("InitArgv = %q, want a --remote flag", argv)
	}
	if !strings.Contains(argv, "--non-interactive") || !strings.Contains(argv, "--init-if-missing") || !strings.Contains(argv, "--prefix myrepo") {
		t.Fatalf("InitArgv = %q, missing a required flag", argv)
	}
	// EnsureDB always probes the version too (both branches), so bd is
	// invoked for `init` and then `version` — never `doctor` on this path.
	loggedArgv := readLoggedArgs(t, log)
	if len(loggedArgv) != 2 || loggedArgv[0] != argv || loggedArgv[1] != "version" {
		t.Fatalf("logged invocations = %v, want [init..., version]", loggedArgv)
	}

	// Never emit bd's destructive init flags.
	for _, forbidden := range []string{"--reinit-local", "--force", "--discard-remote"} {
		if strings.Contains(argv, forbidden) {
			t.Fatalf("InitArgv %q must never contain destructive flag %q", argv, forbidden)
		}
	}
}

func TestEnsureDB_ExistingDirIsIdempotentAndRunsDoctor(t *testing.T) {
	a, log := newFakeInitAdapter(t)
	if err := os.MkdirAll(a.BeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_DOCTOR_OUT", "all checks passed\n")

	res, err := a.EnsureDB(context.Background(), EnsureOpts{Prefix: "myrepo"})
	if err != nil {
		t.Fatalf("EnsureDB: %v", err)
	}
	if res.Initialized {
		t.Fatal("Initialized = true, want false for a pre-existing .beads dir")
	}
	if res.InitArgv != nil {
		t.Fatalf("InitArgv = %v, want nil (no init should run)", res.InitArgv)
	}
	for _, line := range readLoggedArgs(t, log) {
		if strings.HasPrefix(line, "init") {
			t.Fatalf("bd init was invoked on an existing DB: %q", line)
		}
	}

	if !res.DoctorRan {
		t.Fatal("DoctorRan = false, want true for the existing-DB path")
	}
	if !res.DoctorOK {
		t.Errorf("DoctorOK = false, want true (fake doctor exits 0)")
	}
	if !strings.Contains(res.DoctorOutput, "all checks passed") {
		t.Errorf("DoctorOutput = %q, want the fake doctor's stdout", res.DoctorOutput)
	}
	if res.SnapshotPath == "" {
		t.Error("SnapshotPath is empty, want a snapshot taken before doctor ran")
	}
	if _, err := os.Stat(res.SnapshotPath); err != nil {
		t.Errorf("snapshot file missing at %q: %v", res.SnapshotPath, err)
	}

	foundDoctor := false
	for _, line := range readLoggedArgs(t, log) {
		if line == "doctor" {
			foundDoctor = true
		}
	}
	if !foundDoctor {
		t.Error("bd doctor was not invoked")
	}
}

func TestEnsureDB_ExistingDirDoctorFailingSurfacesWithoutError(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	if err := os.MkdirAll(a.BeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_DOCTOR_EXIT", "1")
	t.Setenv("BD_DOCTOR_ERR", "schema drift detected\n")

	res, err := a.EnsureDB(context.Background(), EnsureOpts{Prefix: "myrepo"})
	if err != nil {
		t.Fatalf("EnsureDB returned an error for a failing bd doctor: %v (doctor findings must surface, not fail the call)", err)
	}
	if res.DoctorOK {
		t.Error("DoctorOK = true, want false when doctor exits non-zero")
	}
	if !strings.Contains(res.DoctorOutput, "schema drift detected") {
		t.Errorf("DoctorOutput = %q, want doctor's stderr surfaced", res.DoctorOutput)
	}
}

func TestEnsureDB_VersionTooOldSurfacesRemediationWithoutError(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	t.Setenv("BD_VERSION_LINE", "bd version 1.0.3 (fake)")

	res, err := a.EnsureDB(context.Background(), EnsureOpts{Prefix: "myrepo"})
	if err != nil {
		t.Fatalf("EnsureDB returned an error for an old bd version: %v (must not fail hard)", err)
	}
	if res.VersionOK {
		t.Error("VersionOK = true, want false for bd 1.0.3 < MinVersion")
	}
	if res.VersionStatus == "" || !strings.Contains(res.VersionStatus, "1.0.3") {
		t.Errorf("VersionStatus = %q, want it to mention 1.0.3", res.VersionStatus)
	}
	if res.Remediation == "" {
		t.Fatal("Remediation is empty, want the version-skew remediation text")
	}
	if !strings.Contains(res.Remediation, MinVersion) {
		t.Errorf("Remediation = %q, want it to mention MinVersion %s", res.Remediation, MinVersion)
	}
	// Init still happened; version gating is orthogonal to init/doctor outcome.
	if !res.Initialized {
		t.Error("Initialized = false, want true — an old bd should not block init")
	}
}

func TestEnsureDB_VersionNotFound(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	t.Setenv("KORYPH_BD_BIN", "/nonexistent/definitely-not-bd")

	res, err := a.EnsureDB(context.Background(), EnsureOpts{Prefix: "myrepo"})
	if err != nil {
		t.Fatalf("EnsureDB: %v", err)
	}
	if res.VersionOK {
		t.Error("VersionOK = true, want false when bd cannot be resolved")
	}
	if res.VersionStatus != "bd not found" {
		t.Errorf("VersionStatus = %q, want \"bd not found\"", res.VersionStatus)
	}
	if !strings.Contains(res.Remediation, "not on PATH") {
		t.Errorf("Remediation = %q, want a not-found message", res.Remediation)
	}
}

// --- DeriveSyncRemote --------------------------------------------------------

func TestDeriveSyncRemote(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"https already dot-git", "https://github.com/owner/repo.git", "git+https://github.com/owner/repo.git"},
		{"https missing dot-git", "https://github.com/owner/repo", "git+https://github.com/owner/repo.git"},
		{"http scheme", "http://example.com/owner/repo", "git+http://example.com/owner/repo.git"},
		{"ssh scp-like github", "git@github.com:owner/repo.git", "git+https://github.com/owner/repo.git"},
		{"ssh scp-like gitlab no dot-git", "git@gitlab.com:owner/repo", "git+https://gitlab.com/owner/repo.git"},
		{"ssh url form", "ssh://git@github.com/owner/repo.git", "git+https://github.com/owner/repo.git"},
		{"ssh url form no user", "ssh://example.com/owner/repo.git", "git+https://example.com/owner/repo.git"},
		{"already derived form (idempotent)", "git+https://github.com/owner/repo.git", "git+https://github.com/owner/repo.git"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"local filesystem path", "/Users/me/src/myrepo", ""},
		{"windows drive path", `C:\Users\me\myrepo`, ""},
		{"garbage", "not a url at all", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveSyncRemote(tc.input); got != tc.want {
				t.Errorf("DeriveSyncRemote(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// --- Harden ------------------------------------------------------------------

func TestHarden_UninitializedReturnsError(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	actions, err := a.Harden(context.Background())
	if err == nil {
		t.Fatal("Harden on an absent .beads dir returned nil error, want one")
	}
	if actions != nil {
		t.Errorf("actions = %v, want nil on error", actions)
	}
	if _, statErr := os.Stat(a.BeadsDir); statErr == nil {
		t.Error("Harden must not create .beads when the DB was never initialized")
	}
}

func TestHarden_AppendsGitignoreLine(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	if err := os.MkdirAll(a.BeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	giPath := filepath.Join(a.BeadsDir, ".gitignore")
	if err := os.WriteFile(giPath, []byte("dolt/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	actions, err := a.Harden(context.Background())
	if err != nil {
		t.Fatalf("Harden: %v", err)
	}

	data, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "issues.jsonl") {
		t.Fatalf(".gitignore after Harden = %q, want it to contain issues.jsonl", data)
	}
	if !strings.Contains(string(data), "dolt/") {
		t.Fatalf(".gitignore after Harden = %q, want the pre-existing line preserved", data)
	}

	var gitignoreAction *HardenAction
	for i := range actions {
		if actions[i].Name == "gitignore-issues-jsonl" {
			gitignoreAction = &actions[i]
		}
	}
	if gitignoreAction == nil {
		t.Fatal("no gitignore-issues-jsonl action reported")
	}
	if !gitignoreAction.Applied {
		t.Error("gitignore action Applied = false, want true (this call fixed it)")
	}
}

func TestHarden_GitignoreAlreadyPresentIsIdempotent(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	if err := os.MkdirAll(a.BeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	giPath := filepath.Join(a.BeadsDir, ".gitignore")
	original := "dolt/\nissues.jsonl\n"
	if err := os.WriteFile(giPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Harden(context.Background()); err != nil {
		t.Fatalf("Harden: %v", err)
	}
	// Second call must not duplicate the line.
	actions, err := a.Harden(context.Background())
	if err != nil {
		t.Fatalf("Harden (second call): %v", err)
	}
	for _, act := range actions {
		if act.Name == "gitignore-issues-jsonl" {
			t.Fatalf("gitignore action reported on an already-hardened .gitignore: %+v", act)
		}
	}

	data, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "issues.jsonl") != 1 {
		t.Fatalf(".gitignore = %q, want exactly one issues.jsonl line after repeated Harden calls", data)
	}
}

func TestHarden_FullyHardenedReportsNoActions(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	if err := os.MkdirAll(a.BeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.BeadsDir, ".gitignore"), []byte("issues.jsonl\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.BeadsDir, "config.yaml"), []byte(`sync.remote: "git+https://example.com/owner/repo.git"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(a.RepoRoot, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("#!/bin/sh\n# BEADS INTEGRATION\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	actions, err := a.Harden(context.Background())
	if err != nil {
		t.Fatalf("Harden: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none for a fully-hardened DB", actions)
	}
}

func TestHarden_ReportsMissingSyncRemote(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	if err := os.MkdirAll(a.BeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.BeadsDir, ".gitignore"), []byte("issues.jsonl\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.BeadsDir, "config.yaml"), []byte("# no sync remote configured\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(a.RepoRoot, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("# BEADS INTEGRATION\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	actions, err := a.Harden(context.Background())
	if err != nil {
		t.Fatalf("Harden: %v", err)
	}
	var remoteAction *HardenAction
	for i := range actions {
		if actions[i].Name == "sync-remote" {
			remoteAction = &actions[i]
		}
	}
	if remoteAction == nil {
		t.Fatal("no sync-remote action reported for a config.yaml lacking sync.remote")
	}
	if remoteAction.Applied {
		t.Error("sync-remote Applied = true, want false (Harden never edits config.yaml directly)")
	}
	if !strings.Contains(remoteAction.Detail, "EnsureDB") {
		t.Errorf("sync-remote Detail = %q, want it to point at EnsureDB's Remote option", remoteAction.Detail)
	}
}

func TestHarden_ReportsMissingHooksWithSubcommandGuidance(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	if err := os.MkdirAll(a.BeadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.BeadsDir, ".gitignore"), []byte("issues.jsonl\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.BeadsDir, "config.yaml"), []byte(`sync.remote: "git+https://example.com/owner/repo.git"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No .git/hooks dir at all -> hooks not installed.

	t.Run("hooks subcommand available", func(t *testing.T) {
		t.Setenv("BD_HOOKS_EXIT", "0")
		actions, err := a.Harden(context.Background())
		if err != nil {
			t.Fatalf("Harden: %v", err)
		}
		hooksAction := findHardenAction(actions, "git-hooks")
		if hooksAction == nil {
			t.Fatal("no git-hooks action reported")
		}
		if hooksAction.Applied {
			t.Error("git-hooks Applied = true, want false (advice only)")
		}
		if !strings.Contains(hooksAction.Detail, "bd hooks install") {
			t.Errorf("Detail = %q, want it to name `bd hooks install`", hooksAction.Detail)
		}
	})

	t.Run("hooks subcommand unavailable falls back to re-init guidance", func(t *testing.T) {
		t.Setenv("BD_HOOKS_EXIT", "1")
		actions, err := a.Harden(context.Background())
		if err != nil {
			t.Fatalf("Harden: %v", err)
		}
		hooksAction := findHardenAction(actions, "git-hooks")
		if hooksAction == nil {
			t.Fatal("no git-hooks action reported")
		}
		if strings.Contains(hooksAction.Detail, "bd hooks install") {
			t.Errorf("Detail = %q, want generic re-init guidance when `bd hooks --help` fails", hooksAction.Detail)
		}
		if !strings.Contains(hooksAction.Detail, "bd init") {
			t.Errorf("Detail = %q, want it to mention re-running bd init", hooksAction.Detail)
		}
	})
}

func findHardenAction(actions []HardenAction, name string) *HardenAction {
	for i := range actions {
		if actions[i].Name == name {
			return &actions[i]
		}
	}
	return nil
}

// Sanity: the fake bd binary this file defines must itself be runnable,
// otherwise every test above is exercising a shell-syntax bug rather than
// initx.go. This mirrors adapter_test.go's TestAvailable but also confirms
// `sh` accepts the script (a cheap guard against a broken heredoc/quote).
func TestFakeInitBDIsRunnable(t *testing.T) {
	a, _ := newFakeInitAdapter(t)
	out, err := exec.Command(a.Bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("fake bd version: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "bd version") {
		t.Fatalf("fake bd version output = %q", out)
	}
}
