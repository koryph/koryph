// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package beads

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/version"
)

// MinVersion is the minimum bd (beads) release whose `bd list --json` and
// `bd ready --json` emit the `parent` field koryph depends on. bd 1.0.3 and
// older OMIT `parent` from list/ready output entirely, so an older bd silently
// degrades koryph: the TUI Queue tab renders flat (every issue a top-level row,
// no epic folds, no child counts) and any consumer of parent linkage
// (epic-scoped views, child association) mis-groups. Verified against real
// output: 1.0.5 emits `parent`, 1.0.3 does not. Bump this if a newer bd changes
// the on-disk/JSON contract koryph relies on.
const MinVersion = "1.0.5"

// ResolveBin returns the bd binary koryph invokes: the KORYPH_BD_BIN override
// when set, else "bd" (found on PATH). Central so every adapter and the version
// preflight agree on which bd is actually in play — the same binary the failure
// mode hinges on.
func ResolveBin() string {
	if v := strings.TrimSpace(os.Getenv("KORYPH_BD_BIN")); v != "" {
		return v
	}
	return "bd"
}

// bdSemverRE extracts the X.Y.Z from a `bd version` line like
// "bd version 1.0.5 (Homebrew)" or "bd version 1.0.3 (dev)".
var bdSemverRE = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

// VersionInfo describes the resolved bd binary and whether it is new enough for
// koryph's parent-linkage features.
type VersionInfo struct {
	Bin     string // as invoked ("bd" or the KORYPH_BD_BIN override)
	Path    string // absolute resolved path; "" when not found on PATH
	Raw     string // first line of `bd version`
	Version string // parsed X.Y.Z; "" when unparseable
	Found   bool   // the binary resolved on PATH
	FromNix bool   // Path is under /nix/store/ (pinned by a nix env/devshell)
	OK      bool   // Version satisfies >= MinVersion
}

// ProbeVersion resolves the bd binary koryph will use and reports its version
// and whether it meets MinVersion. It never returns an error: an unresolved or
// unparseable bd yields Found/OK=false so a single caller-side warning covers
// every degraded case.
func ProbeVersion(ctx context.Context) VersionInfo {
	bin := ResolveBin()
	info := VersionInfo{Bin: bin}
	path, err := exec.LookPath(bin)
	if err != nil {
		return info // Found=false
	}
	info.Found = true
	info.Path = path
	info.FromNix = strings.Contains(path, "/nix/store/")

	// Bound the probe: `bd version` is instant, but a preflight/doctor check
	// must never hang on a wedged bd (or block the engine startup). 3s is
	// generous.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, rerr := execx.Run(ctx, execx.Cmd{Name: bin, Args: []string{"version"}})
	if rerr != nil {
		return info // resolved but unrunnable; OK stays false
	}
	line := res.Stdout
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	info.Raw = strings.TrimSpace(line)
	info.Version = bdSemverRE.FindString(info.Raw)
	if info.Version != "" {
		if ok, verr := version.Satisfied(info.Version, MinVersion); verr == nil {
			info.OK = ok
		}
	}
	return info
}

// flakeBeadsInputRE matches a flake input whose URL points at the beads repo,
// e.g. `beads.url = "github:gastownhall/beads/v1.1.0";`. Capture 1 is the input
// name, capture 2 its URL.
var flakeBeadsInputRE = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_-]*)\.url\s*=\s*"([^"]*beads[^"]*)"`)

// FlakeBeadsInput scans <flakeDir>/flake.nix for the flake input that provides
// beads (a URL referencing the beads repo) and returns the input's name and
// URL. found is false when there is no flake.nix or no beads input — koryph then
// cannot offer a targeted `nix flake lock --update-input`. This is how doctor
// turns "bd is nix-provided and stale" into a concrete, project-specific
// upgrade offer rather than generic advice.
func FlakeBeadsInput(flakeDir string) (name, url string, found bool) {
	data, err := os.ReadFile(filepath.Join(flakeDir, "flake.nix"))
	if err != nil {
		return "", "", false
	}
	if m := flakeBeadsInputRE.FindStringSubmatch(string(data)); m != nil {
		return m[1], m[2], true
	}
	return "", "", false
}

// Remediation returns the operator-facing fix advice for a bd that is missing or
// too old, tailored to whether the binary comes from a nix environment (where
// the fix is a flake/devshell pin bump, not a package-manager upgrade).
func (v VersionInfo) Remediation() string {
	if !v.Found {
		return "bd (beads) is not on PATH — install beads >= " + MinVersion +
			" (or set KORYPH_BD_BIN to its path)"
	}
	ver := v.Version
	if ver == "" {
		ver = "an unknown version"
	}
	head := "bd " + ver + " at " + v.Path + " is older than the required " + MinVersion +
		": `bd list --json` omits the `parent` field, so the TUI Queue tab renders flat " +
		"(no epic folds) and parent-linked views are degraded. "
	if v.FromNix {
		return head + "This bd is provided by a nix environment (" + v.Path +
			") — bump the beads pin in the flake input / devshell that supplies it, " +
			"or set KORYPH_BD_BIN to a newer bd (e.g. a Homebrew `bd`) as a stopgap."
	}
	return head + "Upgrade beads to >= " + MinVersion +
		" (e.g. `brew upgrade beads`), or set KORYPH_BD_BIN to a newer bd."
}
