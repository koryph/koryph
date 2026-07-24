// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/runtime"
	_ "github.com/koryph/koryph/internal/runtime/codex"
	"github.com/koryph/koryph/internal/sysdeps"
)

// Detect builds the read-only Snapshot for root (design §3.1). It never
// writes anywhere: every sub-probe is either a filesystem read, a read-only
// git/bd subcommand (via onboard.Inspect, which already carries that
// contract), or a PATH lookup. A missing root is the only hard error; every
// other sub-probe degrades gracefully (matching onboard.Inspect's own
// tolerance for a failed sub-probe).
func Detect(ctx context.Context, root string) (*Snapshot, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("adopt: resolve root %q: %w", root, err)
	}

	inv, err := onboard.Inspect(ctx, absRoot)
	if err != nil {
		return nil, err
	}

	snap := &Snapshot{
		Root:      absRoot,
		ProjectID: Slugify(filepath.Base(absRoot)),
		Inventory: inv,
	}

	snap.Platform = sysdeps.Detect()
	snap.FlakeNixPresent = fsx.Exists(filepath.Join(absRoot, "flake.nix"))
	snap.Tools = detectTools(ctx, snap.Platform)

	snap.AccountCandidates = account.Discover(ctx)
	if claude, ok := snap.Tools["claude"]; ok {
		claude.Authed = anyVerified(snap.AccountCandidates)
		snap.Tools["claude"] = claude
	}

	snap.ForgeProposal = onboard.InferForge(inv.Remote)
	snap.GateProposals = onboard.InferGate(absRoot)
	snap.AreaMap, snap.AreaMapProvenance = onboard.InferAreaMap(absRoot)

	// Registry lookup is READ-ONLY: registry.NewStore (unlike openStore) never
	// calls Init, so a fresh machine with no ~/.koryph yet still detects
	// cleanly (FindByPath tolerates a missing registry.d — see store.List).
	store := registry.NewStore()
	if rec, rerr := store.FindByPath(absRoot); rerr == nil {
		snap.ExistingRecord = rec
	}

	if inv.AdapterPresent {
		if cfg, cerr := project.Load(absRoot); cerr == nil {
			snap.ProjectConfig = cfg
			snap.RuntimeName = cfg.DefaultRuntime
		}
	}

	return snap, nil
}

// anyVerified reports whether any discovered account candidate verified.
func anyVerified(cands []account.Candidate) bool {
	for _, c := range cands {
		if c.Verified {
			return true
		}
	}
	return false
}

// detectTools probes git, every locally supported runtime, bd, and gh: presence, version, and — for
// whichever tool needs one — the sysdeps install plan. git carries no
// sysdeps route (sysdeps.Tool has no ToolGit; installing git itself varies
// too much by platform and predates every other prerequisite here), so its
// ToolStatus.Plan always stays nil.
func detectTools(ctx context.Context, p sysdeps.Platform) map[string]ToolStatus {
	out := make(map[string]ToolStatus, 5)

	out["git"] = probeSimpleTool(ctx, "git", []string{"--version"})

	claudeStatus := probeSimpleTool(ctx, "claude", []string{"--version"})
	if !claudeStatus.Found {
		plan := sysdeps.Plan(p, sysdeps.ToolClaude)
		claudeStatus.Plan = &plan
	}
	out["claude"] = claudeStatus

	codexStatus := probeSimpleTool(ctx, "codex", []string{"--version"})
	if !codexStatus.Found {
		plan := sysdeps.Plan(p, sysdeps.ToolCodex)
		codexStatus.Plan = &plan
	}
	if rt, ok := runtime.Default.Get("codex"); ok && codexStatus.Found {
		codexStatus.Authed = rt.AuthCheck(ctx, runtime.Profile{}) == nil
	}
	out["codex"] = codexStatus

	out["bd"] = detectBD(ctx, p)

	ghStatus := probeSimpleTool(ctx, "gh", []string{"--version"})
	if !ghStatus.Found {
		plan := sysdeps.Plan(p, sysdeps.ToolGH)
		ghStatus.Plan = &plan
	}
	out["gh"] = ghStatus

	return out
}

// probeSimpleTool resolves name on PATH and, when found, runs versionArgv to
// capture a one-line version string. A version-probe failure (or a tool that
// simply prints nothing recognizable) still counts the tool as Found —
// VersionOK stays true for every tool but bd, which alone has a real
// minimum-version contract (beads.MinVersion).
func probeSimpleTool(ctx context.Context, name string, versionArgv []string) ToolStatus {
	path, err := exec.LookPath(name)
	if err != nil {
		return ToolStatus{Name: name, Found: false}
	}
	status := ToolStatus{Name: name, Found: true, Path: path, VersionOK: true}
	res, rerr := execx.Run(ctx, execx.Cmd{Name: name, Args: versionArgv, Timeout: probeTimeout})
	if rerr == nil && res.ExitCode == 0 {
		status.Version = firstLine(res.Stdout)
	}
	return status
}

// detectBD resolves bd through beads.ProbeVersion (the same probe
// internal/beads' own preflight and doctor use) rather than re-deriving
// version-parsing logic here, so adopt and doctor can never disagree about
// whether the resolved bd is new enough.
func detectBD(ctx context.Context, p sysdeps.Platform) ToolStatus {
	info := beads.ProbeVersion(ctx)
	status := ToolStatus{
		Name:      "bd",
		Found:     info.Found,
		Path:      info.Path,
		Version:   info.Raw,
		VersionOK: info.OK,
	}
	switch {
	case !info.Found:
		plan := sysdeps.Plan(p, sysdeps.ToolBD)
		status.Plan = &plan
	case !info.OK:
		// Found but too old: the fix is an upgrade/pin-bump, not a fresh
		// install argv — beads.VersionInfo.Remediation() already tailors the
		// message to a nix-provided bd vs. a package-manager one (§4.1/§5).
		status.Remediation = info.Remediation()
	}
	return status
}

// probeTimeout bounds a --version probe so a wedged/misbehaving binary on
// PATH can never hang the detect phase.
const probeTimeout = 3 * time.Second

// firstLine returns the trimmed first line of s.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// Slugify lowercases s and collapses every run of non [a-z0-9] into a single
// dash, trimming leading/trailing dashes. Empty input yields "project".
// Mirrors internal/onboard's unexported register.go:slugify byte-for-byte —
// duplicated (not imported) because it is a tiny pure function and the
// wizard needs it BEFORE onboard.Register runs (to predict the project id
// for the plan header and the beads --prefix), while onboard.Register itself
// still derives the authoritative id the same way when ProjectID is passed
// through explicitly (see cmd/koryph/adopt.go).
func Slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "project"
	}
	return out
}
