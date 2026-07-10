// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package personas

import (
	"testing"

	"github.com/koryph/koryph/agents"
)

// dispatchDefaultPersonas are the persona names the engine hardcodes as
// dispatch defaults across the codebase (e.g. epicreview.defaultPersona,
// internal/project's epic-validation default, the implementer/reviewer/etc.
// defaults resolved per stage). Every one MUST ship in the embedded agents/
// corpus, because personas.Install copies only agents/*.md into a managed
// project's .claude/agents — a hardcoded default with no embedded file
// dispatches `claude --agent <name>` against a persona that was never
// installed, breaking that feature in every project except koryph's own
// self-hosted checkout (which carries the file under .claude/agents directly).
//
// koryph-epic-validator was exactly this gap (2026-07-10 audit P0): present in
// .claude/agents but absent from agents/. Keep this list in sync when adding a
// new hardcoded persona default.
var dispatchDefaultPersonas = []string{
	"koryph-implementer",
	"koryph-architect",
	"koryph-security-reviewer",
	"koryph-test-engineer",
	"koryph-debugger",
	"koryph-explorer",
	"koryph-plan-scorer",
	"koryph-feature-docs-author",
	"koryph-epic-validator",
	"koryph-merge-readiness",
	"koryph-migration-analyst",
	"koryph-quota-analyst",
	"koryph-recovery-analyst",
}

func TestDispatchDefaultPersonasAreEmbedded(t *testing.T) {
	for _, name := range dispatchDefaultPersonas {
		if _, err := agents.FS.ReadFile(name + ".md"); err != nil {
			t.Errorf("persona %q is a hardcoded dispatch default but is not in the embedded agents/ corpus (%v); "+
				"add agents/%s.md so personas.Install ships it to every managed project", name, err, name)
		}
	}
}
