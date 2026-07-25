// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gc

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
)

func TestProjectBudgetPolicyDefaultsAndHardFloor(t *testing.T) {
	got := (ProjectBudgetPolicy{}).effective()
	if got.SoftMB != defaultProjectSoftMB ||
		got.HardMB != defaultProjectHardMB ||
		got.TranscriptRetainDays != defaultTranscriptDays ||
		got.FailureRetainDays != defaultFailureDays ||
		got.LogTailKB != defaultLogTailKB {
		t.Fatalf("defaults = %+v", got)
	}
	got = (ProjectBudgetPolicy{SoftMB: 10, HardMB: 5}).effective()
	if got.HardMB != 10 {
		t.Fatalf("hard budget = %d, want clamped soft floor 10", got.HardMB)
	}
}

func TestRunDirectoryNameAcceptsCollisionSafeAndLegacyIDs(t *testing.T) {
	for _, name := range []string{"20260725-153045", "20260725-153045-000001", "20260725-153045-999999"} {
		if !runDirectoryName(name) {
			t.Errorf("valid run ID rejected: %q", name)
		}
	}
	for _, name := range []string{
		"canary", "20260725-153045-x", "20260725-153045-00001",
		"20260725-153045-0000000", "20261325-153045-000001",
	} {
		if runDirectoryName(name) {
			t.Errorf("invalid run ID accepted: %q", name)
		}
	}
}

func TestTerminalTranscriptAgesOutButCompactEvidenceAndCanaryRemain(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "merged")
	phase := filepath.Join(runDir, "bead1")
	transcript := filepath.Join(phase, "session.log")
	content := strings.Repeat("prefix-", 20_000) + "TAIL"
	writeGCFile(t, transcript, []byte(content))
	for _, name := range []string{"manifest.json", "result.json", "SUMMARY.md", "gate-output.log"} {
		writeGCFile(t, filepath.Join(phase, name), []byte(name))
	}
	evidence := filepath.Join(runDir, ".evidence", "bead1", "general-review.json")
	writeGCFile(t, evidence, []byte(`{"schema":"koryph.review-artifact/v1"}`))
	canary := filepath.Join(repo, ".plan-logs", "koryph", "canary", "autonomous-loop-reliability.json")
	writeGCFile(t, canary, []byte(`{"schema":"koryph.autonomy-report/v1"}`))
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		RunDirs: RunDirPolicy{CompressAfterDaysNever: true, DeleteAfterDaysNever: true},
		ProjectBudget: ProjectBudgetPolicy{
			SoftMB: 100, HardMB: 200, TranscriptRetainDays: 1,
			FailureRetainDays: 30, LogTailKB: 1,
		},
	}.effective()
	t.Setenv("KORYPH_HOME", t.TempDir())
	if _, err := Run(Options{RepoRoot: repo, Config: &cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("aged transcript still exists: %v", err)
	}
	tail, err := os.ReadFile(transcript + ".tail")
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1024 || !strings.HasSuffix(string(tail), "TAIL") {
		t.Fatalf("tail length/suffix = %d/%q", len(tail), string(tail[len(tail)-4:]))
	}
	for _, path := range []string{
		filepath.Join(runDir, "ledger.json"),
		filepath.Join(phase, "manifest.json"),
		filepath.Join(phase, "result.json"),
		filepath.Join(phase, "SUMMARY.md"),
		filepath.Join(phase, "gate-output.log"),
		evidence,
		canary,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("durable evidence removed %s: %v", path, err)
		}
	}
}

func TestFailureTranscriptRetentionOutranksHardBudget(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "blocked")
	transcript := filepath.Join(runDir, "bead1", "stream.jsonl")
	writeGCFile(t, transcript, make([]byte, 2*1024*1024))
	cfg := Config{
		RunDirs: RunDirPolicy{CompressAfterDaysNever: true, DeleteAfterDaysNever: true},
		ProjectBudget: ProjectBudgetPolicy{
			SoftMB: 1, HardMB: 1, TranscriptRetainDays: 1,
			FailureRetainDays: 30, LogTailKB: 1,
		},
	}.effective()
	t.Setenv("KORYPH_HOME", t.TempDir())
	result, err := Run(Options{RepoRoot: repo, Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Fatalf("retained failure transcript removed: %v", err)
	}
	if len(classResult(t, result, "project-budget").Warnings) == 0 {
		t.Fatal("protected hard-budget overage was not reported")
	}
}

func TestFailureFullArchiveRetentionOutranksHardBudget(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "blocked")
	writeGCFile(t, filepath.Join(runDir, "bead1", "manifest.json"), []byte(`{"ok":true}`))
	evidence := runDir + durableEvidenceArchiveExt
	if err := compressEvidenceDir(runDir, evidence); err != nil {
		t.Fatal(err)
	}
	full := runDir + ".tar.gz"
	writeGCFile(t, full, make([]byte, 2*1024*1024))
	withinFailureWindow := time.Now().Add(-14 * 24 * time.Hour)
	if err := os.Chtimes(full, withinFailureWindow, withinFailureWindow); err != nil {
		t.Fatal(err)
	}
	policy := ProjectBudgetPolicy{
		TranscriptRetainDays: 7,
		FailureRetainDays:    30,
	}.effective()
	for _, candidate := range collectReclaimCandidates(repo, time.Now(), policy, true) {
		if candidate.path == full {
			t.Fatal("hard budget made failure full archive immediately reclaimable")
		}
	}

	afterFailureWindow := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(full, afterFailureWindow, afterFailureWindow); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range collectReclaimCandidates(repo, time.Now(), policy, true) {
		found = found || candidate.path == full
	}
	if !found {
		t.Fatal("failure full archive did not become reclaimable after retention window")
	}
}

func TestRetainedSourcePhaseProtectsSiblingFullArchive(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "merged")
	writeGCFile(t, filepath.Join(runDir, "bead1", "manifest.json"), []byte(`{"ok":true}`))
	writeGCFile(t, filepath.Join(runDir, "bead1", ".retain"), nil)
	evidence := runDir + durableEvidenceArchiveExt
	if err := compressEvidenceDir(runDir, evidence); err != nil {
		t.Fatal(err)
	}
	full := runDir + ".tar.gz"
	writeGCFile(t, full, make([]byte, 2*1024*1024))
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatal(err)
	}
	policy := ProjectBudgetPolicy{
		TranscriptRetainDays: 7,
		FailureRetainDays:    30,
	}.effective()
	for _, candidate := range collectReclaimCandidates(repo, time.Now(), policy, true) {
		if candidate.path == full {
			t.Fatal("retained source phase did not protect sibling full archive")
		}
	}
}

func TestSourceRunArchiveGuardRejectsCorruptOrRedirectedPhases(t *testing.T) {
	t.Run("run identity mismatch", func(t *testing.T) {
		_, runDir := gcCacheFixture(t, "merged")
		ledgerRaw := `{"run_id":"other-run","slots":{"bead1":{` +
			`"phase_id":"bead1","status":"merged"}},"status":"done"}`
		writeGCFile(t, filepath.Join(runDir, "ledger.json"), []byte(ledgerRaw))
		if !sourceRunBlocksArchiveReclaim(runDir) {
			t.Fatal("mismatched source ledger identity authorized archive reclaim")
		}
	})

	t.Run("unsafe phase id", func(t *testing.T) {
		_, runDir := gcCacheFixture(t, "merged")
		ledgerRaw := `{"run_id":"20260724-120000","slots":{"bead1":{` +
			`"phase_id":"../escape","status":"merged"}},"status":"done"}`
		writeGCFile(t, filepath.Join(runDir, "ledger.json"), []byte(ledgerRaw))
		if !sourceRunBlocksArchiveReclaim(runDir) {
			t.Fatal("unsafe source phase identity authorized archive reclaim")
		}
	})

	t.Run("redirected phase", func(t *testing.T) {
		_, runDir := gcCacheFixture(t, "merged")
		phaseDir := filepath.Join(runDir, "bead1")
		if err := os.RemoveAll(phaseDir); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		writeGCFile(t, filepath.Join(outside, ".retain"), nil)
		if err := os.Symlink(outside, phaseDir); err != nil {
			t.Fatal(err)
		}
		if !sourceRunBlocksArchiveReclaim(runDir) {
			t.Fatal("redirected source phase authorized archive reclaim")
		}
	})
}

func TestAbortedRunFullArchiveUsesFailureRetention(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "merged")
	ledgerRaw := `{"run_id":"20260724-120000","slots":{"bead1":{` +
		`"phase_id":"bead1","status":"merged"}},"status":"aborted"}`
	writeGCFile(t, filepath.Join(runDir, "ledger.json"), []byte(ledgerRaw))
	writeGCFile(t, filepath.Join(runDir, "bead1", "manifest.json"), []byte(`{"ok":true}`))
	evidence := runDir + durableEvidenceArchiveExt
	if err := compressEvidenceDir(runDir, evidence); err != nil {
		t.Fatal(err)
	}
	full := runDir + ".tar.gz"
	writeGCFile(t, full, make([]byte, 2*1024*1024))
	withinFailureWindow := time.Now().Add(-14 * 24 * time.Hour)
	if err := os.Chtimes(full, withinFailureWindow, withinFailureWindow); err != nil {
		t.Fatal(err)
	}
	policy := ProjectBudgetPolicy{
		TranscriptRetainDays: 7,
		FailureRetainDays:    30,
	}.effective()
	for _, candidate := range collectReclaimCandidates(repo, time.Now(), policy, true) {
		if candidate.path == full {
			t.Fatal("aborted run full archive used success retention")
		}
	}
}

func TestProjectBudgetAccountingIncludesRetainedTail(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "merged")
	transcript := filepath.Join(runDir, "bead1", "session.log")
	const transcriptBytes = 32 * 1024
	writeGCFile(t, transcript, make([]byte, transcriptBytes))
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		RunDirs: RunDirPolicy{CompressAfterDaysNever: true, DeleteAfterDaysNever: true},
		ProjectBudget: ProjectBudgetPolicy{
			SoftMB: 100, HardMB: 200, TranscriptRetainDays: 1,
			FailureRetainDays: 30, LogTailKB: 1,
		},
	}.effective()
	t.Setenv("KORYPH_HOME", t.TempDir())
	cacheRoot, err := validatedProjectCacheRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	before := projectArtifactBytes(repo, cacheRoot)
	result, err := Run(Options{RepoRoot: repo, Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	after := projectArtifactBytes(repo, cacheRoot)
	got := int64(classResult(t, result, "project-budget").ReclaimedMB * 1024 * 1024)
	want := before - after
	if got != want || want != transcriptBytes-1024 {
		t.Fatalf("reported/actual reclaim = %d/%d, want %d", got, want, transcriptBytes-1024)
	}
}

func TestProjectBudgetDryRunDoesNotCountAgedCandidateTwice(t *testing.T) {
	_, runDir := gcCacheFixture(t, "merged")
	transcript := filepath.Join(runDir, "bead1", "session.log")
	writeGCFile(t, transcript, make([]byte, 2*1024*1024))
	// Protected evidence keeps the projected total above the hard target after
	// the aged transcript is considered, forcing the immediate-hard pass too.
	writeGCFile(t, filepath.Join(runDir, ".retain"), nil)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}
	// Retain applies to the run, so construct the aged candidate directly;
	// this isolates the two-pass accounting from live mutation.
	candidate := reclaimCandidate{
		path: transcript, size: 2 * 1024 * 1024,
		modTime: old, kind: "transcript",
	}
	policy := (ProjectBudgetPolicy{LogTailKB: 1}).effective()
	result := ClassResult{DryRun: true}
	seen := make(map[string]bool)
	total := int64(4 * 1024 * 1024)
	total = reclaimCandidates([]reclaimCandidate{candidate}, total, -1, policy,
		Options{DryRun: true}, &result, seen)
	total = reclaimCandidates([]reclaimCandidate{candidate}, total, 1024*1024, policy,
		Options{DryRun: true}, &result, seen)
	want := float64(2*1024*1024-1024) / (1024 * 1024)
	if result.Deleted != 1 || result.ReclaimedMB != want {
		t.Fatalf("dry-run deleted/reclaimed = %d/%f, want 1/%f", result.Deleted, result.ReclaimedMB, want)
	}
	if total != 2*1024*1024+1024 {
		t.Fatalf("projected total = %d, want %d", total, 2*1024*1024+1024)
	}
}

func TestCanaryDirectoryDoesNotBlockIdleSharedCachePrune(t *testing.T) {
	repo, _ := gcCacheFixture(t, "merged")
	writeGCFile(t,
		filepath.Join(repo, ".plan-logs", "koryph", "canary", "autonomous-loop-reliability.json"),
		[]byte(`{"schema":"koryph.autonomy-report/v1"}`),
	)
	if hasNonterminalRun(repo) {
		t.Fatal("retained canary directory was misclassified as a live run")
	}
	corruptRun := filepath.Join(repo, ".plan-logs", "koryph", "20260725-120000")
	writeGCFile(t, filepath.Join(corruptRun, "ledger.json"), []byte("{"))
	if !hasNonterminalRun(repo) {
		t.Fatal("corrupt timestamped run did not fail closed")
	}
}

func TestProjectCacheHardBudgetDryRunLiveAndIdempotent(t *testing.T) {
	repo, _ := gcCacheFixture(t, "merged")
	cacheFile := filepath.Join(repo, ".git", "koryph-cache", "go", "build-key", "cache")
	writeGCFile(t, cacheFile, make([]byte, 2*1024*1024))
	cfg := Config{
		RunDirs: RunDirPolicy{CompressAfterDaysNever: true, DeleteAfterDaysNever: true},
		ProjectBudget: ProjectBudgetPolicy{
			SoftMB: 1, HardMB: 1, TranscriptRetainDays: 30,
			FailureRetainDays: 30,
		},
	}.effective()
	t.Setenv("KORYPH_HOME", t.TempDir())
	dry, err := Run(Options{RepoRoot: repo, Config: &cfg, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if classResult(t, dry, "project-budget").ReclaimedMB < 1 {
		t.Fatal("dry-run did not report cache reclaim")
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("dry-run mutated cache: %v", err)
	}
	if _, err := Run(Options{RepoRoot: repo, Config: &cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cacheFile); !os.IsNotExist(err) {
		t.Fatalf("cache survived hard budget: %v", err)
	}
	if _, err := Run(Options{RepoRoot: repo, Config: &cfg}); err != nil {
		t.Fatalf("idempotent rerun: %v", err)
	}
}

func TestProjectCachePruneRetainsCacheForLiveEngineAdmission(t *testing.T) {
	repo, _ := gcCacheFixture(t, "merged")
	cacheFile := filepath.Join(repo, ".git", "koryph-cache", "go", "build-key", "cache")
	writeGCFile(t, cacheFile, make([]byte, 2*1024*1024))
	lock, err := ledger.NewStore(repo).RunLock("live")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock() //nolint:errcheck
	cfg := Config{
		RunDirs: RunDirPolicy{CompressAfterDaysNever: true, DeleteAfterDaysNever: true},
		ProjectBudget: ProjectBudgetPolicy{
			SoftMB: 1, HardMB: 1, TranscriptRetainDays: 30,
			FailureRetainDays: 30,
		},
	}.effective()
	t.Setenv("KORYPH_HOME", t.TempDir())
	result, err := Run(Options{RepoRoot: repo, Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("live engine cache was pruned: %v", err)
	}
	class := classResult(t, result, "project-budget")
	if class.Skipped == 0 || len(class.Warnings) == 0 {
		t.Fatalf("live engine retention was not reported: %+v", class)
	}
}

func TestProjectCachePruneSerializesConcurrentCallers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	for i := 0; i < 10; i++ {
		writeGCFile(t, filepath.Join(root, "entry", string(rune('a'+i))), []byte("cache"))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pruneProjectCache(root)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "gc.lock" {
		t.Fatalf("cache after concurrent prune = %v", entries)
	}
}

func TestProjectCachePruneRejectsSymlinkRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	writeGCFile(t, sentinel, []byte("preserve"))
	cacheRoot := filepath.Join(repo, ".git", "koryph-cache")
	if err := os.Symlink(outside, cacheRoot); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		RunDirs:       RunDirPolicy{CompressAfterDaysNever: true, DeleteAfterDaysNever: true},
		ProjectBudget: ProjectBudgetPolicy{SoftMB: 1, HardMB: 1},
	}.effective()
	t.Setenv("KORYPH_HOME", t.TempDir())
	result, err := Run(Options{RepoRoot: repo, Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	class := classResult(t, result, "project-budget")
	if len(class.Errors) == 0 || !strings.Contains(class.Errors[0], "not a real directory") {
		t.Fatalf("redirected cache errors = %v", class.Errors)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("cache GC escaped project root: %v", err)
	}
	if err := pruneProjectCache(cacheRoot); err == nil {
		t.Fatal("direct cache prune accepted symlink root")
	}
}

func TestFiledPlanningSnapshotEligibleAndPostureSnapshotExempt(t *testing.T) {
	repo, _ := gcCacheFixture(t, "merged")
	planning := filepath.Join(repo, ".plan-logs", "koryph-plan", "plan.json")
	post := filepath.Join(repo, ".plan-logs", "koryph-plan", "plan.post.json")
	snapshotRaw := []byte(`{"issues":[]}`)
	postRaw := []byte(`{"strict":"ship"}`)
	writeGCFile(t, planning, snapshotRaw)
	writeGCFile(t, post, postRaw)
	marker, err := json.Marshal(planningSnapshotMarker{
		Schema: planningSnapshotSchema, EpicID: "proj-100",
		DesignPath:     "docs/designs/example.md",
		DesignCommit:   strings.Repeat("a", 40),
		SnapshotDigest: contentSHA256(snapshotRaw),
		GraphDigest:    contentSHA256(postRaw),
		PostFilePath:   ".plan-logs/koryph-plan/plan.post.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeGCFile(t, planning+".filed.json", marker)
	oldCommitCheck := planningCommitContains
	oldEpicCheck := planningEpicHasDigest
	planningCommitContains = func(string, string, string) bool { return true }
	planningEpicHasDigest = func(_ string, _ string, digest string) bool {
		return digest == contentSHA256(postRaw)
	}
	t.Cleanup(func() {
		planningCommitContains = oldCommitCheck
		planningEpicHasDigest = oldEpicCheck
	})
	posture := filepath.Join(repo, ".koryph", "snapshots", "settings.json")
	writeGCFile(t, posture, []byte(`{"rollback":true}`))
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(planning, old, old)
	cfg := Config{
		RunDirs: RunDirPolicy{CompressAfterDaysNever: true, DeleteAfterDaysNever: true},
		ProjectBudget: ProjectBudgetPolicy{
			SoftMB: 100, HardMB: 200, TranscriptRetainDays: 1,
		},
	}.effective()
	t.Setenv("KORYPH_HOME", t.TempDir())
	if _, err := Run(Options{RepoRoot: repo, Config: &cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(planning); !os.IsNotExist(err) {
		t.Fatalf("filed planning snapshot survived: %v", err)
	}
	if _, err := os.Stat(planning + ".filed.json"); !os.IsNotExist(err) {
		t.Fatalf("filed planning marker survived: %v", err)
	}
	if _, err := os.Stat(post); !os.IsNotExist(err) {
		t.Fatalf("filed strict post-file evidence survived: %v", err)
	}
	if _, err := os.Stat(posture); err != nil {
		t.Fatalf("posture snapshot was not exempt: %v", err)
	}
}

func TestPlanningMarkerFailsClosedWithoutAuthenticatedBindings(t *testing.T) {
	repo := t.TempDir()
	planning := filepath.Join(repo, ".plan-logs", "koryph-plan", "plan.json")
	post := filepath.Join(repo, ".plan-logs", "koryph-plan", "plan.post.json")
	snapshotRaw := []byte(`{"issues":[]}`)
	postRaw := []byte(`{"strict":"ship"}`)
	writeGCFile(t, planning, snapshotRaw)
	writeGCFile(t, post, postRaw)
	base := planningSnapshotMarker{
		Schema: planningSnapshotSchema, EpicID: "proj-100",
		DesignPath:     "docs/designs/example.md",
		DesignCommit:   strings.Repeat("a", 40),
		SnapshotDigest: contentSHA256(snapshotRaw),
		GraphDigest:    contentSHA256(postRaw),
		PostFilePath:   ".plan-logs/koryph-plan/plan.post.json",
	}
	raw, _ := json.Marshal(base)
	oldCommitCheck := planningCommitContains
	oldEpicCheck := planningEpicHasDigest
	t.Cleanup(func() {
		planningCommitContains = oldCommitCheck
		planningEpicHasDigest = oldEpicCheck
	})
	planningCommitContains = func(string, string, string) bool { return false }
	planningEpicHasDigest = func(string, string, string) bool { return true }
	if validPlanningSnapshotMarker(repo, planning, raw) {
		t.Fatal("marker accepted without committed design binding")
	}
	planningCommitContains = func(string, string, string) bool { return true }
	planningEpicHasDigest = func(string, string, string) bool { return false }
	if validPlanningSnapshotMarker(repo, planning, raw) {
		t.Fatal("marker accepted without Beads epic digest binding")
	}
}

func TestCompressionCreatesCompactEvidenceArchiveWithoutTranscript(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "merged")
	phase := filepath.Join(runDir, "bead1")
	writeGCFile(t, filepath.Join(phase, "manifest.json"), []byte(`{"ok":true}`))
	writeGCFile(t, filepath.Join(phase, "SUMMARY.md"), []byte("summary"))
	writeGCFile(t, filepath.Join(phase, "stream.jsonl"), []byte("large transcript"))
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(runDir, old, old); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		RunDirs: RunDirPolicy{CompressAfterDays: 1, DeleteAfterDaysNever: true},
		ProjectBudget: ProjectBudgetPolicy{
			SoftMB: 100, HardMB: 200, TranscriptRetainDays: 30,
		},
	}.effective()
	t.Setenv("KORYPH_HOME", t.TempDir())
	if _, err := Run(Options{RepoRoot: repo, Config: &cfg}); err != nil {
		t.Fatal(err)
	}
	evidencePath := runDir + durableEvidenceArchiveExt
	names := tarNames(t, evidencePath)
	if !containsSuffix(names, "/ledger.json") ||
		!containsSuffix(names, "/bead1/manifest.json") ||
		!containsSuffix(names, "/bead1/SUMMARY.md") {
		t.Fatalf("compact evidence archive missing durable files: %v", names)
	}
	if containsSuffix(names, "/bead1/stream.jsonl") {
		t.Fatalf("compact evidence archive retained transcript: %v", names)
	}
	if !containsSuffix(names, "/bead1/stream.jsonl.tail") {
		t.Fatalf("compact evidence archive omitted selected transcript tail: %v", names)
	}
}

func TestDeleteWithFullCompressionDisabledStillPreservesCompactEvidence(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "merged")
	phase := filepath.Join(runDir, "bead1")
	writeGCFile(t, filepath.Join(phase, "manifest.json"), []byte(`{"ok":true}`))
	writeGCFile(t, filepath.Join(phase, "review.json"), []byte(`{"verdict":{"blocking":false}}`))
	writeGCFile(t, filepath.Join(phase, "stream.jsonl"), []byte("large transcript"))
	// Existence alone must not authorize deletion. GC replaces this corrupt
	// placeholder atomically from the still-live source and validates it first.
	writeGCFile(t, runDir+durableEvidenceArchiveExt, []byte("not a gzip archive"))
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(runDir, old, old); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		RunDirs: RunDirPolicy{
			CompressAfterDaysNever: true,
			DeleteAfterDays:        1,
		},
		ProjectBudget: ProjectBudgetPolicy{SoftMB: 100, HardMB: 200},
	}.effective()
	t.Setenv("KORYPH_HOME", t.TempDir())
	if _, err := Run(Options{RepoRoot: repo, Config: &cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("expired full run directory survived: %v", err)
	}
	names := tarNames(t, runDir+durableEvidenceArchiveExt)
	for _, suffix := range []string{"/ledger.json", "/bead1/manifest.json", "/bead1/review.json"} {
		if !containsSuffix(names, suffix) {
			t.Errorf("compact evidence missing %s: %v", suffix, names)
		}
	}
	if containsSuffix(names, "/bead1/stream.jsonl") {
		t.Fatalf("compact evidence retained transcript: %v", names)
	}
	if !containsSuffix(names, "/bead1/stream.jsonl.tail") {
		t.Fatalf("compact evidence omitted selected transcript tail: %v", names)
	}
}

func TestInvalidCompactEvidenceCannotAuthorizeFullArchiveReclaim(t *testing.T) {
	for _, tc := range []string{"empty", "corrupt", "symlink"} {
		t.Run(tc, func(t *testing.T) {
			repo := t.TempDir()
			runID := "20260724-120000"
			root := filepath.Join(repo, ".plan-logs", "koryph")
			full := filepath.Join(root, runID+".tar.gz")
			evidence := filepath.Join(root, runID+durableEvidenceArchiveExt)
			writeGCFile(t, full, []byte("full archive"))
			switch tc {
			case "empty":
				writeGCFile(t, evidence, nil)
			case "corrupt":
				writeGCFile(t, evidence, []byte("not gzip"))
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				writeGCFile(t, target, []byte("not gzip"))
				if err := os.Symlink(target, evidence); err != nil {
					t.Fatal(err)
				}
			}
			candidates := collectReclaimCandidates(
				repo, time.Now().Add(48*time.Hour),
				ProjectBudgetPolicy{TranscriptRetainDays: 1}.effective(), true,
			)
			for _, candidate := range candidates {
				if candidate.path == full {
					t.Fatalf("%s compact evidence authorized full archive reclaim", tc)
				}
			}
		})
	}
}

func TestCompactEvidenceRequiresPerSlotManifestAndDeclaredDigest(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "merged")
	archive := runDir + durableEvidenceArchiveExt
	if err := compressEvidenceDir(runDir, archive); err != nil {
		t.Fatal(err)
	}
	if err := validCompactEvidenceArchive(archive, filepath.Base(runDir)); err == nil ||
		!strings.Contains(err.Error(), "manifest") {
		t.Fatalf("ledger-only compact archive validation = %v", err)
	}

	manifest := filepath.Join(runDir, "bead1", "manifest.json")
	writeGCFile(t, manifest, []byte(`{"schema_version":"koryph.manifest/v2"}`))
	gate := filepath.Join(runDir, ".engine-evidence", "bead1", "gate-evidence.json")
	writeGCFile(t, gate, []byte(`{"gate":"ship"}`))
	ledgerRaw := `{"run_id":"20260724-120000","slots":{"bead1":{` +
		`"phase_id":"bead1","status":"merged",` +
		`"gate_evidence_path":` + strconv.Quote(gate) + `,` +
		`"gate_evidence_digest":"sha256:` + strings.Repeat("0", 64) + `"}},"status":"done"}`
	writeGCFile(t, filepath.Join(runDir, "ledger.json"), []byte(ledgerRaw))
	if err := compressEvidenceDir(runDir, archive); err != nil {
		t.Fatal(err)
	}
	if err := validCompactEvidenceArchive(archive, filepath.Base(runDir)); err == nil ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered declared evidence validation = %v", err)
	}

	ledgerRaw = `{"run_id":"20260724-120000","slots":{"bead1":{` +
		`"phase_id":"bead1","status":"merged",` +
		`"gate_evidence_path":` + strconv.Quote(gate) + `}},"status":"done"}`
	writeGCFile(t, filepath.Join(runDir, "ledger.json"), []byte(ledgerRaw))
	if err := compressEvidenceDir(runDir, archive); err != nil {
		t.Fatal(err)
	}
	if err := validCompactEvidenceArchive(archive, filepath.Base(runDir)); err == nil ||
		!strings.Contains(err.Error(), "path/digest pair incomplete") {
		t.Fatalf("digest-less declared evidence validation = %v", err)
	}

	ledgerRaw = `{"run_id":"20260724-120000","slots":{"bead1":{` +
		`"phase_id":"bead1","status":"merged",` +
		`"gate_evidence_digest":"sha256:` + strings.Repeat("0", 64) + `"}},"status":"done"}`
	writeGCFile(t, filepath.Join(runDir, "ledger.json"), []byte(ledgerRaw))
	if err := compressEvidenceDir(runDir, archive); err != nil {
		t.Fatal(err)
	}
	if err := validCompactEvidenceArchive(archive, filepath.Base(runDir)); err == nil ||
		!strings.Contains(err.Error(), "path/digest pair incomplete") {
		t.Fatalf("orphan declared digest validation = %v", err)
	}
	_ = repo
}

func TestCompanionManifestWriteDoesNotFollowSymlink(t *testing.T) {
	repo, runDir := gcCacheFixture(t, "merged")
	writeGCFile(t, filepath.Join(runDir, "bead1", "manifest.json"), []byte(`{"ok":true}`))
	outside := filepath.Join(t.TempDir(), "sentinel")
	writeGCFile(t, outside, []byte("preserve"))
	manifestOut := filepath.Join(filepath.Dir(runDir), filepath.Base(runDir)+".manifest.json")
	if err := os.Symlink(outside, manifestOut); err != nil {
		t.Fatal(err)
	}
	if err := compressDir(runDir, runDir+".tar.gz", filepath.Dir(runDir)); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "preserve" {
		t.Fatalf("outside sentinel changed: %q, %v", raw, err)
	}
	info, err := os.Lstat(manifestOut)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("companion manifest was not an atomic regular replacement: %v, %v", info, err)
	}
	_ = repo
}

func TestCompactEvidenceRejectsBroadReviewNamedWorkerLogs(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"bead/review.json", true},
		{"bead/review-envelope.json", true},
		{".engine-evidence/bead/general-review-candidate.json", true},
		{".engine-evidence/bead/.runtime-scratch/session.log", false},
		{".evidence/bead/general-review-candidate.json", true},
		{"bead/spill-review-full-gate.log", false},
		{"bead/gate-output.log", false},
		{"bead/my-review-transcript.txt", false},
	} {
		if got := durableEvidencePath(tc.path); got != tc.want {
			t.Errorf("durableEvidencePath(%q) = %t, want %t", tc.path, got, tc.want)
		}
	}
}

func TestTranscriptArtifactPreservesReviewEnvelope(t *testing.T) {
	if transcriptArtifact("review-envelope.json") {
		t.Fatal("review-envelope.json classified as disposable transcript")
	}
}

func TestInterruptedArchiveNeverPublishesPartialFinalPath(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run")
	writeGCFile(t, filepath.Join(runDir, "ledger.json"), []byte(`{"status":"done"}`))
	archive := filepath.Join(filepath.Dir(runDir), "run.tar.gz")
	previous := archiveCopy
	archiveCopy = func(io.Writer, io.Reader) (int64, error) {
		return 0, errors.New("injected copy interruption")
	}
	t.Cleanup(func() { archiveCopy = previous })
	if err := compressDir(runDir, archive, filepath.Dir(runDir)); err == nil {
		t.Fatal("interrupted compression succeeded")
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("partial final archive published: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(runDir), ".run.tar.gz.tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("interrupted archive temps survived: %v", matches)
	}
}

func writeGCFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func tarNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var names []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	return names
}

func containsSuffix(values []string, suffix string) bool {
	for _, value := range values {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}
