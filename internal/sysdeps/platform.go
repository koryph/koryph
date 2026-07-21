// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package sysdeps detects the host platform (OS, Linux distro family, and
// which package managers are on PATH) and turns that into a consented
// install plan for koryph's external tool dependencies (bd, claude, gh).
//
// This package is pure planning: Detect and Plan never execute an install,
// write a file, or touch the network. The only "execution" here is the
// LookPath-style PATH probe Detect uses to see which package managers
// exist; running the argv a plan proposes is the adopt wizard's job, one
// confirmed item at a time (docs/designs/2026-07-adopt.md §3.3, §4). No
// function in this package ever shells out to install anything, and no
// Manual instruction ever suggests curl-pipe-sh: manual fallbacks point at
// the tool's official install docs or release page instead.
package sysdeps

import (
	"os"
	"runtime"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// Manager identifies a package manager (or pseudo-route) sysdeps knows how to
// plan an install through. Values: "brew", "apt", "dnf", "pacman", "zypper",
// "nix-profile", "npm" (a pseudo-route, claude only), and "manual" (no
// package-manager route — see InstallPlan.Route).
type Manager string

const (
	ManagerBrew   Manager = "brew"
	ManagerApt    Manager = "apt"
	ManagerDnf    Manager = "dnf"
	ManagerPacman Manager = "pacman"
	ManagerZypper Manager = "zypper"
	ManagerNix    Manager = "nix-profile"
	// ManagerNpm is not a system package manager; it is a pseudo-route some
	// tool specs (claude) use because the JS ecosystem's install path is more
	// reliably documented than a platform package's exact name.
	ManagerNpm Manager = "npm"
	// ManagerManual is the InstallPlan.Route value when no package-manager
	// route is available (or none is present on PATH): the fallback is
	// human instructions, never a command sysdeps runs itself.
	ManagerManual Manager = "manual"
)

// managerBinary names the executable Detect probes on PATH to decide a
// Manager is usable. nix-profile probes "nix" (the CLI subcommand `nix
// profile` requires the same binary); apt probes "apt-get", the stable
// scriptable entry point ("apt" itself warns about an unstable CLI).
var managerBinary = map[Manager]string{
	ManagerBrew:   "brew",
	ManagerApt:    "apt-get",
	ManagerDnf:    "dnf",
	ManagerPacman: "pacman",
	ManagerZypper: "zypper",
	ManagerNix:    "nix",
	ManagerNpm:    "npm",
}

// Platform describes the host sysdeps is planning installs for.
type Platform struct {
	OS string // "darwin", "linux", "windows" (runtime.GOOS)
	// DistroID and DistroLike come from /etc/os-release (linux only): the ID
	// field ("ubuntu", "debian", "fedora", "arch", "opensuse-leap", ...) and
	// the whitespace-separated ID_LIKE tokens used to map an unrecognized
	// derivative distro (e.g. "linuxmint", ID_LIKE=ubuntu) to its family.
	DistroID   string
	DistroLike []string
	// Managers are the package managers/pseudo-routes detected on PATH, in
	// the platform's preference order: on darwin, brew first; on linux, the
	// distro's native manager first, then brew (linuxbrew), then
	// nix-profile, then npm. Only managers actually found on PATH are
	// included — this is availability, not a static wishlist.
	Managers []Manager
}

// distroNativeManagers maps an /etc/os-release ID/ID_LIKE token prefix to the
// native package manager for that distro family. Matched by prefix so a
// versioned ID like "opensuse-leap" or "opensuse-tumbleweed" still resolves
// to zypper. Order matters only in that the first matching prefix wins; the
// prefixes themselves are disjoint in practice.
var distroNativeManagers = []struct {
	Prefix  string
	Manager Manager
}{
	{"debian", ManagerApt},
	{"ubuntu", ManagerApt},
	{"fedora", ManagerDnf},
	{"rhel", ManagerDnf},
	{"centos", ManagerDnf},
	{"rocky", ManagerDnf},
	{"almalinux", ManagerDnf},
	{"arch", ManagerPacman},
	{"manjaro", ManagerPacman},
	{"opensuse", ManagerZypper},
	{"suse", ManagerZypper},
	{"sles", ManagerZypper},
}

// nativeManagerFor returns the native package manager for a distro, checking
// DistroID first and then each DistroLike token in order (so a derivative
// distro koryph has never heard of by name still resolves via ID_LIKE, e.g.
// Linux Mint: ID=linuxmint, ID_LIKE=ubuntu → apt).
func nativeManagerFor(distroID string, distroLike []string) (Manager, bool) {
	tokens := make([]string, 0, 1+len(distroLike))
	if distroID != "" {
		tokens = append(tokens, distroID)
	}
	tokens = append(tokens, distroLike...)
	for _, tok := range tokens {
		for _, entry := range distroNativeManagers {
			if strings.HasPrefix(tok, entry.Prefix) {
				return entry.Manager, true
			}
		}
	}
	return "", false
}

// Detect probes the running platform: GOOS, the Linux distro family (from
// /etc/os-release, when present), and which package managers are on PATH.
// It performs no writes and no network access; the only side effect is the
// PATH lookup itself.
func Detect() Platform {
	return detect(runtime.GOOS, readOSRelease, execx.LookPath)
}

// detect is the internal, fully-injectable variant Detect wraps: goos stands
// in for runtime.GOOS, osRelease stands in for reading /etc/os-release, and
// lookPath stands in for checking PATH — so tests can exercise every distro
// and PATH combination without touching the real filesystem or environment.
func detect(goos string, osRelease func() (string, bool), lookPath func(string) bool) Platform {
	p := Platform{OS: goos}
	if goos == "linux" {
		if content, ok := osRelease(); ok {
			p.DistroID, p.DistroLike = parseOSRelease(content)
		}
	}
	p.Managers = detectManagers(p.OS, p.DistroID, p.DistroLike, lookPath)
	return p
}

// readOSRelease reads /etc/os-release, the real (non-test) source Detect
// uses. ok is false when the file is absent or unreadable (e.g. non-Linux,
// or a minimal container image) — Detect then leaves DistroID/DistroLike
// empty and simply finds no native manager.
func readOSRelease() (string, bool) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", false
	}
	return string(data), true
}

// parseOSRelease extracts the ID and ID_LIKE fields from the contents of an
// /etc/os-release file. Values may be double-quoted, single-quoted, or bare
// per the file's shell-variable-assignment format; ID_LIKE is a
// whitespace-separated list, most-general-last (e.g. "ubuntu" for a
// Debian-like, sometimes several tokens for deeper derivatives).
func parseOSRelease(content string) (id string, like []string) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := line[:eq]
		val := unquoteOSReleaseValue(line[eq+1:])
		switch key {
		case "ID":
			id = val
		case "ID_LIKE":
			like = strings.Fields(val)
		}
	}
	return id, like
}

// unquoteOSReleaseValue strips a single layer of matching double or single
// quotes from an os-release value, leaving bare values untouched.
func unquoteOSReleaseValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// detectManagers builds the platform-preference-ordered list of managers
// actually present on PATH: darwin tries brew; linux tries its native
// manager first, then brew (linuxbrew). nix-profile and npm are appended
// last on every OS — they are cross-platform fallbacks, never the first
// choice, per docs/designs/2026-07-adopt.md §4 ("Route order: ... Homebrew
// ... native ... nix profile install ... manual").
func detectManagers(goos, distroID string, distroLike []string, lookPath func(string) bool) []Manager {
	var order []Manager
	switch goos {
	case "darwin":
		order = append(order, ManagerBrew)
	case "linux":
		if native, ok := nativeManagerFor(distroID, distroLike); ok {
			order = append(order, native)
		}
		order = append(order, ManagerBrew) // linuxbrew, if installed
	}
	order = append(order, ManagerNix, ManagerNpm)

	var out []Manager
	seen := map[Manager]bool{}
	for _, m := range order {
		if seen[m] {
			continue
		}
		seen[m] = true
		bin := managerBinary[m]
		if bin != "" && lookPath(bin) {
			out = append(out, m)
		}
	}
	return out
}

// hasManager reports whether m was detected on PATH for p.
func hasManager(p Platform, m Manager) bool {
	for _, x := range p.Managers {
		if x == m {
			return true
		}
	}
	return false
}
