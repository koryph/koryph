// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gc

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
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
	DryRun      bool
}

var disposablePhaseCache = regexp.MustCompile(`^(?:go-cache|go-mod-cache|go-build[0-9]+)$`)

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
		// Skip the active run.
		if opts.ActiveRunID != "" && name == opts.ActiveRunID {
			cr.Skipped++
			continue
		}
		if name == latestTarget {
			cr.Skipped++
			continue
		}

		runDir := filepath.Join(koryphRoot, name)

		// Check if all slots are terminal before touching.
		phaseNames, terminal := terminalPhaseNames(runDir)
		if !terminal {
			cr.Skipped++
			continue
		}

		// Phase-local Go caches are disposable once every slot is terminal. Prune
		// them immediately rather than retaining them until the whole run reaches
		// its archival age; the rest of the phase evidence remains in place.
		prunePhaseCaches(runDir, phaseNames, &cr, opts.DryRun)

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
				if cerr := compressDir(runDir, archiveName, koryphRoot); cerr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("compress %s: %v", name, cerr))
					continue
				}
				// Remove the original dir after successful compression.
				if rerr := os.RemoveAll(runDir); rerr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("remove %s after compress: %v", name, rerr))
					continue
				}
			}
			cr.Compressed++
			cr.ReclaimedMB += sz
			alreadyArchived = true
		}

		// Step 2: delete if past deleteAfterDays.
		if !pol.DeleteAfterDaysNever && pol.DeleteAfterDays > 0 && ageDays >= pol.DeleteAfterDays {
			if !opts.DryRun {
				if alreadyArchived {
					archiveSz := fileSizeMB(archiveName)
					// Also remove the companion manifest.json if present.
					manifestPath := filepath.Join(koryphRoot, name+".manifest.json")
					_ = os.Remove(manifestPath)
					if rerr := os.Remove(archiveName); rerr != nil {
						cr.Errors = append(cr.Errors, fmt.Sprintf("delete archive %s.tar.gz: %v", name, rerr))
						continue
					}
					cr.ReclaimedMB += archiveSz
				} else if e.IsDir() {
					sz2 := dirSizeMB(runDir)
					if rerr := os.RemoveAll(runDir); rerr != nil {
						cr.Errors = append(cr.Errors, fmt.Sprintf("delete rundir %s: %v", name, rerr))
						continue
					}
					cr.ReclaimedMB += sz2
				}
			} else {
				// dry-run: count the archive or dir size as would-be-reclaimed
				if alreadyArchived {
					cr.ReclaimedMB += fileSizeMB(archiveName)
				}
			}
			cr.Deleted++
		}
	}

	return cr
}

// resolveLatest reads the "latest" symlink target (bare run ID).
func resolveLatest(koryphRoot string) string {
	target, err := os.Readlink(filepath.Join(koryphRoot, "latest"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// allSlotsTerminal reads ledger.json for the run and returns true only if
// all slots are terminal (or there are no slots). A missing or unreadable
// ledger is retained conservatively because its run state is unknown.
func allSlotsTerminal(runDir string) bool {
	_, terminal := terminalPhaseNames(runDir)
	return terminal
}

// terminalPhaseNames returns the direct child directory names that belong to
// terminal slots. The ledger is the authority for identifying phase dirs, so
// unrelated run-level directories are never considered for cache pruning.
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
	for _, sl := range run.Slots {
		if sl != nil && !ledger.Terminal(sl.Status) {
			return nil, false
		}
		if sl != nil && sl.PhaseID != "" && filepath.Base(sl.PhaseID) == sl.PhaseID {
			phaseNames = append(phaseNames, sl.PhaseID)
		}
	}
	return phaseNames, true
}

// prunePhaseCaches deletes only recognized cache directories directly below a
// phase directory. It never walks arbitrary phase contents, preserving the
// ledger, manifests, streams, logs, summaries, and unknown diagnostics.
func prunePhaseCaches(runDir string, phaseNames []string, cr *ClassResult, dryRun bool) {
	for _, phaseName := range phaseNames {
		phaseDir := filepath.Join(runDir, phaseName)
		entries, rerr := os.ReadDir(phaseDir)
		if rerr != nil {
			if !errors.Is(rerr, os.ErrNotExist) {
				cr.Errors = append(cr.Errors, fmt.Sprintf("read phase %s: %v", phaseName, rerr))
			}
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !disposablePhaseCache.MatchString(entry.Name()) {
				continue
			}
			cacheDir := filepath.Join(phaseDir, entry.Name())
			sz := dirSizeMB(cacheDir)
			cr.ScannedMB += sz
			if !dryRun {
				if rerr := removeAllWritable(cacheDir); rerr != nil {
					cr.Errors = append(cr.Errors, fmt.Sprintf("remove phase cache %s: %v", cacheDir, rerr))
					continue
				}
			}
			cr.ReclaimedMB += sz
			cr.Deleted++
		}
	}
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
	f, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	runBase := filepath.Base(runDir)
	err = filepath.WalkDir(runDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		// Skip symlinks.
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		rel, err := filepath.Rel(filepath.Dir(runDir), path)
		if err != nil {
			return err
		}

		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
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

	// Write companion manifest.json beside the archive (uncompressed).
	// We scan each phase dir for manifest.json and write them all.
	manifestOut := filepath.Join(koryphRoot, runBase+".manifest.json")
	writeCompanionManifest(runDir, manifestOut)

	return nil
}

// writeCompanionManifest writes a JSON object containing each phase's
// manifest.json content to manifestOut. Best-effort; errors are ignored.
func writeCompanionManifest(runDir, manifestOut string) {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return
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
		return
	}
	out, merr := json.MarshalIndent(combined, "", "  ")
	if merr != nil {
		return
	}
	_ = os.WriteFile(manifestOut, out, 0o600)
}

// --- jsonl log rotation gc -------------------------------------------------

// gcRotateLog handles size-based rotation for a single append-only JSONL log.
func gcRotateLog(logPath string, pol RotatePolicy, class string, opts Options) ClassResult {
	cr := ClassResult{Class: class, DryRun: opts.DryRun}
	now := opts.now()

	fi, err := os.Stat(logPath)
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
			if rerr := rotateGzip(logPath, rotatedGz); rerr != nil {
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

// rotateGzip compresses src into dst.gz and truncates src (log rotation).
func rotateGzip(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	gw := gzip.NewWriter(out)
	if _, err := io.Copy(gw, in); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}

	// Truncate the original log file (keep the inode alive so open writers
	// continue appending; the OS will re-use the space).
	if err := in.Close(); err != nil {
		return err
	}
	f, err := os.OpenFile(src, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
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
