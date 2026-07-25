// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gc

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/ledger"
)

const (
	defaultProjectSoftMB       = 2048
	defaultProjectHardMB       = 5120
	defaultTranscriptDays      = 14
	defaultFailureDays         = 30
	defaultLogTailKB           = 64
	planningSnapshotSchema     = "koryph.planning-snapshot/v1"
	durableEvidenceArchiveExt  = ".evidence.tar.gz"
	explicitArtifactRetainFile = ".retain"
)

func (p ProjectBudgetPolicy) effective() ProjectBudgetPolicy {
	if p.SoftMB <= 0 {
		p.SoftMB = defaultProjectSoftMB
	}
	if p.HardMB <= 0 {
		p.HardMB = defaultProjectHardMB
	}
	if p.HardMB < p.SoftMB {
		p.HardMB = p.SoftMB
	}
	if p.TranscriptRetainDays <= 0 {
		p.TranscriptRetainDays = defaultTranscriptDays
	}
	if p.FailureRetainDays <= 0 {
		p.FailureRetainDays = defaultFailureDays
	}
	if p.LogTailKB <= 0 {
		p.LogTailKB = defaultLogTailKB
	}
	return p
}

func successfulTerminalStatus(status string) bool {
	switch status {
	case ledger.SlotMerged, ledger.SlotPROpened, ledger.SlotDone:
		return true
	default:
		return false
	}
}

func transcriptArtifact(name string) bool {
	name = strings.ToLower(filepath.Base(name))
	switch name {
	case "stream.jsonl", "session.log":
		return true
	case "review-envelope.json":
		// The raw runtime envelope is authenticated cost/review evidence, not a
		// disposable worker transcript despite its suffix.
		return false
	}
	return strings.HasSuffix(name, "-envelope.json") ||
		strings.Contains(name, "transcript")
}

func runDirectoryName(name string) bool {
	const baseLength = len("20060102-150405")
	if len(name) < baseLength {
		return false
	}
	if _, err := time.Parse("20060102-150405", name[:baseLength]); err != nil {
		return false
	}
	if len(name) == baseLength {
		return true
	}
	suffix := name[baseLength:]
	if len(suffix) != len("-000000") || suffix[0] != '-' {
		return false
	}
	for _, char := range suffix[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func durableEvidencePath(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || strings.HasPrefix(rel, "../") {
		return false
	}
	if strings.Contains("/"+rel+"/", "/.runtime-scratch/") {
		return false
	}
	base := filepath.Base(rel)
	switch base {
	case "ledger.json", "manifest.json", "result.json", "SUMMARY.md",
		"review.json", "review-envelope.json", "review-degraded.json",
		"events.jsonl", "pressure-state.json",
		"supervisor.json", "alerts.jsonl":
		return true
	}
	if strings.HasSuffix(base, ".tail") ||
		strings.HasPrefix(base, "gate-evidence-") ||
		strings.HasPrefix(base, "general-review-") ||
		strings.HasPrefix(base, "security-review-") {
		return true
	}
	// Engine-private evidence is a sibling of worker-writable phase dirs.
	return strings.HasPrefix(rel, ".evidence/") ||
		strings.HasPrefix(rel, "evidence/") ||
		strings.HasPrefix(rel, ".engine-evidence/")
}

type planningSnapshotMarker struct {
	Schema         string `json:"schema"`
	EpicID         string `json:"epic_id"`
	DesignPath     string `json:"design_path"`
	DesignCommit   string `json:"design_commit"`
	SnapshotDigest string `json:"snapshot_digest"`
	GraphDigest    string `json:"graph_digest"`
	PostFilePath   string `json:"post_file_path"`
}

type reclaimCandidate struct {
	path       string
	size       int64
	modTime    time.Time
	kind       string
	companions []string
}
