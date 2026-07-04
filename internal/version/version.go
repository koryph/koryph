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
	"strconv"
	"strings"
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
const Engine = "0.5.0" // x-release-please-version

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
