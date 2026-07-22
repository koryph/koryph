// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package beads

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
)

// scpLikeSSHRE matches git's SCP-like remote syntax, "user@host:path" (e.g.
// "git@github.com:owner/repo.git"). The user segment is required: without
// it, a bare "host:path" is indistinguishable from a Windows drive path
// ("C:\Users\x"), so an unprefixed form is treated as unrecognizable rather
// than guessed at.
var scpLikeSSHRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+@([A-Za-z0-9_.-]+):(.+)$`)

// DeriveSyncRemote turns a git origin URL into bd's sync.remote form,
// `git+https://<host>/<path>.git` — the form this repo's own
// .beads/config.yaml already carries. It accepts http(s) origin URLs
// (passed through under the git+ scheme) and SSH forms (scp-like
// "git@host:path" and "ssh://[user@]host/path"), normalizing both to https
// and appending a missing ".git" suffix. Anything else (a filesystem path,
// an empty string, an unparseable scheme) returns "" — the caller then does
// a local-only `bd init` (docs/designs/2026-07-adopt.md §5): guessing wrong
// here would wire a beads DB to push/pull against a remote nobody chose.
func DeriveSyncRemote(originURL string) string {
	u := strings.TrimSpace(originURL)
	if u == "" {
		return ""
	}
	// Idempotent: already bd's own form.
	if strings.HasPrefix(u, "git+http://") || strings.HasPrefix(u, "git+https://") {
		return ensureDotGitSuffix(u)
	}
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return ensureDotGitSuffix("git+" + u)
	}
	if rest, ok := strings.CutPrefix(u, "ssh://"); ok {
		if i := strings.IndexByte(rest, '@'); i >= 0 {
			rest = rest[i+1:]
		}
		return ensureDotGitSuffix("git+https://" + rest)
	}
	if m := scpLikeSSHRE.FindStringSubmatch(u); m != nil {
		return ensureDotGitSuffix("git+https://" + m[1] + "/" + m[2])
	}
	return ""
}

// ensureDotGitSuffix appends ".git" unless u already ends with it.
func ensureDotGitSuffix(u string) string {
	if strings.HasSuffix(u, ".git") {
		return u
	}
	return u + ".git"
}

// EnsureOpts parameterizes EnsureDB's absent-DB init path.
type EnsureOpts struct {
	Prefix string // bd's --prefix (issue-id prefix); required by `bd init`.
	Remote string // bd's --remote (sync.remote); "" = local-only init.
}

// EnsureResult reports what EnsureDB found and did, so an adopt-style caller
// can render a plan/report line without re-deriving any of this itself.
type EnsureResult struct {
	// Initialized is true only when this call ran `bd init` (the .beads dir
	// was absent beforehand).
	Initialized bool
	// InitArgv is the exact `bd init ...` argv used; nil unless Initialized.
	InitArgv []string
	// RemoteNote is set when a --remote init could not clone (the remote has
	// no beads history yet — the fresh-adopt common case) and EnsureDB fell
	// back to a local init with sync.remote configured for future pushes.
	RemoteNote string

	// VersionOK mirrors ProbeVersion(ctx).OK — whether the resolved bd
	// satisfies MinVersion. Populated on both the init and existing-DB
	// paths.
	VersionOK bool
	// VersionStatus is ProbeVersion's raw `bd version` first line, or
	// "bd not found" when the binary did not resolve at all.
	VersionStatus string
	// Remediation carries VersionInfo.Remediation() when VersionOK is
	// false, "" otherwise. EnsureDB never fails just because the version is
	// old — the caller decides what to do with this text (§4 installer,
	// or print-and-continue).
	Remediation string

	// SnapshotPath is the tar backup EnsureDB took before running `bd
	// doctor` against an existing DB. "" on the init-from-absent path:
	// there is nothing yet to protect.
	SnapshotPath string

	// DoctorRan, DoctorOutput, DoctorOK are populated only on the
	// existing-DB path. bd doctor owns schema/migration checks; koryph only
	// surfaces its findings, so a non-zero doctor exit is recorded here, not
	// returned as an error.
	DoctorRan    bool
	DoctorOutput string
	DoctorOK     bool
}

// EnsureDB makes sure a beads DB exists at a.BeadsDir: initializing one via
// `bd init --non-interactive --init-if-missing --prefix <opts.Prefix>
// [--remote <opts.Remote>]` when absent, or — when present — snapshotting
// first and then running `bd doctor` so its schema/migration findings
// surface without koryph ever forking bd's migration logic. EnsureDB NEVER
// passes bd's destructive init flags (--reinit-local, --force,
// --discard-remote); those are exclusively operator-driven, never
// wizard-driven, under any circumstance.
func (a *Adapter) EnsureDB(ctx context.Context, opts EnsureOpts) (EnsureResult, error) {
	var res EnsureResult

	if !fsx.Exists(a.BeadsDir) {
		base := []string{"init", "--non-interactive", "--init-if-missing", "--prefix", opts.Prefix}
		args := base
		if opts.Remote != "" {
			args = append(append([]string{}, base...), "--remote", opts.Remote)
		}
		if _, err := a.run(ctx, args...); err != nil {
			// `bd init --remote` CLONES the remote's beads history — right for
			// a collaborator joining an already-seeded project, but a fresh
			// adopt's remote has no beads refs yet and the clone fails. Only a
			// clone failure earns the fallback: a local init with sync.remote
			// recorded for future `bd dolt push` (the same config.yaml key a
			// cloning init would have set). Any other init failure (bad
			// prefix, disk full, ...) propagates — retrying it locally would
			// just mask a real error behind a reassuring message.
			if opts.Remote == "" || !strings.Contains(err.Error(), "clone") {
				return res, fmt.Errorf("beads: init: %w", err)
			}
			if _, lerr := a.run(ctx, base...); lerr != nil {
				return res, fmt.Errorf("beads: init (local fallback after remote clone failed): %w", lerr)
			}
			if _, cerr := a.run(ctx, "config", "set", "sync.remote", opts.Remote); cerr != nil {
				res.RemoteNote = fmt.Sprintf("could not clone %s (no beads history there yet?); initialized locally — configure sync yourself (`bd config set sync.remote %s` failed: %v)", opts.Remote, opts.Remote, cerr)
			} else {
				res.RemoteNote = fmt.Sprintf("could not clone %s (no beads history there yet?); initialized locally with sync.remote set — `bd dolt push` will seed it", opts.Remote)
			}
			args = base
		}
		res.Initialized = true
		res.InitArgv = args
	} else {
		// bd doctor may itself apply fixes; snapshot first (koryph's one
		// precondition for any beads mutation it did not just create).
		snap, err := a.Snapshot(ctx)
		if err != nil {
			return res, fmt.Errorf("beads: snapshot before doctor: %w", err)
		}
		res.SnapshotPath = snap

		doctorRes, err := execx.Run(ctx, execx.Cmd{Dir: a.RepoRoot, Name: a.Bin, Args: []string{"doctor"}})
		if err != nil {
			return res, fmt.Errorf("beads: run bd doctor: %w", err)
		}
		res.DoctorRan = true
		res.DoctorOutput = combineOutput(doctorRes.Stdout, doctorRes.Stderr)
		res.DoctorOK = doctorRes.ExitCode == 0
	}

	info := ProbeVersion(ctx)
	res.VersionOK = info.OK
	if info.Found {
		res.VersionStatus = info.Raw
	} else {
		res.VersionStatus = "bd not found"
	}
	if !info.OK {
		res.Remediation = info.Remediation()
	}

	return res, nil
}

// combineOutput joins a command's stdout and stderr into one trimmed block
// for a result field that must carry both (bd doctor mixes its findings
// across the two streams).
func combineOutput(stdout, stderr string) string {
	stdout, stderr = strings.TrimSpace(stdout), strings.TrimSpace(stderr)
	switch {
	case stdout == "":
		return stderr
	case stderr == "":
		return stdout
	default:
		return stdout + "\n" + stderr
	}
}

// HardenAction reports one hardening check Harden performed: either it was
// fixed by this call (Applied=true), or it needs manual/`bd`-driven action
// and Detail carries the remediation (Applied=false). A fully-hardened DB
// yields a nil/empty slice — Harden re-run on a healthy project is silent.
type HardenAction struct {
	Name    string
	Applied bool
	Detail  string
}

// Harden is the writable counterpart of onboard's read-only hardened check
// (onboard.detectBeadsHardened / detectBeadsHooks, internal/onboard/
// inspect.go:163-237): it fixes what koryph is safe to fix directly (the
// .gitignore ignore line) and reports what only bd or the user can fix
// (sync.remote — bd owns config.yaml; git hooks — bd's own installer owns
// them). It errors if .beads is absent: harden never creates a DB, only
// tidies one that already exists (call EnsureDB first).
func (a *Adapter) Harden(ctx context.Context) ([]HardenAction, error) {
	if !fsx.Exists(a.BeadsDir) {
		return nil, fmt.Errorf("beads: harden %s: no beads db present (run EnsureDB first)", a.BeadsDir)
	}

	var actions []HardenAction

	applied, err := a.ensureGitignoreIgnoresIssuesJSONL()
	if err != nil {
		return actions, err
	}
	if applied {
		actions = append(actions, HardenAction{
			Name:    "gitignore-issues-jsonl",
			Applied: true,
			Detail:  "appended `issues.jsonl` to .beads/.gitignore",
		})
	}

	// sync.remote lives in bd's config.yaml, which koryph never edits
	// directly (bd owns its own schema/comments there) — report, don't fix.
	cfgData, _ := os.ReadFile(filepath.Join(a.BeadsDir, "config.yaml"))
	if !syncRemoteConfigured(string(cfgData)) {
		actions = append(actions, HardenAction{
			Name:    "sync-remote",
			Applied: false,
			Detail:  "sync.remote is not set in .beads/config.yaml — pass Remote to EnsureDB (bd init --remote git+https://...) instead of hand-editing bd's config",
		})
	}

	if !beadsHooksInstalled(ctx, a.RepoRoot) {
		remediation := "bd git hooks not detected — re-run `bd init` to (re)install them"
		if a.hooksSubcommandAvailable(ctx) {
			remediation = "bd git hooks not detected — run `bd hooks install`"
		}
		actions = append(actions, HardenAction{
			Name:    "git-hooks",
			Applied: false,
			Detail:  remediation,
		})
	}

	return actions, nil
}

// ensureGitignoreIgnoresIssuesJSONL appends an issues.jsonl ignore line to
// .beads/.gitignore when it is missing (creating the file if .beads exists
// but .gitignore does not — bd normally writes this file itself, but a
// stale/hand-rolled .beads dir may lack it). Returns applied=false when the
// line is already present: idempotent re-runs are silent, not repeated
// appends.
func (a *Adapter) ensureGitignoreIgnoresIssuesJSONL() (applied bool, err error) {
	path := filepath.Join(a.BeadsDir, ".gitignore")
	data, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, fmt.Errorf("beads: read %s: %w", path, readErr)
	}
	if strings.Contains(string(data), "issues.jsonl") {
		return false, nil
	}
	if err := fsx.AppendLine(path, []byte("issues.jsonl")); err != nil {
		return false, fmt.Errorf("beads: append %s: %w", path, err)
	}
	return true, nil
}

// syncRemoteConfigured reports whether beads config.yaml text declares an
// uncommented sync.remote, accepting both the nested (sync:\n  remote: <x>)
// and flat (sync.remote: <x>) forms bd supports. Mirrors
// onboard.syncRemoteSet; duplicated rather than imported because
// internal/onboard imports internal/beads (the reverse import would cycle),
// and this parse is otherwise self-contained config.yaml-shape knowledge.
func syncRemoteConfigured(yaml string) bool {
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

// gitHooksDir resolves the git hooks directory for root: core.hookspath when
// set (absolute or root-relative), else .git/hooks. Mirrors
// onboard.detectBeadsHooks's resolution (duplicated for the same
// import-cycle reason as syncRemoteConfigured).
func gitHooksDir(ctx context.Context, root string) string {
	hooksDir := filepath.Join(root, ".git", "hooks")
	res, err := execx.Run(ctx, execx.Cmd{Dir: root, Name: "git", Args: []string{"config", "core.hookspath"}})
	if err == nil && res.ExitCode == 0 {
		if s := strings.TrimSpace(res.Stdout); s != "" {
			if filepath.IsAbs(s) {
				hooksDir = s
			} else {
				hooksDir = filepath.Join(root, s)
			}
		}
	}
	return hooksDir
}

// beadsHooksInstalled reports whether any file under root's git hooks dir
// carries bd's install marker.
func beadsHooksInstalled(ctx context.Context, root string) bool {
	dir := gitHooksDir(ctx, root)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr == nil && bytes.Contains(data, []byte("BEADS INTEGRATION")) {
			return true
		}
	}
	return false
}

// hooksSubcommandAvailable reports whether the bd binary in play understands
// `bd hooks --help` (exit 0), so Harden can point at the precise `bd hooks
// install` remedy instead of generic re-init guidance when it does not.
func (a *Adapter) hooksSubcommandAvailable(ctx context.Context) bool {
	if !a.Available() {
		return false
	}
	res, err := execx.Run(ctx, execx.Cmd{Dir: a.RepoRoot, Name: a.Bin, Args: []string{"hooks", "--help"}})
	return err == nil && res.ExitCode == 0
}
