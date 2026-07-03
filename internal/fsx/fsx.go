// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package fsx provides small filesystem helpers shared across the engine.
package fsx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic writes data to path via a temp file + rename so readers never
// observe a partial file. Parent directories are created as needed.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// WriteJSONAtomic marshals v with indentation and writes it atomically (0644).
func WriteJSONAtomic(path string, v any) error {
	return WriteJSONAtomicPerm(path, v, 0o644)
}

// WriteJSONAtomicPerm is WriteJSONAtomic with an explicit file mode — use 0600
// for private state under KORYPH_HOME.
func WriteJSONAtomicPerm(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(path, append(data, '\n'), perm)
}

// ReadJSON unmarshals the JSON file at path into v.
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// AppendLine appends one line (adding a trailing newline) to path, creating
// it if absent (0644). Used for append-only JSONL logs.
func AppendLine(path string, line []byte) error {
	return AppendLinePerm(path, line, 0o644)
}

// AppendLinePerm is AppendLine with an explicit file mode for a newly-created
// file — use 0600 for private logs under KORYPH_HOME.
func AppendLinePerm(path string, line []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// Exists reports whether path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
