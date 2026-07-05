// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/epicreview"
	"github.com/koryph/koryph/internal/project"
)

func init() {
	registerCmd(command{
		name:    "epic",
		summary: "epic lifecycle management (validate, …)",
		run:     cmdEpic,
		DocLinks: []string{
			"user-guide/epic-validation.md",
			"concepts/beads.md",
		},
		subs: []command{
			{
				name:     "validate",
				summary:  "on-demand epic validation: completeness + structural health review",
				run:      cmdEpicValidate,
				DocLinks: []string{"user-guide/epic-validation.md"},
			},
		},
	})
}

// cmdEpic dispatches the epic sub-verbs.
func cmdEpic(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "epic", "epic lifecycle management", []subVerb{
			{
				"validate <epic-id> --project ID [--round N] [--json]",
				"on-demand epic validation (completeness + structural health); backfill or recovery path",
			},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "validate":
		return cmdEpicValidate(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown epic subcommand %q", sub))
	}
}

// cmdEpicValidate implements `koryph epic validate <epic-id>`.
//
// It loads the project epic_validation config block for defaults, verifies the
// target is an epic whose children are all closed, runs internal/epicreview.Validate,
// then acts on the verdict per design §4:
//
//   - met     → note + close epic (respecting auto_close)
//   - gaps    → file round-labeled child beads with depends_on edges, note on epic
//   - structural → standalone beads labeled validation:structural
//   - degraded → note + validation:degraded label + nonzero exit
//
// The --json flag emits the raw verdict JSON instead of human-readable output.
// Works with the loop stopped; the enabled flag does not gate the explicit command.
func cmdEpicValidate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("epic validate", stderr)
	projectID := fs.String("project", "", "project id (required)")
	round := fs.Int("round", 0, "validation round override (0 = auto-detect from prior verdict files)")
	asJSON := fs.Bool("json", false, "emit the raw verdict JSON; actions still apply")
	setUsage(fs, stdout,
		"on-demand epic validation: completeness + structural health review",
		"<epic-id> --project ID [--round N] [--json]")

	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) < 1 {
		return usageErr(stderr, "epic validate: <epic-id> is required")
	}
	epicID := pos[0]
	if *projectID == "" {
		return usageErr(stderr, "epic validate: --project is required")
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, err := store.Get(*projectID)
	if err != nil {
		return fail(stderr, err)
	}

	// Load project config for epic_validation defaults (nil config is fine; all
	// fields fall back to their documented defaults via Effective()).
	cfg, _ := project.Load(rec.Root)
	evcfg := epicValidationConfig(cfg)

	// Open the beads adapter.
	bd := beads.New(rec.Root)
	if bin := os.Getenv("KORYPH_BD_BIN"); bin != "" {
		bd.Bin = bin
	}

	// Verify the target is an epic.
	epic, err := bd.Show(ctx, epicID)
	if err != nil {
		fmt.Fprintf(stderr, "epic validate: cannot show %s: %v\n", epicID, err)
		return engine.ExitFatal
	}
	if epic.IssueType != "epic" {
		fmt.Fprintf(stderr, "epic validate: %s is type %q, not epic\n", epicID, epic.IssueType)
		return engine.ExitFatal
	}

	// Require all children to be closed.
	children, err := bd.ListChildren(ctx, epicID)
	if err != nil {
		fmt.Fprintf(stderr, "epic validate: cannot list children of %s: %v\n", epicID, err)
		return engine.ExitFatal
	}
	var openKids []string
	for _, c := range children {
		if c.Status != "closed" && c.Status != "done" {
			openKids = append(openKids, c.ID)
		}
	}
	if len(openKids) > 0 {
		fmt.Fprintf(stderr, "epic validate: epic %s has unclosed children: %s\n",
			epicID, strings.Join(openKids, ", "))
		return engine.ExitFatal
	}

	// Determine round number (auto-detect from existing verdict files).
	outDir := filepath.Join(rec.Root, ".koryph", "epic-reviews")
	validationRound := *round
	if validationRound <= 0 {
		validationRound = detectNextRound(outDir, epicID)
	}

	// Load prior verdicts (raw JSON) for round context in the validator prompt.
	priorVerdicts := loadPriorVerdicts(outDir, epicID, validationRound)

	// Build epicreview children list.
	erChildren := make([]epicreview.Child, 0, len(children))
	for _, c := range children {
		erChildren = append(erChildren, epicreview.Child{
			ID:          c.ID,
			Title:       c.Title,
			Description: c.Description,
			CloseReason: c.Status,
			Labels:      c.Labels,
		})
	}

	// Resolve account profile (falls back to flat AccountProfile/ClaudeConfigDir
	// fields for projects predating runtime_accounts — every project today).
	ra := rec.AccountFor("claude")
	profile := account.Profile{Name: rec.AccountProfile, ConfigDir: ra.ConfigDir}

	// Build validator opts from config defaults + caller overrides.
	opts := epicreview.Opts{
		EpicID:          epicID,
		EpicTitle:       epic.Title,
		EpicDescription: epic.Description,
		EpicNotes:       epic.Notes,
		Children:        erChildren,
		PriorVerdicts:   priorVerdicts,
		Round:           validationRound,
		RepoRoot:        rec.Root,
		Profile:         profile,
		Persona:         evcfg.Persona,
		Model:           evcfg.Model,
		TimeoutSec:      evcfg.TimeoutSeconds,
		OutDir:          outDir,
	}
	if bin := os.Getenv("KORYPH_CLAUDE_BIN"); bin != "" {
		opts.ClaudeBin = bin
	}

	// Run the validator.
	verdict := epicreview.Validate(ctx, opts)

	// Emit JSON output when requested (before actions so the caller sees the
	// verdict even when an action step fails).
	if *asJSON {
		raw := verdict.Raw
		if raw == "" {
			// Degraded: emit a minimal JSON object describing the failure.
			data, _ := marshalVerdictJSON(verdict)
			fmt.Fprintln(stdout, string(data))
		} else {
			fmt.Fprintln(stdout, raw)
		}
	}

	// Act on the verdict deterministically per design §4.
	return applyVerdict(ctx, bd, epicID, validationRound, evcfg, verdict, stdout, stderr, *asJSON)
}

// epicValidationConfig returns the effective EpicValidationConfig for cfg, applying
// documented defaults. It is safe to call with a nil cfg.
func epicValidationConfig(cfg *project.Config) project.EpicValidationConfig {
	if cfg != nil {
		return cfg.EffectiveEpicValidation()
	}
	return (*project.EpicValidationConfig)(nil).Effective()
}

// applyVerdict implements the deterministic verdict-handling rules from design §4.
// quiet suppresses human-readable lines when --json was requested.
func applyVerdict(
	ctx context.Context,
	bd *beads.Adapter,
	epicID string,
	round int,
	evcfg project.EpicValidationConfig,
	v epicreview.Verdict,
	stdout, stderr io.Writer,
	quiet bool,
) int {
	switch {
	case v.Degraded:
		// Note + label + nonzero exit. Never fail the loop; the engine marks
		// validation:parked after max_rounds but the CLI just exits non-zero.
		note := fmt.Sprintf("validation:degraded (round %d): %s", round, v.Reason)
		if err := bd.AppendNotes(ctx, epicID, note); err != nil {
			fmt.Fprintf(stderr, "epic validate: append degraded note: %v\n", err)
		}
		if err := bd.AddLabel(ctx, epicID, "validation:degraded"); err != nil {
			fmt.Fprintf(stderr, "epic validate: add degraded label: %v\n", err)
		}
		if !quiet {
			fmt.Fprintf(stdout, "validation degraded (round %d): %s\n", round, v.Reason)
		}
		return engine.ExitFatal

	case v.Met:
		// File structural findings (they never affect met).
		if err := fileStructuralBeads(ctx, bd, epicID, round, v.Structural, evcfg.StructuralParent, stderr); err != nil {
			fmt.Fprintf(stderr, "epic validate: file structural beads: %v\n", err)
		}
		// Append summary note.
		noteText := buildMetNote(round, v.Summary, len(v.Structural))
		if err := bd.AppendNotes(ctx, epicID, noteText); err != nil {
			fmt.Fprintf(stderr, "epic validate: append met note: %v\n", err)
		}
		// Close or label depending on auto_close.
		if *evcfg.AutoClose {
			reason := fmt.Sprintf("validated round %d", round)
			if err := bd.Close(ctx, epicID, reason); err != nil {
				fmt.Fprintf(stderr, "epic validate: close epic: %v\n", err)
				return engine.ExitFatal
			}
			if !quiet {
				fmt.Fprintf(stdout, "epic %s validated (round %d) and closed\n", epicID, round)
				fmt.Fprintf(stdout, "%s\n", v.Summary)
			}
		} else {
			if err := bd.AddLabel(ctx, epicID, "validation:passed"); err != nil {
				fmt.Fprintf(stderr, "epic validate: add validation:passed label: %v\n", err)
			}
			if !quiet {
				fmt.Fprintf(stdout, "epic %s validated (round %d); auto_close=false — label validation:passed added\n",
					epicID, round)
			}
		}
		return 0

	default: // met=false — gaps found
		// File structural findings (independent of gap closure).
		if err := fileStructuralBeads(ctx, bd, epicID, round, v.Structural, evcfg.StructuralParent, stderr); err != nil {
			fmt.Fprintf(stderr, "epic validate: file structural beads: %v\n", err)
		}
		// File gap follow-up child beads.
		gapIDs, err := fileGapBeads(ctx, bd, epicID, round, v.Gaps, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "epic validate: file gap beads: %v\n", err)
			return engine.ExitFatal
		}
		// Append gap note on the epic.
		noteText := buildGapNote(round, v.Summary, v.Gaps, gapIDs)
		if err := bd.AppendNotes(ctx, epicID, noteText); err != nil {
			fmt.Fprintf(stderr, "epic validate: append gap note: %v\n", err)
		}
		if !quiet {
			fmt.Fprintf(stdout, "epic %s: validation round %d found %d gap(s); %d follow-up bead(s) filed\n",
				epicID, round, len(v.Gaps), len(gapIDs))
		}
		return 0
	}
}

// fileGapBeads creates a child bead of epicID for each gap, labeled with the
// next-round validation label and wired with depends_on edges. Returns the
// list of created bead IDs in the same order as gaps. The dedup guard (title
// match) is deliberately lightweight here — the engine's full guard runs
// during loop dispatch.
func fileGapBeads(
	ctx context.Context,
	bd *beads.Adapter,
	epicID string,
	round int,
	gaps []epicreview.Gap,
	stderr io.Writer,
) ([]string, error) {
	roundLabel := fmt.Sprintf("validation:round-%d", round+1)
	ids := make([]string, 0, len(gaps))
	// indexToID maps 0-based gap index → created bead id for sibling depends_on.
	indexToID := make(map[int]string, len(gaps))

	for i, g := range gaps {
		labels := append([]string{roundLabel}, g.Labels...)
		desc := fmt.Sprintf(
			"Validation gap filed by `koryph epic validate` (round %d, epic %s)\n\n"+
				"**Why:** %s\n\n**Acceptance:** %s",
			round, epicID, g.Why, g.Acceptance)

		in := beads.CreateInput{
			Title:       g.Title,
			Description: desc,
			Labels:      labels,
			IssueType:   g.Type,
			Parent:      epicID,
		}

		// Resolve depends_on: entries are either 0-based sibling gap indices or
		// existing bead IDs. We process gaps in order so a sibling created
		// earlier in this loop has its ID available; a forward reference (index
		// pointing to a later gap) is skipped because the target isn't created
		// yet — the engine rewires such edges during its own filing pass.
		for _, dep := range g.DependsOn {
			var idx int
			if n, _ := fmt.Sscan(dep, &idx); n == 1 && idx >= 0 && idx < len(gaps) {
				if sibID, ok := indexToID[idx]; ok {
					in.DependsOn = append(in.DependsOn, sibID)
				}
				// forward refs silently dropped (see comment above)
			} else {
				// treat as an existing bead id
				in.DependsOn = append(in.DependsOn, dep)
			}
		}

		id, err := bd.Create(ctx, in)
		if err != nil {
			return ids, fmt.Errorf("gap %d %q: %w", i, g.Title, err)
		}
		indexToID[i] = id
		ids = append(ids, id)
	}
	return ids, nil
}

// fileStructuralBeads creates standalone structural follow-up beads (not children
// of the epic). When structuralParent is non-empty they are parented there
// (e.g. a standing code-quality epic); otherwise they stand alone.
func fileStructuralBeads(
	ctx context.Context,
	bd *beads.Adapter,
	sourceEpicID string,
	round int,
	structural []epicreview.Structural,
	structuralParent string,
	stderr io.Writer,
) error {
	for _, s := range structural {
		labels := append([]string{"validation:structural"}, s.Labels...)
		desc := fmt.Sprintf(
			"Structural finding surfaced by epic validation (epic %s, round %d)\n\n"+
				"**Category:** %s\n\n**Why:** %s\n\n**Acceptance:** %s",
			sourceEpicID, round, s.Category, s.Why, s.Acceptance)
		in := beads.CreateInput{
			Title:       s.Title,
			Description: desc,
			Labels:      labels,
			IssueType:   s.Type,
			Parent:      structuralParent, // "" = standalone
		}
		if _, err := bd.Create(ctx, in); err != nil {
			// Non-fatal: log and continue — structural beads never block met.
			fmt.Fprintf(stderr, "epic validate: file structural bead %q: %v\n", s.Title, err)
		}
	}
	return nil
}

// buildMetNote constructs the note appended to the epic when validation passes.
func buildMetNote(round int, summary string, structuralCount int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "validation round %d: met\n\n%s", round, summary)
	if structuralCount > 0 {
		fmt.Fprintf(&b, "\n\n%d structural finding(s) filed as standalone follow-up bead(s).", structuralCount)
	}
	return b.String()
}

// buildGapNote constructs the note appended to the epic when gaps are filed.
func buildGapNote(round int, summary string, gaps []epicreview.Gap, gapIDs []string) string {
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

// detectNextRound reads outDir for verdict files matching <epicID>-round<N>.json
// and returns the highest N+1. Falls back to round 1 when no prior files exist.
func detectNextRound(outDir, epicID string) int {
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

// loadPriorVerdicts reads raw JSON for rounds 1..(currentRound-1) from outDir.
func loadPriorVerdicts(outDir, epicID string, currentRound int) []string {
	var out []string
	for r := 1; r < currentRound; r++ {
		path := filepath.Join(outDir, fmt.Sprintf("%s-round%d.json", epicID, r))
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		out = append(out, strings.TrimSpace(string(data)))
	}
	return out
}

// marshalVerdictJSON encodes a (possibly degraded) verdict as compact JSON for
// the --json fallback when verdict.Raw is empty.
func marshalVerdictJSON(v epicreview.Verdict) ([]byte, error) {
	type wire struct {
		Met      bool   `json:"met"`
		Degraded bool   `json:"degraded"`
		Reason   string `json:"reason,omitempty"`
		Attempts int    `json:"attempts,omitempty"`
	}
	w := wire{Met: v.Met, Degraded: v.Degraded, Reason: v.Reason, Attempts: v.Attempts}
	var b strings.Builder
	enc := func() {
		fmt.Fprintf(&b, `{"met":%v,"degraded":%v`, w.Met, w.Degraded)
		if w.Reason != "" {
			fmt.Fprintf(&b, `,"reason":%q`, w.Reason)
		}
		if w.Attempts > 0 {
			fmt.Fprintf(&b, `,"attempts":%d`, w.Attempts)
		}
		b.WriteByte('}')
	}
	enc()
	return []byte(b.String()), nil
}
