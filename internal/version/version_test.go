// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package version

import "testing"

// Under `go test` (like `go run`) the Go toolchain embeds no VCS metadata and
// the linker `-X` stamps are absent, so the provenance accessors must report
// empty. This is the contract that keeps `koryph version` a single canonical
// "koryph <semver>" line for the release-parity hooks (goreleaser's before-hook
// and `make version-check`), which parse that line's last field.
func TestBuildProvenanceEmptyWhenUnstamped(t *testing.T) {
	if got := Build(); got != "" {
		t.Errorf("Build() = %q, want empty for an unstamped test binary", got)
	}
	if got := Commit(); got != "" {
		t.Errorf("Commit() = %q, want empty for an unstamped test binary", got)
	}
	if got := Date(); got != "" {
		t.Errorf("Date() = %q, want empty for an unstamped test binary", got)
	}
}

func TestSatisfied(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
		wantErr    bool
	}{
		{"0.2.0", "", true, false},
		{"0.2.0", "0.2+", true, false},
		{"0.2.0", "0.2", true, false},
		{"0.2.0", ">=0.2", true, false},
		{"0.2.0", "v0.2+", true, false},
		{"0.2.0", "0.2.1+", false, false},
		{"0.2.0", "0.3+", false, false},
		{"0.2.0", "1+", false, false},
		{"1.0.0", "0.9.9+", true, false},
		{"1.2.3", "1.2.3", true, false},
		{"1.2.2", "1.2.3", false, false},
		{"2.0.0", "1.99.99+", true, false},
		{"0.2.0", "abc", false, true},
		{"", "0.2+", false, true},
	}
	for _, c := range cases {
		ok, err := Satisfied(c.have, c.want)
		if (err != nil) != c.wantErr {
			t.Errorf("Satisfied(%q,%q) err = %v, wantErr %v", c.have, c.want, err, c.wantErr)
			continue
		}
		if !c.wantErr && ok != c.ok {
			t.Errorf("Satisfied(%q,%q) = %v, want %v", c.have, c.want, ok, c.ok)
		}
	}
}
