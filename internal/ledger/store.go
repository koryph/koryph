// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"encoding/json"
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
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/procx"
	"github.com/koryph/koryph/internal/schemaver"
)

// log is the ledger component logger.
var log = obs.For("ledger")

// lockWaitWarnThreshold is how long acquireReclaimGuard's blocking flock may
// wait before it logs what it is waiting on (koryph-lwnq: a known silent-wait
// site — see execx.slowWarnThreshold for the identical rationale). A var, not
// a const, so tests can shrink it.
var lockWaitWarnThreshold = 30 * time.Second

// File / layout constants.
const (
	// runIDLayout is the UTC timestamp format used as a run's directory name.
	// A same-second collision receives a fixed-width numeric suffix, preserving
	// reverse-lexical chronological ordering while legacy bare IDs remain valid.
	runIDLayout = "20060102-150405"

	latestLink        = "latest"
	runAllocationLock = "run-allocation.lock"
	ledgerFile        = "ledger.json"
	manifestFile      = "manifest.json"
	evidenceDir       = ".engine-evidence"
	// A manifest is small structured state. Bounding it before allocation and
	// reading it through the phase directory descriptor prevents a worker-owned
	// symlink/FIFO/device from turning recovery into an unbounded or blocking
	// read.
	maxManifestBytes = 1 << 20
	lockFile         = "koryph.lock"
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
	now        func() time.Time
	saveRun    func(*Run) error
}

// NewStore returns a Store rooted at the project's koryph run directory.
func NewStore(repoRoot string) *Store {
	return &Store{KoryphRoot: paths.KoryphRoot(repoRoot)}
}

// nowRFC3339 is the canonical mutation timestamp (RFC3339, UTC).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// NewRun creates a fresh run under a cross-process allocation lock. The first
// run in a second keeps the legacy timestamp-only ID; collisions receive an
// exclusive numeric suffix. latest is repointed only after the complete ledger
// is durable, so a failed allocation cannot expose an incomplete run.
func (s *Store) NewRun(projectID, source, engineVersion string) (*Run, error) {
	if err := os.MkdirAll(s.KoryphRoot, 0o755); err != nil {
		return nil, err
	}
	allocationLock, err := os.OpenFile(
		filepath.Join(s.KoryphRoot, runAllocationLock),
		os.O_CREATE|os.O_RDWR,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	defer allocationLock.Close()
	if err := syscall.Flock(int(allocationLock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(allocationLock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	now := s.clock()
	runID, dir, err := s.allocateRunDir(now)
	if err != nil {
		return nil, err
	}
	started := now.Format(time.RFC3339)
	run := &Run{
		SchemaVersion:  schemaver.Current(schemaver.LedgerRun),
		TokenSemantics: CurrentTokenSemantics,
		RunID:          runID,
		ProjectID:      projectID,
		EngineVersion:  engineVersion,
		StartedAt:      started,
		UpdatedAt:      started,
		Status:         RunRunning,
		Source:         source,
		Slots:          map[string]*Slot{},
	}
	save := s.saveRun
	if save == nil {
		save = s.SaveRun
	}
	if err := save(run); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := s.repointLatest(runID); err != nil {
		return nil, err
	}
	return run, nil
}

func (s *Store) allocateRunDir(now time.Time) (string, string, error) {
	base := now.UTC().Format(runIDLayout)
	for collision := 0; collision < 1_000_000; collision++ {
		runID := base
		if collision > 0 {
			runID = fmt.Sprintf("%s-%06d", base, collision)
		}
		dir := filepath.Join(s.KoryphRoot, runID)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return runID, dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("allocate run ID at %s: collision limit exhausted", base)
}

// repointLatest atomically swaps the `latest` symlink to point at runID. The
// target is relative (bare runID) so the tree stays relocatable.
func (s *Store) repointLatest(runID string) error {
	if err := os.MkdirAll(s.KoryphRoot, 0o755); err != nil {
		return err
	}
	link := filepath.Join(s.KoryphRoot, latestLink)
	tempLink := filepath.Join(
		s.KoryphRoot,
		fmt.Sprintf(".latest-%s-%d", runID, os.Getpid()),
	)
	_ = os.Remove(tempLink)
	if err := os.Symlink(runID, tempLink); err != nil {
		return err
	}
	if err := os.Rename(tempLink, link); err != nil {
		_ = os.Remove(tempLink)
		return err
	}
	if root, err := os.Open(s.KoryphRoot); err == nil {
		_ = root.Sync()
		_ = root.Close()
	}
	return nil
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

// EvidenceDir returns the engine-owned evidence directory for one slot.
// It is deliberately outside PhaseDir: the latter is passed to workers for
// status, summaries, focused-test logs, and terminal results, while gate and
// semantic-review proofs are admitted only through this private namespace.
func (s *Store) EvidenceDir(runID, phaseID string) string {
	dir := filepath.Join(s.KoryphRoot, runID, evidenceDir, phaseID)
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// LoadRun reads ledger.json for runID.
func (s *Store) LoadRun(runID string) (*Run, error) {
	var run Run
	path := filepath.Join(s.KoryphRoot, runID, ledgerFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, _, err = schemaver.Migrate(schemaver.LedgerRun, raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if run.Slots == nil {
		run.Slots = map[string]*Slot{}
	}
	run.TokenSemantics = EffectiveTokenSemantics(&run)
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
// IDs are UTC timestamps with optional fixed-width collision suffixes, so
// reverse-lexical order is newest-first.
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
	path := filepath.Join(s.KoryphRoot, run.RunID, ledgerFile)
	if err := checkLedgerWrite(path, schemaver.LedgerRun); err != nil {
		return err
	}
	run.SchemaVersion = schemaver.Current(schemaver.LedgerRun)
	run.TokenSemantics = EffectiveTokenSemantics(run)
	now := s.clock().Format(time.RFC3339Nano)
	stampFinalizationTimings(run, now)
	run.UpdatedAt = now
	return fsx.WriteJSONAtomic(path, run)
}

// checkLedgerWrite prevents an older binary from replacing a newer ledger
// before the caller mutates its in-memory value or touches the file. Missing
// files are new allocations and therefore safe to stamp at this build's
// current surface version.
func checkLedgerWrite(path string, surface schemaver.Surface) error {
	var stamped struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := fsx.ReadJSON(path, &stamped); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return schemaver.CheckWrite(surface, stamped.SchemaVersion)
}

// SetSlot installs (or replaces) a slot keyed by its PhaseID, stamps the slot's
// UpdatedAt, and persists the run. Single-writer: no locking needed.
func (s *Store) SetSlot(run *Run, sl *Slot) error {
	if run.Slots == nil {
		run.Slots = map[string]*Slot{}
	}
	if previous := run.Slots[sl.PhaseID]; previous != nil {
		switch {
		case sl.Attempts < previous.Attempts:
			return fmt.Errorf("ledger: attempt regression for %s: %d after %d",
				sl.PhaseID, sl.Attempts, previous.Attempts)
		case sl.Attempts > previous.Attempts+1:
			return fmt.Errorf("ledger: attempt gap for %s: %d after %d",
				sl.PhaseID, sl.Attempts, previous.Attempts)
		case sl.Attempts == previous.Attempts &&
			sl.DispatchGeneration == previous.DispatchGeneration &&
			sl.SessionID == previous.SessionID:
			// Re-persisting the same launch is idempotent. Preserve its first
			// dispatch boundary and timing record.
			if previous.DispatchedAt != "" {
				sl.DispatchedAt = previous.DispatchedAt
			}
			sl.FinalizationTimings = previous.FinalizationTimings
		case sl.Attempts == previous.Attempts:
			// Fault accounting may intentionally keep the same logical attempt
			// number across a model relaunch. A new generation/session is still a
			// real implementation dispatch and must remain observable so the
			// autonomy canary can fail resume-without-redispatch.
			now := s.clock().Format(time.RFC3339Nano)
			closeActiveFinalization(previous, now)
			upsertAttemptSnapshot(run, previous)
		default:
			now := s.clock().Format(time.RFC3339Nano)
			closeActiveFinalization(previous, now)
			upsertAttemptSnapshot(run, previous)
		}
	}
	sl.UpdatedAt = s.clock().Format(time.RFC3339Nano)
	run.Slots[sl.PhaseID] = sl
	return s.SaveRun(run)
}

func upsertAttemptSnapshot(run *Run, sl *Slot) {
	if run == nil || sl == nil || sl.Attempts <= 0 {
		return
	}
	copy := cloneSlot(*sl)
	for i := range run.AttemptHistory {
		current := &run.AttemptHistory[i].Slot
		if current.PhaseID == copy.PhaseID && current.Attempts == copy.Attempts &&
			current.DispatchGeneration == copy.DispatchGeneration &&
			current.SessionID == copy.SessionID {
			run.AttemptHistory[i].Slot = copy
			return
		}
	}
	run.AttemptHistory = append(run.AttemptHistory, AttemptSnapshot{Slot: copy})
	sort.SliceStable(run.AttemptHistory, func(i, j int) bool {
		left, right := run.AttemptHistory[i].Slot, run.AttemptHistory[j].Slot
		if left.PhaseID != right.PhaseID {
			return left.PhaseID < right.PhaseID
		}
		if left.Attempts != right.Attempts {
			return left.Attempts < right.Attempts
		}
		if left.DispatchGeneration != right.DispatchGeneration {
			return left.DispatchGeneration < right.DispatchGeneration
		}
		return left.SessionID < right.SessionID
	})
}

func cloneSlot(sl Slot) Slot {
	sl.BeadLabels = append([]string(nil), sl.BeadLabels...)
	sl.Resources = append([]string(nil), sl.Resources...)
	if sl.Footprint != nil {
		footprint := *sl.Footprint
		footprint.Reads = append([]string(nil), sl.Footprint.Reads...)
		footprint.Writes = append([]string(nil), sl.Footprint.Writes...)
		sl.Footprint = &footprint
	}
	return sl
}

func stampFinalizationTimings(run *Run, now string) {
	if run == nil {
		return
	}
	for _, sl := range run.Slots {
		if sl == nil {
			continue
		}
		stage := sl.FinalizationStage
		timings := &sl.FinalizationTimings
		if timings.ActiveStage != stage {
			closeActiveFinalization(sl, now)
			if timing := timingForStage(timings, stage); timing != nil {
				queued := now
				if stage == "gate" && sl.FinalizationQueuedAt != "" {
					queued = sl.FinalizationQueuedAt
				}
				if timing.QueuedAt == "" {
					timing.QueuedAt = queued
				}
				if timing.StartedAt == "" {
					timing.StartedAt = now
				}
				timings.ActiveStage = stage
			}
		}
		if Terminal(sl.Status) {
			closeActiveFinalization(sl, now)
		}
	}
}

func closeActiveFinalization(sl *Slot, now string) {
	if sl == nil {
		return
	}
	timings := &sl.FinalizationTimings
	if timing := timingForStage(timings, timings.ActiveStage); timing != nil &&
		timing.StartedAt != "" && timing.CompletedAt == "" {
		timing.CompletedAt = now
	}
	timings.ActiveStage = ""
}

func timingForStage(timings *FinalizationTimings, stage string) *StageTimingEvidence {
	if timings == nil {
		return nil
	}
	switch stage {
	case "gate":
		return &timings.Gate
	case "review":
		return &timings.Review
	case "security-review":
		return &timings.SecurityReview
	case "merge":
		return &timings.Merge
	case "pr":
		return &timings.PR
	default:
		return nil
	}
}

// MutateSlot mutates the slot for phaseID in place via mut and stamps its
// UpdatedAt, WITHOUT persisting. Use it to batch several per-tick progress
// updates into a single SaveRun. A missing slot is created.
func (s *Store) MutateSlot(run *Run, phaseID string, mut func(*Slot)) {
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
}

// UpdateSlot mutates the slot for phaseID via mut and persists the run
// immediately. Use it for state transitions that must survive a crash; for
// per-tick progress refresh prefer MutateSlot + a single SaveRun. Single-writer:
// no locking needed.
func (s *Store) UpdateSlot(run *Run, phaseID string, mut func(*Slot)) error {
	s.MutateSlot(run, phaseID, mut)
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

// SaveManifest stamps the current manifest schema version and UpdatedAt, then
// writes the per-slot checkpoint at <run>/<phase>/manifest.json atomically.
func (s *Store) SaveManifest(runID, phaseID string, m *Manifest) error {
	path := filepath.Join(s.PhaseDir(runID, phaseID), manifestFile)
	if err := checkLedgerWrite(path, schemaver.LedgerManifest); err != nil {
		return err
	}
	m.SchemaVersion = schemaver.Current(schemaver.LedgerManifest)
	m.UpdatedAt = nowRFC3339()
	return fsx.WriteJSONAtomic(path, m)
}

// LoadManifest reads the per-slot checkpoint for phaseID within runID.
func (s *Store) LoadManifest(runID, phaseID string) (*Manifest, error) {
	var m Manifest
	phaseDir := s.PhaseDir(runID, phaseID)
	path := filepath.Join(phaseDir, manifestFile)
	read, err := fsx.ReadRegularConfined(path, maxManifestBytes, phaseDir)
	if err != nil {
		return nil, err
	}
	raw, _, err := schemaver.Migrate(schemaver.LedgerManifest, read.Data)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", read.Path, err)
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

	// Every admission—not only stale-lock recovery—takes the guard. GC uses the
	// same guard while deciding that no engine can be consuming shared caches,
	// closing the former empty-lock fast-path race with cache deletion.
	guard, err := acquireReclaimGuard(s.KoryphRoot)
	if err != nil {
		return nil, err
	}
	defer guard.release()

	l, err := acquireLock(path, runID)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}

	// Lock exists and we hold the reclaim guard: decide whether it is stale.
	if pid, ok := readLockPID(path); ok && processAlive(pid) {
		return nil, fmt.Errorf("koryph already running for run %s (pid %d, lock %s)", runID, pid, path)
	}
	// Stale or unreadable holder: clear it and re-acquire. No other process can
	// be in this section concurrently (guard held), so the removal cannot race a
	// fresh acquire.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return acquireLock(path, runID)
}

// WithRunAdmissionGuard runs fn while new engine admissions are excluded. It
// returns admitted=false when a live engine owns koryph.lock. A malformed lock
// fails closed; a stale, well-formed dead-PID lock does not prevent safe
// maintenance, and is left for the next RunLock acquisition to reclaim.
func (s *Store) WithRunAdmissionGuard(fn func() error) (admitted bool, err error) {
	if err := os.MkdirAll(s.KoryphRoot, 0o755); err != nil {
		return false, err
	}
	guard, err := acquireReclaimGuard(s.KoryphRoot)
	if err != nil {
		return false, err
	}
	defer guard.release()

	path := filepath.Join(s.KoryphRoot, lockFile)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return true, fn()
	} else if err != nil {
		return false, err
	}
	pid, ok := readLockPID(path)
	if !ok {
		return false, fmt.Errorf("ledger: refuse maintenance with unreadable lock %s", path)
	}
	if processAlive(pid) {
		return false, nil
	}
	return true, fn()
}

// reclaimGuard is an exclusive flock over the sidecar guard file that serializes
// RunLock's stale-lock reclaim critical section across processes.
type reclaimGuard struct{ f *os.File }

// acquireReclaimGuard takes an exclusive (blocking) flock on
// <root>/koryph.lock.guard. The guard file is never removed — it is only a
// flock anchor, and the kernel releases the flock on process exit even after a
// crash, so a dead holder never wedges the guard.
func acquireReclaimGuard(root string) (*reclaimGuard, error) {
	guardPath := filepath.Join(root, lockFile+".guard")
	f, err := os.OpenFile(guardPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	// Slow-lock watchdog (koryph-lwnq): this flock blocks indefinitely on a
	// held guard, so a one-shot timer names what it is waiting on if the wait
	// exceeds lockWaitWarnThreshold — otherwise a wedged reclaim looks
	// identical to any other silent wait. stop() fires the moment Flock
	// returns; the common (uncontended) case never logs.
	watchdog := time.AfterFunc(lockWaitWarnThreshold, func() {
		log.Info(fmt.Sprintf("ledger: still waiting on lock guard %s (running %s)", guardPath, lockWaitWarnThreshold),
			"path", guardPath)
	})
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	watchdog.Stop()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &reclaimGuard{f: f}, nil
}

// release drops the flock and closes the guard file.
func (g *reclaimGuard) release() {
	if g == nil || g.f == nil {
		return
	}
	_ = syscall.Flock(int(g.f.Fd()), syscall.LOCK_UN)
	_ = g.f.Close()
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

// LockHolder is a read-only peek at this project's process-singleton lock
// (koryph-1es): unlike RunLock it never creates, removes, or reclaims
// koryph.lock — it only reports what is there right now. ok is false when no
// lock file exists (no engine has ever run, or the last one shut down
// cleanly and Unlock removed it). When ok is true, pid is the recorded
// holder and alive reports whether that pid is currently live — the signal
// `koryph ops reconcile` uses to refuse touching a run a live engine still
// owns.
func (s *Store) LockHolder() (pid int, alive bool, ok bool) {
	path := filepath.Join(s.KoryphRoot, lockFile)
	pid, ok = readLockPID(path)
	if !ok {
		return 0, false, false
	}
	return pid, processAlive(pid), true
}

// LockPID returns the PID recorded in this project's koryph.lock WITHOUT
// probing whether it is alive — the caller supplies its own liveness func. This
// is the injectable-probe counterpart to LockHolder (which bakes in the
// real signal-0 probe): the cockpit provider passes its test-swappable alive
// stub so run-level liveness (ledger.RunDead) is unit-testable without real OS
// process state. ok is false when no lock file exists or it is malformed.
func (s *Store) LockPID() (pid int, ok bool) {
	return readLockPID(filepath.Join(s.KoryphRoot, lockFile))
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

// processAlive reports whether pid is a live process (signal-0 probe).
func processAlive(pid int) bool { return procx.Alive(pid) }

// hostname returns the machine hostname, or "unknown" if it cannot be read.
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}
