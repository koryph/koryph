// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package forge

import "strings"

// SniffRemote examines a git remote URL and returns a *suggested* forge
// provider name. The caller is responsible for the final provider selection —
// this function is an onboarding assist, never an authoritative decision.
//
// It returns "" when no known forge is recognised, allowing callers to fall
// back gracefully (e.g. prompt the operator or apply a project default).
//
// Recognised patterns:
//   - "github" when the URL contains "github.com"
//   - "gitlab" when the URL contains "gitlab.com" or "gitlab." (self-managed)
//
// Examples:
//
//	SniffRemote("git@github.com:acme/widgets.git")  → "github"
//	SniffRemote("https://gitlab.com/acme/app.git")  → "gitlab"
//	SniffRemote("https://git.acme.corp/foo.git")    → ""
func SniffRemote(remoteURL string) string {
	u := strings.ToLower(remoteURL)
	switch {
	case strings.Contains(u, "github.com"):
		return "github"
	case strings.Contains(u, "gitlab.com"),
		strings.Contains(u, "gitlab."):
		return "gitlab"
	default:
		return ""
	}
}
