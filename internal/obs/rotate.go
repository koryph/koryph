// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PruneOld removes JSONL files in dir whose modification time is older than
// maxAge.  It returns the number of files removed.  Errors opening or removing
// individual files are accumulated and returned as a combined error after all
// files have been considered.
func PruneOld(dir string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // no dir yet — nothing to prune
		}
		return 0, fmt.Errorf("obs prune: read dir %s: %w", dir, err)
	}

	cutoff := time.Now().Add(-maxAge)
	var pruned int
	var errs []string

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fi, serr := os.Stat(path)
		if serr != nil {
			errs = append(errs, serr.Error())
			continue
		}
		if fi.ModTime().Before(cutoff) {
			if rerr := os.Remove(path); rerr != nil {
				errs = append(errs, rerr.Error())
			} else {
				pruned++
			}
		}
	}

	if len(errs) > 0 {
		return pruned, fmt.Errorf("obs prune: %s", strings.Join(errs, "; "))
	}
	return pruned, nil
}

// PruneFromConfig reads the telemetry retention window from cfg and removes
// stale JSONL files from the canonical telemetry directory.  It returns the
// number of files removed.  A missing directory is not an error.
func PruneFromConfig(cfg Config) (int, error) {
	days := cfg.TelemetryRetentionDays
	if days <= 0 {
		days = 30
	}
	dir := telemetryDirPath()
	return PruneOld(dir, time.Duration(days)*24*time.Hour)
}
