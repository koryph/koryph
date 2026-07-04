// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TelemetryRecord is a minimal representation of a JSONL telemetry line for
// human-readable rendering. Fields not present in a given line are left as
// zero values.
type TelemetryRecord struct {
	// Standard slog-JSONL fields
	Time      string `json:"time"`
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	Component string `json:"component"`

	// Canonical koryph attributes
	RunID   string `json:"run_id"`
	Project string `json:"project"`
	BeadID  string `json:"bead_id"`
}

// TailOptions configures a telemetry tail operation.
type TailOptions struct {
	// Dir is the telemetry directory to scan (default: paths.TelemetryDir()).
	Dir string
	// Component filters records to those with a matching component field.
	// Empty string means all components.
	Component string
	// Level filters records to those at or above this level.
	// Only active when HasLevelFilter is true; slog.LevelInfo == 0 so the
	// zero value of slog.Level cannot be used as a "no filter" sentinel.
	Level slog.Level
	// HasLevelFilter must be true for Level to take effect.
	HasLevelFilter bool
	// N is the number of trailing records to print. 0 means no cap.
	N int
}

// TailRecords reads the last N telemetry records from dir, optionally filtered
// by component and/or level. Records are returned in chronological order. The
// telemetry dir may hold multiple JSONL files; they are processed in
// lexicographic order (koryph names them by timestamp so this is also
// chronological).
func TailRecords(opts TailOptions) ([]TelemetryRecord, error) {
	dir := opts.Dir
	if dir == "" {
		// Import-time circular dependency avoided: caller sets Dir from paths.TelemetryDir().
		return nil, fmt.Errorf("obs: TailOptions.Dir must be set")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no telemetry yet; not an error
		}
		return nil, fmt.Errorf("obs: read telemetry dir %q: %w", dir, err)
	}

	// Collect JSONL files in sorted order.
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)

	var all []TelemetryRecord
	for _, path := range files {
		recs, rerr := readJSONLFile(path, opts)
		if rerr != nil {
			// A corrupt file is a warning, not fatal.
			continue
		}
		all = append(all, recs...)
	}

	// Apply N cap — take the last N records.
	if opts.N > 0 && len(all) > opts.N {
		all = all[len(all)-opts.N:]
	}
	return all, nil
}

func readJSONLFile(path string, opts TailOptions) ([]TelemetryRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []TelemetryRecord
	sc := bufio.NewScanner(f)
	// Raise the default 64 KB token cap so large telemetry lines don't
	// silently truncate the file (Scanner returns an error on overflow,
	// which TailRecords treats as a corrupt-file skip).
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// First unmarshal into the well-known fields.
		var rec TelemetryRecord
		if jerr := json.Unmarshal(line, &rec); jerr != nil {
			continue
		}
		// Apply component filter.
		if opts.Component != "" && rec.Component != opts.Component {
			continue
		}
		// Apply level filter. HasLevelFilter guards the check because
		// slog.LevelInfo == 0, making the zero value ambiguous.
		if opts.HasLevelFilter {
			if l, ok := ParseLevel(rec.Level); !ok || l < opts.Level {
				continue
			}
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// FormatRecord renders one TelemetryRecord as a single human-readable line.
// Format: "TIME LEVEL [component] msg  key=val…"
func FormatRecord(w io.Writer, rec TelemetryRecord) {
	// Parse and reformat the time if possible.
	ts := rec.Time
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		ts = t.Format("15:04:05.000")
	} else if t2, err2 := time.Parse(time.RFC3339, ts); err2 == nil {
		ts = t2.Format("15:04:05")
	}

	level := rec.Level
	if level == "" {
		level = "INFO"
	}
	// Pad level to 5 chars.
	for len(level) < 5 {
		level += " "
	}

	comp := ""
	if rec.Component != "" {
		comp = fmt.Sprintf("[%s] ", rec.Component)
	}

	msg := rec.Msg

	var extras []string
	if rec.RunID != "" {
		extras = append(extras, "run="+rec.RunID)
	}
	if rec.Project != "" {
		extras = append(extras, "project="+rec.Project)
	}
	if rec.BeadID != "" {
		extras = append(extras, "bead="+rec.BeadID)
	}

	line := fmt.Sprintf("%s %s %s%s", ts, level, comp, msg)
	if len(extras) > 0 {
		line += "  " + strings.Join(extras, " ")
	}
	fmt.Fprintln(w, line)
}

// TailFollow streams new records from the telemetry directory until ctx is
// cancelled, printing each in human-readable form. It polls every 500 ms.
// componentFilter and levelFilter mirror TailOptions.Component/Level.
// hasLvlFilter must be true for levelFilter to take effect; this avoids
// the ambiguity where slog.LevelInfo == 0 (the zero value of slog.Level).
//
// Offsets are seeded to the current file sizes before the first poll so that
// only records written after TailFollow returns are streamed; existing history
// is not re-printed.
func TailFollow(ctx interface{ Done() <-chan struct{} }, w io.Writer, dir, componentFilter string, levelFilter slog.Level, hasLvlFilter bool) {
	// Seed offsets to current EOF so the first drainNewRecords call only
	// emits records written after this function was called, not history.
	offsets := map[string]int64{}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				path := filepath.Join(dir, e.Name())
				if fi, serr := os.Stat(path); serr == nil {
					offsets[path] = fi.Size()
				}
			}
		}
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		drainNewRecords(w, dir, componentFilter, levelFilter, hasLvlFilter, offsets)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func drainNewRecords(w io.Writer, dir, comp string, lvl slog.Level, hasLvlFilter bool, offsets map[string]int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)

	for _, path := range files {
		off := offsets[path]
		newOff, recs := readFromOffset(path, off, comp, lvl, hasLvlFilter)
		offsets[path] = newOff
		for _, r := range recs {
			FormatRecord(w, r)
		}
	}
}

// readFromOffset reads JSONL records from path starting at the given byte
// offset. It returns the new offset (advanced past all complete newline-
// terminated lines consumed) and any matching records.
//
// Only lines terminated by '\n' are consumed; an incomplete final line (no
// newline yet) is left for the next poll. This avoids the off-by-one drift
// that occurs when the writer has not yet flushed a complete line.
// CRLF line endings are handled by stripping a trailing '\r'.
func readFromOffset(path string, offset int64, comp string, lvl slog.Level, hasLvlFilter bool) (int64, []TelemetryRecord) {
	f, err := os.Open(path)
	if err != nil {
		return offset, nil
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() <= offset {
		return offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, nil
	}

	// Read all bytes from offset to current EOF in one shot. This avoids
	// bufio.Scanner's internal buffering which would make it impossible to
	// recover the exact byte position of the last consumed newline.
	data, rerr := io.ReadAll(f)
	if rerr != nil || len(data) == 0 {
		return offset, nil
	}

	var out []TelemetryRecord
	var consumed int64
	remaining := data
	for len(remaining) > 0 {
		idx := bytes.IndexByte(remaining, '\n')
		if idx < 0 {
			// No newline — incomplete line, stop and wait for next poll.
			break
		}
		lineRaw := remaining[:idx]
		remaining = remaining[idx+1:]
		consumed += int64(idx) + 1 // idx content bytes + 1 for '\n'

		// Strip trailing '\r' to handle CRLF line endings.
		line := lineRaw
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			continue
		}

		var rec TelemetryRecord
		if jerr := json.Unmarshal(line, &rec); jerr != nil {
			continue
		}
		if comp != "" && rec.Component != comp {
			continue
		}
		if hasLvlFilter {
			if l, ok := ParseLevel(rec.Level); !ok || l < lvl {
				continue
			}
		}
		out = append(out, rec)
	}
	return offset + consumed, out
}
