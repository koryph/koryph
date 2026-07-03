// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/promptc"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/sched"
	"github.com/koryph/koryph/internal/worktree"
)

// loop runs waves until drained, quota pause, ctx cancellation, or (Once)
// exactly one wave has fully settled.
func (r *runner) loop(ctx context.Context) (Outcome, error) {
	for {
		if ctx.Err() != nil {
			return r.interrupted()
		}
		r.run.Wave++
		_ = r.store.SaveRun(r.run)

		// Governor.
		level, calibrated, usage := r.governor(ctx)
		advisory, advisoryWhy := r.guardMode(calibrated)
		if advisory {
			// Advisory: measure + log, never block, never switch billing.
			r.billing, r.apiKey = account.BillingSubscription, ""
			if level != quota.LevelOK {
				r.progress("billing guard ADVISORY (%s): governor level %s — not blocking", advisoryWhy, level)
			}
		} else {
			r.billing, r.apiKey = r.billingFor(level)
		}

		allowDispatch := true
		if !r.opts.Manual && !advisory {
			switch level {
			case quota.LevelStop:
				if r.billing != account.BillingAPIKey {
					if r.activeCount() == 0 {
						r.run.Status = ledger.RunPausedQuota
						_ = r.store.SaveRun(r.run)
						r.progress("governor stop: run %s paused (quota)", r.run.RunID)
						return r.outcome(ExitOK, "quota-stop", false), nil
					}
					allowDispatch = false
					r.progress("governor stop: no new dispatch; waiting on %d active slot(s)", r.activeCount())
				}
			case quota.LevelDrain:
				allowDispatch = false
				r.progress("governor drain: no new dispatch; finishing %d active slot(s)", r.activeCount())
			case quota.LevelWarn:
				r.progress("governor warn: usage past %.0f%% of the window ceiling", quota.WarnFraction*100)
			}
		}

		width := r.width
		if !r.opts.Manual && calibrated && !advisory {
			if scaled := quota.ScaleSlots(usage, width); scaled < width {
				width = scaled
			}
		}

		// Per-run budget ceiling: once cumulative run cost reaches the cap, stop
		// starting new agents; any active slots still finish.
		budgetHit := false
		if r.opts.BudgetUSD > 0 {
			if spent := r.runCostUSD(); spent >= r.opts.BudgetUSD {
				budgetHit = true
				if allowDispatch {
					r.progress("run budget reached: $%.2f spent >= $%.2f cap — no new dispatch", spent, r.opts.BudgetUSD)
				}
				allowDispatch = false
			}
		}

		// Frontier scan + wave build.
		issues, err := r.adapter.Ready(ctx, beads.ReadyOpts{Parent: r.opts.Parent})
		if err != nil {
			return r.outcome(ExitFatal, "bd ready failed", false), fmt.Errorf("engine: bd ready: %w", err)
		}
		// --only narrows the frontier to a single operator-chosen bead; once it
		// closes it drops out of `bd ready` and the run drains.
		if r.opts.Only != "" {
			issues = onlyBead(issues, r.opts.Only)
		}
		active := r.activeIDs()
		w, err := sched.BuildWave(ctx, issues, r.cfg, sched.Opts{
			Max:          width,
			DefaultModel: r.opts.DefaultModel,
			Parent:       r.opts.Parent,
			ActiveIDs:    active,
		}, r.childLister(ctx))
		if err != nil {
			return r.outcome(ExitFatal, "wave build failed", false), fmt.Errorf("engine: build wave: %w", err)
		}

		eligible := 0
		for _, iss := range issues {
			if ok, _ := sched.Eligible(iss, active); ok {
				eligible++
			}
		}

		// Drained: nothing eligible, nothing active, nothing batched.
		if eligible == 0 && len(active) == 0 && len(w.Items) == 0 {
			r.run.Status = ledger.RunDrained
			_ = r.store.FinalizeRun(r.run)
			r.dropDemand() // withdraw from the fair-share denominator
			r.progress("drained: no ready work, no active slots")
			return r.outcome(ExitDrained, "drained", true), nil
		}

		// Gate with nothing active to finish: pause rather than spin. The
		// per-run budget cap and the quota governor both land here.
		if !allowDispatch && len(active) == 0 {
			reason := "quota-" + string(level)
			if budgetHit {
				reason = "budget-cap"
			}
			r.run.Status = ledger.RunPausedQuota
			_ = r.store.SaveRun(r.run)
			return r.outcome(ExitOK, reason, false), nil
		}

		// Preflight (loop mode only, calibrated + enforcing governor only).
		est := r.waveEstimate(w.Items)
		if allowDispatch && !r.opts.NoPreflight && !r.opts.Manual && calibrated && !advisory && len(w.Items) > 0 {
			if ok, reason := quota.Preflight(usage, est, r.quotaCfg); !ok {
				allowDispatch = false
				r.progress("preflight refused wave: %s", reason)
				if len(active) == 0 {
					_ = r.store.FinalizeRun(r.run)
					return r.outcome(ExitOK, reason, false), nil
				}
			}
		}

		if allowDispatch {
			r.progress("wave %d: %d ready, dispatching %d%s",
				r.run.Wave, w.ReadyCount, len(w.Items), r.windowNote(calibrated, usage, est))
			r.reportWaveSkips(w)

			if r.opts.DryRun {
				for _, it := range w.Items {
					model := it.Model
					if model == "" {
						model = "(stage default)"
					}
					r.progress("dry-run: would dispatch %s (%s) model %s footprint %v",
						it.Issue.ID, it.Issue.Title, model, it.Footprint.Tokens)
				}
				_ = r.store.FinalizeRun(r.run)
				return r.outcome(ExitOK, "dry-run", false), nil
			}

			// Nothing dispatchable and nothing running: report, don't spin.
			if len(w.Items) == 0 && len(active) == 0 {
				_ = r.store.FinalizeRun(r.run)
				return r.outcome(ExitOK, "no dispatchable work (all ready items deferred)", false), nil
			}

			r.refreshDemand()
			r.warnIfOverFairShare()
			stagger := r.staggerDelay()
			for i, it := range w.Items {
				if i > 0 && stagger > 0 {
					select {
					case <-ctx.Done():
						return r.interrupted()
					case <-time.After(stagger):
					}
				}
				// Global concurrency cap (across all projects). A denial defers
				// the rest of this wave — same-project shares won't free up until
				// a running agent finishes — so break and re-scan next wave.
				if !r.acquireGlobalSlot(it.Issue.ID) {
					r.progress("wave %d: global governor cap reached — deferring %d bead(s) to a later wave",
						r.run.Wave, len(w.Items)-i)
					break
				}
				r.issues[it.Issue.ID] = it.Issue
				r.dispatchBead(ctx, dispatchReq{issue: it.Issue, epicID: it.EpicID, attempt: 1})
			}
		}

		// Poll this wave's slots (and any adopted ones) to a terminal state.
		if err := r.pollUntilIdle(ctx); err != nil {
			return r.interrupted()
		}

		if r.opts.Once {
			_ = r.store.FinalizeRun(r.run)
			return r.outcome(ExitOK, "", false), nil
		}
	}
}

// governor loads the current quota config and measures usage. An account that
// has never been calibrated (both ceilings zero) short-circuits to an
// advisory LevelOK without measuring — quota.State would report the same
// verdict, and skipping the snapshot avoids a pointless (and possibly slow)
// ccusage/transcript probe.
func (r *runner) governor(ctx context.Context) (quota.Level, bool, quota.Usage) {
	if cfgQ, err := quota.LoadConfig(r.quotaName()); err == nil {
		r.quotaCfg = cfgQ
	}
	if r.quotaCfg.WindowCeilingUSD <= 0 && r.quotaCfg.WeeklyCeilingUSD <= 0 {
		return quota.LevelOK, false, quota.Usage{Account: r.quotaCfg.Account}
	}
	u, _ := quota.Snapshot(ctx, r.profile, r.quotaCfg)
	level, calibrated := quota.State(u, r.quotaCfg)
	return level, calibrated, u
}

// billingFor selects the billing mode for this wave. Subscription always,
// EXCEPT at governor stop when the operator explicitly opted into api-key
// fallback (flag + registry policy + resolvable key). Logged loudly: this is
// the only path to per-token spend.
// guardMode resolves whether the billing guard's throttling constraints are
// advisory for this run, and why. Precedence: run flag > project registry
// setting > baseline (uncalibrated governor). Enforced is the default.
func (r *runner) guardMode(calibrated bool) (advisory bool, why string) {
	if r.opts.NoBillingGuard {
		return true, "--no-billing-guard"
	}
	if r.rec.BillingGuard == "advisory" {
		return true, "project billing_guard=advisory"
	}
	if !calibrated {
		return true, "baseline: governor uncalibrated"
	}
	return false, ""
}

func (r *runner) billingFor(level quota.Level) (account.BillingMode, string) {
	if level == quota.LevelStop && r.opts.AllowAPISpend &&
		r.rec.APIFallback == "explicit" && r.rec.APIKeyEnvVar != "" {
		if key := os.Getenv(r.rec.APIKeyEnvVar); key != "" {
			r.progress("!!! governor stop: switching to API-KEY billing from $%s (explicit opt-in) — per-token spend ahead", r.rec.APIKeyEnvVar)
			return account.BillingAPIKey, key
		}
	}
	return account.BillingSubscription, ""
}

// runCostUSD is the cumulative recorded cost of every slot in this run — the
// figure the per-run --budget ceiling is measured against.
func (r *runner) runCostUSD() float64 {
	var total float64
	for _, sl := range r.run.Slots {
		if sl != nil {
			total += sl.CostUSD
		}
	}
	return total
}

// onlyBead narrows a frontier to the single bead with id, or empty when it is
// not currently ready.
func onlyBead(issues []beads.Issue, id string) []beads.Issue {
	for _, iss := range issues {
		if iss.ID == id {
			return []beads.Issue{iss}
		}
	}
	return nil
}

// waveEstimate sums the per-item cost estimates for a candidate wave.
func (r *runner) waveEstimate(items []sched.Item) float64 {
	var est float64
	for _, it := range items {
		model := it.Model
		if model == "" {
			model = modelroute.TierSonnet
		}
		est += quota.EstimateItem(r.quotaCfg, model, quota.SizeOf(len(it.Issue.Description)))
	}
	return est
}

// windowNote renders the estimate/usage suffix for the wave progress line.
func (r *runner) windowNote(calibrated bool, u quota.Usage, est float64) string {
	if !calibrated {
		return " (governor uncalibrated)"
	}
	return fmt.Sprintf(" (est $%.2f / window %.0f%%)", est, u.Window5h.Fraction()*100)
}

// childLister adapts beads.ListChildren for sched.BuildWave. Adapter errors
// are treated as "no children" so a bd hiccup cannot wedge the wave.
func (r *runner) childLister(ctx context.Context) func(string) (bool, error) {
	return func(id string) (bool, error) {
		kids, err := r.adapter.ListChildren(ctx, id)
		if err != nil {
			return false, nil
		}
		for _, k := range kids {
			if k.Status != "closed" && k.Status != "done" {
				return true, nil
			}
		}
		return false, nil
	}
}

// dispatchReq describes one dispatch (fresh, requeue, or review bounce).
type dispatchReq struct {
	issue           beads.Issue
	epicID          string
	attempt         int
	resumeSHA       string
	resumeSessionID string
	reviewPath      string
	reviewIters     int
	note            string
}

// dispatchBead runs the full dispatch flow for one bead: model routing,
// worktree, prompt, backend launch, bd claim, ledger slot, manifest v2, audit.
// Failures block the slot and never fall through.
func (r *runner) dispatchBead(ctx context.Context, q dispatchReq) {
	beadID := q.issue.ID

	res, err := modelroute.Resolve(modelroute.Req{
		Stage:         modelroute.StageImplement,
		Labels:        q.issue.Labels,
		RunDefault:    r.opts.DefaultModel,
		AllowedModels: r.rec.AllowedModels,
		Stages:        r.cfg.Stages,
	})
	if err != nil {
		r.blockSlot(beadID, q, "model resolution: "+err.Error())
		return
	}
	// Persona metadata: the meta model never overrides the resolved tier
	// (tier wins); only the effort hint is taken.
	effort := res.Effort
	if _, metaEffort, err := modelroute.PersonaMeta(r.rec.Root, res.Persona); err == nil && effort == "" {
		effort = metaEffort
	}

	branch := worktree.BranchFor(beadID)
	wt, err := worktree.Ensure(ctx, worktree.EnsureOpts{
		RepoRoot:     r.rec.Root,
		WorktreeRoot: r.rec.WorktreeRoot,
		Branch:       branch,
		Base:         r.rec.DefaultBranch,
	})
	if err != nil {
		r.blockSlot(beadID, q, "worktree: "+err.Error())
		return
	}
	if wt.Created && len(r.cfg.Bootstrap) > 0 {
		if err := worktree.Bootstrap(ctx, wt.Path, r.cfg.Bootstrap, nil); err != nil {
			r.blockSlot(beadID, q, "bootstrap: "+err.Error())
			return
		}
	}

	phaseDir := r.store.PhaseDir(r.run.RunID, beadID)
	policy := r.mergePolicy(ctx, q.epicID)

	prompt := promptc.Compile(promptc.Input{
		EngineVersion:  EngineVersion,
		ProjectName:    r.rec.Name,
		Gate:           r.cfg.Gate,
		CommitStyle:    r.cfg.CommitStyle,
		CommitTemplate: r.cfg.CommitTemplate,
		Bootstrap:      r.cfg.Bootstrap,
		Bead:           q.issue,
		ResumeSHA:      q.resumeSHA,
		ReviewPath:     q.reviewPath,
		PhaseDir:       phaseDir,
		SummaryPath:    filepath.Join(phaseDir, "SUMMARY.md"),
		StatusPath:     filepath.Join(phaseDir, "status.json"),
		LogPath:        filepath.Join(phaseDir, "session.log"),
	})

	sessionID := newSessionID()
	sessionName := "koryph/" + r.opts.ProjectID + "/" + beadID + "/a" + strconv.Itoa(q.attempt)

	handle, err := r.backend.Dispatch(ctx, dispatch.Spec{
		ProjectID:        r.opts.ProjectID,
		RepoRoot:         r.rec.Root,
		RunID:            r.run.RunID,
		PhaseID:          beadID,
		PhaseDir:         phaseDir,
		Worktree:         wt.Path,
		Branch:           branch,
		Persona:          res.Persona,
		Model:            res.Model,
		Effort:           effort,
		Profile:          r.profile,
		ExpectedIdentity: r.rec.ExpectedIdentity,
		Billing:          r.billing,
		APIKey:           r.apiKey,
		MaxBudgetUSD:     r.quotaCfg.PerAgentMaxUSD,
		Prompt:           prompt,
		SessionID:        sessionID,
		SessionName:      sessionName,
		ResumeSessionID:  q.resumeSessionID,
		BeadsDir:         r.beadsDir,
		Attempt:          q.attempt,
		SSHAuthSock:      r.sshAuthSock,
		EnvPassthrough:   r.rec.EnvPassthrough,
	})
	if err != nil {
		r.blockSlot(beadID, q, "dispatch refused: "+err.Error())
		return
	}

	_ = r.adapter.Claim(ctx, beadID) // best-effort
	r.holdGlobalSlot(beadID, handle.PID, res.Model)

	now := time.Now().UTC().Format(time.RFC3339)
	sl := &ledger.Slot{
		PhaseID:          beadID,
		BeadID:           beadID,
		EpicID:           q.epicID,
		Branch:           branch,
		Worktree:         wt.Path,
		SessionID:        sessionID,
		SessionName:      sessionName,
		Agent:            res.Persona,
		Model:            res.Model,
		ModelWhy:         res.Rationale,
		Effort:           effort,
		AccountProfile:   r.rec.AccountProfile,
		ClaudeConfigDir:  r.rec.ClaudeConfigDir,
		VerifiedIdentity: handle.VerifiedIdentity,
		VerifiedAt:       now,
		BillingMode:      string(r.billing),
		PID:              handle.PID,
		Stream:           handle.StreamPath,
		StatusPath:       handle.StatusPath,
		LogPath:          filepath.Join(phaseDir, "session.log"),
		Status:           ledger.SlotRunning,
		Attempts:         q.attempt,
		ResumeSHA:        q.resumeSHA,
		ReviewIters:      q.reviewIters,
		DispatchedAt:     now,
		Note:             q.note,
	}
	_ = r.store.SetSlot(r.run, sl)
	r.dispatched++

	_ = r.store.SaveManifest(r.run.RunID, beadID, &ledger.Manifest{
		ProjectID:       r.opts.ProjectID,
		BeadID:          beadID,
		EpicID:          q.epicID,
		AccountProfile:  r.rec.AccountProfile,
		ClaudeConfigDir: r.rec.ClaudeConfigDir,
		SessionID:       sessionID,
		SessionName:     sessionName,
		Model:           res.Model,
		ModelWhy:        res.Rationale,
		WorktreePath:    wt.Path,
		Branch:          branch,
		BaseCommit:      r.baseCommit(ctx),
		Attempt:         q.attempt,
		ExecutionState:  "running",
		RecoveryTier:    recoveryTier(q.issue, r.cfg),
		MergePolicy:     string(policy),
		AutoMerge:       r.opts.AutoMerge,
		BillingMode:     string(r.billing),
		BootstrapCmds:   r.cfg.Bootstrap,
		PromptCache:     r.rec.PromptCachePolicy,
		BatchAllowed:    r.rec.BatchPolicy == "explicit",
		ReviewStatus:    reviewStatus(q.reviewPath),
	})

	_ = r.reg.Audit(registry.Event{
		Kind:      "dispatch",
		ProjectID: r.opts.ProjectID,
		Actor:     r.owner,
		Detail: map[string]string{
			"bead":    beadID,
			"model":   res.Model,
			"account": r.rec.AccountProfile,
			"billing": string(r.billing),
		},
	})
	r.progress("bead %s: dispatched attempt %d (model %s — %s; pid %d)",
		beadID, q.attempt, res.Model, res.Rationale, handle.PID)
}

// blockSlot records a slot that could not be dispatched. Blocked is terminal:
// it never falls through to a merge.
func (r *runner) blockSlot(beadID string, q dispatchReq, why string) {
	_ = r.store.UpdateSlot(r.run, beadID, func(s *ledger.Slot) {
		s.BeadID = beadID
		s.EpicID = q.epicID
		s.Status = ledger.SlotBlocked
		s.Attempts = q.attempt
		s.Note = why
	})
	r.releaseGlobalSlot(beadID) // terminal: free the reserved/held slot
	r.progress("bead %s: blocked (%s)", beadID, why)
}

// mergePolicy resolves the effective merge policy: an epic merge:* label wins
// over the project config; Show errors fall back to the config.
// reportWaveSkips surfaces the scheduler's skip/deferral reasons so an operator
// can see WHY a ready bead did not dispatch (the reasons were previously
// computed and discarded, koryph-6g2.1). Structural skips (non-task types, gt:*
// gates) never dispatch as-is, so they are reported ONCE per run with a fix
// hint; deferrals (footprint conflict, wave full, container, no-dispatch) are
// transient and summarized per wave — or listed in full under --dry-run.
func (r *runner) reportWaveSkips(w sched.Wave) {
	if r.reportedSkips == nil {
		r.reportedSkips = map[string]bool{}
	}
	for _, s := range w.Skipped {
		if r.reportedSkips[s.ID] {
			continue
		}
		r.reportedSkips[s.ID] = true
		r.progress("skipped %s: %s — not dispatchable as-is (file as task/bug/chore; area:* label; drop gt:*)", s.ID, s.Reason)
	}
	if len(w.Deferred) == 0 {
		return
	}
	if r.opts.DryRun {
		for _, d := range w.Deferred {
			r.progress("dry-run: deferred %s (%s): %s", d.ID, d.Title, d.Reason)
		}
		return
	}
	r.progress("wave %d: deferred %d bead(s): %s", r.run.Wave, len(w.Deferred), summarizeReasons(w.Deferred, 3))
}

// summarizeReasons renders up to n "id(reason)" pairs, with a trailing "+k more".
func summarizeReasons(rs []sched.Reason, n int) string {
	var parts []string
	for i, d := range rs {
		if i >= n {
			parts = append(parts, fmt.Sprintf("+%d more", len(rs)-n))
			break
		}
		parts = append(parts, fmt.Sprintf("%s(%s)", d.ID, d.Reason))
	}
	return strings.Join(parts, ", ")
}

func (r *runner) mergePolicy(ctx context.Context, epicID string) project.Policy {
	if epicID != "" {
		if epic, err := r.adapter.Show(ctx, epicID); err == nil {
			for _, l := range epic.Labels {
				switch l {
				case "merge:auto":
					return project.PolicyAuto
				case "merge:manual":
					return project.PolicyManual
				case "merge:pr":
					return project.PolicyPR
				}
			}
		}
	}
	return r.cfg.MergePolicy
}

// recoveryTier resolves the recovery tier: an rt:<n> label wins over the
// project default.
func recoveryTier(issue beads.Issue, cfg *project.Config) int {
	for _, v := range issue.LabelValues("rt:") {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 3 {
			return n
		}
	}
	return cfg.RiskTierDefault
}

// reviewStatus annotates a manifest for a review-bounce dispatch.
func reviewStatus(reviewPath string) string {
	if reviewPath == "" {
		return ""
	}
	return "bounced: blocking findings at " + reviewPath
}

// baseCommit resolves the default branch HEAD in the primary checkout
// (tolerating errors with an empty result).
func (r *runner) baseCommit(ctx context.Context) string {
	res, err := execx.Run(ctx, execx.Cmd{
		Dir: r.rec.Root, Name: "git", Args: []string{"rev-parse", r.rec.DefaultBranch},
	})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}
