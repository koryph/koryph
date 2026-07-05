// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ExportOptions configures a koryph obs export operation.
type ExportOptions struct {
	// Dir is the telemetry directory to scan.  Defaults to the canonical
	// telemetry directory when empty.
	Dir string
	// RunID selects only records whose run_id field matches.
	// An empty RunID returns all records (useful for debugging).
	RunID string
}

// ExportResult summarises the outcome of an export.
type ExportResult struct {
	// Records is the number of records written to the output.
	Records int
	// Files is the number of JSONL files scanned.
	Files int
}

// ExportRun scans every JSONL file in opts.Dir for records whose run_id field
// equals opts.RunID, re-verifies redaction on each record (via RedactRecord),
// and writes the surviving records as JSONL to w.
//
// Re-verification re-applies the redaction layer so that even records written
// before the current redaction rules were deployed are safe to export.  The
// output is suitable for piping to jq, duckdb, or an external OTLP collector.
func ExportRun(opts ExportOptions, w io.Writer) (ExportResult, error) {
	dir := opts.Dir
	if dir == "" {
		dir = telemetryDirPath()
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ExportResult{}, nil // no telemetry yet
		}
		return ExportResult{}, fmt.Errorf("obs export: read dir %q: %w", dir, err)
	}

	// Collect JSONL files in chronological (lexicographic) order.
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)

	bw := bufio.NewWriter(w)
	var res ExportResult
	for _, path := range files {
		res.Files++
		n, ferr := exportFile(path, opts.RunID, bw)
		if ferr != nil {
			// A corrupt file is skipped, not fatal — best-effort export.
			continue
		}
		res.Records += n
	}
	if ferr := bw.Flush(); ferr != nil {
		return res, fmt.Errorf("obs export: flush: %w", ferr)
	}
	return res, nil
}

// exportFile reads all matching records from one JSONL file and writes them to
// bw.  It returns the number of records written.
func exportFile(path, runID string, bw *bufio.Writer) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var written int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		// Quick run_id filter before full unmarshal.
		if runID != "" && !containsRunID(line, runID) {
			continue
		}

		// Re-verify redaction: unmarshal → apply RedactRecord → re-marshal.
		cleaned, ok := redactJSONLine(line)
		if !ok {
			continue // skip malformed lines
		}
		if _, werr := bw.Write(cleaned); werr != nil {
			return written, werr
		}
		if werr := bw.WriteByte('\n'); werr != nil {
			return written, werr
		}
		written++
	}
	return written, sc.Err()
}

// containsRunID performs a fast substring check before full JSON parse.
// This avoids unmarshalling every record when most don't match.
func containsRunID(line []byte, runID string) bool {
	// Look for the exact quoted run_id value.
	needle := `"run_id":"` + runID + `"`
	return strings.Contains(string(line), needle)
}

// redactJSONLine unmarshals a raw JSON line into a map, applies the redaction
// patterns to every string value, and re-marshals.  This is a best-effort
// approach that covers the common flat-key slog JSON structure; deeply nested
// structures are handled recursively.
//
// Returns the re-marshalled bytes and ok=true, or nil, false on parse error.
func redactJSONLine(line []byte) ([]byte, bool) {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, false
	}
	m = redactMap(m)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// redactMap recursively applies redaction to a map's values.
// The function mirrors the slog-level RedactRecord logic but operates on
// a decoded map (used for export re-verification).
//
// IsSecretKey is checked first, before the type switch, so secret-keyed
// fields of any type (strings, arrays, nested objects) are fully redacted.
func redactMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if IsSecretKey(k) {
			out[k] = Redacted
			continue
		}
		switch val := v.(type) {
		case string:
			out[k] = RedactValue(val)
		case map[string]any:
			out[k] = redactMap(val)
		case []any:
			out[k] = redactSlice(val)
		default:
			out[k] = v
		}
	}
	return out
}

// redactSlice applies redaction to each element of a JSON array value.
// String elements are pattern-scanned; nested maps are recursively redacted;
// nested slices are recursively handled; scalars pass through unchanged.
func redactSlice(s []any) []any {
	out := make([]any, len(s))
	for i, elem := range s {
		switch val := elem.(type) {
		case string:
			out[i] = RedactValue(val)
		case map[string]any:
			out[i] = redactMap(val)
		case []any:
			out[i] = redactSlice(val)
		default:
			out[i] = val
		}
	}
	return out
}
