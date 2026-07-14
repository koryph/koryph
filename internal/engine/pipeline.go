// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/stage"
)

// runPipelineStages executes the project's configured post-implement stages
// (koryph-a14) sequentially in the slot's worktree — after the implementer's
// commits land and before review/merge. Each stage is a persona agent that may
// add its own commits (docs, tests, changelog, ...).
//
// It returns ok=false with the offending stage name when a REQUIRED stage
// fails; optional stages log-and-continue. An empty pipeline is a no-op that
// returns ok=true. Per-stage cost is folded into the governor and every stage
// is audited.
func (r *runner) runPipelineStages(ctx context.Context, sl *ledger.Slot) (ok bool, failed string) {
	if len(r.cfg.Pipeline) == 0 {
		return true, ""
	}
	issue := r.issueFor(ctx, sl)
	phaseDir := r.store.PhaseDir(r.run.RunID, sl.PhaseID)

	for _, st := range r.cfg.Pipeline {
		// Resolve the tier through modelroute so the project allowlist and any
		// model:<stage>:<tier> label are honored (fail closed on a bad tier).
		res, err := modelroute.Resolve(modelroute.Req{
			Stage:         st.Name,
			Labels:        issue.Labels,
			RunDefault:    r.opts.DefaultModel,
			ExplicitModel: st.Model,
			AllowedModels: r.rec.AllowedModels,
			Stages:        r.cfg.Stages,
		})
		if err != nil {
			r.progress("bead %s: stage %q model resolution failed: %v", sl.PhaseID, st.Name, err)
			if st.Optional {
				continue
			}
			return false, st.Name
		}
		persona := res.Persona
		if st.Persona != "" {
			persona = st.Persona
		}
		// Effort precedence: explicit stage config (project.json pipeline[].effort)
		// > res.Effort (currently always empty — Resolve never populates it; kept
		// for forward compat) > the stage persona's own frontmatter `effort:`
		// hint, resolved the same way wave.go's main-dispatch path already does
		// (koryph-77r.8 audit finding: this persona fallback was missing here, so
		// a stage persona's declared effort — e.g. koryph-feature-docs-author's
		// `effort: low` — was silently never applied to pipeline-stage spawns).
		effort := st.Effort
		if effort == "" {
			effort = res.Effort
		}
		if effort == "" {
			if _, metaEffort, _, err := modelroute.PersonaMeta(r.rec.Root, persona); err == nil {
				effort = metaEffort
			}
		}

		r.progress("bead %s: stage %q running (persona %s, model %s)", sl.PhaseID, st.Name, persona, res.Model)
		stageStart := time.Now()
		sr := stage.Run(ctx, stage.Opts{
			RepoRoot:         r.rec.Root,
			Worktree:         sl.Worktree,
			Branch:           sl.Branch,
			Base:             r.rec.DefaultBranch,
			Stage:            st.Name,
			Persona:          persona,
			Model:            res.Model,
			Effort:           effort,
			TimeoutSec:       st.TimeoutSec, // <=0 → stage.DefaultTimeoutSec (koryph-a59)
			ExtraPrompt:      st.Prompt,
			BeadID:           sl.PhaseID,
			BeadTitle:        issue.Title,
			Profile:          r.profile,
			ExpectedIdentity: r.expectedIdentity,
			Billing:          r.billing,
			APIKey:           r.apiKey,
			SSHAuthSock:      r.sshAuthSock,
			MaxBudgetUSD:     r.quotaCfg.PerAgentMaxUSD,
			PhaseDir:         phaseDir,
			ClaudeBin:        os.Getenv(envClaudeBin),
			// Follows sl's already-assigned holdout arm rather than the
			// project's live config, so a stage spawned for a holdout-arm
			// bead stays direct too (koryph-3l1.3) — see
			// proxyBaseURLForSlot's doc (poll.go).
			ProxyBaseURL: r.proxyBaseURLForSlot(sl),
		})

		// Emit stage duration for histograms (Section O2).
		{
			var stageErr error
			if !sr.OK {
				stageErr = fmt.Errorf("%s", sr.Note)
			}
			logStageDuration(sl.PhaseID, st.Name, time.Since(stageStart).Milliseconds(), stageErr)
		}
		if sr.CostUSD > 0 {
			model, size, cost := res.Model, r.sizeClass(sl.PhaseID), sr.CostUSD
			if cfg, err := quota.UpdateConfig(r.quotaName(), func(c *quota.Config) error {
				// Pipeline-stage dispatches don't have a pre-stamped estimate
				// (they are launched by the post-implement pipeline, not the
				// main wave estimator), so pass 0 to skip error-stat updates
				// while still updating the base EWMA calibration (koryph-6bl).
				// sl.ProxyID segments by the bead's assigned arm, same
				// reasoning as completeSlot's RecordForProxy call
				// (koryph-3l1.3).
				quota.RecordForProxy(c, model, size, sl.ProxyID, cost, 0)
				return nil
			}); err == nil {
				r.quotaCfg = cfg
			}
		}
		_ = r.reg.Audit(registry.Event{
			Kind:      "stage",
			ProjectID: r.opts.ProjectID,
			Actor:     r.owner,
			Detail: map[string]string{
				"bead":    sl.PhaseID,
				"stage":   st.Name,
				"persona": persona,
				"model":   res.Model,
				"ok":      fmt.Sprintf("%t", sr.OK),
			},
		})

		if !sr.OK {
			// A timeout is not a failure (koryph-a59): the stage ran out of time,
			// not correctness — surface it distinctly so a complete-but-slow stage
			// is not mistaken for broken work, and point the operator at the lever
			// (its timeout_sec) rather than a phantom failure.
			if sr.TimedOut {
				effTimeout := st.TimeoutSec
				if effTimeout <= 0 {
					effTimeout = stage.DefaultTimeoutSec
				}
				if st.Optional {
					r.progress("bead %s: optional stage %q timed out after %ds — continuing (raise its timeout_sec)", sl.PhaseID, st.Name, effTimeout)
					continue
				}
				r.progress("bead %s: stage %q timed out after %ds — not a failure; raise its timeout_sec", sl.PhaseID, st.Name, effTimeout)
				return false, fmt.Sprintf("%s timed out after %ds (raise its timeout_sec)", st.Name, effTimeout)
			}
			if st.Optional {
				r.progress("bead %s: optional stage %q failed (%s) — continuing", sl.PhaseID, st.Name, sr.Note)
				continue
			}
			r.progress("bead %s: stage %q failed (%s)", sl.PhaseID, st.Name, sr.Note)
			return false, st.Name
		}

		// A stage may have committed: refresh the slot's commit count so its
		// work counts toward the candidate (and the review diff).
		if commits, head, perr := r.branchProgress(ctx, sl.Worktree); perr == nil {
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
				s.Commits = commits
				s.LastCommit = head
			})
		}
	}
	return true, ""
}
