// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"os"

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
		effort := st.Effort
		if effort == "" {
			effort = res.Effort
		}

		r.progress("bead %s: stage %q running (persona %s, model %s)", sl.PhaseID, st.Name, persona, res.Model)
		sr := stage.Run(ctx, stage.Opts{
			RepoRoot:         r.rec.Root,
			Worktree:         sl.Worktree,
			Branch:           sl.Branch,
			Base:             r.rec.DefaultBranch,
			Stage:            st.Name,
			Persona:          persona,
			Model:            res.Model,
			Effort:           effort,
			ExtraPrompt:      st.Prompt,
			BeadID:           sl.PhaseID,
			BeadTitle:        issue.Title,
			Profile:          r.profile,
			ExpectedIdentity: r.rec.ExpectedIdentity,
			Billing:          r.billing,
			APIKey:           r.apiKey,
			SSHAuthSock:      r.sshAuthSock,
			MaxBudgetUSD:     r.quotaCfg.PerAgentMaxUSD,
			PhaseDir:         phaseDir,
			ClaudeBin:        os.Getenv(envClaudeBin),
		})

		if sr.CostUSD > 0 {
			model, size, cost := res.Model, r.sizeClass(sl.PhaseID), sr.CostUSD
			if cfg, err := quota.UpdateConfig(r.quotaName(), func(c *quota.Config) error {
				quota.Record(c, model, size, cost)
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
