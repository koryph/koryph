// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gc

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
	"golang.org/x/sys/unix"
)

// ClassResult is the per-artifact-class summary from a GC run.
type ClassResult struct {
	Class       string  // "run-dirs" | "audit-log" | "runs-index" | "telemetry"
	ScannedMB   float64 // total size of files considered
	ReclaimedMB float64 // bytes freed (0 in dry-run)
	Compressed  int     // archives created
	Deleted     int     // files/dirs removed
	Skipped     int     // exempted items (active run, live slots, posture snapshots)
	Errors      []string
	Warnings    []string
	DryRun      bool
}

var disposablePhaseCache = regexp.MustCompile(
	`^(?:cache|go-cache|go-mod-cache|go-build[0-9]+|go-tmp|go-telemetry|` +
		`gocache|gomodcache|go-build-cache|runtime-cache)$`,
)

var archiveCopy = io.Copy
var logArchiveCopy = io.Copy

// Result is the aggregate output of a GC run.
type Result struct {
	At      string
	DryRun  bool
	Classes []ClassResult
}

// TotalReclaimedMB returns the sum of reclaimed bytes across all classes.
func (r *Result) TotalReclaimedMB() float64 {
	var total float64
	for _, c := range r.Classes {
		total += c.ReclaimedMB
	}
	return total
}

// Options configures a GC run.
type Options struct {
	// RepoRoot is the project repository root. May be "" for global-only gc.
	RepoRoot string
	// DryRun, when true, scans and reports without making any changes.
	DryRun bool
	// Config overrides automatic config loading (useful in tests).
	Config *Config
	// Now is injectable for tests.
	Now func() time.Time
	// ActiveRunID is the current run ID to exempt from gc. May be "".
	ActiveRunID string
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Run applies the retention policy and returns a Result.
func Run(opts Options) (*Result, error) {
	var cfg Config
	if opts.Config != nil {
		cfg = opts.Config.effective()
	} else {
		var err error
		cfg, err = LoadConfig(opts.RepoRoot)
		if err != nil {
			return nil, err
		}
	}

	res := &Result{
		At:     opts.now().UTC().Format(time.RFC3339),
		DryRun: opts.DryRun,
	}

	if opts.RepoRoot != "" {
		rdc := gcRunDirs(opts.RepoRoot, cfg, opts)
		res.Classes = append(res.Classes, rdc)
		budget := gcProjectBudget(opts.RepoRoot, cfg, opts)
		res.Classes = append(res.Classes, budget)
	}

	auditC := gcRotateLog(paths.AuditLog(), cfg.AuditLog, "audit-log", opts)
	res.Classes = append(res.Classes, auditC)

	runsC := gcRotateLog(paths.RunsIndex(), cfg.RunsIndex, "runs-index", opts)
	res.Classes = append(res.Classes, runsC)

	return res, nil
}

// --- run-dirs gc -----------------------------------------------------------

// gcRunDirs compresses and deletes old run phase-directories.
func gcRunDirs(repoRoot string, cfg Config, opts Options) ClassResult {
	cr := ClassResult{Class: "run-dirs", DryRun: opts.DryRun}
	koryphRoot := paths.KoryphRoot(repoRoot)
	pol := cfg.RunDirs
	now := opts.now()

	entries, err := os.ReadDir(koryphRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cr
		}
		cr.Errors = append(cr.Errors, fmt.Sprintf("read koryphRoot: %v", err))
		return cr
	}

	// Resolve the "latest" symlink target so we can exempt it.
	latestTarget := resolveLatest(koryphRoot)

	for _, e := range entries {
		name := e.Name()
		// Skip non-run entries: symlinks (latest), lock files, archives.
		if !e.IsDir() || name == "latest" || name == "koryph.lock" {
			continue
		}
		runDir := filepath.Join(koryphRoot, name)
		if retained(runDir) {
			cr.Skipped++
			continue
		}

		// Terminal phase scratch becomes reclaimable immediately, even while
		// another slot in the same run remains live. The ledger—not directory
		// naming—is authoritative, so live slot trees remain untouched.
		phaseNames, terminal := terminalPhaseNames(runDir)
		prunedMB := prunePhaseCaches(runDir, phaseNames, &cr, opts.DryRun)
		if opts.ActiveRunID != "" && name == opts.ActiveRunID {
			cr.Skipped++
			continue
		}
		if !terminal {
			cr.Skipped++
			continue
		}
		if anyRetainedPhase(runDir, phaseNames) {
			cr.Skipped++
			continue
		}

		// "latest" protects the terminal run's durable evidence from ordinary
		// archival/deletion, not its compiler scratch. Keeping temporary trees
		// merely because the symlink has not advanced defeats terminal cleanup.
		if name == latestTarget {
			cr.Skipped++
			continue
		}

		// Determine run age from the directory mtime.
		fi, serr := os.Lstat(runDir)
		if serr != nil {
			cr.Errors = append(cr.Errors, fmt.Sprintf("stat %s: %v", name, serr))
			continue
		}
		age := now.Sub(fi.ModTime())
		ageDays := int(age.Hours() / 24)
		sz := dirSizeMB(runDir)
		cr.ScannedMB += sz

		archiveName := runDir + ".tar.gz"
		alreadyArchived := fileExists(archiveName)

		// Step 1: compress if old enough and not yet archived.
		if !alreadyArchived && !pol.CompressAfterDaysNever &&
			pol.CompressAfterDays > 0 && ageDays >= pol.CompressAfterDays {
			if !opts.DryRun {
				beforeSource := pathBytes(runDir)
				beforeSiblings := runArchiveSiblingBytes(runDir)
				if terr := preserveTerminalTranscriptTails(
					runDir, phaseNames, int64(cfg.ProjectBudget.effective().LogTailKB)*1024,
				); terr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("preserve transcript tails %s: %v", name, terr))
					continue
				}
				evidencePath := runDir + durableEvidenceArchiveExt
				if err := validCompactEvidenceArchive(evidencePath, name); err != nil {
					if cerr := compressEvidenceDir(runDir, evidencePath); cerr != nil {
						cr.Errors = append(cr.Errors, fmt.Sprintf("compact durable evidence %s: %v", name, cerr))
						continue
					}
				}
				if err := validCompactEvidenceArchive(evidencePath, name); err != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("validate compact durable evidence %s: %v", name, err))
					continue
				}
				if cerr := compressDir(runDir, archiveName, koryphRoot); cerr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("compress %s: %v", name, cerr))
					continue
				}
				// Remove the original dir after successful compression.
				if rerr := os.RemoveAll(runDir); rerr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("remove %s after compress: %v", name, rerr))
					continue
				}
				reclaimed := beforeSource - (runArchiveSiblingBytes(runDir) - beforeSiblings)
				if reclaimed > 0 {
					cr.ReclaimedMB += bytesMB(reclaimed)
				}
			}
			cr.Compressed++
			alreadyArchived = true
		}

		// Step 2: delete if past deleteAfterDays.
		if !pol.DeleteAfterDaysNever && pol.DeleteAfterDays > 0 && ageDays >= pol.DeleteAfterDays {
			// Deletion never outranks compact durable evidence. This matters
			// when full-run compression was explicitly disabled: the prior
			// policy could remove the only ledger/review/gate copy at the
			// delete horizon.
			evidencePath := runDir + durableEvidenceArchiveExt
			evidenceBefore := regularFileBytes(evidencePath)
			evidenceValidBefore := validCompactEvidenceArchive(evidencePath, name) == nil
			if !opts.DryRun {
				if terr := preserveTerminalTranscriptTails(
					runDir, phaseNames, int64(cfg.ProjectBudget.effective().LogTailKB)*1024,
				); terr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("preserve transcript tails %s: %v", name, terr))
					continue
				}
				if err := validCompactEvidenceArchive(evidencePath, name); err != nil {
					if cerr := compressEvidenceDir(runDir, evidencePath); cerr != nil {
						cr.Errors = append(cr.Errors, fmt.Sprintf(
							"preserve compact evidence before deleting %s: %v", name, cerr,
						))
						continue
					}
				}
				if err := validCompactEvidenceArchive(evidencePath, name); err != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf(
						"validate compact evidence before deleting %s: %v", name, err,
					))
					continue
				}
			}
			if !opts.DryRun {
				if alreadyArchived {
					// Also remove the companion manifest.json if present.
					manifestPath := filepath.Join(koryphRoot, name+".manifest.json")
					removedBytes := regularFileBytes(archiveName)
					if manifestBytes := regularFileBytes(manifestPath); manifestBytes > 0 {
						if err := os.Remove(manifestPath); err == nil {
							removedBytes += manifestBytes
						}
					}
					if rerr := os.Remove(archiveName); rerr != nil {
						cr.Errors = append(cr.Errors, fmt.Sprintf("delete archive %s.tar.gz: %v", name, rerr))
						continue
					}
					evidenceGrowth := regularFileBytes(evidencePath) - evidenceBefore
					if reclaimed := removedBytes - evidenceGrowth; reclaimed > 0 {
						cr.ReclaimedMB += bytesMB(reclaimed)
					}
				} else if e.IsDir() {
					sourceBytes := pathBytes(runDir)
					if rerr := os.RemoveAll(runDir); rerr != nil {
						cr.Errors = append(cr.Errors, fmt.Sprintf("delete rundir %s: %v", name, rerr))
						continue
					}
					evidenceGrowth := regularFileBytes(evidencePath) - evidenceBefore
					if reclaimed := sourceBytes - evidenceGrowth; reclaimed > 0 {
						cr.ReclaimedMB += bytesMB(reclaimed)
					}
				}
			} else {
				// Compression ratios and a not-yet-created compact archive are
				// unknowable without mutating state. Dry-run therefore reports
				// only bytes whose retained-evidence cost is already materialized.
				if alreadyArchived {
					cr.ReclaimedMB += fileSizeMB(archiveName)
				} else if evidenceValidBefore {
					cr.ReclaimedMB += dirSizeMB(runDir) - prunedMB
				}
			}
			cr.Deleted++
		}
	}

	return cr
}

func runArchiveSiblingBytes(runDir string) int64 {
	return regularFileBytes(runDir+".tar.gz") +
		regularFileBytes(runDir+durableEvidenceArchiveExt) +
		regularFileBytes(filepath.Join(filepath.Dir(runDir), filepath.Base(runDir)+".manifest.json"))
}

// resolveLatest reads the "latest" symlink target (bare run ID).
func resolveLatest(koryphRoot string) string {
	target, err := os.Readlink(filepath.Join(koryphRoot, "latest"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// terminalPhaseNames returns every terminal slot phase plus whether the whole
// run is terminal. This permits immediate per-slot scratch cleanup without
// making a partially-running run eligible for archive or deletion.
func terminalPhaseNames(runDir string) ([]string, bool) {
	ledgerPath := filepath.Join(runDir, "ledger.json")
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		return nil, false
	}
	var run ledger.Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, false
	}
	phaseNames := make([]string, 0, len(run.Slots))
	allSlotsTerminal := true
	for _, sl := range run.Slots {
		if sl != nil && !ledger.Terminal(sl.Status) {
			allSlotsTerminal = false
		}
		if sl != nil && ledger.Terminal(sl.Status) && safePhaseName(sl.PhaseID) {
			phaseNames = append(phaseNames, sl.PhaseID)
		}
	}
	return phaseNames, terminalRunStatus(run.Status) && allSlotsTerminal
}

func terminalRunStatus(status string) bool {
	switch status {
	case ledger.RunDone, ledger.RunDrained, ledger.RunAborted:
		return true
	}
	return false
}

func anyRetainedPhase(runDir string, phaseNames []string) bool {
	for _, phaseName := range phaseNames {
		if retained(filepath.Join(runDir, phaseName)) {
			return true
		}
	}
	return false
}

func safePhaseName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}

// prunePhaseCaches deletes only recognized cache directories directly below a
// phase directory. It never walks arbitrary phase contents, preserving the
// ledger, manifests, streams, logs, summaries, and unknown diagnostics.
func prunePhaseCaches(runDir string, phaseNames []string, cr *ClassResult, dryRun bool) float64 {
	var prunedMB float64
	for _, phaseName := range phaseNames {
		phaseDir := filepath.Join(runDir, phaseName)
		if retained(phaseDir) {
			cr.Skipped++
			continue
		}
		entries, rerr := os.ReadDir(phaseDir)
		if rerr != nil {
			if !errors.Is(rerr, os.ErrNotExist) {
				cr.Errors = append(cr.Errors, fmt.Sprintf("read phase %s: %v", phaseName, rerr))
			}
			continue
		}
		for _, entry := range entries {
			if !disposablePhaseCache.MatchString(entry.Name()) {
				continue
			}
			cacheDir := filepath.Join(phaseDir, entry.Name())
			if entry.Type()&os.ModeSymlink != 0 {
				if !dryRun {
					if rerr := os.Remove(cacheDir); rerr != nil {
						cr.Errors = append(cr.Errors, fmt.Sprintf("unlink phase cache symlink %s: %v", cacheDir, rerr))
						continue
					}
				}
				cr.Deleted++
				continue
			}
			if !entry.IsDir() {
				continue
			}
			sz := dirSizeMB(cacheDir)
			cr.ScannedMB += sz
			if !dryRun {
				if rerr := removeAllWritable(cacheDir); rerr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("remove phase cache %s: %v", cacheDir, rerr))
					continue
				}
			}
			cr.ReclaimedMB += sz
			prunedMB += sz
			cr.Deleted++
		}
		reviewScratch := filepath.Join(runDir, ".engine-evidence", phaseName, ".runtime-scratch")
		if info, err := os.Lstat(reviewScratch); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if !dryRun {
				if rerr := os.Remove(reviewScratch); rerr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("unlink review scratch symlink %s: %v", reviewScratch, rerr))
					continue
				}
			}
			cr.Deleted++
		} else if err == nil && info.IsDir() {
			sz := dirSizeMB(reviewScratch)
			cr.ScannedMB += sz
			if !dryRun {
				if rerr := removeAllWritable(reviewScratch); rerr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("remove review scratch %s: %v", reviewScratch, rerr))
					continue
				}
			}
			cr.ReclaimedMB += sz
			prunedMB += sz
			cr.Deleted++
		}
	}
	return prunedMB
}

// PruneTerminalSlotScratch performs the terminal-immediate lifecycle step
// without running project-budget scans or global log rotation. The engine
// calls it after a durable terminal slot transition; the ledger is re-read as
// authority, so an early/nonterminal call is a no-op.
func PruneTerminalSlotScratch(repoRoot, runID, phaseID string) error {
	if !runDirectoryName(runID) || !safePhaseName(phaseID) {
		return nil
	}
	runDir := filepath.Join(paths.KoryphRoot(repoRoot), runID)
	if retained(runDir) {
		return nil
	}
	terminal, _ := terminalPhaseNames(runDir)
	found := false
	for _, candidate := range terminal {
		if candidate == phaseID {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	result := ClassResult{Class: "terminal-slot"}
	prunePhaseCaches(runDir, []string{phaseID}, &result, false)
	if len(result.Errors) > 0 {
		return errors.New(strings.Join(result.Errors, "; "))
	}
	return nil
}

func preserveTerminalTranscriptTails(runDir string, phaseNames []string, limit int64) error {
	for _, phaseName := range phaseNames {
		entries, err := os.ReadDir(filepath.Join(runDir, phaseName))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !transcriptArtifact(entry.Name()) {
				continue
			}
			path := filepath.Join(runDir, phaseName, entry.Name())
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if err := preserveLogTail(path, limit); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeAllWritable handles Go module-cache directories whose downloaded
// contents are intentionally read-only. Only directories inside an already
// recognized disposable cache are made owner-writable before removal.
func removeAllWritable(path string) error {
	err := filepath.WalkDir(path, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.Chmod(current, info.Mode().Perm()|0o700)
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// compressDir creates a .tar.gz of runDir at archivePath.
// It also writes a companion <runID>.manifest.json beside the archive
// containing the run's manifest.json content (if present), so that history
// queries can introspect archived runs without decompressing.
func compressDir(runDir, archivePath, koryphRoot string) error {
	if err := writeTarGzipAtomic(runDir, archivePath, func(string, fs.DirEntry) bool {
		return true
	}); err != nil {
		return err
	}

	// Write companion manifest.json beside the archive (uncompressed).
	// We scan each phase dir for manifest.json and write them all.
	manifestOut := filepath.Join(koryphRoot, filepath.Base(runDir)+".manifest.json")
	return writeCompanionManifest(runDir, manifestOut)
}

func compressEvidenceDir(runDir, archivePath string) error {
	return writeTarGzipAtomic(runDir, archivePath, func(rel string, entry fs.DirEntry) bool {
		if strings.Contains("/"+filepath.ToSlash(filepath.Clean(rel))+"/", "/.runtime-scratch/") {
			return false
		}
		return entry.IsDir() || durableEvidencePath(rel)
	})
}

// validCompactEvidenceArchive authenticates the minimum recovery authority
// before a full run directory/archive may be discarded. Existence is not
// evidence: the path must be a regular gzip/tar, confined to the expected run
// prefix, with exactly one bounded terminal ledger naming that run.
func validCompactEvidenceArchive(archivePath, expectedRunID string) error {
	info, err := os.Lstat(archivePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("not a regular archive")
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	ledgerName := expectedRunID + "/ledger.json"
	ledgerCount := 0
	var run ledger.Run
	files := make(map[string][]byte)
	var totalBytes int64
	for entries := 0; ; entries++ {
		if entries > 100000 {
			return fmt.Errorf("archive entry limit exceeded")
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		clean := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if clean != header.Name ||
			(clean != expectedRunID && !strings.HasPrefix(clean, expectedRunID+"/")) {
			return fmt.Errorf("entry %q escapes expected run", header.Name)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size < 0 || header.Size > 16<<20 ||
			totalBytes+header.Size > 256<<20 {
			return fmt.Errorf("compact evidence entry exceeds bound")
		}
		raw, err := io.ReadAll(io.LimitReader(tr, header.Size+1))
		if err != nil {
			return err
		}
		if int64(len(raw)) != header.Size {
			return fmt.Errorf("truncated compact evidence entry")
		}
		totalBytes += header.Size
		if _, duplicate := files[clean]; duplicate {
			return fmt.Errorf("duplicate archive entry %q", clean)
		}
		files[clean] = raw
		if clean != ledgerName {
			continue
		}
		ledgerCount++
		if ledgerCount > 1 {
			return fmt.Errorf("duplicate ledger")
		}
		if err := json.Unmarshal(raw, &run); err != nil {
			return fmt.Errorf("decode ledger: %w", err)
		}
	}
	if ledgerCount != 1 {
		return fmt.Errorf("required ledger missing")
	}
	if run.RunID != expectedRunID || !terminalRunStatus(run.Status) {
		return fmt.Errorf("ledger does not authenticate terminal run %s", expectedRunID)
	}
	for _, slot := range run.Slots {
		if slot == nil {
			continue
		}
		if !ledger.Terminal(slot.Status) {
			return fmt.Errorf("ledger contains nonterminal slot")
		}
		if !safePhaseName(slot.PhaseID) {
			return fmt.Errorf("ledger contains unsafe phase id")
		}
		manifestName := expectedRunID + "/" + slot.PhaseID + "/manifest.json"
		if len(files[manifestName]) == 0 {
			return fmt.Errorf("required manifest missing for phase %s", slot.PhaseID)
		}
		if stored := strings.TrimSpace(slot.CandidateResultPath); stored != "" {
			name, ok := storedArtifactArchiveName(stored, expectedRunID)
			if !ok || len(files[name]) == 0 {
				return fmt.Errorf("candidate result missing for phase %s", slot.PhaseID)
			}
		}
		for _, artifact := range []struct {
			label  string
			stored string
			digest string
		}{
			{"gate evidence", slot.GateEvidencePath, slot.GateEvidenceDigest},
			{"general review", slot.GeneralReviewArtifactPath, slot.GeneralReviewArtifactDigest},
			{"security review", slot.SecurityReviewArtifactPath, slot.SecurityReviewArtifactDigest},
		} {
			stored := strings.TrimSpace(artifact.stored)
			digest := strings.TrimSpace(artifact.digest)
			if (stored == "") != (digest == "") {
				return fmt.Errorf("%s path/digest pair incomplete for phase %s", artifact.label, slot.PhaseID)
			}
			if stored == "" {
				continue
			}
			if !sha256Pattern.MatchString(digest) {
				return fmt.Errorf("%s digest malformed for phase %s", artifact.label, slot.PhaseID)
			}
			name, ok := storedArtifactArchiveName(stored, expectedRunID)
			if !ok || len(files[name]) == 0 {
				return fmt.Errorf("%s missing for phase %s", artifact.label, slot.PhaseID)
			}
			sum := sha256.Sum256(files[name])
			if got := fmt.Sprintf("sha256:%x", sum); got != digest {
				return fmt.Errorf("%s digest mismatch for phase %s", artifact.label, slot.PhaseID)
			}
		}
	}
	return nil
}

func storedArtifactArchiveName(stored, runID string) (string, bool) {
	clean := filepath.ToSlash(filepath.Clean(stored))
	prefix := runID + "/"
	if strings.HasPrefix(clean, prefix) {
		return clean, true
	}
	needle := "/" + prefix
	if index := strings.LastIndex(clean, needle); index >= 0 {
		return clean[index+1:], true
	}
	return "", false
}

// writeTarGzipAtomic never exposes a partial archive. An interrupted writer
// leaves only a uniquely named temp file, which a later GC may ignore and
// replace; the final path appears only after file and gzip/tar closure.
func writeTarGzipAtomic(
	runDir, archivePath string,
	include func(string, fs.DirEntry) bool,
) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(archivePath), "."+filepath.Base(archivePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create archive temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	gw := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gw)

	err = filepath.WalkDir(runDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		relWithin, err := filepath.Rel(runDir, path)
		if err != nil {
			return err
		}
		if !include(relWithin, entry) {
			return nil
		}
		relArchive, err := filepath.Rel(filepath.Dir(runDir), path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relArchive)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := archiveCopy(tw, source)
		closeErr := source.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, archivePath); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(archivePath)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// writeCompanionManifest writes a JSON object containing each phase's
// manifest.json content to manifestOut without following an attacker-planted
// destination symlink.
func writeCompanionManifest(runDir, manifestOut string) error {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return err
	}
	combined := map[string]json.RawMessage{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mpath := filepath.Join(runDir, e.Name(), "manifest.json")
		data, rerr := os.ReadFile(mpath)
		if rerr != nil {
			continue
		}
		combined[e.Name()] = json.RawMessage(data)
	}
	// Also include the top-level ledger.json.
	if data, rerr := os.ReadFile(filepath.Join(runDir, "ledger.json")); rerr == nil {
		combined["ledger"] = json.RawMessage(data)
	}
	if len(combined) == 0 {
		return nil
	}
	out, merr := json.MarshalIndent(combined, "", "  ")
	if merr != nil {
		return merr
	}
	return fsx.WriteAtomic(manifestOut, out, 0o600)
}

// --- jsonl log rotation gc -------------------------------------------------

// gcRotateLog handles size-based rotation for a single append-only JSONL log.
func gcRotateLog(logPath string, pol RotatePolicy, class string, opts Options) ClassResult {
	cr := ClassResult{Class: class, DryRun: opts.DryRun}
	now := opts.now()

	var active *os.File
	var fi os.FileInfo
	var err error
	if opts.DryRun {
		fi, err = os.Stat(logPath)
	} else {
		active, err = os.OpenFile(logPath, os.O_RDWR, 0o600)
		if err == nil {
			defer active.Close()
			if err = unix.Flock(int(active.Fd()), unix.LOCK_EX); err == nil {
				defer unix.Flock(int(active.Fd()), unix.LOCK_UN) //nolint:errcheck
				fi, err = active.Stat()
			}
		}
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cr
		}
		cr.Errors = append(cr.Errors, fmt.Sprintf("stat %s: %v", logPath, err))
		return cr
	}

	sizeMB := float64(fi.Size()) / (1024 * 1024)
	cr.ScannedMB += sizeMB
	rotateMB := float64(pol.RotateSizeMB)

	// Rotate the active log file if oversized.
	if sizeMB >= rotateMB {
		datePart := now.UTC().Format("20060102")
		dir := filepath.Dir(logPath)
		base := strings.TrimSuffix(filepath.Base(logPath), ".jsonl")
		rotatedGz := filepath.Join(dir, fmt.Sprintf("%s-%s.jsonl.gz", base, datePart))
		// Avoid collision by appending a counter.
		rotatedGz = uniquePath(rotatedGz)

		if !opts.DryRun {
			if rerr := rotateGzip(active, rotatedGz); rerr != nil {
				cr.Errors = append(cr.Errors, fmt.Sprintf("rotate %s: %v", logPath, rerr))
			} else {
				cr.Compressed++
				// Don't count the original file as "reclaimed" — we replaced it with
				// a smaller empty file; the compressed archive is smaller.
			}
		} else {
			cr.Compressed++ // would rotate
		}
	}

	// Prune rotated .jsonl.gz files past their retention window.
	if !pol.RetainDaysNever && pol.RetainDays > 0 {
		dir := filepath.Dir(logPath)
		base := strings.TrimSuffix(filepath.Base(logPath), ".jsonl")
		pattern := filepath.Join(dir, base+"-*.jsonl.gz")
		matches, gerr := filepath.Glob(pattern)
		if gerr == nil {
			cutoff := now.Add(-time.Duration(pol.RetainDays) * 24 * time.Hour)
			for _, m := range matches {
				mfi, serr := os.Stat(m)
				if serr != nil {
					continue
				}
				cr.ScannedMB += float64(mfi.Size()) / (1024 * 1024)
				if mfi.ModTime().Before(cutoff) {
					if !opts.DryRun {
						if rerr := os.Remove(m); rerr != nil {
							cr.Errors = append(cr.Errors, fmt.Sprintf("prune %s: %v", m, rerr))
							continue
						}
						cr.ReclaimedMB += float64(mfi.Size()) / (1024 * 1024)
					} else {
						cr.ReclaimedMB += float64(mfi.Size()) / (1024 * 1024)
					}
					cr.Deleted++
				}
			}
		}
	}

	return cr
}

// rotateGzip atomically publishes a gzip copy of the already exclusively
// locked active log and only then truncates the same inode. AppendLinePerm
// takes the same inode flock, so an append can land wholly before the copy or
// wholly after truncation, never in the lossy copy/truncate window.
func rotateGzip(in *os.File, dst string) (retErr error) {
	if in == nil {
		return fmt.Errorf("active log is not open")
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := out.Name()
	defer func() {
		_ = out.Close()
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err := out.Chmod(0o600); err != nil {
		return err
	}

	gw := gzip.NewWriter(out)
	if _, err := logArchiveCopy(gw, in); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	if err := in.Truncate(0); err != nil {
		return err
	}
	_, err = in.Seek(0, io.SeekStart)
	return err
}

// uniquePath appends a counter to path until the path does not exist.
func uniquePath(path string) string {
	if !fileExists(path) {
		return path
	}
	ext := filepath.Ext(path)               // .gz
	base := strings.TrimSuffix(path, ext)   // strip .gz
	ext2 := filepath.Ext(base)              // .jsonl
	base2 := strings.TrimSuffix(base, ext2) // strip .jsonl
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d%s%s", base2, i, ext2, ext)
		if !fileExists(candidate) {
			return candidate
		}
	}
	return path // fallback: overwrite
}

// --- helpers ---------------------------------------------------------------

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileSizeMB(path string) float64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(fi.Size()) / (1024 * 1024)
}

func dirSizeMB(dir string) float64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, ferr := d.Info()
		if ferr != nil {
			return nil
		}
		total += fi.Size()
		return nil
	})
	return float64(total) / (1024 * 1024)
}
