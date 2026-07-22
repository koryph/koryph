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
//
// The rename is followed by an fsync of the PARENT directory: on the common
// crash-consistency journaling modes (e.g. ext4 ordered/writeback, ordinary
// APFS), a rename is only durable once the directory entry change itself is
// flushed — fsyncing the temp file before rename guarantees the file's DATA
// survives a crash, but not that the rename that makes it visible at path
// does. Without this, a crash between rename() and the next unrelated fsync
// of that directory can resurrect the pre-rename state (old content, or no
// file at all) even though the caller observed a successful write. Best
// effort: a directory fsync failure (e.g. an fs that rejects O_RDONLY dir
// fsync) is not fatal — the rename already succeeded — but is returned so
// callers/tests can see it.
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// fsyncDir opens dir and fsyncs it, making a prior rename/create/remove in
// that directory durable across a crash. Opening the directory should never
// fail immediately after a successful rename into it, but if it does (e.g. a
// permissions race), that's treated as best-effort — the caller's actual
// write already succeeded — while a real fsync failure on an opened
// directory descriptor is surfaced like any other durability error.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil // best effort: cannot make durability stronger than the OS allows
	}
	defer d.Close()
	return d.Sync()
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
