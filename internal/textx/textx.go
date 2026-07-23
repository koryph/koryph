// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package textx holds tiny, dependency-free string helpers shared across
// packages that would otherwise each keep their own byte-identical copy
// (koryph-fiv finding #6). It imports nothing outside the standard library so
// any package — including low-level ones like worktree — can depend on it
// without risking an import cycle.
package textx

// Tail returns the last n bytes of s (all of s when it is already <= n bytes).
// It is a byte, not rune, clip: callers use it to bound captured command
// output and log tails where an exact rune boundary does not matter, and the
// cost of decoding UTF-8 would. Before this helper the same four lines were
// copied into internal/merge, internal/worktree, and internal/agentjson.
func Tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
