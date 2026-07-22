// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// This file holds the mutating EXECUTE-phase helpers (design §3.4). Each is
// idempotent and safe to re-run; cmd/koryph/adopt.go sequences them and
// streams the "ok/skip/fail <step> — detail" lines the design specifies —
// that streaming/sequencing is CLI glue, kept out of this package so every
// mutation here stays unit-testable without a fake stdout.

// --- beads (design §3.4 step 3) ---------------------------------------------

// BeadsOpts parameterizes the beads execute step.
type BeadsOpts struct {
	Prefix         string // bd --prefix; the project id
	RemoteURL      string // the inventory's detected git origin URL
	RemoteOverride string // --remote flag; wins over the derived origin when set
	NoRemote       bool   // --no-remote flag; forces a local-only init
}

// ResolveBeadsRemote computes the sync.remote EnsureDB should use: --no-remote
// forces "" (local-only); an explicit --remote wins next; otherwise the
// origin URL is derived via beads.DeriveSyncRemote (empty when there is no
// origin, or it doesn't parse as a supported form — EnsureDB then does a
// local-only init exactly as it would for a bare `bd init`).
func ResolveBeadsRemote(o BeadsOpts) string {
	if o.NoRemote {
		return ""
	}
	if o.RemoteOverride != "" {
		return o.RemoteOverride
	}
	return beads.DeriveSyncRemote(o.RemoteURL)
}

// ExecuteBeads ensures the beads DB exists (init if absent, else
// snapshot+`bd doctor` on an existing one) and hardens it (gitignore,
// reporting the sync-remote/hooks findings bd itself owns). Runs BEFORE the
// assets step (design §3.4: "so bd's settings/AGENTS.md integration lands
// first and koryph's settings merge migrates bd's prime hook in place").
func ExecuteBeads(ctx context.Context, root string, opts BeadsOpts) (beads.EnsureResult, []beads.HardenAction, error) {
	a := beads.New(root)
	res, err := a.EnsureDB(ctx, beads.EnsureOpts{Prefix: opts.Prefix, Remote: ResolveBeadsRemote(opts)})
	if err != nil {
		return res, nil, err
	}
	actions, herr := a.Harden(ctx)
	return res, actions, herr
}

// --- register + config (design §3.4 step 4) ---------------------------------

// RegisterAndConfigure registers the project (a no-op when snap.ExistingRecord
// is already set) and writes the confirmed gate/forge/area_map into
// koryph.project.json — but ONLY when the adapter config did not already
// exist at detect time (snap.Inventory.AdapterPresent). An existing config's
// gate/area_map/forge are NEVER overwritten here: the caller's plan already
// marked that step "done (existing config kept)" and confirm never asked
// about it, so silently overwriting it here would betray that contract.
// force carries the --force flag through to onboard.Register's .envrc
// account-disagreement refusal, exactly as `project add --force` does.
func RegisterAndConfigure(ctx context.Context, store *registry.Store, snap *Snapshot, acct AccountChoice, gate []string, forgeName string, areaMap map[string][]string, force bool) (*registry.Record, *project.Config, error) {
	rec := snap.ExistingRecord
	if rec == nil {
		var err error
		rec, err = onboard.Register(ctx, store, snap.Inventory, onboard.RegisterOpts{
			ProjectID:        snap.ProjectID,
			AccountProfile:   acct.Profile,
			ClaudeConfigDir:  acct.ConfigDir,
			ExpectedIdentity: acct.Identity,
			AuthMode:         acct.AuthMode,
			Credential:       acct.Credential,
			Force:            force,
		})
		if err != nil {
			return nil, nil, err
		}
	}

	// Scaffold the adapter config if it is still missing — covers both the
	// just-registered path (onboard.Register already does this, but doing it
	// again here is a harmless no-op guarded by fsx.Exists) and a pre-existing
	// record whose config file was deleted out from under it.
	cfgPath := filepath.Join(rec.Root, project.ConfigFileName)
	if !fsx.Exists(cfgPath) {
		if err := project.Default(rec.ProjectID).Save(rec.Root); err != nil {
			return rec, nil, fmt.Errorf("adopt: scaffold %s: %w", project.ConfigFileName, err)
		}
	}

	cfg, err := project.Load(rec.Root)
	if err != nil {
		return rec, nil, err
	}

	if !snap.Inventory.AdapterPresent {
		changed := false
		if len(gate) > 0 {
			cfg.Gate = gate
			changed = true
		}
		if forgeName != "" {
			cfg.Forge = forgeName
			changed = true
		}
		if len(areaMap) > 0 && len(cfg.AreaMap) == 0 {
			cfg.AreaMap = areaMap
			changed = true
		}
		if changed {
			if err := cfg.Save(rec.Root); err != nil {
				return rec, cfg, err
			}
		}
	}
	return rec, cfg, nil
}

// --- commit (design §3.4 step 7) --------------------------------------------

// AdoptionCommitPaths are the pathspecs the wizard's commit step considers —
// every path an adopt run might have written. `git status --porcelain` scopes
// to this list; a lenient pathspec is safe there (unmatched entries simply
// contribute no output), unlike `git add`, which is scoped instead to
// whatever DirtyAdoptionPaths actually reports (see CommitAdoption) so it
// never fails on a pathspec that doesn't exist in this repo (e.g. flake.nix
// on a non-nix project).
var AdoptionCommitPaths = []string{
	"AGENTS.md", ".claude", "koryph.project.json", ".beads", "CLAUDE.md",
	"flake.nix", "flake.lock", "hooks",
}

// DirtyAdoptionPaths returns the porcelain-reported paths (new, modified, or
// already staged) among AdoptionCommitPaths, for the commit step's file list
// and the declined-commit summary.
func DirtyAdoptionPaths(ctx context.Context, root string) ([]string, error) {
	args := append([]string{"status", "--porcelain", "--"}, AdoptionCommitPaths...)
	res, err := execx.Run(ctx, execx.Cmd{Dir: root, Name: "git", Args: args})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("git status: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var paths []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		// Renames report "orig -> new"; the new path is what git add wants.
		if i := strings.Index(p, " -> "); i >= 0 {
			p = p[i+4:]
		}
		p = strings.Trim(p, `"`)
		paths = append(paths, p)
	}
	return paths, nil
}

// CommitAdoption stages exactly the dirty adoption paths and creates one
// commit (message defaults to "chore: adopt koryph"). committed is false
// (with a nil error) when there was nothing to commit. The repo's own git
// config governs signing/DCO — this never passes --no-verify, unlike the
// registry Store's own machine-local commits (koryph's own onboarding must
// pass the SAME hooks a human commit would).
func CommitAdoption(ctx context.Context, root, message string) (committed bool, dirty []string, err error) {
	dirty, err = DirtyAdoptionPaths(ctx, root)
	if err != nil {
		return false, nil, err
	}
	if len(dirty) == 0 {
		return false, nil, nil
	}
	addArgs := append([]string{"add", "--"}, dirty...)
	if _, aerr := execx.MustSucceed(ctx, execx.Cmd{Dir: root, Name: "git", Args: addArgs}); aerr != nil {
		return false, dirty, aerr
	}
	if message == "" {
		message = "chore: adopt koryph"
	}
	if _, cerr := execx.MustSucceed(ctx, execx.Cmd{Dir: root, Name: "git", Args: []string{"commit", "-m", message}}); cerr != nil {
		return false, dirty, cerr
	}
	return true, dirty, nil
}
