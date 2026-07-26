// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/commandguard"
	"github.com/koryph/koryph/internal/promptc"
	"github.com/koryph/koryph/internal/resmon"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

func canonicalDispatchSpec(spec runtime.DispatchSpec) runtime.DispatchSpec {
	spec.RuntimeOutputPath = runtime.RuntimeFinalOutputPath(spec.PhaseDir)
	return spec
}

func TestCodexConformsToSharedRuntimeContract(t *testing.T) {
	runtimetest.AssertConforms(t, Codex{Bin: "codex"}, runtimetest.ConformanceFixture{
		Dispatch: runtime.DispatchSpec{
			RepoRoot: "/repo", PhaseDir: "/phase", Model: "gpt-5.6-terra", Effort: "high",
			RuntimeOutputPath: "/.runtime-output/phase/runtime-final.md",
			Profile:           runtime.Profile{ConfigDir: "/profiles/work"},
			SSHAuthSock:       "/run/koryph-signing/signing.sock",
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
	argv, _, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{
		RepoRoot: "/repo", PhaseDir: "/phase", Model: "gpt-5.6-terra", Effort: "high",
		Profile: runtime.Profile{ConfigDir: "/profiles/work"}, Billing: runtime.BillingSubscription,
		SSHAuthSock: "/run/koryph-signing/signing.sock",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"codex", "--ask-for-approval", "never", "exec", "--json",
		"-c", "allow_login_shell=false",
		"--ignore-user-config",
		"-c", `default_permissions="koryph_signing"`,
		"-c", signingFilesystemRule("/repo"),
		"-c", "permissions.koryph_signing.network.enabled=true",
		"-c", unixSocketRule(append([]string{"/run/koryph-signing/signing.sock"}, testAgentSockets("/phase")...)...),
		"--dangerously-bypass-hook-trust", "--add-dir", "/phase", "--add-dir", "/repo/.git",
		"--model", "gpt-5.6-terra", "-c", `model_reasoning_effort="high"`,
		"--output-last-message", "/.runtime-output/phase/runtime-final.md",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %q\nwant = %q", argv, want)
	}
	if strings.Contains(strings.Join(argv, "\x00"), filepath.Join("/phase", "SUMMARY.md")) {
		t.Fatalf("argv targets phase-owned SUMMARY.md: %q", argv)
	}
}

func TestCommandRejectsMissingOrNoncanonicalRuntimeOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "missing"},
		{name: "phase summary", path: "/phase/SUMMARY.md"},
		{name: "legacy phase-local output", path: "/phase/runtime-final.md"},
		{name: "other phase", path: "/other/runtime-final.md"},
		{name: "relative", path: "runtime-final.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase := t.TempDir()
			path := tc.path
			if strings.HasPrefix(path, "/phase/") {
				path = filepath.Join(phase, filepath.Base(path))
			}
			_, _, err := (Codex{Bin: "codex"}).Command(runtime.DispatchSpec{
				PhaseDir: phase, RuntimeOutputPath: path,
			})
			if err == nil || (!strings.Contains(err.Error(), "required") &&
				!strings.Contains(err.Error(), "noncanonical") &&
				!strings.Contains(err.Error(), "absolute")) {
				t.Fatalf("Command error = %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(phase, "go-tmp")); !os.IsNotExist(statErr) {
				t.Fatalf("Command created phase scratch before path refusal: %v", statErr)
			}
		})
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
	argv, _, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{
		RepoRoot: "/repo", PhaseDir: "/phase",
	}))
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
	_, env, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{
		RepoRoot: "/repo", PhaseDir: "/phase", SSHAuthSock: "/signing/socket",
	}))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	projectCache := filepath.Join("/repo", ".git", "koryph-cache", "go")
	for _, want := range []string{
		"PRE_COMMIT_HOME=" + filepath.Join(os.Getenv("HOME"), ".cache", "pre-commit"),
		"GOCACHE=" + filepath.Join(projectCache, "build-"+goBuildCacheKey(goruntime.Version(), goruntime.GOOS, goruntime.GOARCH)),
		"GOMODCACHE=" + filepath.Join(projectCache, "modules"),
		"TEST_TELEMETRY_DIR=/phase/go-telemetry",
		"XDG_CACHE_HOME=/phase/cache",
		"GOTMPDIR=/phase/go-tmp",
		"TMPDIR=/phase/go-tmp",
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
	argv, _, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{
		RepoRoot: "/repo", PhaseDir: phaseDir, SSHAuthSock: "/run/koryph-signing/signing.sock",
	}))
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

func TestCommandWithoutSigningKeepsNonModuleCachesPhaseLocal(t *testing.T) {
	_, env, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{RepoRoot: "/repo", PhaseDir: "/phase"}))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	projectCache := filepath.Join("/repo", ".git", "koryph-cache", "go")
	for _, want := range []string{
		"GOCACHE=" + filepath.Join(projectCache, "build-"+goBuildCacheKey(goruntime.Version(), goruntime.GOOS, goruntime.GOARCH)),
		"GOMODCACHE=" + filepath.Join(projectCache, "modules"),
		"TEST_TELEMETRY_DIR=/phase/go-telemetry",
		"XDG_CACHE_HOME=/phase/cache",
		"GOTMPDIR=/phase/go-tmp",
		"TMPDIR=/phase/go-tmp",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "PRE_COMMIT_HOME=") {
		t.Errorf("ordinary workspace-write launch inherited signing-only PRE_COMMIT_HOME:\n%s", joined)
	}
}

func TestCommandSharesProjectGoCachesAcrossPhases(t *testing.T) {
	t.Setenv("GOMODCACHE", "/ambient/go-mod-cache")
	t.Setenv("GOCACHE", "/ambient/go-cache")

	envFor := func(repo, phase string) map[string]string {
		t.Helper()
		_, env, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{
			RepoRoot: repo,
			PhaseDir: phase,
		}))
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]string)
		for _, pair := range env {
			name, value, ok := strings.Cut(pair, "=")
			if ok {
				got[name] = value
			}
		}
		return got
	}

	first := envFor("/repo-a", "/phase-a")
	second := envFor("/repo-a", "/phase-b")
	otherRepo := envFor("/repo-b", "/phase-c")

	wantRoot := filepath.Join("/repo-a", ".git", "koryph-cache", "go")
	wantModuleCache := filepath.Join(wantRoot, "modules")
	wantBuildCache := filepath.Join(wantRoot, "build-"+goBuildCacheKey(goruntime.Version(), goruntime.GOOS, goruntime.GOARCH))
	if first["GOMODCACHE"] != wantModuleCache || second["GOMODCACHE"] != wantModuleCache {
		t.Fatalf("same-repo module caches = %q, %q; want shared %q",
			first["GOMODCACHE"], second["GOMODCACHE"], wantModuleCache)
	}
	if first["GOCACHE"] != wantBuildCache || second["GOCACHE"] != wantBuildCache {
		t.Fatalf("same-repo build caches = %q, %q; want shared %q",
			first["GOCACHE"], second["GOCACHE"], wantBuildCache)
	}
	if otherRepo["GOMODCACHE"] == wantModuleCache {
		t.Fatalf("separate repo reused module cache %q", otherRepo["GOMODCACHE"])
	}
	if otherRepo["GOCACHE"] == wantBuildCache {
		t.Fatalf("separate repo reused build cache %q", otherRepo["GOCACHE"])
	}
	if first["GOMODCACHE"] == os.Getenv("GOMODCACHE") {
		t.Fatalf("dispatch inherited ambient module cache %q", first["GOMODCACHE"])
	}
	if first["GOCACHE"] == os.Getenv("GOCACHE") {
		t.Fatalf("dispatch inherited ambient build cache %q", first["GOCACHE"])
	}
	for _, name := range []string{"TEST_TELEMETRY_DIR", "XDG_CACHE_HOME", "TMPDIR", "GOTMPDIR"} {
		if first[name] == second[name] {
			t.Errorf("%s unexpectedly shared across phases: %q", name, first[name])
		}
	}
}

func TestGoBuildCacheKeySeparatesToolchainAndTarget(t *testing.T) {
	base := goBuildCacheKey("go1.26.0", "darwin", "arm64")
	for _, changed := range []string{
		goBuildCacheKey("go1.26.1", "darwin", "arm64"),
		goBuildCacheKey("go1.26.0", "linux", "arm64"),
		goBuildCacheKey("go1.26.0", "darwin", "amd64"),
	} {
		if changed == base {
			t.Fatalf("cache key collision: %q", base)
		}
	}
	if strings.Contains(base, "/") || strings.Contains(base, `\`) {
		t.Fatalf("cache key is not path-safe: %q", base)
	}
}

func TestProjectGoCacheRejectsRedirectedMetadata(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, ".git", "koryph-cache")); err != nil {
		t.Fatal(err)
	}
	if err := ensureProjectGoCache(repo, goruntime.GOOS, goruntime.GOARCH); err == nil ||
		!strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("redirected cache error = %v", err)
	}
}

func TestProjectGoCacheCreatesPrivateProjectRoots(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureProjectGoCache(repo, goruntime.GOOS, goruntime.GOARCH); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(repo, ".git", "koryph-cache"),
		filepath.Join(repo, ".git", "koryph-cache", "go"),
		filepath.Join(repo, ".git", "koryph-cache", "go", "modules"),
		filepath.Join(repo, ".git", "koryph-cache", "go", "build-"+
			goBuildCacheKey(goruntime.Version(), goruntime.GOOS, goruntime.GOARCH)),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			t.Fatalf("cache root %s mode = %v", path, info.Mode())
		}
	}
}

func TestProjectGoCacheRejectsRedirectedBuildLeaf(t *testing.T) {
	repo := t.TempDir()
	goRoot := filepath.Join(repo, ".git", "koryph-cache", "go")
	if err := os.MkdirAll(goRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	build := filepath.Join(goRoot, "build-"+
		goBuildCacheKey(goruntime.Version(), goruntime.GOOS, goruntime.GOARCH))
	if err := os.Symlink(t.TempDir(), build); err != nil {
		t.Fatal(err)
	}
	if err := ensureProjectGoCache(repo, goruntime.GOOS, goruntime.GOARCH); err == nil ||
		!strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("redirected build cache error = %v", err)
	}
}

func TestCommandBuildCacheUsesEffectiveDispatchedTarget(t *testing.T) {
	t.Setenv("GOOS", "linux")
	t.Setenv("GOARCH", "amd64")
	_, env, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{
		RepoRoot:       "/repo",
		PhaseDir:       "/phase",
		EnvPassthrough: []string{"GOOS", "GOARCH"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(
		"/repo", ".git", "koryph-cache", "go",
		"build-"+goBuildCacheKey(goruntime.Version(), "linux", "amd64"),
	)
	if got := envMap(env)["GOCACHE"]; got != want {
		t.Fatalf("target cache %q missing from %v", want, env)
	}
}

func TestCommandBuildCacheUsesDispatchedToolchainNotKoryphBuild(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	phase := canonicalCodexTempDir(t)
	previous := resolveDispatchedGoVersion
	resolveDispatchedGoVersion = func(string, string, []string) (string, error) {
		return "go9.9.1", nil
	}
	t.Cleanup(func() { resolveDispatchedGoVersion = previous })

	_, env, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{
		RepoRoot: repo,
		PhaseDir: phase,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(
		repo, ".git", "koryph-cache", "go",
		"build-"+goBuildCacheKey("go9.9.1", goruntime.GOOS, goruntime.GOARCH),
	)
	if got := envMap(env)["GOCACHE"]; got != want {
		t.Fatalf("GOCACHE = %q, want dispatched-toolchain cache %q", got, want)
	}
	if strings.Contains(want, goBuildCacheKey(goruntime.Version(), goruntime.GOOS, goruntime.GOARCH)) {
		t.Fatal("test toolchain key unexpectedly matched Koryph build toolchain")
	}
}

func TestUnresolvedDispatchedToolchainFallsBackToPhaseUniqueCache(t *testing.T) {
	repo := t.TempDir()
	previous := resolveDispatchedGoVersion
	resolveDispatchedGoVersion = func(string, string, []string) (string, error) {
		return "", errors.New("probe unavailable")
	}
	t.Cleanup(func() { resolveDispatchedGoVersion = previous })
	first := effectiveGoToolchainVersion(repo, "/phase/one", []string{"PATH=/tools"})
	second := effectiveGoToolchainVersion(repo, "/phase/two", []string{"PATH=/tools"})
	if first == second {
		t.Fatalf("unresolved toolchain fallback shared across phases: %q", first)
	}
}

func TestPhaseScratchRejectsRedirectedTempLeaf(t *testing.T) {
	phase := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(phase, "go-tmp")); err != nil {
		t.Fatal(err)
	}
	if err := ensurePhaseTempDir(phase); err == nil ||
		!strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("redirected temp error = %v", err)
	}
}

func TestCommandWithoutRepoRootKeepsModuleCachePhaseLocal(t *testing.T) {
	_, env, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{PhaseDir: "/phase"}))
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(env, "\n"); !strings.Contains(joined, "GOMODCACHE=/phase/go-mod-cache") {
		t.Errorf("repo-less dispatch must not use an ambient or shared module cache:\n%s", joined)
	}
}

func TestCommandInstallsPolicyFreeBinaryForwardingShims(t *testing.T) {
	phaseDir := canonicalCodexTempDir(t)
	fakeBin := canonicalCodexTempDir(t)
	guardBin := writeCodexTestExecutable(t, canonicalCodexTempDir(t), "koryph", "exit 99")
	real := make(map[string]string)
	for _, name := range []string{"make", "go", "golangci-lint"} {
		real[name] = writeCodexTestExecutable(t, fakeBin, name, "exit 0")
	}
	previous := commandGuardExecutable
	commandGuardExecutable = func() (string, error) { return guardBin, nil }
	t.Cleanup(func() { commandGuardExecutable = previous })
	t.Setenv("PATH", fakeBin+string(filepath.ListSeparator)+os.Getenv("PATH"))
	t.Setenv("KORYPH_COMMAND_ROLE", "validation")
	t.Setenv("KORYPH_COMMAND_EVENTS", filepath.Join(t.TempDir(), "outside-events"))
	t.Setenv("KORYPH_COMMAND_GUARD_DIR", t.TempDir())

	_, env, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{PhaseDir: phaseDir}))
	if err != nil {
		t.Fatal(err)
	}
	values := envMap(env)
	for _, name := range []string{"KORYPH_COMMAND_ROLE", "KORYPH_COMMAND_EVENTS", "KORYPH_COMMAND_GUARD_DIR"} {
		if _, present := values[name]; present {
			t.Fatalf("deprecated shell-policy input %s leaked into dispatch", name)
		}
	}
	shimDir := strings.Split(values["PATH"], string(filepath.ListSeparator))[0]
	for name, realPath := range real {
		data, err := os.ReadFile(filepath.Join(shimDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(data), renderCommandGuardShim(guardBin, realPath); got != want {
			t.Fatalf("%s shim:\n%s\nwant:\n%s", name, got, want)
		}
		for _, forbidden := range []string{"KORYPH_COMMAND_ROLE", "KORYPH_COMMAND_EVENTS", "KORYPH_COMMAND_GUARD_DIR", "mkdir", "kill"} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("%s shim contains policy token %q:\n%s", name, forbidden, data)
			}
		}
	}
}

func TestInstallPhaseCommandGuardRejectsSymlinkedPaths(t *testing.T) {
	t.Run("phase", func(t *testing.T) {
		link := filepath.Join(canonicalCodexTempDir(t), "phase-link")
		if err := os.Symlink(canonicalCodexTempDir(t), link); err != nil {
			t.Fatal(err)
		}
		if _, err := installPhaseCommandGuard(link, os.Environ()); err == nil {
			t.Fatal("symlinked phase path was accepted")
		}
	})
	t.Run("shim directory", func(t *testing.T) {
		phase := canonicalCodexTempDir(t)
		if err := os.Symlink(canonicalCodexTempDir(t), filepath.Join(phase, "command-guard-bin")); err != nil {
			t.Fatal(err)
		}
		if _, err := installPhaseCommandGuard(phase, os.Environ()); err == nil {
			t.Fatal("symlinked shim directory was accepted")
		}
	})
}

func TestCodexAdapterCommandShimUsesBinarySingleFlight(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	buildDir := canonicalCodexTempDir(t)
	guardBin := filepath.Join(buildDir, "koryph")
	build := exec.Command("go", "build", "-o", guardBin, "./cmd/koryph")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build command guard binary: %v\n%s", err, out)
	}

	phaseDir := canonicalCodexTempDir(t)
	fakeBin := canonicalCodexTempDir(t)
	starts := filepath.Join(canonicalCodexTempDir(t), "starts")
	writeCodexTestExecutable(t, fakeBin, "go", `echo start >>"`+starts+`"; sleep 1; exit 7`)
	previous := commandGuardExecutable
	commandGuardExecutable = func() (string, error) { return guardBin, nil }
	t.Cleanup(func() { commandGuardExecutable = previous })
	t.Setenv("PATH", fakeBin+string(filepath.ListSeparator)+os.Getenv("PATH"))
	_, env, err := (Codex{Bin: "codex"}).Command(canonicalDispatchSpec(runtime.DispatchSpec{PhaseDir: phaseDir}))
	if err != nil {
		t.Fatal(err)
	}
	values := envMap(env)
	shim := filepath.Join(strings.Split(values["PATH"], string(filepath.ListSeparator))[0], "go")
	outsideEvents := filepath.Join(canonicalCodexTempDir(t), "outside-events")
	if err := os.WriteFile(outsideEvents, []byte("sentinel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = setEnv(env, "KORYPH_PHASE_ID", filepath.Base(phaseDir))
	env = setEnv(env, "KORYPH_PHASE_DIR", phaseDir)
	env = setEnv(env, "KORYPH_COMMAND_ROLE", "validation")
	env = setEnv(env, "KORYPH_COMMAND_EVENTS", outsideEvents)
	env = setEnv(env, "KORYPH_COMMAND_GUARD_DIR", canonicalCodexTempDir(t))

	first := exec.Command(shim, "test", "./...")
	second := exec.Command(shim, "test", "./...")
	first.Env, second.Env = env, env
	var firstOut, secondOut bytes.Buffer
	first.Stdout, first.Stderr = &firstOut, &firstOut
	second.Stdout, second.Stderr = &secondOut, &secondOut
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := second.Start(); err != nil {
		_ = first.Process.Signal(syscall.SIGTERM)
		t.Fatal(err)
	}
	firstErr, secondErr := first.Wait(), second.Wait()
	for name, runErr := range map[string]error{"first": firstErr, "second": secondErr} {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 7 {
			t.Fatalf("%s exit = %v\nfirst:\n%s\nsecond:\n%s", name, runErr, firstOut.String(), secondOut.String())
		}
	}
	data, err := os.ReadFile(starts)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "start"); got != 1 {
		t.Fatalf("real broad command starts = %d, want 1:\n%s", got, data)
	}
	combined := firstOut.String() + secondOut.String()
	for _, want := range []string{"koryph command reuse: pid=", "status=running", "log="} {
		if !strings.Contains(combined, want) {
			t.Fatalf("reuse output missing %q:\n%s", want, combined)
		}
	}
	events, err := os.ReadFile(filepath.Join(phaseDir, ".koryph-command", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"start"`, `"event":"reuse"`, `"event":"complete"`, `"peak_rss_kb":`, `"cpu_seconds":`, `"duration_ms":`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("structured evidence missing %s:\n%s", want, events)
		}
	}
	if outside, err := os.ReadFile(outsideEvents); err != nil || string(outside) != "sentinel\n" {
		t.Fatalf("forged evidence path was used: %q, %v", outside, err)
	}
}

// TestRealCodexSingleFlightCanary is an opt-in release canary. It asks the
// real standard-tier Codex runtime to launch the same slow broad command
// twice concurrently through the adapter-installed PATH shim. The fake tool's
// overlap tripwire catches any unguarded second start; generation-bound event
// and result checks prove the duplicate reused the authoritative worker.
func TestRealCodexSingleFlightCanary(t *testing.T) {
	if os.Getenv("KORYPH_REAL_CODEX_CANARY") != "1" {
		t.Skip("set KORYPH_REAL_CODEX_CANARY=1 for the bounded release canary")
	}
	codexBin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("real Codex CLI is unavailable:", err)
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	phaseDir := canonicalCodexTempDir(t)
	guardBin := filepath.Join(phaseDir, "koryph")
	build := exec.Command("go", "build", "-o", guardBin, "./cmd/koryph")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build command guard binary: %v\n%s", err, out)
	}

	worktree := canonicalCodexTempDir(t)
	initRepo := exec.Command("git", "init", "-q")
	initRepo.Dir = worktree
	if out, err := initRepo.CombinedOutput(); err != nil {
		t.Fatalf("initialize canary repo: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(worktree, "go.mod"), []byte("module canary.invalid/singleflight\n\ngo 1.25.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(phaseDir, "real-bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	starts := filepath.Join(phaseDir, "real-starts")
	overlaps := filepath.Join(phaseDir, "overlap-violations")
	tripwire := filepath.Join(phaseDir, "real-tool-active")
	body := "if ! mkdir " + shellSingleQuote(tripwire) + "; then\n" +
		"  echo overlap >>" + shellSingleQuote(overlaps) + "\n" +
		"  exit 89\n" +
		"fi\n" +
		"trap 'rmdir " + shellSingleQuote(tripwire) + " 2>/dev/null || true' EXIT INT TERM\n" +
		"echo start >>" + shellSingleQuote(starts) + "\n" +
		"sleep 3\n" +
		"echo guarded-ok\n"
	realGo := writeCodexTestExecutable(t, fakeBin, "go", body)

	previous := commandGuardExecutable
	commandGuardExecutable = func() (string, error) { return guardBin, nil }
	t.Cleanup(func() { commandGuardExecutable = previous })
	t.Setenv("PATH", fakeBin+string(filepath.ListSeparator)+os.Getenv("PATH"))
	model := runtime.CodexModelMap[runtime.TierStandard]
	argv, env, err := (Codex{Bin: codexBin}).Command(canonicalDispatchSpec(runtime.DispatchSpec{
		RepoRoot: worktree, PhaseDir: phaseDir, Model: model, Effort: "medium",
	}))
	if err != nil {
		t.Fatal(err)
	}
	phaseID := filepath.Base(phaseDir)
	env = setEnv(env, "KORYPH_RUN_ID", "real-codex-singleflight-canary")
	env = setEnv(env, "KORYPH_PHASE_ID", phaseID)
	env = setEnv(env, "KORYPH_PHASE_DIR", phaseDir)
	env = setEnv(env, "KORYPH_DIR", phaseDir)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = worktree
	cmd.Env = env
	cmd.Stdin = strings.NewReader(
		"Run exactly this one shell command and wait for it to finish:\n\n" +
			"go test ./... & go test ./... & wait\n\n" +
			"Do not inspect or modify files and do not run any other command. " +
			"After it finishes, reply CANARY_DONE.",
	)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("real Codex canary failed: %v (context=%v)\n%s", err, ctx.Err(), output.String())
	}
	if !strings.Contains(output.String(), "CANARY_DONE") {
		t.Fatalf("real Codex did not report canary completion:\n%s", output.String())
	}

	startData, err := os.ReadFile(starts)
	if err != nil {
		t.Fatalf("read real-start marker: %v\n%s", err, output.String())
	}
	if got := strings.Count(string(startData), "start"); got != 1 {
		t.Fatalf("real broad command starts = %d, want 1:\n%s\nCodex:\n%s", got, startData, output.String())
	}
	if data, err := os.ReadFile(overlaps); err == nil && len(bytes.TrimSpace(data)) > 0 {
		t.Fatalf("overlap tripwire fired:\n%s\nCodex:\n%s", data, output.String())
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	eventsPath := filepath.Join(phaseDir, ".koryph-command", "events.jsonl")
	eventData, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	generation := ""
	for _, line := range bytes.Split(bytes.TrimSpace(eventData), []byte{'\n'}) {
		var event resmon.CommandEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode command event: %v\n%s", err, line)
		}
		counts[event.Event]++
		if event.Event == "start" || event.Event == "reuse" || event.Event == "complete" {
			if generation == "" {
				generation = event.Generation
			}
			if event.Generation != generation {
				t.Fatalf("event generation = %q, want %q:\n%s", event.Generation, generation, eventData)
			}
		}
		if event.Event == "reuse" && (event.Role != "worker" || event.ExistingRole != "worker") {
			t.Fatalf("reuse roles = %q/%q, want worker/worker", event.Role, event.ExistingRole)
		}
	}
	for _, kind := range []string{"start", "reuse", "complete"} {
		if counts[kind] != 1 {
			t.Fatalf("%s events = %d, want 1:\n%s", kind, counts[kind], eventData)
		}
	}

	results, err := filepath.Glob(filepath.Join(phaseDir, ".koryph-command", "result-*.json"))
	if err != nil || len(results) != 1 {
		t.Fatalf("result files = %v, err=%v", results, err)
	}
	resultData, err := os.ReadFile(results[0])
	if err != nil {
		t.Fatal(err)
	}
	var result commandguard.CommandResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		t.Fatal(err)
	}
	if result.Generation != generation || result.Role != commandguard.CommandRoleWorker ||
		result.Status != "passed" || result.ExitCode != 0 {
		t.Fatalf("unexpected authoritative result: %+v", result)
	}
	if _, err := os.Stat(tripwire); !os.IsNotExist(err) {
		t.Fatalf("real-command tripwire survived completion: %v", err)
	}
	held, err := commandguard.NewProcessGuard(phaseDir).OwnerLeaseHeld(
		[]string{realGo, "test", "./..."},
	)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("authoritative command owner lease survived completion")
	}
	if err := syscall.Kill(-result.Identity.ProcessGroup, 0); err == nil {
		t.Fatalf("authoritative command process group survived completion: %+v", result.Identity)
	} else if !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("probe authoritative process group: %v", err)
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, pair := range env {
		name, value, ok := strings.Cut(pair, "=")
		if ok {
			out[name] = value
		}
	}
	return out
}

func canonicalCodexTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeCodexTestExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestCommandJSONUsesScratchLocalMutableCaches(t *testing.T) {
	_, env, err := (Codex{Bin: "codex"}).CommandJSON(runtime.JSONSpec{RepoRoot: "/repo", ScratchDir: "/scratch"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"GOCACHE=/scratch/go-cache",
		"GOMODCACHE=/scratch/go-mod-cache",
		"GOTMPDIR=/scratch/go-tmp",
		"TMPDIR=/scratch/go-tmp",
		"TEST_TELEMETRY_DIR=/scratch/go-telemetry",
	} {
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
	if !ok || second.Kind != runtime.EventResult || !second.HasUsage ||
		second.InputTokens != 6 || second.CacheReadTokens != 4 || second.OutputTokens != 3 ||
		second.TokenSemantics != runtime.TokenSemanticsDisjointV1 ||
		!second.HasProviderTotalInput || second.ProviderTotalInputTokens != 10 {
		t.Fatalf("second event = %+v", second)
	}
	third, ok, _ := es.Next()
	if !ok || third.Kind != runtime.EventError || !third.RateLimited {
		t.Fatalf("third event = %+v", third)
	}
}

func TestRenderPersonaAndPromptUseCanonicalSource(t *testing.T) {
	role := "<!-- koryph-clause:role/v1 -->\nFollow the protocol."
	rendered, err := (Codex{}).RenderPersona("p", []byte("---\nmodel: standard\n---\n"+role))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(rendered); !strings.Contains(got, "developer_instructions") || strings.Contains(got, "model: standard") {
		t.Errorf("unexpected rendered persona:\n%s", got)
	}
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "AGENTS.md"),
		[]byte("<!-- koryph-clause:repository/v1 -->\nRepository rules."),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "p.md"), []byte(role), 0o644); err != nil {
		t.Fatal(err)
	}
	taskPrompt := promptc.Compile(promptc.Input{
		EngineVersion: "test",
		ProjectName:   "test",
		Bead:          beads.Issue{ID: "koryph-test", Title: "Do the task"},
	})
	prompt, err := (Codex{}).PreparePrompt(root, "p", taskPrompt)
	if err != nil || !strings.Contains(prompt, "Follow the protocol.") || !strings.Contains(prompt, "Do the task") {
		t.Fatalf("prompt=%q err=%v", prompt, err)
	}
	if strings.Contains(prompt, promptc.RepositoryClauseMarker) {
		t.Fatal("native repository clause was duplicated into Codex prompt")
	}
}

func TestPreparePromptRejectsMissingOrDuplicateSemanticOwners(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(root, "agents", "p.md")
	if err := os.WriteFile(rolePath, []byte("<!-- koryph-clause:role/v1 -->"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Codex{}).PreparePrompt(root, "p", "plain review"); err == nil {
		t.Fatal("missing repository contract was accepted")
	}
	if err := os.WriteFile(
		filepath.Join(root, "AGENTS.md"),
		[]byte("<!-- koryph-clause:repository/v1 -->"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		rolePath,
		[]byte("<!-- koryph-clause:role/v1 -->\n<!-- koryph-clause:engine/v1 -->"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := (Codex{}).PreparePrompt(root, "p", "plain review"); err == nil {
		t.Fatal("persona with a foreign semantic owner was accepted")
	}
}
