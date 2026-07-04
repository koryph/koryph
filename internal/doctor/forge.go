// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import "github.com/koryph/koryph/internal/project"

const checkNameForge = "forge"

// checkForge reports the resolved forge provider for the project.
//
//   - ok  "forge: github"  — Forge is "" or "github" (default).
//   - ok  "forge: gitlab"  — Forge is "gitlab".
//   - warn                 — Forge is set to an unrecognised value; the
//     operator should correct koryph.project.json before the forge
//     selection becomes load-bearing in a provider call.
//
// When cfg is nil (project config failed to load) the check degrades
// gracefully to ok with a skip note so it never turns a clean report red
// simply because the project-config check already caught the error.
func checkForge(cfg *project.Config) Finding {
	if cfg == nil {
		return Finding{
			Check:   checkNameForge,
			Level:   LevelOK,
			Message: "skipped — project config unavailable",
		}
	}
	name := cfg.ResolvedForge()
	switch name {
	case "github", "gitlab":
		return Finding{
			Check:   checkNameForge,
			Level:   LevelOK,
			Message: "forge: " + name,
		}
	default:
		return Finding{
			Check:   checkNameForge,
			Level:   LevelWarn,
			Message: "forge " + name + " is not a recognised provider (want github|gitlab); update koryph.project.json",
		}
	}
}
