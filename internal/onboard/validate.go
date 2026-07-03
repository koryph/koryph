// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package onboard

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/sched"
)

// Check levels.
const (
	LevelOK    = "ok"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Validate is the pre-dispatch gate. It loads the record + adapter config,
// confirms the root is a git repo, verifies the logged-in account identity
// (fail closed), confirms bd is available and its ready frontier parses, runs a
// scheduler dry-run, checks hooks wiring (warn-only), snapshots the governor
// calibration (warn when uncalibrated), and proves the worktree root is
// writable. Each check is streamed to out (when non-nil). OK is true iff no
// check is at error level. On green, the CALLER promotes registered→migrated.
func Validate(ctx context.Context, store *registry.Store, projectID string, out io.Writer) (*Validation, error) {
	rec, err := store.Get(projectID)
	if err != nil {
		return nil, err
	}

	v := &Validation{ProjectID: projectID}
	add := func(name, level, detail string) {
		v.Checks = append(v.Checks, Check{Name: name, Level: level, Detail: detail})
		if out != nil {
			fmt.Fprintf(out, "%-5s %s%s\n", level, name, detailSuffix(detail))
		}
	}

	add("registry record", LevelOK, "loaded "+rec.ProjectID)

	// Adapter config.
	cfg, cfgErr := project.Load(rec.Root)
	if cfgErr != nil {
		add("adapter config", LevelError, cfgErr.Error())
	} else {
		add("adapter config", LevelOK, project.ConfigFileName)
	}

	// Root is a git repo on disk.
	if fsx.Exists(filepath.Join(rec.Root, ".git")) {
		add("git repo", LevelOK, rec.Root)
	} else {
		add("git repo", LevelError, "no .git under "+rec.Root)
	}

	// Account identity (fail closed).
	prof := account.Profile{Name: rec.AccountProfile, ConfigDir: rec.ClaudeConfigDir}
	if id, verr := account.VerifyExpected(ctx, prof, rec.ExpectedIdentity); verr != nil {
		add("account identity", LevelError, verr.Error())
	} else {
		add("account identity", LevelOK, "verified "+id.Email)
	}

	// bd availability + ready parse (bd work source only).
	source := "bd"
	if cfg != nil && cfg.WorkSource != "" {
		source = cfg.WorkSource
	}
	var issues []beads.Issue
	bd := newBD(rec.Root)
	if source == "bd" {
		if !bd.Available() {
			add("bd available", LevelError, "bd binary not found ("+bdBin()+")")
		} else {
			add("bd available", LevelOK, bdBin())
			if iss, rerr := bd.Ready(ctx, beads.ReadyOpts{Parent: ""}); rerr != nil {
				add("bd ready parses", LevelError, rerr.Error())
			} else {
				issues = iss
				add("bd ready parses", LevelOK, fmt.Sprintf("%d ready", len(iss)))
			}
		}
	} else {
		add("bd ready parses", LevelWarn, "work_source="+source+" — bd checks skipped")
	}

	// Scheduler dry-run: BuildWave with Max 1 must not error; empty is OK.
	if cfg != nil {
		wave, werr := sched.BuildWave(ctx, issues, cfg, sched.Opts{Max: 1}, nil)
		switch {
		case werr != nil:
			add("scheduler dry-run", LevelError, werr.Error())
		case len(wave.Items) == 0:
			add("scheduler dry-run", LevelOK, "frontier empty")
		default:
			add("scheduler dry-run", LevelOK, fmt.Sprintf("%d item(s) would dispatch", len(wave.Items)))
		}
	}

	// Hooks wiring (warn-only).
	if fileContains(filepath.Join(rec.Root, ".claude", "settings.json"), "bd prime") {
		add("hooks wiring", LevelOK, "'bd prime' present in .claude/settings.json")
	} else {
		add("hooks wiring", LevelWarn, ".claude/settings.json missing 'bd prime' hook")
	}

	// Governor calibration (warn when uncalibrated).
	qprofile := rec.QuotaProfile
	if qprofile == "" {
		qprofile = rec.AccountProfile
	}
	if qcfg, qerr := quota.LoadConfig(qprofile); qerr != nil {
		add("governor", LevelWarn, "load config: "+qerr.Error())
	} else if qcfg.WindowCeilingUSD <= 0 && qcfg.WeeklyCeilingUSD <= 0 {
		add("governor", LevelWarn, "uncalibrated ceilings (run `koryph quota calibrate`)")
	} else {
		add("governor", LevelOK, "calibrated")
	}

	// Worktree root writable.
	wroot := rec.WorktreeRoot
	if wroot == "" {
		wroot = filepath.Join(filepath.Dir(rec.Root), filepath.Base(rec.Root)+"-worktrees")
	}
	if perr := probeWritable(wroot); perr != nil {
		add("worktree root writable", LevelError, perr.Error())
	} else {
		add("worktree root writable", LevelOK, wroot)
	}

	v.OK = true
	for _, c := range v.Checks {
		if c.Level == LevelError {
			v.OK = false
			break
		}
	}
	return v, nil
}

// probeWritable creates and removes a probe directory under dir, proving it is
// writable. The parent (worktree root) is created if missing.
func probeWritable(dir string) error {
	probe := filepath.Join(dir, fmt.Sprintf(".koryph-probe-%d", os.Getpid()))
	if err := os.MkdirAll(probe, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}
	return os.Remove(probe)
}

// detailSuffix renders a check detail as a trailing clause, or "" when empty.
func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " — " + detail
}
