// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package epicreview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/project"
)

// BeadStore is the subset of bd verbs Act needs. *beads.Adapter satisfies it;
// the engine's WorkSource fakes opt in by implementing it (Act callers
// type-assert, so a store without these verbs degrades to "skip with reason"
// rather than a compile break).
type BeadStore interface {
	Show(ctx context.Context, id string) (beads.Issue, error)
	ListChildren(ctx context.Context, id string) ([]beads.Issue, error)
	Create(ctx context.Context, in beads.CreateInput) (string, error)
	Close(ctx context.Context, id, reason string) error
	AppendNotes(ctx context.Context, id, text string) error
	AddLabel(ctx context.Context, id, label string) error
	DepAdd(ctx context.Context, id, blockedBy string) error
}

// Labels and title fragments the act layer stamps. Shared between the CLI and
// the engine hook so the two callers can never drift.
const (
	LabelPassed     = "validation:passed"
	LabelParked     = "validation:parked"
	LabelDegraded   = "validation:degraded"
	LabelStructural = "validation:structural"
	LabelDocs       = "validation:docs"
	LabelNoValidate = "no-validate"

	docsBeadTitlePrefix = "docs: epic "
)

// RoundLabel returns the label stamped on gap follow-ups filed after round N.
func RoundLabel(nextRound int) string {
	return fmt.Sprintf("validation:round-%d", nextRound)
}

// DocsBeadTitle is the canonical title of the auto-filed docs-update bead —
// also the dedup key for "an open docs bead already exists".
func DocsBeadTitle(epicID string) string {
	return docsBeadTitlePrefix + epicID + " documentation update"
}

// ActOpts configures one deterministic verdict application.
type ActOpts struct {
	EpicID string
	Round  int // the round that produced the verdict

	// Config is the EFFECTIVE epic_validation config (defaults resolved).
	Config project.EpicValidationConfig

	// Actor names the caller in filed-bead provenance lines, e.g.
	// "koryph epic validate" or "engine epic-validation".
	Actor string

	// Progress, when non-nil, receives human-readable action lines.
	Progress func(format string, args ...any)
}

// ActResult reports what Act did.
type ActResult struct {
	// Outcome is one of: met-closed, met-pending-docs, gaps-filed, degraded.
	Outcome       string
	GapIDs        []string
	StructuralIDs []string
	DocsBeadID    string // non-empty when this call filed the docs bead
}

// Act applies a validation verdict to the bead graph, deterministically, per
// design §4/§4b (docs/designs/2026-07-epic-validation.md). It is the single
// implementation shared by `koryph epic validate` and the engine's in-loop
// hook — the validator proposes, Act disposes.
//
// Round-cap parking is deliberately NOT here: parking is a pre-validation
// decision (don't spend a frontier validator run past the cap), so the engine
// checks the cap before calling Validate; the explicit CLI never parks — an
// operator re-running past the cap is the recovery path.
func Act(ctx context.Context, bd BeadStore, o ActOpts, v Verdict) (ActResult, error) {
	progress := o.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	res := ActResult{}

	switch {
	case v.Degraded:
		res.Outcome = "degraded"
		note := FormatDegradedNote(o.Round, v.Reason)
		if err := bd.AppendNotes(ctx, o.EpicID, note); err != nil {
			progress("epic %s: append degraded note: %v", o.EpicID, err)
		}
		if err := bd.AddLabel(ctx, o.EpicID, LabelDegraded); err != nil {
			progress("epic %s: add degraded label: %v", o.EpicID, err)
		}
		progress("epic %s: validation degraded (round %d): %s", o.EpicID, o.Round, v.Reason)
		return res, nil

	case v.Met:
		res.StructuralIDs = fileStructural(ctx, bd, o, v.Structural, progress)

		docs := o.Config.EffectiveDocsUpdate()
		if *docs.Enabled {
			// §4b: label passed + file the docs bead; the epic closes when the
			// docs bead closes (the caller's completion check handles that —
			// it sees validation:passed and skips re-validation).
			if err := bd.AddLabel(ctx, o.EpicID, LabelPassed); err != nil {
				progress("epic %s: add %s label: %v", o.EpicID, LabelPassed, err)
			}
			id, filed, err := fileDocsBead(ctx, bd, o, docs.Labels)
			if err != nil {
				return res, fmt.Errorf("file docs bead for %s: %w", o.EpicID, err)
			}
			res.DocsBeadID = id
			note := buildMetNote(o.Round, v.Summary, len(v.Structural))
			if filed {
				note += fmt.Sprintf("\n\nDocs update bead filed: %s — epic closes when it merges.", id)
				progress("epic %s: validated (round %d) — docs bead %s filed; closing on its merge", o.EpicID, o.Round, id)
			} else {
				note += fmt.Sprintf("\n\nDocs update bead already open: %s.", id)
				progress("epic %s: validated (round %d) — docs bead %s already open", o.EpicID, o.Round, id)
			}
			if err := bd.AppendNotes(ctx, o.EpicID, note); err != nil {
				progress("epic %s: append met note: %v", o.EpicID, err)
			}
			res.Outcome = "met-pending-docs"
			return res, nil
		}

		// Docs stage disabled: close immediately per auto_close.
		note := buildMetNote(o.Round, v.Summary, len(v.Structural))
		if err := bd.AppendNotes(ctx, o.EpicID, note); err != nil {
			progress("epic %s: append met note: %v", o.EpicID, err)
		}
		if *o.Config.AutoClose {
			if err := bd.Close(ctx, o.EpicID, fmt.Sprintf("validated round %d", o.Round)); err != nil {
				return res, fmt.Errorf("close epic %s: %w", o.EpicID, err)
			}
			progress("epic %s: validated (round %d) and closed", o.EpicID, o.Round)
		} else {
			if err := bd.AddLabel(ctx, o.EpicID, LabelPassed); err != nil {
				progress("epic %s: add %s label: %v", o.EpicID, LabelPassed, err)
			}
			progress("epic %s: validated (round %d); auto_close=false — %s added", o.EpicID, o.Round, LabelPassed)
		}
		res.Outcome = "met-closed"
		return res, nil

	default: // gaps
		res.Outcome = "gaps-filed"
		res.StructuralIDs = fileStructural(ctx, bd, o, v.Structural, progress)
		gapIDs, err := fileGaps(ctx, bd, o, v.Gaps, progress)
		res.GapIDs = gapIDs
		if err != nil {
			return res, fmt.Errorf("file gap beads for %s: %w", o.EpicID, err)
		}
		note := buildGapNote(o.Round, v.Summary, v.Gaps, gapIDs)
		if err := bd.AppendNotes(ctx, o.EpicID, note); err != nil {
			progress("epic %s: append gap note: %v", o.EpicID, err)
		}
		progress("epic %s: round %d found %d gap(s); %d follow-up(s) filed",
			o.EpicID, o.Round, len(v.Gaps), len(gapIDs))
		return res, nil
	}
}

// fileDocsBead files the §4b docs-update child unless an open one already
// exists (dedup by canonical title OR the validation:docs label). Returns the
// bead id and whether this call created it.
func fileDocsBead(ctx context.Context, bd BeadStore, o ActOpts, labels []string) (string, bool, error) {
	children, err := bd.ListChildren(ctx, o.EpicID)
	if err != nil {
		return "", false, err
	}
	title := DocsBeadTitle(o.EpicID)
	for _, c := range children {
		if c.Status == "closed" || c.Status == "done" {
			continue
		}
		if c.Title == title || c.HasLabel(LabelDocs) {
			return c.ID, false, nil
		}
	}
	desc := fmt.Sprintf(
		"Auto-filed by %s after epic %s passed validation (round %d).\n\n"+
			"**Why:** validation proves the implementation is settled; documentation is "+
			"written against the settled state (design §4b, docs/designs/2026-07-epic-validation.md).\n\n"+
			"## Acceptance Criteria\n"+
			"- Review the epic's implementation: its design doc and every child's merge, "+
			"then update ALL documentation the epic's changes touch — user guide, concepts, "+
			"reference, README claims, anything now stale.\n"+
			"- Follow the project's existing documentation structure and conventions; this "+
			"bead prescribes the outcome, not the mechanism or renderer.\n"+
			"- Epic: %s — read `bd show %s` and `bd children %s` for the full delta.",
		o.Actor, o.EpicID, o.Round, o.EpicID, o.EpicID, o.EpicID)
	in := beads.CreateInput{
		Title:       title,
		Description: desc,
		Labels:      append([]string{LabelDocs}, labels...),
		IssueType:   "task",
		Parent:      o.EpicID,
	}
	id, err := bd.Create(ctx, in)
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// fileGaps creates a follow-up child per gap in two passes — create all, then
// wire ALL depends_on edges (sibling index references work regardless of
// order, unlike an at-create wiring where forward references would be lost).
// A title matching an existing child is skipped (dedup guard: the same gap
// re-reported across rounds must not pile up duplicates).
func fileGaps(ctx context.Context, bd BeadStore, o ActOpts, gaps []Gap, progress func(string, ...any)) ([]string, error) {
	existing := map[string]string{} // title → id
	if children, err := bd.ListChildren(ctx, o.EpicID); err == nil {
		for _, c := range children {
			existing[c.Title] = c.ID
		}
	}

	roundLabel := RoundLabel(o.Round + 1)
	ids := make([]string, len(gaps))
	var firstErr error
	for i, g := range gaps {
		if id, ok := existing[g.Title]; ok {
			progress("epic %s: gap %q already filed as %s — skipping duplicate", o.EpicID, g.Title, id)
			ids[i] = id
			continue
		}
		typ := g.Type
		if typ == "" {
			typ = "task"
		}
		desc := fmt.Sprintf(
			"Validation gap filed by %s (round %d, epic %s)\n\n"+
				"**Why:** %s\n\n## Acceptance Criteria\n%s",
			o.Actor, o.Round, o.EpicID, g.Why, g.Acceptance)
		id, err := bd.Create(ctx, beads.CreateInput{
			Title:       g.Title,
			Description: desc,
			Labels:      append([]string{roundLabel}, g.Labels...),
			IssueType:   typ,
			Parent:      o.EpicID,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("gap %d %q: %w", i, g.Title, err)
			}
			continue
		}
		ids[i] = id
	}

	// Second pass: wire every depends_on — sibling gap indices or bead ids.
	for i, g := range gaps {
		if ids[i] == "" {
			continue
		}
		for _, dep := range g.DependsOn {
			target := dep
			var idx int
			if n, _ := fmt.Sscan(dep, &idx); n == 1 && idx >= 0 && idx < len(gaps) && ids[idx] != "" {
				target = ids[idx]
			}
			if target == ids[i] {
				continue // self-reference
			}
			if err := bd.DepAdd(ctx, ids[i], target); err != nil {
				progress("epic %s: dep %s ← %s: %v", o.EpicID, ids[i], target, err)
			}
		}
	}

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out, firstErr
}

// fileStructural creates the structural follow-ups. Never fatal — structural
// findings never block met (§4).
func fileStructural(ctx context.Context, bd BeadStore, o ActOpts, structural []Structural, progress func(string, ...any)) []string {
	var ids []string
	for _, s := range structural {
		typ := s.Type
		if typ == "" {
			typ = "chore"
		}
		desc := fmt.Sprintf(
			"Structural finding surfaced by epic validation (epic %s, round %d, via %s)\n\n"+
				"**Category:** %s\n\n**Why:** %s\n\n## Acceptance Criteria\n%s",
			o.EpicID, o.Round, o.Actor, s.Category, s.Why, s.Acceptance)
		id, err := bd.Create(ctx, beads.CreateInput{
			Title:       s.Title,
			Description: desc,
			Labels:      append([]string{LabelStructural}, s.Labels...),
			IssueType:   typ,
			Parent:      o.Config.StructuralParent, // "" = standalone
		})
		if err != nil {
			progress("epic %s: file structural bead %q: %v", o.EpicID, s.Title, err)
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func buildMetNote(round int, summary string, structuralCount int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "validation round %d: met\n\n%s", round, summary)
	if structuralCount > 0 {
		fmt.Fprintf(&b, "\n\n%d structural finding(s) filed as follow-up bead(s).", structuralCount)
	}
	return b.String()
}

func buildGapNote(round int, summary string, gaps []Gap, gapIDs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "validation round %d: %d gap(s) found — follow-up beads filed\n\n%s\n\nFiled:\n",
		round, len(gaps), summary)
	for i, g := range gaps {
		id := ""
		if i < len(gapIDs) {
			id = " → " + gapIDs[i]
		}
		fmt.Fprintf(&b, "  - %s%s\n", g.Title, id)
	}
	return b.String()
}

// DetectNextRound reads outDir for verdict files matching
// <epicID>-round<N>.json and returns the highest N+1 (1 when none exist).
// Shared by the CLI and the engine hook.
func DetectNextRound(outDir, epicID string) int {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return 1
	}
	prefix := epicID + "-round"
	max := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		inner := name[len(prefix) : len(name)-len(".json")]
		var n int
		if _, serr := fmt.Sscan(inner, &n); serr == nil && n > max {
			max = n
		}
	}
	return max + 1
}

// LoadPriorVerdicts reads raw verdict JSON for rounds 1..(currentRound-1),
// earliest first. Shared by the CLI and the engine hook.
func LoadPriorVerdicts(outDir, epicID string, currentRound int) []string {
	var out []string
	rounds := make([]int, 0, currentRound-1)
	for r := 1; r < currentRound; r++ {
		rounds = append(rounds, r)
	}
	sort.Ints(rounds)
	for _, r := range rounds {
		path := filepath.Join(outDir, fmt.Sprintf("%s-round%d.json", epicID, r))
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		out = append(out, strings.TrimSpace(string(data)))
	}
	return out
}
