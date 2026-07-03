// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
)

// File / layout constants.
const (
	// runIDLayout is the UTC timestamp format used as a run's directory name.
	// Fixed-width and zero-padded so lexical order == chronological order.
	runIDLayout = "20060102-150405"

	// schemaVersion is the current ledger + manifest schema (v2).
	schemaVersion = 2

	latestLink   = "latest"
	ledgerFile   = "ledger.json"
	manifestFile = "manifest.json"
	lockFile     = "koryph.lock"
)

// Store owns a single project's koryph run ledgers, all rooted at
// KoryphRoot = <repo>/.plan-logs/koryph/. Checkpoints live with the work
// they checkpoint.
//
// Single-writer discipline: the koryph engine process is the ONLY writer of
// any ledger.json or manifest.json beneath KoryphRoot. Every mutation is a
// read-modify-write that refreshes UpdatedAt and lands atomically through
// fsx.WriteJSONAtomic. Because exactly one process writes, no file locking is
// required for ledger correctness; cross-process singleton exclusion is a
// separate concern handled by RunLock.
type Store struct {
	KoryphRoot string
}

// NewStore returns a Store rooted at the project's koryph run directory.
func NewStore(repoRoot string) *Store {
	return &Store{KoryphRoot: paths.KoryphRoot(repoRoot)}
}

// nowRFC3339 is the canonical mutation timestamp (RFC3339, UTC).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// NewRun creates a fresh run: it allocates a UTC-timestamp RunID, makes the
// run directory, writes ledger.json atomically, and repoints the `latest`
// symlink at it (relative target).
func (s *Store) NewRun(projectID, source, engineVersion string) (*Run, error) {
	now := time.Now().UTC()
	runID := now.Format(runIDLayout)
	dir := filepath.Join(s.KoryphRoot, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	started := now.Format(time.RFC3339)
	run := &Run{
		SchemaVersion: schemaVersion,
		RunID:         runID,
		ProjectID:     projectID,
		EngineVersion: engineVersion,
		StartedAt:     started,
		UpdatedAt:     started,
		Status:        RunRunning,
		Source:        source,
		Slots:         map[string]*Slot{},
	}
	if err := s.SaveRun(run); err != nil {
		return nil, err
	}
	if err := s.repointLatest(runID); err != nil {
		return nil, err
	}
	return run, nil
}

// repointLatest atomically-ish swaps the `latest` symlink to point at runID.
// The target is relative (bare runID) so the tree stays relocatable.
func (s *Store) repointLatest(runID string) error {
	if err := os.MkdirAll(s.KoryphRoot, 0o755); err != nil {
		return err
	}
	link := filepath.Join(s.KoryphRoot, latestLink)
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(runID, link)
}

// RunDir returns the directory for runID, creating it on demand.
func (s *Store) RunDir(runID string) string {
	dir := filepath.Join(s.KoryphRoot, runID)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// PhaseDir returns the per-slot checkpoint directory for phaseID within runID,
// creating it on demand.
func (s *Store) PhaseDir(runID, phaseID string) string {
	dir := filepath.Join(s.KoryphRoot, runID, phaseID)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// LoadRun reads ledger.json for runID.
func (s *Store) LoadRun(runID string) (*Run, error) {
	var run Run
	path := filepath.Join(s.KoryphRoot, runID, ledgerFile)
	if err := fsx.ReadJSON(path, &run); err != nil {
		return nil, err
	}
	if run.Slots == nil {
		run.Slots = map[string]*Slot{}
	}
	return &run, nil
}

// LoadLatest resolves the `latest` symlink and loads that run.
func (s *Store) LoadLatest() (*Run, error) {
	link := filepath.Join(s.KoryphRoot, latestLink)
	target, err := os.Readlink(link)
	if err != nil {
		return nil, err
	}
	return s.LoadRun(filepath.Base(target))
}

// ListRuns returns every run ID under KoryphRoot, newest first. Because run
// IDs are fixed-width UTC timestamps, reverse-lexical order is newest-first.
func (s *Store) ListRuns() ([]string, error) {
	entries, err := os.ReadDir(s.KoryphRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var runs []string
	for _, e := range entries {
		name := e.Name()
		if name == latestLink || name == lockFile || !e.IsDir() {
			continue
		}
		if !fsx.Exists(filepath.Join(s.KoryphRoot, name, ledgerFile)) {
			continue
		}
		runs = append(runs, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(runs)))
	return runs, nil
}

// SaveRun refreshes UpdatedAt and writes ledger.json atomically.
func (s *Store) SaveRun(run *Run) error {
	run.UpdatedAt = nowRFC3339()
	path := filepath.Join(s.KoryphRoot, run.RunID, ledgerFile)
	return fsx.WriteJSONAtomic(path, run)
}

// SetSlot installs (or replaces) a slot keyed by its PhaseID, stamps the slot's
// UpdatedAt, and persists the run. Single-writer: no locking needed.
func (s *Store) SetSlot(run *Run, sl *Slot) error {
	if run.Slots == nil {
		run.Slots = map[string]*Slot{}
	}
	sl.UpdatedAt = nowRFC3339()
	run.Slots[sl.PhaseID] = sl
	return s.SaveRun(run)
}

// UpdateSlot mutates the slot for phaseID in place via mut, stamps the slot's
// UpdatedAt, and persists the run. A missing slot is created. Single-writer:
// no locking needed.
func (s *Store) UpdateSlot(run *Run, phaseID string, mut func(*Slot)) error {
	if run.Slots == nil {
		run.Slots = map[string]*Slot{}
	}
	sl, ok := run.Slots[phaseID]
	if !ok {
		sl = &Slot{PhaseID: phaseID}
		run.Slots[phaseID] = sl
	}
	mut(sl)
	sl.UpdatedAt = nowRFC3339()
	return s.SaveRun(run)
}

// FinalizeRun marks a run terminal once every slot is terminal (or there are
// no slots at all). A drained run stays drained; anything else becomes done.
// This is the fix for the stale-"running" bug: a slotless run is never left
// running.
func (s *Store) FinalizeRun(run *Run) error {
	for _, sl := range run.Slots {
		if sl == nil {
			continue
		}
		if !Terminal(sl.Status) {
			return nil // active work remains; not finalizable
		}
	}
	if run.Status != RunDrained {
		run.Status = RunDone
	}
	return s.SaveRun(run)
}

// SaveManifest stamps SchemaVersion=2 and UpdatedAt, then writes the per-slot
// checkpoint at <run>/<phase>/manifest.json atomically.
func (s *Store) SaveManifest(runID, phaseID string, m *Manifest) error {
	m.SchemaVersion = schemaVersion
	m.UpdatedAt = nowRFC3339()
	path := filepath.Join(s.PhaseDir(runID, phaseID), manifestFile)
	return fsx.WriteJSONAtomic(path, m)
}

// LoadManifest reads the per-slot checkpoint for phaseID within runID.
func (s *Store) LoadManifest(runID, phaseID string) (*Manifest, error) {
	var m Manifest
	path := filepath.Join(s.KoryphRoot, runID, phaseID, manifestFile)
	if err := fsx.ReadJSON(path, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Lock is a held process-singleton lock over a KoryphRoot.
type Lock struct {
	path  string
	runID string
}

// RunLock acquires the koryph process-singleton lock at
// <KoryphRoot>/koryph.lock. It writes "<pid> <host>" via O_CREATE|O_EXCL.
// If the lock already exists but its recorded PID is not alive, the stale lock
// is removed and acquisition is retried once. A live holder yields an error.
func (s *Store) RunLock(runID string) (*Lock, error) {
	if err := os.MkdirAll(s.KoryphRoot, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(s.KoryphRoot, lockFile)

	l, err := acquireLock(path, runID)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}

	// Lock exists. Decide whether it is stale.
	if pid, ok := readLockPID(path); ok && processAlive(pid) {
		return nil, fmt.Errorf("koryph already running for run %s (pid %d, lock %s)", runID, pid, path)
	}
	// Stale or unreadable holder: clear it and retry exactly once.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return acquireLock(path, runID)
}

// acquireLock creates the lock file exclusively and records "<pid> <host>".
func acquireLock(path, runID string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	content := fmt.Sprintf("%d %s\n", os.Getpid(), hostname())
	if _, werr := f.WriteString(content); werr != nil {
		f.Close()
		_ = os.Remove(path)
		return nil, werr
	}
	if cerr := f.Close(); cerr != nil {
		return nil, cerr
	}
	return &Lock{path: path, runID: runID}, nil
}

// Unlock releases the lock by removing the lock file.
func (l *Lock) Unlock() error {
	if l == nil {
		return nil
	}
	return os.Remove(l.path)
}

// readLockPID parses the PID (first whitespace-delimited field) from a lock
// file. ok is false if the file is missing or malformed.
func readLockPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether pid is a live process. It probes with signal 0
// (unix kill(pid, 0) semantics): nil error → alive; EPERM → alive but not
// ours; ESRCH (or any other error) → dead. os.FindProcess never fails on unix,
// so the signal probe is the real test.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// hostname returns the machine hostname, or "unknown" if it cannot be read.
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}
