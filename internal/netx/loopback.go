// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package netx provides shared network-address predicates used across koryph's
// security gates. Centralising them here prevents the drift between independent
// copies that constitutes a security debt (see design I4: loopback-only routing
// for dispatched-agent Anthropic traffic).
package netx

import (
	"net"
	"strings"
)

// IsLoopbackHost reports whether host (a URL's Hostname(), already stripped of
// brackets and port by url.URL.Hostname()) is a loopback address:
//   - "localhost" by name (case-insensitive), or
//   - any IP whose net.IP.IsLoopback() returns true — this covers 127.0.0.0/8,
//     ::1, and IPv4-mapped forms such as ::ffff:127.0.0.1.
//
// This is the single authoritative implementation of the loopback-host
// predicate. Both the registry load-time validation and the doctor proxy check
// call this function; there must be no other copy.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
