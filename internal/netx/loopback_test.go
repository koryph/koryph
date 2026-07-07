// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package netx_test

import (
	"testing"

	"github.com/koryph/koryph/internal/netx"
)

func TestIsLoopbackHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host string
		want bool
	}{
		// localhost variants
		{"localhost", true},
		{"LOCALHOST", true},
		{"Localhost", true},

		// 127.0.0.0/8
		{"127.0.0.1", true},
		{"127.0.0.42", true},
		{"127.255.255.255", true},

		// ::1 (IPv6 loopback)
		{"::1", true},

		// IPv4-mapped loopback — the edge case the ad-hoc copy missed
		{"::ffff:127.0.0.1", true},

		// non-loopback
		{"example.com", false},
		{"192.168.1.1", false},
		{"10.0.0.1", false},
		{"8.8.8.8", false},
		{"", false},

		// bracket forms are stripped by url.URL.Hostname() before we're called;
		// we should not see them, but confirm we don't crash.
		{"[::1]", false}, // net.ParseIP("[::1]") returns nil — caller strips brackets
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			if got := netx.IsLoopbackHost(tc.host); got != tc.want {
				t.Errorf("IsLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}
