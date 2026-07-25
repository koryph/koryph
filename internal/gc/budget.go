// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
)

var (
	planningCommitContains = func(repoRoot, commit, designPath string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: repoRoot, Name: "git",
			Args: []string{"cat-file", "-e", commit + ":" + filepath.ToSlash(designPath)},
		})
		return err == nil
	}
	planningEpicHasDigest = func(repoRoot, epicID, digest string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		issue, err := beads.New(repoRoot).Show(ctx, epicID)
		return err == nil && strings.Contains(issue.Notes, "planning-graph-digest: "+digest)
	}
)

var (
	commitDigestPattern = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)
	sha256Pattern       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	epicIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func gcProjectBudget(repoRoot string, cfg Config, opts Options) ClassResult {
	cr := ClassResult{Class: "project-budget", DryRun: opts.DryRun}
	policy := cfg.ProjectBudget.effective()
	now := opts.now()

	cacheRoot, cacheErr := validatedProjectCacheRoot(repoRoot)
	if cacheErr != nil {
		cr.Errors = append(cr.Errors, "validate shared Go cache: "+cacheErr.Error())
		cacheRoot = ""
	}
	total := projectArtifactBytes(repoRoot, cacheRoot)
	cr.ScannedMB = bytesMB(total)

	// Age policy applies even below the soft byte budget. It is intentionally
	// narrow: only terminal transcripts and filed planning snapshots are
	// eligible; compact evidence and live/failure-retained artifacts are not.
	aged := collectReclaimCandidates(repoRoot, now, policy, false)
	reclaimedPaths := make(map[string]bool)
	total = reclaimCandidates(aged, total, -1, policy, opts, &cr, reclaimedPaths)

	if total <= int64(policy.SoftMB)*1024*1024 {
		return cr
	}
	cr.Warnings = append(cr.Warnings, fmt.Sprintf(
		"project artifacts %.1f MiB exceed soft budget %d MiB",
		bytesMB(total), policy.SoftMB,
	))
	if total <= int64(policy.HardMB)*1024*1024 {
		return cr
	}

	// Above the hard limit, terminal success transcripts and full archives
	// that already have a compact evidence archive become immediately
	// reclaimable. Failure transcripts still keep their explicit retention
	// window.
	hard := collectReclaimCandidates(repoRoot, now, policy, true)
	target := int64(policy.SoftMB) * 1024 * 1024
	total = reclaimCandidates(hard, total, target, policy, opts, &cr, reclaimedPaths)

	if total > target {
		cacheBytes := pathBytes(cacheRoot)
		if cacheBytes > 0 {
			if opts.DryRun && hasNonterminalRun(repoRoot) {
				cr.Skipped++
				cr.Warnings = append(cr.Warnings,
					"shared Go cache retained while a nonterminal run may be using it")
			} else if opts.DryRun {
				cr.Deleted++
				cr.ReclaimedMB += bytesMB(cacheBytes)
				total -= cacheBytes
			} else {
				var reclaimed int64
				var retainedNonterminal bool
				admitted, err := ledger.NewStore(repoRoot).WithRunAdmissionGuard(func() error {
					if hasNonterminalRun(repoRoot) {
						retainedNonterminal = true
						return nil
					}
					reclaimed = pathBytes(cacheRoot)
					if reclaimed == 0 {
						return nil
					}
					return pruneProjectCache(cacheRoot)
				})
				switch {
				case err != nil:
					cr.Errors = append(cr.Errors, "coordinate shared Go cache prune: "+err.Error())
				case !admitted:
					cr.Skipped++
					cr.Warnings = append(cr.Warnings,
						"shared Go cache retained while the engine admission lock is live")
				case retainedNonterminal:
					cr.Skipped++
					cr.Warnings = append(cr.Warnings,
						"shared Go cache retained while a nonterminal run may be using it")
				default:
					cr.Deleted++
					cr.ReclaimedMB += bytesMB(reclaimed)
					total -= reclaimed
				}
			}
		}
	}

	if total > int64(policy.HardMB)*1024*1024 {
		cr.Warnings = append(cr.Warnings, fmt.Sprintf(
			"unreclaimable protected evidence leaves project at %.1f MiB above hard budget %d MiB",
			bytesMB(total), policy.HardMB,
		))
	}
	return cr
}

func collectReclaimCandidates(
	repoRoot string,
	now time.Time,
	policy ProjectBudgetPolicy,
	hard bool,
) []reclaimCandidate {
	var out []reclaimCandidate
	runRoot := paths.KoryphRoot(repoRoot)
	entries, _ := os.ReadDir(runRoot)
	for _, entry := range entries {
		path := filepath.Join(runRoot, entry.Name())
		if entry.IsDir() {
			run, ok := readTerminalRun(path)
			if !ok || retained(path) {
				continue
			}
			for _, slot := range run.Slots {
				if slot == nil || !ledger.Terminal(slot.Status) || !safePhaseName(slot.PhaseID) {
					continue
				}
				phaseDir := filepath.Join(path, slot.PhaseID)
				if retained(phaseDir) {
					continue
				}
				phaseEntries, _ := os.ReadDir(phaseDir)
				for _, artifact := range phaseEntries {
					if artifact.IsDir() || !transcriptArtifact(artifact.Name()) {
						continue
					}
					info, err := artifact.Info()
					if err != nil || !info.Mode().IsRegular() {
						continue
					}
					retainDays := policy.FailureRetainDays
					if successfulTerminalStatus(slot.Status) {
						retainDays = policy.TranscriptRetainDays
						if hard {
							retainDays = 0
						}
					}
					if retainDays > 0 && now.Sub(info.ModTime()) < time.Duration(retainDays)*24*time.Hour {
						continue
					}
					out = append(out, reclaimCandidate{
						path: filepath.Join(phaseDir, artifact.Name()),
						size: info.Size(), modTime: info.ModTime(), kind: "transcript",
					})
				}
			}
			continue
		}
		// A full archive is eligible only when its compact durable evidence
		// sibling already exists. Never make deletion itself create evidence
		// from an untrusted or incomplete archive.
		if strings.HasSuffix(entry.Name(), ".tar.gz") &&
			!strings.HasSuffix(entry.Name(), durableEvidenceArchiveExt) {
			evidence := strings.TrimSuffix(path, ".tar.gz") + durableEvidenceArchiveExt
			runID := strings.TrimSuffix(entry.Name(), ".tar.gz")
			if sourceRunBlocksArchiveReclaim(filepath.Join(runRoot, runID)) {
				continue
			}
			run, err := readCompactEvidenceRun(evidence, runID)
			if err != nil {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			retainDays := policy.TranscriptRetainDays
			if !allSlotsSuccessful(run) {
				retainDays = policy.FailureRetainDays
			} else if hard {
				retainDays = 0
			}
			if retainDays > 0 &&
				now.Sub(info.ModTime()) < time.Duration(retainDays)*24*time.Hour {
				continue
			}
			out = append(out, reclaimCandidate{
				path: path, size: info.Size(), modTime: info.ModTime(), kind: "full-archive",
			})
		}
	}

	planningDir := filepath.Join(paths.PlanLogs(repoRoot), "koryph-plan")
	planning, _ := os.ReadDir(planningDir)
	for _, entry := range planning {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".filed.json") ||
			strings.HasSuffix(entry.Name(), ".post.json") {
			continue
		}
		path := filepath.Join(planningDir, entry.Name())
		marker := path + ".filed.json"
		markerInfo, err := os.Lstat(marker)
		if err != nil || !markerInfo.Mode().IsRegular() ||
			markerInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		raw, err := os.ReadFile(marker)
		if err != nil || !validPlanningSnapshotMarker(repoRoot, path, raw) {
			continue
		}
		var filed planningSnapshotMarker
		if json.Unmarshal(raw, &filed) != nil {
			continue
		}
		postPath := filepath.Join(repoRoot, filepath.Clean(filed.PostFilePath))
		info, err := entry.Info()
		if err != nil || (!hard &&
			now.Sub(info.ModTime()) < time.Duration(policy.TranscriptRetainDays)*24*time.Hour) {
			continue
		}
		size := info.Size()
		if markerInfo != nil {
			size += markerInfo.Size()
		}
		size += regularFileBytes(postPath)
		out = append(out, reclaimCandidate{
			path: path, companions: []string{marker, postPath}, size: size,
			modTime: info.ModTime(), kind: "filed-planning-snapshot",
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].modTime.Equal(out[j].modTime) {
			return out[i].path < out[j].path
		}
		return out[i].modTime.Before(out[j].modTime)
	})
	return out
}

func validPlanningSnapshotMarker(repoRoot, snapshotPath string, raw []byte) bool {
	var marker planningSnapshotMarker
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&marker) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return false
	}
	if marker.Schema != planningSnapshotSchema ||
		!epicIDPattern.MatchString(marker.EpicID) ||
		!commitDigestPattern.MatchString(marker.DesignCommit) ||
		!sha256Pattern.MatchString(marker.SnapshotDigest) ||
		!sha256Pattern.MatchString(marker.GraphDigest) {
		return false
	}
	planDir := filepath.Join(paths.PlanLogs(repoRoot), "koryph-plan")
	snapshotRead, err := fsx.ReadRegularConfined(snapshotPath, 16<<20, planDir)
	if err != nil || contentSHA256(snapshotRead.Data) != marker.SnapshotDigest {
		return false
	}
	if filepath.IsAbs(marker.DesignPath) {
		return false
	}
	designPath := filepath.Clean(marker.DesignPath)
	if strings.HasPrefix(designPath, ".."+string(filepath.Separator)) ||
		!strings.HasPrefix(filepath.ToSlash(designPath), "docs/designs/") {
		return false
	}
	if filepath.IsAbs(marker.PostFilePath) {
		return false
	}
	postPath := filepath.Join(repoRoot, filepath.Clean(marker.PostFilePath))
	postRead, err := fsx.ReadRegularConfined(postPath, 16<<20, planDir)
	if err != nil || !strings.HasSuffix(postPath, ".post.json") ||
		contentSHA256(postRead.Data) != marker.GraphDigest {
		return false
	}
	return planningCommitContains(repoRoot, marker.DesignCommit, designPath) &&
		planningEpicHasDigest(repoRoot, marker.EpicID, marker.GraphDigest)
}

func contentSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%x", sum)
}

func reclaimCandidates(
	candidates []reclaimCandidate,
	total, target int64,
	policy ProjectBudgetPolicy,
	opts Options,
	cr *ClassResult,
	seen map[string]bool,
) int64 {
	for _, candidate := range candidates {
		if target >= 0 && total <= target {
			break
		}
		if seen[candidate.path] {
			continue
		}
		seen[candidate.path] = true
		info, err := os.Lstat(candidate.path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if opts.DryRun {
			reclaimed := projectedReclaimBytes(candidate, info, policy)
			cr.Deleted++
			cr.ReclaimedMB += bytesMB(reclaimed)
			total -= reclaimed
			continue
		}
		reclaimed := info.Size()
		if candidate.kind == "transcript" {
			oldTailBytes := regularFileBytes(candidate.path + ".tail")
			if err := preserveLogTail(candidate.path, int64(policy.LogTailKB)*1024); err != nil {
				cr.Errors = append(cr.Errors, "preserve log tail: "+err.Error())
				continue
			}
			reclaimed += oldTailBytes - regularFileBytes(candidate.path+".tail")
		}
		if err := os.Remove(candidate.path); err != nil {
			cr.Errors = append(cr.Errors, "remove "+candidate.path+": "+err.Error())
			continue
		}
		for _, companion := range candidate.companions {
			companionBytes := regularFileBytes(companion)
			if err := os.Remove(companion); err != nil && !errors.Is(err, os.ErrNotExist) {
				cr.Errors = append(cr.Errors, "remove "+companion+": "+err.Error())
			} else {
				reclaimed += companionBytes
			}
		}
		cr.Deleted++
		cr.ReclaimedMB += bytesMB(reclaimed)
		total -= reclaimed
	}
	return total
}

func projectedReclaimBytes(
	candidate reclaimCandidate,
	info fs.FileInfo,
	policy ProjectBudgetPolicy,
) int64 {
	reclaimed := info.Size()
	if candidate.kind == "transcript" {
		oldTailBytes := regularFileBytes(candidate.path + ".tail")
		newTailBytes := info.Size()
		limit := int64(policy.LogTailKB) * 1024
		if newTailBytes > limit {
			newTailBytes = limit
		}
		reclaimed += oldTailBytes - newTailBytes
	}
	for _, companion := range candidate.companions {
		reclaimed += regularFileBytes(companion)
	}
	if reclaimed < 0 {
		return 0
	}
	return reclaimed
}

func regularFileBytes(path string) int64 {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

func readTerminalRun(runDir string) (*ledger.Run, bool) {
	raw, err := os.ReadFile(filepath.Join(runDir, "ledger.json"))
	if err != nil {
		return nil, false
	}
	var run ledger.Run
	if json.Unmarshal(raw, &run) != nil || !terminalRunStatus(run.Status) {
		return nil, false
	}
	for _, slot := range run.Slots {
		if slot != nil && !ledger.Terminal(slot.Status) {
			return nil, false
		}
	}
	return &run, true
}

func readCompactEvidenceRun(archivePath, runID string) (*ledger.Run, error) {
	if err := validCompactEvidenceArchive(archivePath, runID); err != nil {
		return nil, err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	want := runID + "/ledger.json"
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("required ledger missing")
		}
		if err != nil {
			return nil, err
		}
		if header.Name != want || header.Typeflag != tar.TypeReg {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(reader, (16<<20)+1))
		if err != nil {
			return nil, err
		}
		var run ledger.Run
		if err := json.Unmarshal(raw, &run); err != nil {
			return nil, err
		}
		return &run, nil
	}
}

func sourceRunBlocksArchiveReclaim(runDir string) bool {
	info, err := os.Lstat(runDir)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	run, ok := readTerminalRun(runDir)
	if !ok || run.RunID != filepath.Base(runDir) || retained(runDir) {
		return true
	}
	for _, slot := range run.Slots {
		if slot == nil || !safePhaseName(slot.PhaseID) {
			return true
		}
		phaseDir := filepath.Join(runDir, slot.PhaseID)
		phaseInfo, err := os.Lstat(phaseDir)
		if err != nil || !phaseInfo.IsDir() || phaseInfo.Mode()&os.ModeSymlink != 0 {
			return true
		}
		if retained(phaseDir) {
			return true
		}
	}
	return false
}

func allSlotsSuccessful(run *ledger.Run) bool {
	if run == nil || run.Status == ledger.RunAborted || len(run.Slots) == 0 {
		return false
	}
	for _, slot := range run.Slots {
		if slot == nil || !successfulTerminalStatus(slot.Status) {
			return false
		}
	}
	return true
}

func retained(path string) bool {
	info, err := os.Lstat(filepath.Join(path, explicitArtifactRetainFile))
	return err == nil && info.Mode().IsRegular()
}

func preserveLogTail(path string, limit int64) error {
	if limit <= 0 {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	tail := make([]byte, info.Size()-start)
	n, err := file.ReadAt(tail, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return fsx.WriteAtomic(path+".tail", tail[:n], 0o600)
}

func projectArtifactBytes(repoRoot, cacheRoot string) int64 {
	return pathBytes(paths.KoryphRoot(repoRoot)) +
		pathBytes(cacheRoot) +
		pathBytes(filepath.Join(paths.PlanLogs(repoRoot), "koryph-plan"))
}

func pathBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func bytesMB(bytes int64) float64 { return float64(bytes) / (1024 * 1024) }

func hasNonterminalRun(repoRoot string) bool {
	entries, err := os.ReadDir(paths.KoryphRoot(repoRoot))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if !entry.IsDir() || !runDirectoryName(entry.Name()) {
			continue
		}
		if _, ok := readTerminalRun(filepath.Join(paths.KoryphRoot(repoRoot), entry.Name())); !ok {
			return true
		}
	}
	return false
}

func validatedProjectCacheRoot(repoRoot string) (string, error) {
	current := repoRoot
	for _, name := range []string{".git", "koryph-cache"} {
		current = filepath.Join(current, name)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Join(repoRoot, ".git", "koryph-cache"), nil
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is not a real directory", current)
		}
	}
	return current, nil
}

func pruneProjectCache(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(root, "gc.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == filepath.Base(lockPath) {
			continue
		}
		if err := removeAllWritable(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
