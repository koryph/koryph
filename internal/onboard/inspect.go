// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package onboard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/worktree"
)

// envrcOpenMarker is the start of the managed claude-account direnv block.
const envrcOpenMarker = "# >>> claude-account (managed) >>>"

// envrcCloseMarker prefixes the end of the managed block.
const envrcCloseMarker = "# <<<"

// Inspect builds a read-only inventory of the project at root. It NEVER writes
// anywhere, NEVER sources .envrc (it is parsed as text), and NEVER mutates
// beads or git state — every probe is a read (file read, or a read-only git/bd
// subcommand). Sub-probe failures are tolerated (the corresponding field is
// simply left at its zero value); only a missing root is an error.
func Inspect(ctx context.Context, root string) (*Inventory, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("onboard: resolve root %q: %w", root, err)
	}
	if !fsx.Exists(absRoot) {
		return nil, fmt.Errorf("onboard: root %q does not exist", absRoot)
	}

	inv := &Inventory{Root: absRoot}
	inv.IsGitRepo = fsx.Exists(filepath.Join(absRoot, ".git"))
	if inv.IsGitRepo {
		inv.DefaultBranch = detectDefaultBranch(ctx, absRoot)
		inv.Remote = detectRemote(ctx, absRoot)
	}

	// Beads.
	inv.HasBeads = fsx.Exists(filepath.Join(absRoot, ".beads"))
	if inv.HasBeads {
		inv.BeadsHardened = detectBeadsHardened(absRoot)
	}
	inv.BeadsHooks = detectBeadsHooks(ctx, absRoot)
	bd := newBD(absRoot)
	inv.BDAvailable = bd.Available()
	if inv.BDAvailable {
		if v, verr := bd.Version(ctx); verr == nil {
			inv.BDVersion = v
		}
	}

	// Claude wiring.
	settings := filepath.Join(absRoot, ".claude", "settings.json")
	inv.ClaudeSettings = fsx.Exists(settings)
	if inv.ClaudeSettings {
		inv.BDPrimeHook = fileContains(settings, "bd prime")
	}
	inv.Personas = detectPersonas(absRoot)

	// Legacy koryph fork.
	inv.LegacyKoryph, inv.LegacyHints = detectLegacy(absRoot)

	// .envrc managed account block (parsed as TEXT — never sourced).
	if data, rerr := os.ReadFile(filepath.Join(absRoot, ".envrc")); rerr == nil {
		inv.EnvrcProfile, inv.EnvrcDir = classifyEnvrc(string(data))
	} else {
		inv.EnvrcProfile = "none"
	}

	// Worktrees.
	if inv.IsGitRepo {
		if wts, werr := worktree.List(ctx, absRoot); werr == nil {
			for _, w := range wts {
				inv.Worktrees = append(inv.Worktrees, WorktreeState{
					Path:   w.Path,
					Branch: w.Branch,
					Dirty:  w.Dirty,
				})
			}
		}
	}

	inv.AdapterPresent = fsx.Exists(filepath.Join(absRoot, project.ConfigFileName))
	inv.PlansDir = detectPlansDir(absRoot)

	return inv, nil
}

// --- bd helpers (shared with validate.go) ----------------------------------

// bdBin resolves the bd binary, honoring the KORYPH_BD_BIN override used by
// tests and non-default installs.
func bdBin() string {
	if v := os.Getenv("KORYPH_BD_BIN"); v != "" {
		return v
	}
	return "bd"
}

// newBD returns a beads adapter for root with the resolved binary.
func newBD(root string) *beads.Adapter {
	a := beads.New(root)
	a.Bin = bdBin()
	return a
}

// --- git probes (all read-only) --------------------------------------------

// gitOut runs a read-only git subcommand in dir and returns trimmed stdout and
// whether it succeeded (spawned and exited zero).
func gitOut(ctx context.Context, dir string, args ...string) (string, bool) {
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "git", Args: args})
	if err != nil || res.ExitCode != 0 {
		return "", false
	}
	return strings.TrimSpace(res.Stdout), true
}

// detectDefaultBranch resolves the repo's default branch: origin/HEAD, then the
// current symbolic branch (works on an unborn branch), then init.defaultBranch,
// then main|master by ref existence.
func detectDefaultBranch(ctx context.Context, root string) string {
	if s, ok := gitOut(ctx, root, "symbolic-ref", "refs/remotes/origin/HEAD"); ok && s != "" {
		return strings.TrimPrefix(s, "refs/remotes/origin/")
	}
	if s, ok := gitOut(ctx, root, "symbolic-ref", "--short", "HEAD"); ok && s != "" && s != "HEAD" {
		return s
	}
	if s, ok := gitOut(ctx, root, "config", "init.defaultBranch"); ok && s != "" {
		return s
	}
	if _, ok := gitOut(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/main"); ok {
		return "main"
	}
	if _, ok := gitOut(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/master"); ok {
		return "master"
	}
	return ""
}

// detectRemote returns the origin fetch URL, or "" when there is no origin.
func detectRemote(ctx context.Context, root string) string {
	if s, ok := gitOut(ctx, root, "config", "--get", "remote.origin.url"); ok {
		return s
	}
	return ""
}

// --- beads probes ----------------------------------------------------------

// detectBeadsHardened reports whether the project's beads is hardened:
// .beads/.gitignore exists AND ignores issues.jsonl AND .beads/config.yaml
// declares an uncommented sync.remote.
func detectBeadsHardened(root string) bool {
	giData, err := os.ReadFile(filepath.Join(root, ".beads", ".gitignore"))
	if err != nil || !strings.Contains(string(giData), "issues.jsonl") {
		return false
	}
	cfgData, err := os.ReadFile(filepath.Join(root, ".beads", "config.yaml"))
	if err != nil {
		return false
	}
	return syncRemoteSet(string(cfgData))
}

// syncRemoteSet reports whether the beads config declares an uncommented
// sync.remote, accepting both the nested (sync:\n  remote: <x>) and flat
// (sync.remote: <x>) forms.
func syncRemoteSet(yaml string) bool {
	inSync := false
	for _, raw := range strings.Split(yaml, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if noSpace := strings.ReplaceAll(trimmed, " ", ""); strings.HasPrefix(noSpace, "sync.remote:") {
			if valueAfterColon(trimmed) != "" {
				return true
			}
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if indent == 0 {
			inSync = strings.HasPrefix(trimmed, "sync:")
			continue
		}
		if inSync && strings.HasPrefix(trimmed, "remote:") && valueAfterColon(trimmed) != "" {
			return true
		}
	}
	return false
}

// valueAfterColon returns the trimmed content after the first colon in s.
func valueAfterColon(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return ""
}

// detectBeadsHooks reports whether any file under core.hookspath (or .git/hooks)
// carries the beads integration marker.
func detectBeadsHooks(ctx context.Context, root string) bool {
	hooksDir := filepath.Join(root, ".git", "hooks")
	if s, ok := gitOut(ctx, root, "config", "core.hookspath"); ok && s != "" {
		if filepath.IsAbs(s) {
			hooksDir = s
		} else {
			hooksDir = filepath.Join(root, s)
		}
	}
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if fileContains(filepath.Join(hooksDir, e.Name()), "BEADS INTEGRATION") {
			return true
		}
	}
	return false
}

// --- claude / legacy / plans probes ----------------------------------------

// detectPersonas returns the sorted persona names (basenames without .md) under
// .claude/agents.
func detectPersonas(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, ".claude", "agents"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(out)
	return out
}

// detectLegacy reports whether a legacy koryph/ fork is present and returns
// generation hints (scheduler.sh, --source bd, workflows.sh, lib/bd-source.sh).
func detectLegacy(root string) (bool, []string) {
	condDir := filepath.Join(root, "koryph")
	present := fsx.Exists(condDir)
	var hints []string

	scheduler := filepath.Join(condDir, "scheduler.sh")
	if fsx.Exists(scheduler) {
		present = true
		hints = append(hints, "koryph/scheduler.sh")
		if fileContains(scheduler, "--source bd") {
			hints = append(hints, "scheduler.sh uses --source bd")
		}
	}
	if fsx.Exists(filepath.Join(condDir, "workflows.sh")) {
		present = true
		hints = append(hints, "koryph/workflows.sh")
	}
	if fsx.Exists(filepath.Join(condDir, "lib", "bd-source.sh")) {
		present = true
		hints = append(hints, "koryph/lib/bd-source.sh")
	}
	return present, hints
}

// detectPlansDir returns the first conventional plans directory that exists.
func detectPlansDir(root string) string {
	for _, p := range []string{"docs/plans", "plans"} {
		if fsx.Exists(filepath.Join(root, p)) {
			return p
		}
	}
	return ""
}

// --- .envrc classification (text-only) -------------------------------------

// envrcConfigDirRe extracts the RHS of a CLAUDE_CONFIG_DIR= assignment.
var envrcConfigDirRe = regexp.MustCompile(`CLAUDE_CONFIG_DIR=["']?([^"'\n]+)["']?`)

// varRefRe matches a bare shell variable reference that is the ENTIRE value —
// i.e. CLAUDE_CONFIG_DIR is set via another variable rather than a literal
// path.  It matches $VARNAME and ${VARNAME} but NOT $HOME/.claude-work (has a
// slash) or ${VAR:-default} (has a colon/dash), which are already resolvable
// as written.
var varRefRe = regexp.MustCompile(`^\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))$`)

// varAssignRe finds simple shell variable assignments within a block, with an
// optional leading "export".  Used to resolve one level of variable
// indirection: WORK_DIR="$HOME/.claude-work" + CLAUDE_CONFIG_DIR="$WORK_DIR".
var varAssignRe = regexp.MustCompile(`(?m)^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=["']?([^"'\n]+)["']?`)

// classifyEnvrc parses the managed account block out of .envrc content (as
// text) and classifies the declared profile. It never executes the file.
func classifyEnvrc(content string) (profile, dir string) {
	idx := strings.Index(content, envrcOpenMarker)
	if idx < 0 {
		return "none", ""
	}
	rest := content[idx+len(envrcOpenMarker):]
	block := rest
	if c := strings.Index(rest, envrcCloseMarker); c >= 0 {
		block = rest[:c]
	}

	switch {
	case strings.Contains(block, ".claude-work"):
		d := extractEnvrcDir(block)
		if d == "" {
			d = "~/.claude-work"
		}
		return "work", d
	case strings.Contains(block, `$HOME/.claude"`),
		strings.Contains(block, `$HOME/.claude'`),
		strings.Contains(block, ":-$HOME/.claude}"):
		return "personal-explicit-deprecated", extractEnvrcDir(block)
	case strings.Contains(block, "unset CLAUDE_CONFIG_DIR"):
		return "personal-unset", ""
	default:
		return "none", ""
	}
}

// extractEnvrcDir returns the directory from the block's CLAUDE_CONFIG_DIR
// assignment, resolving one level of variable indirection when the assignment
// is a bare variable reference (e.g. "$WORK_DIR" or "${WORK_DIR}").  When the
// reference cannot be resolved within the block, the reference is returned
// as-is.  The value is never shell-executed.
func extractEnvrcDir(block string) string {
	m := envrcConfigDirRe.FindStringSubmatch(block)
	if len(m) < 2 {
		return ""
	}
	val := strings.TrimSpace(m[1])

	// Resolve one level of variable indirection: if the value is a pure
	// variable reference with no path component (e.g. "$WORK_DIR", not
	// "$HOME/.claude-work"), look up that variable's assignment in the block.
	if ref := varRefRe.FindStringSubmatch(val); len(ref) >= 2 {
		varName := ref[1] // braced form: ${VAR}
		if varName == "" {
			varName = ref[2] // simple form: $VAR
		}
		for _, am := range varAssignRe.FindAllStringSubmatch(block, -1) {
			if am[1] == varName {
				return strings.TrimSpace(am[2])
			}
		}
		// No definition found in block — return the reference unchanged so
		// callers can still display it.
	}

	return val
}

// --- small io helper -------------------------------------------------------

// fileContains reports whether the file at path contains needle.
func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte(needle))
}
