// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package fsx provides small filesystem helpers shared across the engine.
package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
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

// RemoveDurable removes path and fsyncs its parent directory so a successful
// retirement cannot be undone by a crash that resurrects the removed
// directory entry. Callers retain normal os.Remove error semantics.
func RemoveDurable(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return fsyncDir(filepath.Dir(path))
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

// ConfinedRead is one regular file read through a trusted root directory
// descriptor. Path is the cleaned absolute spelling selected under that root;
// Data and Digest describe the same pinned file descriptor.
type ConfinedRead struct {
	Path   string
	Data   []byte
	Digest string
}

// ReadRegularConfined reads path only when every path component below one of
// roots is a real directory entry (never a symlink), the final entry is a
// nonempty regular file, and its content fits within limit. It opens the root
// once, walks descendants with openat(2)+O_NOFOLLOW, and parses/digests the
// resulting descriptor. Consequently a parent-directory replacement cannot
// redirect an already-open read outside the trusted root.
//
// Relative paths are tried beneath roots in order. Absolute paths must be
// lexically contained by one of roots. The same-UID worker can still mutate
// its own filesystem; this helper provides fail-closed validation, not an ACL
// boundary.
func ReadRegularConfined(path string, limit int64, roots ...string) (ConfinedRead, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return ConfinedRead{}, errors.New("path is required")
	}
	if limit <= 0 {
		return ConfinedRead{}, errors.New("positive read limit is required")
	}
	if len(roots) == 0 {
		return ConfinedRead{}, errors.New("at least one trusted root is required")
	}

	absolute := filepath.IsAbs(path)
	var lastErr, matchedErr error
	for _, root := range roots {
		rootAbs, err := filepath.Abs(strings.TrimSpace(root))
		if err != nil {
			lastErr = err
			continue
		}
		rootAbs = filepath.Clean(rootAbs)
		candidate := path
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(rootAbs, candidate)
		}
		candidate, err = filepath.Abs(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		candidate = filepath.Clean(candidate)
		rel, err := filepath.Rel(rootAbs, candidate)
		if err != nil || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			lastErr = errors.New("path is outside trusted roots")
			continue
		}
		if rel == "." {
			return ConfinedRead{}, fmt.Errorf("%s: file must be nonempty, regular, and within size limit (%d bytes)", candidate, limit)
		}
		read, err := readRegularAt(rootAbs, rel, candidate, limit)
		if err == nil {
			return read, nil
		}
		if absolute {
			return ConfinedRead{}, err
		}
		if matchedErr == nil {
			matchedErr = err
		}
	}
	if matchedErr != nil {
		return ConfinedRead{}, matchedErr
	}
	if lastErr != nil {
		return ConfinedRead{}, lastErr
	}
	return ConfinedRead{}, errors.New("path is outside trusted roots")
}

func readRegularAt(root, rel, displayPath string, limit int64) (ConfinedRead, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ConfinedRead{}, fmt.Errorf("%s: open trusted root: %w", root, err)
	}
	currentFD := rootFD
	defer func() { _ = unix.Close(currentFD) }()

	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return ConfinedRead{}, fmt.Errorf("%s: invalid path component", displayPath)
		}
		last := i == len(parts)-1
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if last {
			flags |= unix.O_NONBLOCK
		} else {
			flags |= unix.O_DIRECTORY
		}
		nextFD, err := unix.Openat(currentFD, part, flags, 0)
		if err != nil {
			if errors.Is(err, unix.ELOOP) {
				return ConfinedRead{}, fmt.Errorf("%s: path is outside trusted roots or contains a symlink: %w", displayPath, err)
			}
			return ConfinedRead{}, fmt.Errorf("%s: open confined component %q: %w", displayPath, part, err)
		}
		_ = unix.Close(currentFD)
		currentFD = nextFD
	}

	f := os.NewFile(uintptr(currentFD), displayPath)
	if f == nil {
		return ConfinedRead{}, fmt.Errorf("%s: open confined file", displayPath)
	}
	// f owns the descriptor from this point; keep the deferred close from
	// closing it a second time.
	currentFD = -1
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ConfinedRead{}, fmt.Errorf("%s: stat: %w", displayPath, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return ConfinedRead{}, fmt.Errorf("%s: file must be nonempty, regular, and within size limit (%d bytes)", displayPath, limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return ConfinedRead{}, fmt.Errorf("%s: read: %w", displayPath, err)
	}
	if int64(len(data)) == 0 || int64(len(data)) > limit {
		return ConfinedRead{}, fmt.Errorf("%s: file must be nonempty, regular, and within size limit (%d bytes)", displayPath, limit)
	}
	sum := sha256.Sum256(data)
	return ConfinedRead{
		Path: displayPath, Data: data, Digest: hex.EncodeToString(sum[:]),
	}, nil
}

// ReadJSONConfined is ReadRegularConfined followed by JSON decoding of the
// exact bytes read and digested from the pinned descriptor.
func ReadJSONConfined(path string, v any, limit int64, roots ...string) error {
	read, err := ReadRegularConfined(path, limit, roots...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(read.Data, v); err != nil {
		return fmt.Errorf("%s: %w", read.Path, err)
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
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:errcheck
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
