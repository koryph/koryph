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
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/runtimeconfig"
)

func init() {
	registerCmd(command{
		name:    "epic",
		summary: "on-demand epic validation: completeness + structural health review",
		run:     cmdEpic,
		DocLinks: []string{
			"user-guide/epic-validation.md",
			"concepts/beads.md",
		},
		subs: []command{
			{
				name:    "validate",
				summary: "on-demand epic validation: completeness + structural health review",
				run:     cmdEpic,
				// koryph-b8g #24: 'epic' is a single-child noun group;
				// flattened so 'epic <epic-id> ...' is the primary form. The
				// two-word 'epic validate <epic-id> ...' still works —
				// hidden so it doesn't clutter help/completion/docgen.
				hidden:   true,
				DocLinks: []string{"user-guide/epic-validation.md"},
			},
		},
	})
}

// cmdEpic implements `koryph epic <epic-id> --project ID [--round N]
// [--json]`.
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
// Works with the loop stopped; the enabled flag does not gate the explicit
// command. koryph-b8g #24: 'epic' was a single-child noun group ('epic
// validate <epic-id> ...'); flattened so the epic id is a direct argument.
// The two-word 'epic validate <epic-id> ...' form still works as a hidden
// alias.
func cmdEpic(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "validate" {
		args = args[1:]
	}
	fs := newFlagSet("epic", stderr)
	projectID := fs.String("project", "", "project id (default: the project containing the current directory)")
	round := fs.Int("round", 0, "validation round override (0 = auto-detect from prior verdict files)")
	asJSON := fs.Bool("json", false, "emit the raw verdict JSON; actions still apply")
	setUsage(fs, stdout,
		"on-demand epic validation: completeness + structural health review",
		"<epic-id> [--project ID] [--round N] [--json]")

	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) < 1 {
		return usageErr(stderr, "epic: <epic-id> is required")
	}
	epicID := pos[0]

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, code := resolveProjectRecordCwd(stderr, store, *projectID, "epic")
	if code != 0 {
		return code
	}

	// Load project config for epic_validation defaults (nil config is fine; all
	// fields fall back to their documented defaults via Effective()).
	cfg, _ := project.Load(rec.Root)
	if cfg == nil {
		cfg = project.Default(rec.ProjectID)
	}
	evcfg := epicValidationConfig(cfg)

	// Open the beads adapter.
	bd := beads.New(rec.Root)
	if bin := os.Getenv("KORYPH_BD_BIN"); bin != "" {
		bd.Bin = bin
	}

	// Verify the target is an epic.
	epic, err := bd.Show(ctx, epicID)
	if err != nil {
		fmt.Fprintf(stderr, "epic: cannot show %s: %v\n", epicID, err)
		return engine.ExitFatal
	}
	if epic.IssueType != "epic" {
		fmt.Fprintf(stderr, "epic: %s is type %q, not epic\n", epicID, epic.IssueType)
		return engine.ExitFatal
	}

	// Require all children to be closed.
	children, err := bd.ListChildrenAll(ctx, epicID)
	if err != nil {
		fmt.Fprintf(stderr, "epic: cannot list children of %s: %v\n", epicID, err)
		return engine.ExitFatal
	}
	var openKids []string
	for _, c := range children {
		if c.Status != "closed" && c.Status != "done" {
			openKids = append(openKids, c.ID)
		}
	}
	if len(openKids) > 0 {
		fmt.Fprintf(stderr, "epic: epic %s has unclosed children: %s\n",
			epicID, strings.Join(openKids, ", "))
		return engine.ExitFatal
	}

	// Validation already passed → the docs bead was the last child: close
	// WITHOUT spawning another validator round (§4b). Shares the exact check
	// the engine's in-loop hook applies (internal/engine/epicvalidate.go's
	// maybeStartEpicValidation) via internal/epicreview.ClosedAfterDocs, so
	// the two callers can never drift. Without this, doctor/health-patrol's
	// documented recovery command for a stalled close-after-docs epic —
	// `koryph epic validate <id>` — would instead burn a full validator round
	// on an epic that has nothing left to validate (koryph-4b50 BUG-1).
	if epicreview.ClosedAfterDocs(epic, children) {
		if err := bd.Close(ctx, epicID, "validated; docs update merged"); err != nil {
			fmt.Fprintf(stderr, "epic: close after docs merge: %v\n", err)
			return engine.ExitFatal
		}
		msg := fmt.Sprintf("epic %s: docs update merged — epic closed\n", epicID)
		if *asJSON {
			fmt.Fprintln(stdout, `{"met":true,"degraded":false,"reason":"validation already passed; docs update merged"}`)
			fmt.Fprint(stderr, msg)
		} else {
			fmt.Fprint(stdout, msg)
		}
		return 0
	}

	// Determine round number (auto-detect from existing verdict files).
	outDir := filepath.Join(rec.Root, ".koryph", "epic-reviews")
	validationRound := *round
	if validationRound <= 0 {
		validationRound = epicreview.DetectNextRound(outDir, epicID)
	}

	// Load prior verdicts (raw JSON) for round context in the validator prompt.
	priorVerdicts := epicreview.LoadPriorVerdicts(outDir, epicID, validationRound)

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

	// Resolve the project's active runtime and its account. Explicit CLI
	// validation must behave like the engine loop: never silently substitute
	// Claude for a Codex-configured project.
	runtimeName, _ := modelroute.ResolveRuntimeName(nil, cfg.DefaultRuntime)
	rt, ok := runtimeconfig.Get(runtimeName)
	if !ok {
		return fail(stderr, fmt.Errorf("epic: runtime %q is not registered", runtimeName))
	}
	if runtimeName != "claude" && !cfg.Runtimes[runtimeName].Enabled {
		return fail(stderr, fmt.Errorf("epic: runtime %q is not enabled in koryph.project.json", runtimeName))
	}
	ra := rec.AccountFor(runtimeName)
	profile := account.Profile{Name: rec.AccountProfile, ConfigDir: ra.ConfigDir}

	// Build validator opts from config defaults + caller overrides.
	// Same koryph-77r.8 wiring as the in-loop caller (internal/engine/
	// epicvalidate.go): honor the validator persona's declared frontmatter
	// effort instead of silently dropping it.
	validateEffort := evcfg.Effort
	if validateEffort == "" {
		if _, metaEffort, _, err := modelroute.PersonaMeta(rec.Root, evcfg.Persona); err == nil {
			validateEffort = metaEffort
		}
	}
	modelMap := cfg.ModelMap
	effortMap := map[string]string(nil)
	if runtimeCfg, configured := cfg.Runtimes[runtimeName]; configured {
		modelMap = mergeModelMaps(modelMap, runtimeCfg.ModelMap)
		effortMap = runtimeCfg.EffortMap
	}
	// Effective() fills the historical Claude default "opus". Only a value
	// explicitly present in the project file is a concrete model selection;
	// otherwise the selected runtime's frontier mapping decides the model.
	explicitModel := ""
	if cfg.EpicValidation != nil {
		explicitModel = cfg.EpicValidation.Model
	}
	resolvedModel, err := modelroute.Resolve(modelroute.Req{
		Stage:         modelroute.StageReview,
		Labels:        epic.Labels,
		ExplicitModel: explicitModel,
		AllowedModels: rec.AllowedModels,
		Stages:        cfg.Stages,
		RepoRoot:      rec.Root,
		ModelMap:      modelMap,
		EffortMap:     effortMap,
		Runtime:       runtimeName,
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("epic: validator model resolution: %w", err))
	}
	if validateEffort == "" {
		validateEffort = resolvedModel.Effort
	}
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
		Model:           resolvedModel.Model,
		Effort:          validateEffort,
		Runtime:         rt,
		TimeoutSec:      evcfg.TimeoutSeconds,
		OutDir:          outDir,
		ProxyBaseURL:    rec.ProxyBaseURL(),
		// Progress routes to stderr in --json mode so stdout stays pure JSON.
		Progress: func(format string, args ...any) {
			if *asJSON {
				fmt.Fprintf(stderr, format+"\n", args...)
			} else {
				fmt.Fprintf(stdout, format+"\n", args...)
			}
		},
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

	// Act on the verdict deterministically per design §4/§4b — shared act
	// layer (internal/epicreview.Act), the same implementation the engine's
	// in-loop hook uses, so the two callers can never drift.
	progress := func(format string, args ...any) {
		if !*asJSON {
			fmt.Fprintf(stdout, format+"\n", args...)
		}
	}
	res, actErr := epicreview.Act(ctx, bd, epicreview.ActOpts{
		EpicID:   epicID,
		Round:    validationRound,
		Config:   evcfg,
		Actor:    "koryph epic",
		Progress: progress,
	}, verdict)
	if actErr != nil {
		fmt.Fprintf(stderr, "epic: %v\n", actErr)
		return engine.ExitFatal
	}
	if res.Outcome == "degraded" {
		return engine.ExitFatal
	}
	return 0
}

func mergeModelMaps(base, override map[string]string) map[string]string {
	if len(override) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range override {
		out[key] = value
	}
	return out
}

// epicValidationConfig returns the effective EpicValidationConfig for cfg, applying
// documented defaults. It is safe to call with a nil cfg.
func epicValidationConfig(cfg *project.Config) project.EpicValidationConfig {
	if cfg != nil {
		return cfg.EffectiveEpicValidation()
	}
	return (*project.EpicValidationConfig)(nil).Effective()
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
