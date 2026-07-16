// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/epicreview"
)

// checkNameEpicValidations is the check name reported in doctor findings for
// parked and degraded epic validations.
const checkNameEpicValidations = "epic-validations"

// checkEpicValidations scans all open epics for validation:parked or
// validation:degraded labels and reports each as a LevelWarn finding with the
// round number and reason extracted from the epic's notes field.
//
// This delivers the "never a black box" surfacing promise from design §4
// (docs/designs/2026-07-epic-validation.md):
//
//   - validation:parked → the epic has exceeded max_rounds and is awaiting
//     an operator decision; `koryph epic validate <id>` is the recovery verb.
//   - validation:degraded → the validator infra failed during a round; the
//     epic carries a degraded note and needs a retry.
//
// If bd is unavailable the check returns OK — bd absence is reported by the
// global doctor checks, not here. An error from the list call is surfaced as
// a LevelWarn so the gap is visible without being fatal.
func checkEpicValidations(opts ProjectOptions, repoRoot string) []Finding {
	listEpics := opts.ListEpics
	if listEpics == nil {
		listEpics = defaultListEpics
	}

	epics, err := listEpics(repoRoot)
	if err != nil {
		return []Finding{{
			Check:   checkNameEpicValidations,
			Level:   LevelWarn,
			Message: fmt.Sprintf("cannot list epics: %v", err),
		}}
	}

	var findings []Finding
	for _, epic := range epics {
		if epic.Status == "closed" || epic.Status == "done" {
			continue
		}
		switch {
		case epic.HasLabel(epicreview.LabelParked):
			round, reason := parseParkedNote(epic.Notes)
			findings = append(findings, Finding{
				Check: checkNameEpicValidations,
				Level: LevelWarn,
				Message: fmt.Sprintf(
					"parked epic: %s %q round=%d reason=%q — run `koryph epic validate %s` to recover",
					epic.ID, epic.Title, round, reason, epic.ID),
			})
		case epic.HasLabel(epicreview.LabelDegraded):
			round, reason := parseDegradedNote(epic.Notes)
			findings = append(findings, Finding{
				Check: checkNameEpicValidations,
				Level: LevelWarn,
				Message: fmt.Sprintf(
					"degraded epic: %s %q round=%d reason=%q — validator infra failure; run `koryph epic validate %s` to retry",
					epic.ID, epic.Title, round, reason, epic.ID),
			})
		}
	}

	if len(findings) == 0 {
		return []Finding{{
			Check:   checkNameEpicValidations,
			Level:   LevelOK,
			Message: "no parked or degraded epic validations",
		}}
	}
	return findings
}

// defaultListEpics queries bd for all open epics in repoRoot.
// When bd is not on PATH it returns (nil, nil) — bd unavailability
// is already reported by the global doctor checks.
func defaultListEpics(repoRoot string) ([]beads.Issue, error) {
	a := beads.New(repoRoot)
	if !a.Available() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	all, err := a.List(ctx)
	if err != nil {
		return nil, err
	}
	var epics []beads.Issue
	for _, iss := range all {
		if iss.IssueType == "epic" {
			epics = append(epics, iss)
		}
	}
	return epics, nil
}

// checkNameUnvalidatedEpics is the check name reported for completed-but-
// unvalidated epic findings — deliberately matching
// internal/engine/health.go's patrolCheckEpics string so both surfaces
// report under the same check label (mirrors checkNameEpicValidations's
// pairing with patrolCheckEpicVal above).
const checkNameUnvalidatedEpics = "unvalidated-epics"

// checkUnvalidatedEpics scans all open epics for ones whose children are ALL
// closed but that never closed themselves — the offline counterpart to
// internal/engine/health.go's patrolCheckUnvalidatedEpics live backstop. It
// exists because that backstop only runs inside a live `koryph run` loop: an
// epic that finishes while no loop is running (or between patrol ticks, on a
// crash) is otherwise invisible until an operator happens to look. A `koryph
// doctor` run only reports — it cannot re-queue into a live loop's pending
// set — so the remediation named in the finding is the same recovery path
// epicvalidate.go's own doc comment documents: `koryph epic validate <id>`.
//
// This must call ListChildrenAll, NOT ListChildren: bd's plain list excludes
// closed issues by default, so a fully-closed epic (the exact case this
// check exists to catch) would look indistinguishable from a childless one
// through ListChildren — see beads.Adapter.ListChildren's doc comment.
func checkUnvalidatedEpics(opts ProjectOptions, repoRoot string) []Finding {
	listEpics := opts.ListEpics
	if listEpics == nil {
		listEpics = defaultListEpics
	}
	listChildrenAll := opts.ListChildrenAll
	if listChildrenAll == nil {
		listChildrenAll = defaultListChildrenAll
	}

	epics, err := listEpics(repoRoot)
	if err != nil {
		return []Finding{{
			Check:   checkNameUnvalidatedEpics,
			Level:   LevelWarn,
			Message: fmt.Sprintf("cannot list epics: %v", err),
		}}
	}

	var findings []Finding
	for _, epic := range epics {
		if epic.Status == "closed" || epic.Status == "done" {
			continue
		}
		if epic.HasLabel(epicreview.LabelNoValidate) || epic.HasLabel(epicreview.LabelParked) ||
			epic.HasLabel(epicreview.LabelDegraded) {
			continue // operator-decision or infra-failure states — surfaced by checkEpicValidations, not here
		}
		children, cerr := listChildrenAll(repoRoot, epic.ID)
		if cerr != nil {
			findings = append(findings, Finding{
				Check:   checkNameUnvalidatedEpics,
				Level:   LevelWarn,
				Message: fmt.Sprintf("epic %s: list children: %v", epic.ID, cerr),
			})
			continue
		}
		if len(children) == 0 || anyOpenChild(children) {
			continue // childless, or genuinely still in progress
		}

		reason := "validation was never triggered"
		if epic.HasLabel(epicreview.LabelPassed) {
			reason = "validation:passed but the epic never closed (close-after-docs path stalled)"
		}
		findings = append(findings, Finding{
			Check: checkNameUnvalidatedEpics,
			Level: LevelWarn,
			Message: fmt.Sprintf(
				"epic %s %q: all %d child(ren) closed, %s — run `koryph epic validate %s` to recover, or restart the loop",
				epic.ID, epic.Title, len(children), reason, epic.ID),
		})
	}

	if len(findings) == 0 {
		return []Finding{{
			Check:   checkNameUnvalidatedEpics,
			Level:   LevelOK,
			Message: "no stranded completed epics",
		}}
	}
	return findings
}

// anyOpenChild reports whether any child is non-terminal. Mirrors
// internal/engine/epicvalidate.go's helper of the same name (deferred counts
// as open there too — doctor has no dependency on engine to share it, so the
// two copies must be kept in sync by hand).
func anyOpenChild(children []beads.Issue) bool {
	for _, c := range children {
		if c.Status != "closed" && c.Status != "done" {
			return true
		}
	}
	return false
}

// defaultListChildrenAll queries bd for every child of epicID, open and
// closed. When bd is not on PATH it returns (nil, nil) — bd unavailability is
// already reported by the global doctor checks.
func defaultListChildrenAll(repoRoot, epicID string) ([]beads.Issue, error) {
	a := beads.New(repoRoot)
	if !a.Available() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.ListChildrenAll(ctx, epicID)
}

// --- note parsing -----------------------------------------------------------
//
// Both note formats are written by epicreview (Act's degraded note,
// engine.parkEpic's parked note) and parsed here via the shared codec in
// internal/epicreview/notes.go — the single source of truth for both
// Sprintf and regex, so the two can never drift out of byte-compatibility.

// parseDegradedNote scans the notes field (most-recent line first) and
// returns the round number and reason from the last matching
// "validation:degraded (round N): reason" line. Returns (0, "") when no
// matching line is found.
func parseDegradedNote(notes string) (round int, reason string) {
	lines := strings.Split(notes, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if n, r, ok := epicreview.ParseDegradedNote(strings.TrimSpace(lines[i])); ok {
			return n, r
		}
	}
	return 0, ""
}

// parseParkedNote scans the notes field (most-recent line first) and returns
// the round number and a short description from the last matching parked
// note line (canonical colon form or legacy space form). Returns (0, "")
// when no matching line is found.
func parseParkedNote(notes string) (round int, reason string) {
	lines := strings.Split(notes, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if n, maxRounds, ok := epicreview.ParseParkedNote(strings.TrimSpace(lines[i])); ok {
			return n, fmt.Sprintf("exceeded max_rounds=%d", maxRounds)
		}
	}
	return 0, ""
}
