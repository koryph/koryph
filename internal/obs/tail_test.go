// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeJSONL writes lines to path, each terminated by '\n'.
func writeJSONL(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("writeJSONL: %v", err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

// appendJSONL appends a line to path.
func appendJSONL(t *testing.T, path string, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("appendJSONL: %v", err)
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// TestLevelFilterInfo verifies that HasLevelFilter=false means no filtering,
// and that info records are visible even though slog.LevelInfo == 0.
func TestLevelFilterInfo(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, filepath.Join(dir, "a.jsonl"), []string{
		`{"level":"DEBUG","msg":"low"}`,
		`{"level":"INFO","msg":"mid"}`,
		`{"level":"WARN","msg":"high"}`,
	})

	// No level filter: all three records returned.
	recs, err := TailRecords(TailOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("no filter: want 3 records, got %d", len(recs))
	}

	// HasLevelFilter=true, Level=INFO: only INFO and WARN returned.
	recs, err = TailRecords(TailOptions{
		Dir:            dir,
		Level:          slog.LevelInfo,
		HasLevelFilter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("info filter: want 2 records, got %d", len(recs))
	}
	if recs[0].Msg != "mid" || recs[1].Msg != "high" {
		t.Fatalf("info filter: unexpected records: %v", recs)
	}

	// HasLevelFilter=true, Level=WARN: only WARN returned.
	recs, err = TailRecords(TailOptions{
		Dir:            dir,
		Level:          slog.LevelWarn,
		HasLevelFilter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Msg != "high" {
		t.Fatalf("warn filter: want 1 'high' record, got %v", recs)
	}
}

// TestReadFromOffsetExact verifies that readFromOffset advances the offset by
// exactly the number of bytes consumed (including newlines) so repeated calls
// emit each record exactly once.
func TestReadFromOffsetExact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")

	lines := []string{
		`{"level":"INFO","msg":"one"}`,
		`{"level":"INFO","msg":"two"}`,
		`{"level":"INFO","msg":"three"}`,
	}
	writeJSONL(t, path, lines)

	// First read: expect all three records.
	off1, recs1 := readFromOffset(path, 0, "", 0, false)
	if len(recs1) != 3 {
		t.Fatalf("first read: want 3 records, got %d", len(recs1))
	}

	// Second read at off1: expect zero records (nothing new).
	off2, recs2 := readFromOffset(path, off1, "", 0, false)
	if len(recs2) != 0 {
		t.Fatalf("second read: want 0 records, got %d %v", len(recs2), recs2)
	}
	if off2 != off1 {
		t.Fatalf("second read: offset should be unchanged, got %d (was %d)", off2, off1)
	}

	// Append a fourth record and read again.
	appendJSONL(t, path, `{"level":"INFO","msg":"four"}`)
	_, recs3 := readFromOffset(path, off1, "", 0, false)
	if len(recs3) != 1 || recs3[0].Msg != "four" {
		t.Fatalf("third read: want [four], got %v", recs3)
	}
}

// TestReadFromOffsetIncomplete verifies that an incomplete (no-newline) final
// line is not consumed and the offset is not advanced past it.
func TestReadFromOffsetIncomplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inc.jsonl")

	// Write a complete line then an incomplete one (no trailing newline).
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"level":"INFO","msg":"complete"}`)
	fmt.Fprint(f, `{"level":"INFO","msg":"incomplete"`) // no closing brace or newline
	f.Close()

	off, recs := readFromOffset(path, 0, "", 0, false)
	if len(recs) != 1 || recs[0].Msg != "complete" {
		t.Fatalf("want only 'complete' record, got %v", recs)
	}
	// Offset should stop at the end of the first line, not past the incomplete one.
	completeLen := int64(len(`{"level":"INFO","msg":"complete"}`) + 1) // +1 for \n
	if off != completeLen {
		t.Fatalf("want offset %d, got %d", completeLen, off)
	}
}

// TestTailFollowNoHistory verifies that TailFollow does not re-emit records
// that existed before the call; only records appended after the call appear.
func TestTailFollowNoHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")

	// Pre-seed the file with history.
	writeJSONL(t, path, []string{
		`{"level":"INFO","msg":"history1"}`,
		`{"level":"INFO","msg":"history2"}`,
	})

	ctx, cancel := context.WithCancel(context.Background())

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		TailFollow(ctx, &buf, dir, "", 0, false)
	}()

	// Give TailFollow time to seed offsets before appending new records.
	time.Sleep(50 * time.Millisecond)

	appendJSONL(t, path, `{"level":"INFO","msg":"new1"}`)

	// Wait for at least one poll cycle (500 ms) plus buffer.
	time.Sleep(650 * time.Millisecond)
	cancel()
	<-done

	out := buf.String()
	if strings.Contains(out, "history1") || strings.Contains(out, "history2") {
		t.Errorf("TailFollow emitted history records: %q", out)
	}
	if !strings.Contains(out, "new1") {
		t.Errorf("TailFollow did not emit new record: %q", out)
	}
}

// TestLargeLineNotDropped verifies that a line larger than 64 KB is still
// parsed (scanner buffer raised to 4 MB).
func TestLargeLineNotDropped(t *testing.T) {
	dir := t.TempDir()
	// Build a record with a very large msg field (> 64 KB).
	bigMsg := strings.Repeat("x", 70*1024)
	line := fmt.Sprintf(`{"level":"INFO","msg":%q}`, bigMsg)
	writeJSONL(t, filepath.Join(dir, "big.jsonl"), []string{line})

	recs, err := TailRecords(TailOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].Msg != bigMsg {
		t.Fatalf("msg truncated: got len %d, want %d", len(recs[0].Msg), len(bigMsg))
	}
}
