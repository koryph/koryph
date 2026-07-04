// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
)

// fakeBDNudge is a stand-in `bd` binary for cmdNudge tests: it always
// succeeds and logs its argv (one line per invocation) to $BD_ARGS_LOG, so a
// test can assert exactly what `bd update ... --append-notes` or
// `bd comment` was called with.
const fakeBDNudge = `#!/bin/sh
if [ -n "$BD_ARGS_LOG" ]; then
  echo "$@" >> "$BD_ARGS_LOG"
fi
exit 0
`

// installFakeBD writes fakeBDNudge to a temp dir, points KORYPH_BD_BIN at it,
// and returns the argv-log path.
func installFakeBD(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	if err := os.WriteFile(bin, []byte(fakeBDNudge), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	log := filepath.Join(dir, "argv.log")
	t.Setenv("KORYPH_BD_BIN", bin)
	t.Setenv("BD_ARGS_LOG", log)
	return log
}

// readArgvLog returns the logged argv lines (one call per line), skipping the
// leading "bd " the adapter's execx invocation does NOT include (Args passed
// to execx already exclude argv[0]).
func readArgvLog(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// --- tailFile ---

func TestTailFileReturnsLastNLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := tailFile(path, 3)
	want := "line3\nline4\nline5"
	if got != want {
		t.Errorf("tailFile(3) = %q, want %q", got, want)
	}
}

func TestTailFileNZeroReturnsAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := tailFile(path, 0)
	if !strings.Contains(got, "a") || !strings.Contains(got, "c") {
		t.Errorf("tailFile(0) = %q, want all lines", got)
	}
}

func TestTailFileMissingReturnsPlaceholder(t *testing.T) {
	got := tailFile("/nonexistent/path/session.log", 10)
	if !strings.HasPrefix(got, "(no ") {
		t.Errorf("tailFile missing = %q, want placeholder", got)
	}
}

// --- fileEnd ---

func TestFileEndMissingReturnsZero(t *testing.T) {
	if got := fileEnd("/no/such/file"); got != 0 {
		t.Errorf("fileEnd missing = %d, want 0", got)
	}
}

func TestFileEndReturnsSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	content := []byte("hello world")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := fileEnd(path); got != int64(len(content)) {
		t.Errorf("fileEnd = %d, want %d", got, len(content))
	}
}

// --- printNewContent ---

func TestPrintNewContentPrintsLinesAfterOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	if err := os.WriteFile(path, []byte("old\nnew1\nnew2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	// Start offset at byte 4 (past "old\n").
	newOff := printNewContent(&buf, path, 4, "")
	got := buf.String()
	if strings.Contains(got, "old") {
		t.Errorf("printNewContent included old content: %q", got)
	}
	if !strings.Contains(got, "new1") || !strings.Contains(got, "new2") {
		t.Errorf("printNewContent missing new lines: %q", got)
	}
	if newOff != int64(len("old\nnew1\nnew2\n")) {
		t.Errorf("newOff = %d, want %d", newOff, len("old\nnew1\nnew2\n"))
	}
}

func TestPrintNewContentHoldsBackPartialLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	// Write one complete line and one partial line (no trailing newline).
	if err := os.WriteFile(path, []byte("complete\npartial"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	newOff := printNewContent(&buf, path, 0, "")
	got := buf.String()
	if !strings.Contains(got, "complete") {
		t.Errorf("printNewContent missing complete line: %q", got)
	}
	if strings.Contains(got, "partial") {
		t.Errorf("printNewContent should hold back partial line: %q", got)
	}
	// Offset advances only to the end of the complete line.
	if newOff != int64(len("complete\n")) {
		t.Errorf("newOff = %d, want %d", newOff, len("complete\n"))
	}
}

func TestPrintNewContentPrefixApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stderr.log")
	if err := os.WriteFile(path, []byte("error msg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	printNewContent(&buf, path, 0, "[stderr] ")
	if !strings.Contains(buf.String(), "[stderr] error msg") {
		t.Errorf("prefix not applied: %q", buf.String())
	}
}

func TestPrintNewContentMissingFileReturnsOffset(t *testing.T) {
	var buf bytes.Buffer
	newOff := printNewContent(&buf, "/nonexistent/file", 5, "")
	if newOff != 5 {
		t.Errorf("newOff = %d, want 5", newOff)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected output for missing file: %q", buf.String())
	}
}

// --- printNewInbox ---

func TestPrintNewInboxPrintsBannerOnNewContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "INBOX.md")

	initial := []byte("old nudge\n")
	if err := os.WriteFile(path, initial, 0o644); err != nil {
		t.Fatal(err)
	}
	// Append a new nudge.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n---\nnew nudge text\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	var buf bytes.Buffer
	newOff := printNewInbox(&buf, path, int64(len(initial)))
	got := buf.String()

	if !strings.Contains(got, "INBOX NUDGE") {
		t.Errorf("banner missing from output: %q", got)
	}
	if !strings.Contains(got, "new nudge text") {
		t.Errorf("nudge text missing from output: %q", got)
	}
	if strings.Contains(got, "old nudge") {
		t.Errorf("old content should not appear: %q", got)
	}
	if newOff <= int64(len(initial)) {
		t.Errorf("newOff = %d should have advanced past initial content", newOff)
	}
}

func TestPrintNewInboxSkipsBlankContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "INBOX.md")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Append only whitespace.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("   \n")
	f.Close()

	var buf bytes.Buffer
	printNewInbox(&buf, path, int64(len("base\n")))
	if strings.Contains(buf.String(), "INBOX NUDGE") {
		t.Errorf("banner should not appear for blank content: %q", buf.String())
	}
}

func TestPrintNewInboxMissingFileReturnsOffset(t *testing.T) {
	var buf bytes.Buffer
	newOff := printNewInbox(&buf, "/nonexistent/INBOX.md", 7)
	if newOff != 7 {
		t.Errorf("newOff = %d, want 7", newOff)
	}
}

// --- tailFollow ---

func TestTailFollowExitsOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	// Create files with some content.
	if err := os.WriteFile(filepath.Join(dir, "session.log"), []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	var buf bytes.Buffer
	code := tailFollow(ctx, &buf, dir)
	if code != 0 {
		t.Errorf("tailFollow cancelled context: code = %d, want 0", code)
	}
}

// --- cmdTail (error paths only — full integration needs a seeded ledger) ---

func TestCmdTailMissingProjectFlag(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("tail", "some-phase")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit", code)
	}
	if !strings.Contains(errb, "--project") {
		t.Errorf("stderr = %q, want --project message", errb)
	}
}

func TestCmdTailMissingPhaseID(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("tail", "--project", "demo")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit", code)
	}
	if !strings.Contains(errb, "phase-id") {
		t.Errorf("stderr = %q, want phase-id message", errb)
	}
}

// --- cmdNudge (error paths) ---

func TestCmdNudgeMissingProjectFlag(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("nudge", "some-phase", "text")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit", code)
	}
	if !strings.Contains(errb, "--project") {
		t.Errorf("stderr = %q, want --project message", errb)
	}
}

func TestCmdNudgeMissingText(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("nudge", "--project", "demo", "phase-id")
	if code != engine.ExitUsage {
		t.Errorf("code = %d, want usage exit", code)
	}
	if !strings.Contains(errb, "text") {
		t.Errorf("stderr = %q, want text message", errb)
	}
}

// --- cmdNudge (dispatch-state branching, koryph-o72) ------------------------

// TestCmdNudgePreDispatchNoRunAtAllAppendsNotes covers the real-world failure
// this bead fixes: the bead is queued (no run has ever started for the
// project), so there is no phase dir INBOX.md could land in. The nudge must
// not silently create one anyway — it must go to bd notes, the channel
// promptc.Compile actually reads at the bead's eventual dispatch.
func TestCmdNudgePreDispatchNoRunAtAllAppendsNotes(t *testing.T) {
	isolate(t)
	log := installFakeBD(t)
	rec := registerMinimalProject(t, "proj-nudge-1")

	code, out, errb := runCmd("nudge", "--project", rec.ProjectID, "bead-77", "scope also covers retries")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb)
	}
	if !strings.Contains(out, "not dispatched yet") {
		t.Errorf("stdout = %q, want a not-dispatched-yet notice", out)
	}

	args := readArgvLog(t, log)
	if len(args) != 1 {
		t.Fatalf("bd argv log = %+v, want exactly one call", args)
	}
	if !strings.HasPrefix(args[0], "update bead-77 --append-notes") {
		t.Errorf("bd argv = %q, want an --append-notes call against bead-77", args[0])
	}
	if !strings.Contains(args[0], "scope also covers retries") {
		t.Errorf("bd argv = %q, want the nudge text appended", args[0])
	}

	// No phase dir should have been fabricated for a bead with no run.
	if entries, err := os.ReadDir(filepath.Join(paths.KoryphRoot(rec.Root))); err == nil {
		for _, e := range entries {
			if e.Name() == "bead-77" {
				t.Errorf("unexpected phase dir created for a never-dispatched bead: %s", e.Name())
			}
		}
	}
}

// TestCmdNudgePreDispatchQueuedInActiveRunAppendsNotes covers the same bug in
// its subtler form: a run IS active, but this particular bead has no slot in
// it yet (still queued in `bd ready`, not yet admitted into a wave). The
// nudge must still prefer bd notes over speculatively writing an INBOX.md
// that a future dispatch (possibly in a later run) may never look at.
func TestCmdNudgePreDispatchQueuedInActiveRunAppendsNotes(t *testing.T) {
	isolate(t)
	log := installFakeBD(t)
	rec := registerMinimalProject(t, "proj-nudge-2")
	// A run exists, but with a slot for a DIFFERENT bead only.
	seedTestRun(t, rec, []*ledger.Slot{{PhaseID: "other-bead", Status: ledger.SlotRunning}})

	code, out, errb := runCmd("nudge", "--project", rec.ProjectID, "queued-bead", "must include the retry path")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb)
	}
	if !strings.Contains(out, "not dispatched yet") {
		t.Errorf("stdout = %q, want a not-dispatched-yet notice", out)
	}
	args := readArgvLog(t, log)
	if len(args) != 1 || !strings.HasPrefix(args[0], "update queued-bead --append-notes") {
		t.Fatalf("bd argv log = %+v, want a single --append-notes call against queued-bead", args)
	}
}

// TestCmdNudgeDispatchedWritesInboxAndComments covers the already-dispatched
// case: a slot exists for the bead in the latest run, so the live INBOX.md
// channel applies (the agent is instructed to poll it), plus a best-effort
// bd comment for the audit trail.
func TestCmdNudgeDispatchedWritesInboxAndComments(t *testing.T) {
	isolate(t)
	log := installFakeBD(t)
	rec := registerMinimalProject(t, "proj-nudge-3")
	run := seedTestRun(t, rec, []*ledger.Slot{{PhaseID: "live-bead", Status: ledger.SlotRunning}})

	code, out, errb := runCmd("nudge", "--project", rec.ProjectID, "live-bead", "narrow the scope now")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb)
	}
	if !strings.Contains(out, "nudged live-bead") {
		t.Errorf("stdout = %q, want a nudged confirmation", out)
	}

	inboxPath := filepath.Join(paths.KoryphRoot(rec.Root), run.RunID, "live-bead", "INBOX.md")
	data, err := os.ReadFile(inboxPath)
	if err != nil {
		t.Fatalf("INBOX.md not written at %s: %v", inboxPath, err)
	}
	if !strings.Contains(string(data), "narrow the scope now") {
		t.Errorf("INBOX.md = %q, missing the nudge text", string(data))
	}

	args := readArgvLog(t, log)
	if len(args) != 1 || !strings.HasPrefix(args[0], "comment live-bead") {
		t.Fatalf("bd argv log = %+v, want a single comment call against live-bead", args)
	}
}

// TestCmdNudgePreDispatchNoBDErrorsLoudly asserts the loud-error edge case:
// when the bead is not dispatched yet AND bd is unavailable, the nudge must
// not silently no-op — the operator has no other reliable channel, so it
// must fail with guidance to run `bd update --append-notes` directly.
func TestCmdNudgePreDispatchNoBDErrorsLoudly(t *testing.T) {
	isolate(t)
	t.Setenv("KORYPH_BD_BIN", "/nonexistent/definitely-not-bd")
	rec := registerMinimalProject(t, "proj-nudge-4")

	code, _, errb := runCmd("nudge", "--project", rec.ProjectID, "bead-99", "urgent scope change")
	if code == 0 {
		t.Fatalf("expected a non-zero exit when bd is unavailable pre-dispatch; stderr = %q", errb)
	}
	if !strings.Contains(errb, "--append-notes") {
		t.Errorf("stderr = %q, want guidance to run bd update --append-notes directly", errb)
	}
}
