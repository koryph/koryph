// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// fakeBd installs an executable named "bd" in a fresh temp dir, containing
// script as its body, and returns the dir so it can be prepended to PATH.
func fakeBd(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bd")
	body := "#!/usr/bin/env bash\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	return dir
}

// pathWithoutBd returns the current PATH with every directory that would
// resolve a real `bd` binary stripped out, so tests can exercise the
// missing-bd fail-open path without breaking bash's own PATH-resolved
// dependencies (cat, mktemp, wc, tr, date, mkdir).
func pathWithoutBd(t *testing.T) string {
	t.Helper()
	parts := filepath.SplitList(os.Getenv("PATH"))
	out := make([]string, 0, len(parts))
	for _, dir := range parts {
		if _, err := os.Stat(filepath.Join(dir, "bd")); err == nil {
			continue // this dir would resolve a real bd — drop it
		}
		out = append(out, dir)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

// runPrimeOpts controls one invocation of koryph-prime.sh under test.
type runPrimeOpts struct {
	spawnKind string // KORYPH_SPAWN_KIND, empty = unset
	phaseID   string // KORYPH_PHASE_ID, empty = unset
	koryphDir string // KORYPH_DIR, empty = unset
	pathDirs  string // full PATH override; empty = inherit os.Environ's PATH
}

// runPrime invokes hooks/koryph-prime.sh and returns its stdout, stderr, and
// exit code.
func runPrime(t *testing.T, opts runPrimeOpts) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command("bash", "koryph-prime.sh")
	env := os.Environ()
	if opts.pathDirs != "" {
		env = append(env, "PATH="+opts.pathDirs)
	}
	if opts.spawnKind != "" {
		env = append(env, "KORYPH_SPAWN_KIND="+opts.spawnKind)
	}
	if opts.phaseID != "" {
		env = append(env, "KORYPH_PHASE_ID="+opts.phaseID)
	}
	if opts.koryphDir != "" {
		env = append(env, "KORYPH_DIR="+opts.koryphDir)
	}
	cmd.Env = env
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("running koryph-prime.sh: %v\nstderr:\n%s", err, errBuf.String())
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// TestFullModeByteIdenticalToRawBdPrime is the golden test: with no
// KORYPH_SPAWN_KIND set (the main-dispatch / interactive shape), the
// wrapper's stdout must be byte-for-byte identical to what `bd prime
// --hook-json` itself would have emitted directly — the wrapper must not
// add, drop, or reflow a single byte of the injected context.
func TestFullModeByteIdenticalToRawBdPrime(t *testing.T) {
	known := `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"line one\nline two\n"}}` + "\n"
	dir := fakeBd(t, `
if [[ "$1" == "prime" && "$2" == "--hook-json" ]]; then
  printf '%s' `+shQuote(known)+`
  exit 0
fi
exit 1
`)
	out, _, code := runPrime(t, runPrimeOpts{pathDirs: dir + string(filepath.ListSeparator) + os.Getenv("PATH")})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out != known {
		t.Errorf("stdout = %q, want byte-identical raw bd prime output %q", out, known)
	}
}

// TestFullModeIsDefaultWhenSpawnKindUnsetOrUnknown proves full mode (bd
// invoked, output relayed) is the conservative default for every shape that
// is NOT one of the three slim kinds: unset (interactive/operator), a main
// dispatch (KORYPH_PHASE_ID set, no spawn kind), and an unrecognized value.
func TestFullModeIsDefaultWhenSpawnKindUnsetOrUnknown(t *testing.T) {
	marker := `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"full-marker"}}`
	dir := fakeBd(t, `printf '%s' `+shQuote(marker)+`; exit 0`)
	fullPath := dir + string(filepath.ListSeparator) + os.Getenv("PATH")

	cases := []struct {
		name string
		opts runPrimeOpts
	}{
		{"unset spawn kind", runPrimeOpts{pathDirs: fullPath}},
		{"main dispatch (phase id, no spawn kind)", runPrimeOpts{pathDirs: fullPath, phaseID: "koryph-77r.4"}},
		{"unrecognized spawn kind", runPrimeOpts{pathDirs: fullPath, spawnKind: "banana"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, code := runPrime(t, tc.opts)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if out != marker {
				t.Errorf("stdout = %q, want the full bd prime marker output %q", out, marker)
			}
		})
	}
}

// TestSlimModeForEachSecondarySpawnKind proves review/stage/epicreview all
// get a small hook-json-shaped substitute payload, under the ~500 byte
// budget, WITHOUT bd ever being invoked (a fake bd that errors loudly
// proves that — if it ran, its distinctive output would show up).
func TestSlimModeForEachSecondarySpawnKind(t *testing.T) {
	dir := fakeBd(t, `echo "FAKE BD SHOULD NOT HAVE BEEN INVOKED" >&2; exit 99`)
	fullPath := dir + string(filepath.ListSeparator) + os.Getenv("PATH")

	for _, kind := range []string{"review", "stage", "epicreview"} {
		t.Run(kind, func(t *testing.T) {
			out, _, code := runPrime(t, runPrimeOpts{pathDirs: fullPath, spawnKind: kind})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if len(out) > 500 {
				t.Errorf("slim payload for %q is %d bytes, want <= 500:\n%s", kind, len(out), out)
			}
			if !strings.Contains(out, kind) {
				t.Errorf("slim payload for %q doesn't mention its own kind:\n%s", kind, out)
			}
			if !strings.Contains(out, `"hookSpecificOutput"`) || !strings.Contains(out, `"additionalContext"`) {
				t.Errorf("slim payload for %q isn't hook-json-shaped:\n%s", kind, out)
			}
			if strings.Contains(out, "SHOULD NOT HAVE BEEN INVOKED") {
				t.Errorf("slim mode for %q invoked the fake bd, it must not: %s", kind, out)
			}
		})
	}
}

// TestFailOpenWhenBdMissing proves that when bd cannot be found on PATH at
// all, the wrapper still exits 0 (never wedges session start) with empty
// stdout.
func TestFailOpenWhenBdMissing(t *testing.T) {
	out, _, code := runPrime(t, runPrimeOpts{pathDirs: pathWithoutBd(t)})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (fail-open on missing bd)", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty (no bd to relay output from)", out)
	}
}

// TestFailOpenWhenBdErrors proves that when bd exits non-zero, whatever it
// printed before failing is still relayed and the wrapper itself still
// exits 0 — a bd-side failure must never propagate into a blocked session
// start.
func TestFailOpenWhenBdErrors(t *testing.T) {
	dir := fakeBd(t, `printf 'partial output before failure\n'; exit 1`)
	out, _, code := runPrime(t, runPrimeOpts{pathDirs: dir + string(filepath.ListSeparator) + os.Getenv("PATH")})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (fail-open despite bd's non-zero exit)", code)
	}
	if out != "partial output before failure\n" {
		t.Errorf("stdout = %q, want bd's partial output relayed verbatim", out)
	}
}

// TestFailOpenWhenBdEmitsUnparsableOutput proves the wrapper doesn't try to
// validate bd's output as JSON before relaying it — an unparsable (but
// successful-exit) response is passed through as-is rather than swallowed.
func TestFailOpenWhenBdEmitsUnparsableOutput(t *testing.T) {
	dir := fakeBd(t, `printf 'not json at all {{{\n'; exit 0`)
	out, _, code := runPrime(t, runPrimeOpts{pathDirs: dir + string(filepath.ListSeparator) + os.Getenv("PATH")})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out != "not json at all {{{\n" {
		t.Errorf("stdout = %q, want bd's unparsable output relayed verbatim", out)
	}
}

// sizeLogLineRE matches one koryph-prime.sh size-log line:
// "<ISO-8601 UTC timestamp> bytes=<n> mode=<token>".
var sizeLogLineRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z bytes=\d+ mode=\S+$`)

// TestSizeLogWritesToKoryphDirNotStdout proves the byte-size measurement is
// appended to $KORYPH_DIR/prime-size.log in the documented format, and never
// leaks into stdout (which is the injected hook context).
func TestSizeLogWritesToKoryphDirNotStdout(t *testing.T) {
	marker := `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"x"}}`
	dir := fakeBd(t, `printf '%s' `+shQuote(marker)+`; exit 0`)
	koryphDir := t.TempDir()

	out, _, code := runPrime(t, runPrimeOpts{
		pathDirs:  dir + string(filepath.ListSeparator) + os.Getenv("PATH"),
		koryphDir: koryphDir,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(out, "bytes=") {
		t.Errorf("size-log line leaked into stdout: %q", out)
	}

	logPath := filepath.Join(koryphDir, "prime-size.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read %s: %v", logPath, err)
	}
	line := strings.TrimRight(string(data), "\n")
	if !sizeLogLineRE.MatchString(line) {
		t.Errorf("size-log line %q doesn't match expected format %q", line, sizeLogLineRE.String())
	}
	if !strings.Contains(line, "bytes="+lenStr(len(marker))) {
		t.Errorf("size-log line %q doesn't report the correct byte count (%d)", line, len(marker))
	}
	if !strings.Contains(line, "mode=full") {
		t.Errorf("size-log line %q should report mode=full", line)
	}
}

// TestSizeLogFallsBackToStderrWithoutKoryphDir proves that when KORYPH_DIR
// is unset, the size measurement still happens (visible on stderr) rather
// than silently vanishing, and stdout stays clean.
func TestSizeLogFallsBackToStderrWithoutKoryphDir(t *testing.T) {
	marker := `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"x"}}`
	dir := fakeBd(t, `printf '%s' `+shQuote(marker)+`; exit 0`)

	out, errOut, code := runPrime(t, runPrimeOpts{pathDirs: dir + string(filepath.ListSeparator) + os.Getenv("PATH")})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(out, "bytes=") {
		t.Errorf("size-log line leaked into stdout: %q", out)
	}
	errLine := strings.TrimRight(errOut, "\n")
	if !sizeLogLineRE.MatchString(errLine) {
		t.Errorf("stderr size-log line %q doesn't match expected format", errLine)
	}
}

// TestSlimModeSizeLogModeIncludesKind proves the size-log mode token
// distinguishes which slim kind was served (not just "slim").
func TestSlimModeSizeLogModeIncludesKind(t *testing.T) {
	dir := fakeBd(t, `echo "should not run" >&2; exit 99`)
	koryphDir := t.TempDir()
	_, _, code := runPrime(t, runPrimeOpts{
		pathDirs:  dir + string(filepath.ListSeparator) + os.Getenv("PATH"),
		spawnKind: "stage",
		koryphDir: koryphDir,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	data, err := os.ReadFile(filepath.Join(koryphDir, "prime-size.log"))
	if err != nil {
		t.Fatalf("read prime-size.log: %v", err)
	}
	if !strings.Contains(string(data), "mode=slim-stage") {
		t.Errorf("size-log %q should report mode=slim-stage", data)
	}
}

// TestFullModeAgainstRealBdOnPath is an integration golden check: when a
// real `bd` binary is on PATH (as in this repo's dev/CI environment), the
// wrapper's full-mode stdout must equal that real binary's raw
// `bd prime --hook-json` output byte-for-byte. Skips gracefully when bd
// isn't installed rather than failing the suite.
func TestFullModeAgainstRealBdOnPath(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not installed on PATH; skipping real-binary golden check")
	}
	raw, err := exec.Command(bdPath, "prime", "--hook-json").Output()
	if err != nil {
		t.Skipf("real bd prime --hook-json failed (%v); skipping golden check", err)
	}
	// Replay the captured bytes through a fake bd for the byte-identity
	// check: bd prime's output reflects LIVE bead/memory state, so letting
	// the wrapper invoke the real bd a second time races any bd write (or
	// rolling relative timestamp) between the two invocations — observed as
	// a 1-byte drift failing an unrelated bead's gate (koryph-yx4). The
	// live capture above keeps the real-output-shape intent; the replay
	// makes the comparison hermetic.
	capture := filepath.Join(t.TempDir(), "bd-prime-capture.json")
	if err := os.WriteFile(capture, raw, 0o644); err != nil {
		t.Fatalf("write capture: %v", err)
	}
	dir := fakeBd(t, `cat `+shQuote(capture))
	out, _, code := runPrime(t, runPrimeOpts{pathDirs: dir + string(filepath.ListSeparator) + os.Getenv("PATH")})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out != string(raw) {
		t.Errorf("wrapper full-mode output diverges from captured `bd prime --hook-json` (len %d vs %d)", len(out), len(raw))
	}
}

// shQuote produces a single-quoted POSIX shell literal for s, suitable for
// embedding a fake bd's expected output inside a generated script body.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func lenStr(n int) string {
	return strconv.Itoa(n)
}
