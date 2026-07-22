// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package account

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// discoverVerifyTimeout bounds how long Discover waits on any single
// candidate's Verify call. Verify only ever does a local file read+parse,
// but a per-candidate deadline keeps a stray stale/slow mount (or a future,
// slower Verify implementation) from turning discovery into a hang — the
// adopt wizard's detect phase (docs/designs/2026-07-adopt.md §3.1) must stay
// snappy across however many profile directories a machine has.
const discoverVerifyTimeout = 2 * time.Second

// Candidate is a discoverable account profile with its provenance and, when
// verifiable, the identity email read from its .claude.json.
type Candidate struct {
	// Profile is the resolved account context, ready to hand to
	// onboard.Register or account.Verify as-is. The personal candidate's
	// Profile always has ConfigDir == "" — never pointed explicitly at the
	// resolved home directory, matching account.go's "never point personal
	// at ~/.claude explicitly" contract for live dispatch.
	Profile Profile
	// Identity is the verified oauthAccount.emailAddress, "" when
	// unverifiable.
	Identity string
	// Verified reports whether Profile's .claude.json was read and parsed
	// with a non-empty emailAddress (see Verify). False means Identity is ""
	// and Err explains why.
	Verified bool
	// Provenance is a one-line, human-facing description of where this
	// candidate came from — e.g. "derived from ~/.claude.json" or
	// "found ~/.claude-work/.claude.json" — surfaced by the adopt wizard's
	// plan and confirm phases (docs/designs/2026-07-adopt.md §3.1, §3.3) so
	// a proposed account is never a mystery value.
	Provenance string
	// Err is a one-line, koryph-voiced reason Verified is false, for the
	// wizard's "blocked" display. When the underlying cause is simply that
	// the profile has never been logged in (no .claude.json yet), Err
	// carries auth guidance (e.g. "run `claude auth login`") instead of a
	// raw file-not-found error. Empty when Verified is true.
	Err string
}

// Discover enumerates candidate Claude account profiles on this machine —
// the default (personal) profile plus any CLAUDE_CONFIG_DIR-style profile
// directory under $HOME (the ~/.claude-* convention, e.g. ~/.claude-work) —
// and verifies each against its .claude.json, so the adopt wizard can
// PROPOSE an account instead of demanding --account/--identity flags
// (docs/designs/2026-07-adopt.md §3.1, "account candidates"). Discover never
// invents accounts and never reads a repo's .envrc — the .envrc
// claude-account managed block is a separate, repo-scoped source that
// onboard.Inspect already parses; this function is purely the machine-level
// scan.
func Discover(ctx context.Context) []Candidate {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return discover(ctx, home)
}

// discover is Discover's injectable core: it takes home explicitly so tests
// can point discovery at a fixture directory instead of the real $HOME,
// without touching process-global state (no t.Setenv("HOME", ...) required).
func discover(ctx context.Context, home string) []Candidate {
	var out []Candidate
	seen := make(map[string]bool) // de-dup by resolved ConfigJSONPath

	// add verifies the .claude.json at path and appends one Candidate for
	// profile. Verification is done against a local copy of profile whose
	// ConfigDir is pinned to path's directory — never against the stored
	// profile itself, since the personal candidate's stored Profile keeps
	// ConfigDir == "" (see Candidate.Profile's doc). Pinning the verify-time
	// ConfigDir is what lets an injected home differ from the process's real
	// $HOME in tests while still reading the right fixture file.
	add := func(profile Profile, path, provenance string) {
		if seen[path] {
			return
		}
		seen[path] = true

		cand := Candidate{Profile: profile, Provenance: provenance}
		vctx, cancel := context.WithTimeout(ctx, discoverVerifyTimeout)
		defer cancel()
		verifyProfile := profile
		verifyProfile.ConfigDir = filepath.Dir(path)
		id, err := Verify(vctx, verifyProfile)
		if err != nil {
			cand.Err = verifyErrMessage(path, err)
		} else {
			cand.Verified = true
			cand.Identity = id.Email
		}
		out = append(out, cand)
	}

	// 1. Default profile: $HOME/.claude.json. Always emitted, whether or not
	// the file exists yet — the adopt wizard needs to say "no account found,
	// here's how to get one" as much as "here's the one we found".
	add(Profile{Name: "personal"}, filepath.Join(home, ".claude.json"), "derived from ~/.claude.json")

	// 2. Config-dir profiles: $HOME/.claude-* directories that contain a
	// .claude.json. filepath.Glob sorts its matches, so candidate order is
	// deterministic across runs. A .claude-* directory WITHOUT a .claude.json
	// is not a candidate at all (it is not a Claude profile — e.g. a stray
	// cache or unrelated dotdir) and is silently skipped.
	matches, _ := filepath.Glob(filepath.Join(home, ".claude-*"))
	for _, dir := range matches {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		candPath := filepath.Join(dir, ".claude.json")
		if _, err := os.Stat(candPath); err != nil {
			continue
		}
		base := filepath.Base(dir)
		name := strings.TrimPrefix(base, ".claude-")
		add(Profile{Name: name, ConfigDir: dir}, candPath, fmt.Sprintf("found ~/%s/.claude.json", base))
	}

	return out
}

// verifyErrMessage renders a Verify error as a koryph-voiced, one-line
// reason for Candidate.Err. The common "never logged in" case — no
// .claude.json at path yet — gets explicit auth guidance instead of a raw
// os.ReadFile error; every other failure (unparseable JSON, empty
// emailAddress) is Verify's own message with the dispatch-specific
// "— refusing dispatch" tail trimmed, since a discovery listing is not a
// dispatch refusal.
func verifyErrMessage(path string, err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("no %s yet — run `claude auth login` to create it", path)
	}
	msg := err.Error()
	if i := strings.Index(msg, " — refusing dispatch"); i >= 0 {
		msg = msg[:i]
	}
	return msg
}
