// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sysdeps

import (
	"reflect"
	"testing"
)

const (
	osReleaseUbuntu = `NAME="Ubuntu"
ID=ubuntu
ID_LIKE=debian
VERSION_ID="22.04"
`
	osReleaseDebian = `NAME="Debian GNU/Linux"
ID=debian
VERSION_ID="12"
`
	osReleaseFedora = `NAME=Fedora Linux
ID=fedora
VERSION_ID=40
`
	osReleaseArch = `NAME="Arch Linux"
ID=arch
`
	osReleaseOpenSUSE = `NAME="openSUSE Leap"
ID="opensuse-leap"
ID_LIKE="suse opensuse"
VERSION_ID="15.5"
`
	// Linux Mint declares its own ID but ID_LIKE=ubuntu, so it must resolve
	// to apt via the ID_LIKE fallback, not by recognizing "linuxmint"
	// directly.
	osReleaseLinuxMint = `NAME="Linux Mint"
ID=linuxmint
ID_LIKE=ubuntu
VERSION_ID="21.3"
`
)

// lookPathSet returns a lookPath stub that reports true only for the named
// binaries — the fixture for "what's on this machine's PATH".
func lookPathSet(bins ...string) func(string) bool {
	set := map[string]bool{}
	for _, b := range bins {
		set[b] = true
	}
	return func(name string) bool { return set[name] }
}

func osRelease(content string) func() (string, bool) {
	return func() (string, bool) { return content, true }
}

func noOSRelease() (string, bool) { return "", false }

func TestParseOSRelease(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantID   string
		wantLike []string
	}{
		{"ubuntu", osReleaseUbuntu, "ubuntu", []string{"debian"}},
		{"debian no id_like", osReleaseDebian, "debian", nil},
		{"fedora bare values", osReleaseFedora, "fedora", nil},
		{"arch", osReleaseArch, "arch", nil},
		{"opensuse multi id_like", osReleaseOpenSUSE, "opensuse-leap", []string{"suse", "opensuse"}},
		{"linuxmint id_like ubuntu", osReleaseLinuxMint, "linuxmint", []string{"ubuntu"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, like := parseOSRelease(tc.content)
			if id != tc.wantID {
				t.Errorf("ID = %q, want %q", id, tc.wantID)
			}
			if !reflect.DeepEqual(like, tc.wantLike) {
				t.Errorf("ID_LIKE = %v, want %v", like, tc.wantLike)
			}
		})
	}
}

func TestNativeManagerFor(t *testing.T) {
	cases := []struct {
		name       string
		distroID   string
		distroLike []string
		want       Manager
		wantOK     bool
	}{
		{"ubuntu", "ubuntu", []string{"debian"}, ManagerApt, true},
		{"debian", "debian", nil, ManagerApt, true},
		{"fedora", "fedora", nil, ManagerDnf, true},
		{"arch", "arch", nil, ManagerPacman, true},
		{"opensuse-leap", "opensuse-leap", []string{"suse", "opensuse"}, ManagerZypper, true},
		{"linuxmint via ID_LIKE", "linuxmint", []string{"ubuntu"}, ManagerApt, true},
		{"unknown distro, no fallback", "voidlinux", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nativeManagerFor(tc.distroID, tc.distroLike)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("nativeManagerFor(%q, %v) = (%q, %v), want (%q, %v)",
					tc.distroID, tc.distroLike, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestDetect_Darwin(t *testing.T) {
	p := detect("darwin", noOSRelease, lookPathSet("brew"))
	if p.OS != "darwin" {
		t.Errorf("OS = %q, want darwin", p.OS)
	}
	if p.DistroID != "" || p.DistroLike != nil {
		t.Errorf("darwin should have no distro fields: id=%q like=%v", p.DistroID, p.DistroLike)
	}
	if !reflect.DeepEqual(p.Managers, []Manager{ManagerBrew}) {
		t.Errorf("Managers = %v, want [brew]", p.Managers)
	}
}

func TestDetect_DarwinNoBrew(t *testing.T) {
	p := detect("darwin", noOSRelease, lookPathSet())
	if len(p.Managers) != 0 {
		t.Errorf("Managers = %v, want empty when brew is not on PATH", p.Managers)
	}
}

func TestDetect_UbuntuApt(t *testing.T) {
	p := detect("linux", osRelease(osReleaseUbuntu), lookPathSet("apt-get"))
	if p.DistroID != "ubuntu" {
		t.Errorf("DistroID = %q, want ubuntu", p.DistroID)
	}
	if !reflect.DeepEqual(p.Managers, []Manager{ManagerApt}) {
		t.Errorf("Managers = %v, want [apt]", p.Managers)
	}
}

func TestDetect_ArchPacman(t *testing.T) {
	p := detect("linux", osRelease(osReleaseArch), lookPathSet("pacman"))
	if !reflect.DeepEqual(p.Managers, []Manager{ManagerPacman}) {
		t.Errorf("Managers = %v, want [pacman]", p.Managers)
	}
}

func TestDetect_LinuxNativeThenBrewThenNixThenNpm(t *testing.T) {
	// Ubuntu box with linuxbrew, nix, and npm all installed: order must be
	// native (apt) first, then brew, then nix-profile, then npm — never
	// brew-before-native on linux (RULES: "on linux: native manager first").
	p := detect("linux", osRelease(osReleaseUbuntu), lookPathSet("apt-get", "brew", "nix", "npm"))
	want := []Manager{ManagerApt, ManagerBrew, ManagerNix, ManagerNpm}
	if !reflect.DeepEqual(p.Managers, want) {
		t.Errorf("Managers = %v, want %v", p.Managers, want)
	}
}

func TestDetect_LinuxNoNativeManager_NixFallback(t *testing.T) {
	// A distro sysdeps doesn't recognize, with nix on PATH but no native
	// manager binary present: only nix-profile should surface.
	p := detect("linux", osRelease(`ID=voidlinux`+"\n"), lookPathSet("nix"))
	if !reflect.DeepEqual(p.Managers, []Manager{ManagerNix}) {
		t.Errorf("Managers = %v, want [nix-profile]", p.Managers)
	}
}

func TestDetect_LinuxMissingOSRelease(t *testing.T) {
	p := detect("linux", noOSRelease, lookPathSet("apt-get"))
	if p.DistroID != "" {
		t.Errorf("DistroID = %q, want empty when os-release is unreadable", p.DistroID)
	}
	if len(p.Managers) != 0 {
		t.Errorf("Managers = %v, want empty: no distro means no native manager to try", p.Managers)
	}
}

func TestDetect_Windows(t *testing.T) {
	// windows has no native-manager or brew branch, but the cross-platform
	// nix-profile/npm pseudo-routes are still probed and surfaced.
	p := detect("windows", noOSRelease, lookPathSet("brew", "nix", "npm"))
	want := []Manager{ManagerNix, ManagerNpm}
	if !reflect.DeepEqual(p.Managers, want) {
		t.Errorf("Managers = %v, want %v (brew has no windows branch, so it must not appear)", p.Managers, want)
	}
}
