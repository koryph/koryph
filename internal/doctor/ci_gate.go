// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

// ci_gate.go — checkCIGatePipeline: project-mode doctor check that reports
// drift between the installed gate CI pipeline and what `koryph ci setup`
// would render (via the forge CIService).
//
// The check:
//   - Returns LevelOK ("skipped — no forge remote") when the project has no
//     recognised forge git remote. Forge detection uses the injectable
//     CIService field on ProjectOptions (test seam — skips remote detection
//     entirely when set) or falls back to running `git remote get-url origin`
//     and calling forge.SniffRemote to detect GitHub or GitLab remotes.
//   - Returns LevelOK ("skipped — …") when the forge CIService cannot render
//     "gate" (e.g. the render kind is not yet implemented — ErrUnsupported).
//     This is the correct degraded path while koryph-lqz.1 (CIService gate
//     kind) has not yet landed.
//   - Returns LevelWarn when the gate pipeline file is absent or its content
//     differs from the current render output. Both states include the exact
//     `koryph ci setup` remediation command.
//   - Returns LevelOK when the installed file matches the current render.
//
// Gate pipeline paths are resolved via ciinstall.KindPath so GitHub and GitLab
// use their forge-native locations:
//
//	GitHub → .github/workflows/koryph-gate.yml
//	GitLab → .koryph/ci/koryph-gate.yml

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/koryph/koryph/internal/ciinstall"
	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/project"
)

const checkNameCIAssets = "ci-assets"

// checkCIGatePipeline is the project-mode doctor check for CI gate pipeline
// drift. It is called from RunProject after the core structural checks.
//
// Forge detection uses opts.CIService when set (test isolation); otherwise the
// real detection path runs via opts.gitHubRepo.
func checkCIGatePipeline(opts ProjectOptions, repoRoot string, cfg *project.Config) Finding {
	// Resolve the CI service and gate pipeline path.
	ciSvc, relPath, skipMsg := resolveCIForGate(opts, repoRoot, cfg)
	if skipMsg != "" {
		return Finding{
			Check:   checkNameCIAssets,
			Level:   LevelOK,
			Message: skipMsg,
		}
	}

	// Render the gate pipeline via the CI service.
	content, err := ciSvc.Render("gate")
	if err != nil {
		// ErrUnsupported or any other render error: skip gracefully.
		// This is the expected path while koryph-lqz.1 (adding CIService.Render("gate")
		// to the real forge providers) has not yet landed.
		return Finding{
			Check:   checkNameCIAssets,
			Level:   LevelOK,
			Message: fmt.Sprintf("ci-assets: gate pipeline render skipped: %v", err),
		}
	}

	// Compare installed file against rendered content.
	absPath := filepath.Join(repoRoot, relPath)
	onDisk, readErr := os.ReadFile(absPath)
	if errors.Is(readErr, os.ErrNotExist) {
		return Finding{
			Check:   checkNameCIAssets,
			Level:   LevelWarn,
			Message: fmt.Sprintf("ci-assets: gate pipeline absent at %s (run `koryph ci setup`)", relPath),
		}
	}
	if readErr != nil {
		return Finding{
			Check:   checkNameCIAssets,
			Level:   LevelWarn,
			Message: fmt.Sprintf("ci-assets: read gate pipeline %s: %v", relPath, readErr),
		}
	}

	wantHash := sha256.Sum256(content)
	diskHash := sha256.Sum256(onDisk)
	if wantHash == diskHash {
		return Finding{
			Check:   checkNameCIAssets,
			Level:   LevelOK,
			Message: fmt.Sprintf("ci-assets: gate pipeline present and current (%s)", relPath),
		}
	}
	return Finding{
		Check:   checkNameCIAssets,
		Level:   LevelWarn,
		Message: fmt.Sprintf("ci-assets: gate pipeline drifted from current template at %s (run `koryph ci setup`)", relPath),
	}
}

// resolveCIForGate returns the forge CIService, the repository-relative gate
// pipeline path, and a skip-message. When skipMsg is non-empty, the caller
// should return an LevelOK finding with that message immediately.
//
// Gate pipeline paths are resolved via ciinstall.KindPath so both GitHub and
// GitLab are handled without hardcoding forge-specific paths here.
func resolveCIForGate(opts ProjectOptions, repoRoot string, cfg *project.Config) (ciSvc forge.CIService, relPath string, skipMsg string) {
	// Test seam: when CIService is injected, skip forge detection entirely and
	// derive the path from the config via ciinstall.KindPath.
	if opts.CIService != nil {
		forgeName := ""
		if cfg != nil {
			forgeName = cfg.ResolvedForge()
		}
		kindPath, ok := ciinstall.KindPath(forgeName, "gate")
		if !ok {
			return nil, "", fmt.Sprintf("ci-assets: no gate pipeline path defined for forge %q (skipped)", forgeName)
		}
		return opts.CIService, kindPath, ""
	}

	// Real path: detect forge from git remote URL (supports GitHub and GitLab).
	forgeName, gitErr := opts.gitForgeRemote(repoRoot)
	if gitErr != nil || forgeName == "" {
		return nil, "", "ci-assets: no forge remote detected (skipped)"
	}

	if cfg == nil {
		return nil, "", "ci-assets: no project config (skipped)"
	}

	// Prefer the project config's forge when explicitly set; the remote sniff
	// confirms a forge is present but the config is the authoritative source.
	if cfgForge := cfg.ResolvedForge(); cfgForge != "" {
		forgeName = cfgForge
	}

	kindPath, ok := ciinstall.KindPath(forgeName, "gate")
	if !ok {
		return nil, "", fmt.Sprintf("ci-assets: no gate pipeline path defined for forge %q (skipped)", forgeName)
	}

	f, fOK := forge.Default.Get(forgeName)
	if !fOK {
		return nil, "", fmt.Sprintf("ci-assets: forge %q not registered (skipped)", forgeName)
	}

	return f.CI(), kindPath, ""
}
