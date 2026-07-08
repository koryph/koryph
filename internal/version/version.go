// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package version holds the engine's semantic version and the requirement
// check projects use to pin a minimum engine ("engine_version" in
// koryph.project.json).
//
// Requirement syntax (minimum-style, per the project contract):
//
//	"0.2+"   — engine >= 0.2.0
//	"1+"     — engine >= 1.0.0
//	"0.2.3+" — engine >= 0.2.3
//	"0.2"    — same as "0.2+" (bare versions are minimums)
//	">=0.2"  — same as "0.2+"
//	"v0.2+"  — leading v accepted everywhere
package version

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// Engine is the koryph engine's semantic version. Bump per semver;
// releases are tagged v<Engine>.
//
// release-please's "go" release-type strategy ignores `version-file`
// entirely, so this constant is NOT wired via that mechanism. Instead it is
// listed in release-please-config.json's `extra-files` (generic updater),
// which finds and rewrites the quoted version on whichever line carries the
// `x-release-please-version` annotation comment below. Do not remove that
// annotation or move the version off this line — see
// docs/developer-guide/releasing.md.
const Engine = "0.8.0" // x-release-please-version

// Linker-injected build provenance. These are stamped via `-ldflags -X` by the
// Makefile (`make build`/`make install`) and .goreleaser.yaml so a binary can
// self-report the exact commit it was cut from — the release tag on a tagged
// build, or an intermediate "v0.8.0-5-gabc1234[-dirty]" for an untagged/dirty
// tree. They stay empty for `go run`/`go test` and any build without the stamp;
// in that case the runtime/debug VCS fallback (see resolve) recovers the commit
// and timestamp the Go toolchain already embeds into `go build`/`go install`
// binaries. Engine (above) remains the plain semver used by Satisfied and
// release-please — it is deliberately NOT overridden here.
var (
	describe string // `git describe --tags --always --dirty`; "" if unstamped
	commit   string // short hash, with a "-dirty" suffix for a modified tree
	date     string // RFC3339 build/commit timestamp
)

var (
	resolveOnce               sync.Once
	rDescribe, rCommit, rDate string
)

// resolve folds the linker-stamped values together with the VCS metadata the Go
// toolchain embeds into `go build`/`go install` binaries, so a binary built
// without the Makefile stamp still reports its commit and dirty state. Computed
// once, lazily, on first accessor call.
func resolve() {
	resolveOnce.Do(func() {
		rDescribe, rCommit, rDate = describe, commit, date
		if rCommit != "" && rDate != "" {
			return
		}
		bi, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		var rev, mod, t string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				mod = s.Value
			case "vcs.time":
				t = s.Value
			}
		}
		if rev != "" && rCommit == "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if mod == "true" {
				rev += "-dirty"
			}
			rCommit = rev
		}
		if t != "" && rDate == "" {
			rDate = t
		}
	})
}

// Build returns the `git describe` provenance stamped into the binary
// ("v0.8.0", "v0.8.0-5-gabc1234-dirty", ...), or "" when the binary carries no
// such stamp (e.g. `go run`, `go test`, or a `go install` that recorded only a
// bare commit). Callers that want a never-empty string can fall back to Engine.
func Build() string { resolve(); return rDescribe }

// Commit returns the abbreviated build commit — with a "-dirty" suffix for a
// modified tree — from the linker stamp or the Go VCS build info; "" if absent.
func Commit() string { resolve(); return rCommit }

// Date returns the RFC3339 build/commit timestamp, or "" if unknown.
func Date() string { resolve(); return rDate }

// parse splits a semantic version into major/minor/patch (missing parts = 0).
func parse(v string) (maj, min, pat int, err error) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return 0, 0, 0, fmt.Errorf("empty version")
	}
	parts := strings.SplitN(v, ".", 3)
	nums := [3]int{}
	for i, p := range parts {
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 0 {
			return 0, 0, 0, fmt.Errorf("invalid version %q", v)
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], nil
}

// Satisfied reports whether engine version `have` meets requirement `want`.
// An empty requirement is always satisfied.
func Satisfied(have, want string) (bool, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return true, nil
	}
	want = strings.TrimPrefix(want, ">=")
	want = strings.TrimSuffix(strings.TrimSpace(want), "+")

	hmaj, hmin, hpat, err := parse(have)
	if err != nil {
		return false, fmt.Errorf("engine version: %w", err)
	}
	wmaj, wmin, wpat, err := parse(want)
	if err != nil {
		return false, fmt.Errorf("engine_version requirement: %w", err)
	}
	if hmaj != wmaj {
		return hmaj > wmaj, nil
	}
	if hmin != wmin {
		return hmin > wmin, nil
	}
	return hpat >= wpat, nil
}
