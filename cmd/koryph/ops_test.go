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
)

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
