// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runSpill invokes hooks/koryph-spill.sh with the given label and command
// argv, using dir as the phase dir (KORYPH_PHASE_DIR) so the spill file
// location is hermetic and inspectable. Returns combined stdout+stderr and
// the process exit code.
func runSpill(t *testing.T, dir, label string, cmdArgv ...string) (output string, exitCode int) {
	t.Helper()
	args := append([]string{"koryph-spill.sh", label, "--"}, cmdArgv...)
	cmd := exec.Command("bash", args...)
	cmd.Env = append(os.Environ(), "KORYPH_PHASE_DIR="+dir)
	out, err := cmd.CombinedOutput()
	output = string(out)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return output, ee.ExitCode()
		}
		t.Fatalf("running koryph-spill.sh: %v\noutput:\n%s", err, output)
	}
	return output, 0
}

// spillFile returns the single spill-<label>-*.log file written to dir,
// failing the test if there isn't exactly one.
func spillFile(t *testing.T, dir, label string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "spill-"+label+"-*.log"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one spill-%s-*.log in %s, got %v", label, dir, matches)
	}
	return matches[0]
}

// TestSpillFileByteEqualsRawOutput is contract (a): the spill file must be
// byte-identical to the wrapped command's combined stdout+stderr.
func TestSpillFileByteEqualsRawOutput(t *testing.T) {
	dir := t.TempDir()
	script := `printf 'out-1\n'; printf 'err-1\n' >&2; printf 'out-2\n'`
	out, code := runSpill(t, dir, "byteeq", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0:\n%s", code, out)
	}
	path := spillFile(t, dir, "byteeq")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	want := "out-1\nerr-1\nout-2\n"
	if string(got) != want {
		t.Errorf("spill file = %q, want %q (byte-identical to combined stdout+stderr)", got, want)
	}
}

// TestSpillPreservesExitCode is contract (b).
func TestSpillPreservesExitCode(t *testing.T) {
	dir := t.TempDir()
	_, code := runSpill(t, dir, "exitcode", "sh", "-c", "exit 7")
	if code != 7 {
		t.Errorf("exit code = %d, want 7 (preserved from the wrapped command)", code)
	}
}

// TestSpillSmallOutputPrintedUnmodifiedNoSpillNote is contract (d): output
// under the head+tail budget prints as-is, with no "full output:" pointer
// and nothing marked as elided.
func TestSpillSmallOutputPrintedUnmodifiedNoSpillNote(t *testing.T) {
	dir := t.TempDir()
	out, code := runSpill(t, dir, "small", "sh", "-c", "printf 'line1\\nline2\\nline3\\n'")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0:\n%s", code, out)
	}
	if out != "line1\nline2\nline3\n" {
		t.Errorf("output = %q, want the raw 3 lines unmodified", out)
	}
	if strings.Contains(out, "full output:") {
		t.Errorf("small output must not carry a spill note:\n%s", out)
	}
	if strings.Contains(out, "elided") {
		t.Errorf("small output must not be marked as elided:\n%s", out)
	}
}

// TestSpillErrorLinesNeverDroppedFromElidedMiddle is contract (c): an
// error-shaped line buried in the elided middle (outside both the printed
// head and tail windows) must still reach the printed summary verbatim,
// under an "errors" section — the summarizer must never eat a failure
// signal (design I3).
func TestSpillErrorLinesNeverDroppedFromElidedMiddle(t *testing.T) {
	dir := t.TempDir()
	// 100 lines total; default head=20, tail=40 -> elided middle is lines
	// 21-60. Line 50 sits well inside that window.
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		if i == 50 {
			fmt.Fprintln(&b, "boom: something FAILED unexpectedly")
		} else {
			fmt.Fprintf(&b, "noise line %d\n", i)
		}
	}
	script := "cat <<'BODY'\n" + b.String() + "BODY\n"
	out, code := runSpill(t, dir, "errmid", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "boom: something FAILED unexpectedly") {
		t.Errorf("error-shaped line from the elided middle was dropped:\n%s", out)
	}
	if !strings.Contains(out, "errors") {
		t.Errorf("expected an errors section:\n%s", out)
	}
	if !strings.Contains(out, "full output:") {
		t.Errorf("output over budget must carry a spill note:\n%s", out)
	}
	if strings.Contains(out, "noise line 50") {
		t.Fatalf("test construction bug: line 50 should have been the error line, not noise")
	}
}

// TestSpillErrorKeywordsCaseInsensitive proves each of the four
// error-keyword shapes (error/FAIL/panic/fatal) is detected regardless of
// case, mirroring I3's "error lines are verbatim" intent.
func TestSpillErrorKeywordsCaseInsensitive(t *testing.T) {
	keywords := []string{"Error", "FAIL", "Panic", "fatal"}
	for _, kw := range keywords {
		t.Run(kw, func(t *testing.T) {
			dir := t.TempDir()
			var b strings.Builder
			marker := "marker-line-with-" + kw
			for i := 1; i <= 100; i++ {
				if i == 50 {
					fmt.Fprintln(&b, marker)
				} else {
					fmt.Fprintf(&b, "noise line %d\n", i)
				}
			}
			script := "cat <<'BODY'\n" + b.String() + "BODY\n"
			out, code := runSpill(t, dir, "kw-"+kw, "sh", "-c", script)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0:\n%s", code, out)
			}
			if !strings.Contains(out, marker) {
				t.Errorf("keyword %q not preserved from elided middle:\n%s", kw, out)
			}
		})
	}
}

// TestSpillNeverClobbersExistingSpillFiles proves repeat invocations with
// the same label get distinct, incrementing spill files rather than
// overwriting each other.
func TestSpillNeverClobbersExistingSpillFiles(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if _, code := runSpill(t, dir, "dup", "sh", "-c", "printf 'run\\n'"); code != 0 {
			t.Fatalf("run %d: exit code %d", i, code)
		}
	}
	matches, err := filepath.Glob(filepath.Join(dir, "spill-dup-*.log"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 3 {
		t.Errorf("expected 3 distinct spill files after 3 runs, got %d: %v", len(matches), matches)
	}
}

// TestSpillRequiresCommand proves the usage-error paths (missing "--",
// missing command) fail loudly rather than silently running nothing.
func TestSpillRequiresCommand(t *testing.T) {
	dir := t.TempDir()

	// Missing the "--" separator entirely (runSpill always inserts one, so
	// this case builds argv directly).
	cmd := exec.Command("bash", "koryph-spill.sh", "nolabelsep", "echo", "hi")
	cmd.Env = append(os.Environ(), "KORYPH_PHASE_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("missing '--' separator: want non-zero exit, got 0:\n%s", out)
	}

	if _, code := runSpill(t, dir, "nocmd"); code == 0 {
		t.Error("missing command after '--': want non-zero exit, got 0")
	}
}

// TestSpillHeadTailBudgetOverride proves KORYPH_SPILL_HEAD_LINES /
// KORYPH_SPILL_TAIL_LINES retune the elision budget (a test/tuning seam
// documented in the script header).
func TestSpillHeadTailBudgetOverride(t *testing.T) {
	dir := t.TempDir()
	args := []string{"koryph-spill.sh", "budget", "--", "sh", "-c", "printf 'a\\nb\\nc\\nd\\ne\\n'"}
	cmd := exec.Command("bash", args...)
	cmd.Env = append(os.Environ(), "KORYPH_PHASE_DIR="+dir, "KORYPH_SPILL_HEAD_LINES=1", "KORYPH_SPILL_TAIL_LINES=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("koryph-spill.sh: %v\noutput:\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "full output:") {
		t.Errorf("5 lines over a 2-line budget should elide and note the spill path:\n%s", got)
	}
	if !strings.Contains(got, "elided") {
		t.Errorf("expected an elision marker with a tightened budget:\n%s", got)
	}
}
