// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package version

import "testing"

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
