// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/agents"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/bot"
	"github.com/koryph/koryph/internal/commands"
	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/posture"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/signing"
	"github.com/koryph/koryph/internal/worktree"
)

// worktreeEntry is a minimal git worktree descriptor used by the orphan check.
type worktreeEntry struct {
	Path   string
	Branch string
}

// ProjectOptions configures a project-mode doctor run.
type ProjectOptions struct {
	// RepoRoot is the project repository root. When set, registry look-up is
	// skipped. Either RepoRoot or ProjectID (with Home) must be set.
	RepoRoot string
	// ProjectID looks up the project root from the registry under Home.
	// Ignored when RepoRoot is set.
	ProjectID string
	// WorktreeRoot overrides the default worktree root directory
	// (<parent>/<repo>-worktrees) used by the orphan check.
	WorktreeRoot string
	// Home overrides paths.KoryphHome() (tests use t.TempDir()).
	Home string
	// Fix enables auto-remediation: missing assets are installed; stale
	// assets are reinstalled when Force is also set.
	Fix bool
	// Force, when combined with Fix, causes stale (content-differing) asset
	// files to be overwritten. Without Force, only missing files are written
	// and differing files are left untouched.
	Force bool
	// Now supplies the current time (injectable for tests).
	Now func() time.Time
	// Alive reports whether a pid is a live process (injectable for tests).
	Alive func(pid int) bool
	// LookPath locates a binary on PATH (injectable for tests).
	LookPath func(name string) (string, error)
	// StallThreshold is how long a running slot may be silent before it is
	// flagged as stalled (default 30 min).
	StallThreshold time.Duration
	// ListWorktrees lists git worktrees by path and branch for the given repo
	// root (injectable for tests; default delegates to worktree.List).
	ListWorktrees func(root string) ([]worktreeEntry, error)
	// CommandsFS overrides the embedded commands FS (injectable for tests).
	CommandsFS fs.FS
	// AgentsFS overrides the embedded agents FS (injectable for tests).
	AgentsFS fs.FS

	// GitHubRepo derives the "owner/repo" slug for the project (injectable for
	// tests). nil means: run `git remote get-url origin` and parse the URL.
	GitHubRepo func(repoRoot string) (string, error)
	// GHSecretList lists secret names for the given owner/repo via `gh secret
	// list`. Return (nil, err) on failure; the release-bot-secrets check degrades
	// gracefully on error. nil means: invoke the real `gh` CLI.
	GHSecretList func(ownerRepo string) ([]string, error)
	// GHActionsPermissions returns can_approve_pull_request_reviews for the
	// given owner/repo. Return (false, err) on failure; the actions-approval
	// check degrades gracefully on error. nil means: invoke the real `gh` CLI.
	GHActionsPermissions func(ownerRepo string) (bool, error)
	// BotCredentialCheck returns offline PEM-validity findings for all stored
	// bots. nil means: call bot.CheckCredentials() against the real filesystem.
	// Inject a fake in tests to avoid touching ~/.koryph/bots/.
	BotCredentialCheck func() ([]bot.CredentialFinding, error)

	// PostureDriftCheck returns whether the live GitHub repo has posture drift
	// from the given profile. Return (false, nil) when no drift, (true, nil) on
	// drift, and (_, err) on failure (doctor degrades gracefully on error).
	// nil means: call the real posture check functions via the gh CLI.
	// Inject a fake in tests to avoid gh network calls.
	PostureDriftCheck func(repoRoot string, cfg *project.PostureConfig) (bool, error)

	// OrgPostureDriftCheck returns whether the live GitHub org has posture
	// drift from the given profile.  Return (false, nil) when no drift,
	// (true, nil) on drift, and (_, err) on failure (doctor degrades
	// gracefully on error).  nil means: call posture.CheckOrgRulesets via
	// the gh CLI.  Inject a fake in tests to avoid gh network calls.
	OrgPostureDriftCheck func(repoRoot string, cfg *project.PostureConfig) (bool, error)

	// FragmentDriftCheck returns the fragment drift for the given fragments.
	// Return (nil, nil) when no drift, (drifts, nil) on drift, and (_, err) on
	// failure (doctor degrades gracefully on error).
	// nil means: call posture.CheckFragmentDrift against the real filesystem.
	// Inject a fake in tests to avoid touching the working tree.
	FragmentDriftCheck func(repoRoot string, fragments []string) ([]posture.FragmentDrift, error)

	// ListEpics returns all open epics for the project. Return (nil, nil) when
	// bd is unavailable; (nil, err) on failure — the check degrades gracefully
	// on error. nil means: call beads.Adapter.List and filter for type "epic".
	// Inject a fake in tests to avoid spawning bd.
	ListEpics func(repoRoot string) ([]beads.Issue, error)

	// CIService overrides the forge CIService used for gate pipeline rendering
	// (test seam). When set, forge remote detection is skipped entirely; the
	// injected service is used to call Render("gate") directly. Inject a fake
	// CIService in tests to avoid running git remote commands and to exercise
	// the present/drifted/current paths with a service that actually supports
	// Render("gate"). nil means: detect the forge via git remote and use the
	// real forge CIService.
	CIService forge.CIService
}

func (o *ProjectOptions) home() string {
	if o.Home != "" {
		return o.Home
	}
	return paths.KoryphHome()
}

func (o *ProjectOptions) commandsFS() fs.FS {
	if o.CommandsFS != nil {
		return o.CommandsFS
	}
	return commands.FS
}

func (o *ProjectOptions) agentsFS() fs.FS {
	if o.AgentsFS != nil {
		return o.AgentsFS
	}
	return agents.FS
}

func (o *ProjectOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *ProjectOptions) stallThreshold() time.Duration {
	if o.StallThreshold > 0 {
		return o.StallThreshold
	}
	return 30 * time.Minute
}

func (o *ProjectOptions) listWorktrees(root string) ([]worktreeEntry, error) {
	if o.ListWorktrees != nil {
		return o.ListWorktrees(root)
	}
	return defaultListWorktrees(root)
}

func (o *ProjectOptions) gitHubRepo(repoRoot string) (string, error) {
	if o.GitHubRepo != nil {
		return o.GitHubRepo(repoRoot)
	}
	return defaultGitHubRepo(repoRoot)
}

func (o *ProjectOptions) ghSecretList(ownerRepo string) ([]string, error) {
	if o.GHSecretList != nil {
		return o.GHSecretList(ownerRepo)
	}
	return defaultGHSecretList(ownerRepo)
}

func (o *ProjectOptions) ghActionsPermissions(ownerRepo string) (bool, error) {
	if o.GHActionsPermissions != nil {
		return o.GHActionsPermissions(ownerRepo)
	}
	return defaultGHActionsPermissions(ownerRepo)
}

// resolveRoot returns the project's repository root, either from RepoRoot or
// by looking up ProjectID in the registry at Home.
func (o *ProjectOptions) resolveRoot() (string, string, error) {
	if o.RepoRoot != "" {
		id := o.ProjectID
		if id == "" {
			// Try to derive from config.
			if cfg, err := project.Load(o.RepoRoot); err == nil {
				id = cfg.ProjectID
			}
		}
		return o.RepoRoot, id, nil
	}
	if o.ProjectID == "" {
		return "", "", fmt.Errorf("doctor: either RepoRoot or ProjectID must be set")
	}
	store := registry.NewStoreAt(o.home())
	rec, err := store.Get(o.ProjectID)
	if err != nil {
		return "", "", fmt.Errorf("doctor: load project %q from registry: %w", o.ProjectID, err)
	}
	return rec.Root, rec.ProjectID, nil
}

// --- check name constants for project mode ----------------------------------

const (
	checkNameProjectConfig   = "project-config"
	checkNameGitRepo         = "git-repo"
	checkNameHooksWiring     = "hooks-wiring"
	checkNameSigning         = "signing"
	checkNameProtectedPaths  = "protected-paths"
	checkNameStalledRuns     = "stalled-runs"
	checkNameOrphanWorktrees = "orphan-worktrees"
	checkNameAssetDrift      = "asset-drift"
)

// RunProject executes project-scoped health checks and returns the report.
// It reuses onboard.Validate structural checks (config, git repo, hooks) and
// adds stalled-run, orphan-worktree, signing, and protected-path checks.
func RunProject(opts ProjectOptions) (*Report, error) {
	repoRoot, projectID, err := opts.resolveRoot()
	if err != nil {
		return nil, err
	}

	r := &Report{
		At:      opts.now().UTC().Format(time.RFC3339),
		Home:    repoRoot,
		Project: projectID,
	}

	// Load config once; subsequent checks reference it.
	cfg, cfgFinding := checkProjectConfig(repoRoot)
	r.add(cfgFinding)
	r.add(checkGitRepo(repoRoot))
	r.addAll(checkHooksWiring(repoRoot))
	if cfg != nil {
		r.addAll(checkSigning(cfg))
		r.add(checkProtectedPaths(cfg))
	}
	r.addAll(checkStalledRuns(opts, repoRoot))
	r.addAll(checkOrphanWorktrees(opts, repoRoot, cfg))
	r.addAll(checkAssetDrift(opts, repoRoot))
	r.addAll(checkReleaseInfra(opts, repoRoot, cfg))
	r.add(checkPostureDrift(opts, repoRoot, cfg))
	r.add(checkOrgPostureDrift(opts, repoRoot, cfg))
	r.addAll(checkFragmentDrift(opts, repoRoot, cfg))
	r.add(checkForge(cfg))
	r.addAll(checkEpicValidations(opts, repoRoot))
	r.add(checkCIGatePipeline(opts, repoRoot, cfg))

	for _, f := range r.Findings {
		if f.Fixed {
			r.FixedCount++
		}
	}
	return r, nil
}

// --- check functions --------------------------------------------------------

// checkProjectConfig loads and validates koryph.project.json.
func checkProjectConfig(repoRoot string) (*project.Config, Finding) {
	cfg, err := project.Load(repoRoot)
	if err != nil {
		return nil, Finding{
			Check:   checkNameProjectConfig,
			Level:   LevelError,
			Message: err.Error(),
		}
	}
	return cfg, Finding{
		Check:   checkNameProjectConfig,
		Level:   LevelOK,
		Message: "project_id=" + cfg.ProjectID + " work_source=" + cfg.WorkSource,
	}
}

// checkGitRepo verifies that .git exists at the repo root.
func checkGitRepo(repoRoot string) Finding {
	if _, err := os.Stat(filepath.Join(repoRoot, ".git")); err == nil {
		return Finding{Check: checkNameGitRepo, Level: LevelOK, Message: ".git present at " + repoRoot}
	}
	return Finding{
		Check:   checkNameGitRepo,
		Level:   LevelError,
		Message: "no .git at " + repoRoot + " (not a git repository)",
	}
}

// checkHooksWiring checks that each koryph hook marker is present in
// .claude/settings.json. Missing markers are warnings (run `koryph rules install`).
func checkHooksWiring(repoRoot string) []Finding {
	settingsPath := filepath.Join(repoRoot, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Finding{{
				Check:   checkNameHooksWiring,
				Level:   LevelWarn,
				Message: ".claude/settings.json absent (run `koryph rules install`)",
			}}
		}
		return []Finding{{
			Check:   checkNameHooksWiring,
			Level:   LevelError,
			Message: fmt.Sprintf("read .claude/settings.json: %v", err),
		}}
	}
	content := string(data)
	markers := []struct{ label, marker string }{
		{"bd-prime", "bd prime"},
		{"boundary-guard", "agent-boundary-guard.sh"},
		{"worktree-guard", "worktree-guard.sh"},
	}
	var findings []Finding
	for _, m := range markers {
		if strings.Contains(content, m.marker) {
			findings = append(findings, Finding{
				Check:   checkNameHooksWiring,
				Level:   LevelOK,
				Message: m.label + ": present",
			})
		} else {
			findings = append(findings, Finding{
				Check:   checkNameHooksWiring,
				Level:   LevelWarn,
				Message: m.label + ": missing from .claude/settings.json (run `koryph rules install`)",
			})
		}
	}
	return findings
}

// checkSigning validates the project's signing configuration sanity and
// classifies the key posture using the posture ladder:
//
//	vault provider  → OK
//	keychain        → OK
//	encrypted-file / passphrase-protected OpenSSH key → OK with info note
//	plaintext file  → WARN + migration hint
//
// Note: project.Load already runs signing.Config.Validate(), so by the time
// this runs the config shape is guaranteed valid. This check focuses on
// incomplete-setup states that are valid per Validate() but will fail at
// dispatch time (e.g. provider configured but public_key not yet captured).
func checkSigning(cfg *project.Config) []Finding {
	sc := cfg.Signing
	if sc == nil {
		return []Finding{{Check: checkNameSigning, Level: LevelOK, Message: "signing not configured"}}
	}
	// SSH mode with a provider configured but no public key means `koryph
	// signing setup` has not been completed — commits won't be signed/verified.
	if sc.Provider != "" && sc.PublicKey == "" && sc.EffectiveMode() == "ssh" {
		return []Finding{{
			Check:   checkNameSigning,
			Level:   LevelWarn,
			Message: "signing configured but public_key not captured (run `koryph signing setup`)",
		}}
	}
	prefix := ""
	if sc.Required {
		prefix = "required; "
	}

	// Posture ladder check.
	posture := signing.ClassifyPosture(sc)
	base := fmt.Sprintf("%smode=%s provider=%s identity=%s",
		prefix, sc.EffectiveMode(), sc.Provider, sc.Identity)

	findings := []Finding{{
		Check:   checkNameSigning,
		Level:   LevelOK,
		Message: base,
	}}

	switch posture.Level {
	case signing.PostureVault, signing.PostureKeychain:
		// OK — no additional note needed.
	case signing.PosturePassphraseProtected:
		findings = append(findings, Finding{
			Check:   checkNameSigning,
			Level:   LevelOK,
			Message: "signing posture: " + posture.Summary + " — " + posture.Note,
		})
	case signing.PosturePlaintext:
		if sc.Provider != "" {
			findings = append(findings, Finding{
				Check:   checkNameSigning,
				Level:   LevelWarn,
				Message: "signing posture: " + posture.Note,
			})
		}
	}

	return findings
}

// checkProtectedPaths validates the project's protected_paths list for sanity.
func checkProtectedPaths(cfg *project.Config) Finding {
	if len(cfg.ProtectedPaths) == 0 {
		return Finding{
			Check:   checkNameProtectedPaths,
			Level:   LevelOK,
			Message: "no extra protected_paths configured (engine defaults apply)",
		}
	}
	var emptyIdx []int
	seen := map[string]bool{}
	var dupes []string
	for i, p := range cfg.ProtectedPaths {
		if strings.TrimSpace(p) == "" {
			emptyIdx = append(emptyIdx, i)
			continue
		}
		if seen[p] {
			dupes = append(dupes, p)
		}
		seen[p] = true
	}
	if len(emptyIdx) > 0 {
		return Finding{
			Check:   checkNameProtectedPaths,
			Level:   LevelError,
			Message: fmt.Sprintf("empty path at index(es) %v in protected_paths", emptyIdx),
		}
	}
	if len(dupes) > 0 {
		return Finding{
			Check:   checkNameProtectedPaths,
			Level:   LevelWarn,
			Message: "duplicate entries: " + strings.Join(dupes, ", "),
		}
	}
	return Finding{
		Check:   checkNameProtectedPaths,
		Level:   LevelOK,
		Message: fmt.Sprintf("%d extra protected path(s)", len(cfg.ProtectedPaths)),
	}
}

// checkStalledRuns scans all ledger runs for non-terminal slots whose UpdatedAt
// timestamp is older than the stall threshold.
func checkStalledRuns(opts ProjectOptions, repoRoot string) []Finding {
	threshold := opts.stallThreshold()
	store := ledger.NewStore(repoRoot)
	runIDs, err := store.ListRuns()
	if err != nil || len(runIDs) == 0 {
		return []Finding{{Check: checkNameStalledRuns, Level: LevelOK, Message: "no ledger runs"}}
	}

	var stalled []Finding
	for _, runID := range runIDs {
		run, rerr := store.LoadRun(runID)
		if rerr != nil {
			continue
		}
		if run.Status != ledger.RunRunning {
			continue // only active runs can have stalled slots
		}
		for phaseID, slot := range run.Slots {
			if slot == nil || ledger.Terminal(slot.Status) {
				continue
			}
			if slot.UpdatedAt == "" {
				continue
			}
			t, perr := time.Parse(time.RFC3339, slot.UpdatedAt)
			if perr != nil {
				continue
			}
			age := opts.now().Sub(t)
			if age > threshold {
				stalled = append(stalled, Finding{
					Check: checkNameStalledRuns,
					Level: LevelWarn,
					Message: fmt.Sprintf("stalled slot: run=%s phase=%s status=%s age=%s",
						runID, phaseID, slot.Status, age.Truncate(time.Second)),
				})
			}
		}
	}

	if len(stalled) == 0 {
		return []Finding{{Check: checkNameStalledRuns, Level: LevelOK, Message: "no stalled slots"}}
	}
	return stalled
}

// checkOrphanWorktrees finds git worktrees under the project's worktree root
// that have a koryph agent branch but no corresponding active slot in any
// currently-running ledger run.
func checkOrphanWorktrees(opts ProjectOptions, repoRoot string, cfg *project.Config) []Finding {
	wtRoot := opts.WorktreeRoot
	if wtRoot == "" {
		wtRoot = filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+"-worktrees")
	}

	// Collect worktree paths claimed by active (non-terminal) slots across all
	// currently-running ledger runs.
	store := ledger.NewStore(repoRoot)
	activeWorktrees := map[string]bool{}
	if runIDs, err := store.ListRuns(); err == nil {
		for _, runID := range runIDs {
			run, rerr := store.LoadRun(runID)
			if rerr != nil || run.Status != ledger.RunRunning {
				continue
			}
			for _, slot := range run.Slots {
				if slot == nil || ledger.Terminal(slot.Status) || slot.Worktree == "" {
					continue
				}
				activeWorktrees[filepath.Clean(slot.Worktree)] = true
			}
		}
	}

	wts, err := opts.listWorktrees(repoRoot)
	if err != nil {
		return []Finding{{
			Check:   checkNameOrphanWorktrees,
			Level:   LevelWarn,
			Message: fmt.Sprintf("cannot list git worktrees: %v", err),
		}}
	}

	cleanWTRoot := filepath.Clean(wtRoot)
	var orphans []Finding
	for _, wt := range wts {
		// Only consider worktrees under the project's worktree root.
		if !strings.HasPrefix(filepath.Clean(wt.Path), cleanWTRoot+string(filepath.Separator)) {
			continue
		}
		// Only koryph-managed branches use the agent/ prefix.
		if !strings.HasPrefix(wt.Branch, "agent/") {
			continue
		}
		if activeWorktrees[filepath.Clean(wt.Path)] {
			continue // has a live active slot
		}
		orphans = append(orphans, Finding{
			Check: checkNameOrphanWorktrees,
			Level: LevelWarn,
			Message: fmt.Sprintf("orphan worktree: %s (branch %s, no active slot — review and remove manually if no longer needed)",
				wt.Path, wt.Branch),
		})
	}

	if len(orphans) == 0 {
		return []Finding{{
			Check:   checkNameOrphanWorktrees,
			Level:   LevelOK,
			Message: "no orphan worktrees under " + wtRoot,
		}}
	}
	return orphans
}

// --- asset drift check ------------------------------------------------------

// assetSpec describes one embedded asset set to check for drift.
type assetSpec struct {
	label   string                 // "commands" or "agents" (used in messages)
	fsys    fs.FS                  // the embedded FS to compare against
	destDir string                 // destination dir relative to repoRoot
	filter  func(name string) bool // nil = accept all entries
}

// checkAssetDrift compares the installed .claude/commands/koryph-*.md and
// .claude/agents/koryph-*.md files against the currently embedded set using
// SHA-256 hashes. It reports:
//   - missing: asset is in the embedded set but not installed on disk
//   - stale:   installed file's content differs from the embedded version
//
// When opts.Fix is true, missing files are always installed. Stale files are
// reinstalled only when opts.Force is also true; without Force they are
// reported but left untouched.
func checkAssetDrift(opts ProjectOptions, repoRoot string) []Finding {
	specs := []assetSpec{
		{
			label:   "commands",
			fsys:    opts.commandsFS(),
			destDir: filepath.Join(repoRoot, ".claude", "commands"),
			filter:  nil, // commands.FS only embeds koryph-*.md
		},
		{
			label:   "agents",
			fsys:    opts.agentsFS(),
			destDir: filepath.Join(repoRoot, ".claude", "agents"),
			filter:  func(name string) bool { return strings.HasPrefix(name, "koryph-") },
		},
	}

	var findings []Finding
	totalOK := 0

	for _, spec := range specs {
		entries, err := fs.ReadDir(spec.fsys, ".")
		if err != nil {
			findings = append(findings, Finding{
				Check:   checkNameAssetDrift,
				Level:   LevelError,
				Message: fmt.Sprintf("%s: read embedded FS: %v", spec.label, err),
			})
			continue
		}

		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if spec.filter != nil && !spec.filter(e.Name()) {
				continue
			}

			embedded, rerr := fs.ReadFile(spec.fsys, e.Name())
			if rerr != nil {
				findings = append(findings, Finding{
					Check:   checkNameAssetDrift,
					Level:   LevelError,
					Message: fmt.Sprintf("%s/%s: read embedded asset: %v", spec.label, e.Name(), rerr),
				})
				continue
			}
			embeddedHash := sha256.Sum256(embedded)

			destPath := filepath.Join(spec.destDir, e.Name())
			onDisk, derr := os.ReadFile(destPath)

			if errors.Is(derr, os.ErrNotExist) {
				// Asset is missing from the project.
				f := Finding{
					Check:   checkNameAssetDrift,
					Level:   LevelWarn,
					Message: fmt.Sprintf("%s/%s: missing (run `koryph %s install <root>` or `koryph doctor --project ... --fix`)", spec.label, e.Name(), spec.label),
				}
				if opts.Fix {
					if merr := os.MkdirAll(spec.destDir, 0o755); merr == nil {
						if werr := os.WriteFile(destPath, embedded, 0o644); werr == nil {
							f.Level = LevelOK
							f.Message = fmt.Sprintf("%s/%s: installed (was missing)", spec.label, e.Name())
							f.Fixed = true
						}
					}
				}
				findings = append(findings, f)
				continue
			}

			if derr != nil {
				findings = append(findings, Finding{
					Check:   checkNameAssetDrift,
					Level:   LevelError,
					Message: fmt.Sprintf("%s/%s: read on-disk file: %v", spec.label, e.Name(), derr),
				})
				continue
			}

			diskHash := sha256.Sum256(onDisk)
			if diskHash == embeddedHash {
				totalOK++
				continue // up to date — no finding
			}

			// Asset exists but content differs (stale or locally diverged).
			f := Finding{
				Check:   checkNameAssetDrift,
				Level:   LevelWarn,
				Message: fmt.Sprintf("%s/%s: stale (content differs from embedded version; run `koryph doctor --project ... --fix --force` to reinstall)", spec.label, e.Name()),
			}
			if opts.Fix {
				if opts.Force {
					if werr := os.WriteFile(destPath, embedded, 0o644); werr == nil {
						f.Level = LevelOK
						f.Message = fmt.Sprintf("%s/%s: reinstalled (was stale)", spec.label, e.Name())
						f.Fixed = true
					}
				} else {
					// Differing files are left untouched without --force so user
					// local modifications are never silently clobbered.
					f.Message = fmt.Sprintf("%s/%s: stale, left unchanged (add --force to overwrite)", spec.label, e.Name())
				}
			}
			findings = append(findings, f)
		}
	}

	if len(findings) == 0 {
		return []Finding{{
			Check:   checkNameAssetDrift,
			Level:   LevelOK,
			Message: fmt.Sprintf("%d asset(s) up to date", totalOK),
		}}
	}
	return findings
}

// --- git worktree listing ---------------------------------------------------

// defaultListWorktrees enumerates worktrees registered against repoRoot by
// delegating to worktree.List (which uses execx) and mapping the result to the
// local worktreeEntry descriptor used by the orphan check.
func defaultListWorktrees(root string) ([]worktreeEntry, error) {
	infos, err := worktree.List(context.Background(), root)
	if err != nil {
		return nil, err
	}
	entries := make([]worktreeEntry, len(infos))
	for i, info := range infos {
		entries[i] = worktreeEntry{Path: info.Path, Branch: info.Branch}
	}
	return entries, nil
}
