// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package textx_test

import (
	"testing"

	"github.com/koryph/koryph/internal/textx"
)

func TestTail(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"shorter than n returns all", "abc", 10, "abc"},
		{"equal to n returns all", "abcde", 5, "abcde"},
		{"longer than n keeps last n", "abcdef", 3, "def"},
		{"n zero returns empty", "abc", 0, ""},
		{"empty string", "", 4, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := textx.Tail(tc.s, tc.n); got != tc.want {
				t.Errorf("Tail(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
		})
	}
}
