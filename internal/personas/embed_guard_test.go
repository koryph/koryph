// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package personas

import (
	"strings"
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
	"koryph-reviewer",
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

func TestGeneralReviewerIsFocusedStandardTier(t *testing.T) {
	data, err := agents.FS.ReadFile("koryph-reviewer.md")
	if err != nil {
		t.Fatalf("read koryph-reviewer.md: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"tier: standard",
		"effort: high",
		"exact gated candidate",
		"canonical acceptance ID",
		"worker evidence as a lead",
		"prior stable finding ID",
		"do not repeat broad validation",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("koryph-reviewer.md missing focused-review contract %q", want)
		}
	}
	for _, forbidden := range []string{
		"Bash(make gate",
		"Bash(go test ./...",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("koryph-reviewer.md grants redundant full-gate command %q", forbidden)
		}
	}
}

func TestReviewPersonasUseStrictSharedSeverityVocabulary(t *testing.T) {
	for _, name := range []string{"koryph-reviewer.md", "koryph-security-reviewer.md"} {
		data, err := agents.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(data)
		for _, want := range []string{
			"strict JSON",
			"`blocking`, `major`, or",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing verdict contract %q", name, want)
			}
		}
	}
}

func TestDispatchDefaultPersonasAreEmbedded(t *testing.T) {
	for _, name := range dispatchDefaultPersonas {
		if _, err := agents.FS.ReadFile(name + ".md"); err != nil {
			t.Errorf("persona %q is a hardcoded dispatch default but is not in the embedded agents/ corpus (%v); "+
				"add agents/%s.md so personas.Install ships it to every managed project", name, err, name)
		}
	}
}

func TestPlanningPersonasRequireCanonicalGateEvidence(t *testing.T) {
	for name, wants := range map[string][]string{
		"koryph-architect.md": {
			"decision ledger",
			"schema-versioned pre-file graph evidence",
			"post-file graph",
		},
		"koryph-plan-scorer.md": {
			"canonical schema-versioned pre-file graph",
			"contradiction count",
			"strict-gate",
		},
	} {
		data, err := agents.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, want := range wants {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s missing quality-gate contract %q", name, want)
			}
		}
	}
}

func TestCanonicalPersonasOwnOnlyRoleClauses(t *testing.T) {
	for _, persona := range dispatchDefaultPersonas {
		name := persona + ".md"
		data, err := agents.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(data)
		if got := strings.Count(text, "<!-- koryph-clause:role/v1 -->"); got != 1 {
			t.Errorf("%s role clause count = %d, want 1", name, got)
		}
		for _, forbidden := range []string{
			"<!-- koryph-clause:repository/",
			"<!-- koryph-clause:engine/",
			"<!-- koryph-clause:task/",
			"make gate",
			"make gate-agent",
			"git checkout main",
			"INBOX.md",
			"refactor-core",
			"model:<id>",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains foreign or stale instruction %q", name, forbidden)
			}
		}
	}
}
