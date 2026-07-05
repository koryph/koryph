// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"fmt"

	"github.com/koryph/koryph/internal/gc"
	"github.com/koryph/koryph/internal/paths"
)

const checkNameGC = "gc-footprint"

// checkGCFootprint reports per-class footprint and active policy summary.
// It performs a dry-run GC scan to compute reclaimable bytes and warns when
// the pending-gc footprint exceeds the configured threshold.
func checkGCFootprint(opts Options) []Finding {
	cfg, err := gc.LoadConfig("")
	if err != nil {
		return []Finding{{Check: checkNameGC, Level: LevelWarn,
			Message: fmt.Sprintf("gc-footprint: cannot load retention config: %v", err)}}
	}

	// Dry-run on global artifacts only (no repo root).
	gcOpts := gc.Options{
		RepoRoot: "",
		DryRun:   true,
		Config:   &cfg,
	}
	res, err := gc.Run(gcOpts)
	if err != nil {
		return []Finding{{Check: checkNameGC, Level: LevelWarn,
			Message: fmt.Sprintf("gc-footprint: scan failed: %v", err)}}
	}

	totalMB := res.TotalReclaimedMB()
	totalGB := totalMB / 1024

	warnThreshGB := cfg.FootprintWarnGB

	var findings []Finding

	// Per-class summary.
	for _, c := range res.Classes {
		if c.ScannedMB > 0 || c.ReclaimedMB > 0 {
			findings = append(findings, Finding{
				Check: checkNameGC,
				Level: LevelOK,
				Message: fmt.Sprintf("gc-footprint [%s]: scanned=%.1f MB reclaimable=%.1f MB",
					c.Class, c.ScannedMB, c.ReclaimedMB),
			})
		}
	}

	// Policy summary.
	rdPol := cfg.RunDirs
	compressStr := fmt.Sprintf("%dd", rdPol.CompressAfterDays)
	if rdPol.CompressAfterDaysNever {
		compressStr = "never"
	}
	deleteStr := fmt.Sprintf("%dd", rdPol.DeleteAfterDays)
	if rdPol.DeleteAfterDaysNever {
		deleteStr = "never"
	}
	autoStr := "disabled"
	if cfg.GCAuto {
		autoStr = "enabled"
	}
	findings = append(findings, Finding{
		Check: checkNameGC,
		Level: LevelOK,
		Message: fmt.Sprintf(
			"gc policy: run-dirs compress=%s delete=%s | audit rotate=%dMB | runs rotate=%dMB | auto-gc=%s",
			compressStr, deleteStr,
			cfg.AuditLog.RotateSizeMB, cfg.RunsIndex.RotateSizeMB, autoStr),
	})

	// Telemetry dir note.
	telDir := paths.TelemetryDir()
	findings = append(findings, Finding{
		Check:   checkNameGC,
		Level:   LevelOK,
		Message: fmt.Sprintf("gc-footprint [telemetry]: managed by obs (dir: %s)", telDir),
	})

	// Warn if pending-gc exceeds threshold.
	if totalGB >= warnThreshGB {
		findings = append(findings, Finding{
			Check: checkNameGC,
			Level: LevelWarn,
			Message: fmt.Sprintf(
				"gc-footprint: %.2f GB reclaimable exceeds threshold (%.1f GB) — run `koryph gc [--project ID]` to reclaim space",
				totalGB, warnThreshGB),
		})
	} else {
		findings = append(findings, Finding{
			Check:   checkNameGC,
			Level:   LevelOK,
			Message: fmt.Sprintf("gc-footprint: total reclaimable %.1f MB (threshold %.1f GB)", totalMB, warnThreshGB),
		})
	}

	return findings
}
