// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"bufio"
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

	// Catch-all for any other top-level keys
	Extra map[string]any `json:"-"`
}

// TailOptions configures a telemetry tail operation.
type TailOptions struct {
	// Dir is the telemetry directory to scan (default: paths.TelemetryDir()).
	Dir string
	// Component filters records to those with a matching component field.
	// Empty string means all components.
	Component string
	// Level filters records to those at or above this level.
	// Zero value means all levels.
	Level slog.Level
	// N is the number of trailing records to print. 0 means no cap.
	N int
}

// TailRecords reads the last N telemetry records from dir, optionally filtered
// by component. Records are returned in chronological order. The telemetry dir
// may hold multiple JSONL files; they are processed in lexicographic order
// (koryph names them by timestamp so this is also chronological).
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
		// Apply level filter.
		if opts.Level != 0 {
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
func TailFollow(ctx interface{ Done() <-chan struct{} }, w io.Writer, dir, componentFilter string, levelFilter slog.Level) {
	// Track file positions so we only emit new content.
	offsets := map[string]int64{}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		drainNewRecords(w, dir, componentFilter, levelFilter, offsets)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func drainNewRecords(w io.Writer, dir, comp string, lvl slog.Level, offsets map[string]int64) {
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
		newOff, recs := readFromOffset(path, off, comp, lvl)
		offsets[path] = newOff
		for _, r := range recs {
			FormatRecord(w, r)
		}
	}
}

func readFromOffset(path string, offset int64, comp string, lvl slog.Level) (int64, []TelemetryRecord) {
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

	var out []TelemetryRecord
	sc := bufio.NewScanner(f)
	var bytesRead int64
	for sc.Scan() {
		line := sc.Bytes()
		bytesRead += int64(len(line)) + 1 // +1 for newline
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
		if lvl != 0 {
			if l, ok := ParseLevel(rec.Level); !ok || l < lvl {
				continue
			}
		}
		out = append(out, rec)
	}
	return offset + bytesRead, out
}
