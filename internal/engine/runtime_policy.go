// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"fmt"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/epicreview"
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/sched"
)

// normalRuntimeFor is the routing decision stored on the bead/project before
// an operator applies a run-scoped execution policy. Keeping this separate
// from effectiveRuntimeFor is what makes --runtime-only a filter rather than
// an accidental override.
func (r *runner) normalRuntimeFor(issue beads.Issue) (name, rationale string) {
	return modelroute.ResolveRuntimeName(issue.Labels, r.cfg.DefaultRuntime)
}

// effectiveRuntimeFor is the runtime that may actually launch the bead in
// this run. --runtime-equivalent is an explicit coercion; --runtime-only is
// enforced by frontier filtering and therefore leaves normal routing intact.
func (r *runner) effectiveRuntimeFor(issue beads.Issue) (name, rationale string) {
	normal, why := r.normalRuntimeFor(issue)
	if r.opts.RuntimeEquivalent != "" {
		return r.opts.RuntimeEquivalent, "runtime-equivalent " + r.opts.RuntimeEquivalent + " (normal " + normal + " via " + why + ")"
	}
	return normal, why
}

// applyRuntimePolicy removes runtime-only mismatches before the scheduler
// considers width, resources, or footprints. This avoids a non-selected high
// priority bead occupying a wave slot ahead of selected work. The returned
// reasons are folded into the ledger frontier so a dry run and koryph status
// make the non-dispatch decision auditable.
func (r *runner) applyRuntimePolicy(issues []beads.Issue) ([]beads.Issue, []sched.Reason) {
	if r.opts.RuntimeOnly == "" {
		return issues, nil
	}
	selected := make([]beads.Issue, 0, len(issues))
	skipped := make([]sched.Reason, 0)
	for _, issue := range issues {
		normal, why := r.normalRuntimeFor(issue)
		if normal != r.opts.RuntimeOnly {
			skipped = append(skipped, sched.Reason{
				ID: issue.ID, Title: issue.Title,
				Reason: fmt.Sprintf("runtime-only %s: normal runtime %s via %s", r.opts.RuntimeOnly, normal, why),
			})
			continue
		}
		selected = append(selected, issue)
	}
	return selected, skipped
}

// modelRequestForRuntime builds the same modelroute request for every
// execution path (implementation, dry-run, pipeline, review). The runtime's
// own default selection wins; the legacy project-level default belongs only to
// DefaultRuntime. A run --default-model remains the highest label-less
// override for backwards compatibility.
func (r *runner) modelRequestForRuntime(stage string, labels []string, explicitModel, runtimeName string) modelroute.Req {
	modelMap := r.cfg.ModelMap
	effortMap := map[string]string(nil)
	if rc, ok := r.cfg.Runtimes[runtimeName]; ok {
		modelMap = mergeStringMaps(modelMap, rc.ModelMap)
		effortMap = rc.EffortMap
	}
	runDefault := r.opts.DefaultModel
	runEquivalent := ""
	if runDefault == "" {
		runDefault, runEquivalent = r.cfg.DefaultSelectionForRuntime(runtimeName)
	}
	return modelroute.Req{
		Stage:         stage,
		Labels:        labels,
		RunDefault:    runDefault,
		RunEquivalent: runEquivalent,
		ExplicitModel: explicitModel,
		AllowedModels: r.rec.AllowedModels,
		Stages:        r.cfg.Stages,
		RepoRoot:      r.rec.Root,
		ModelMap:      modelMap,
		EffortMap:     effortMap,
		Runtime:       runtimeName,
	}
}

// resolveModelForRuntime applies the equivalent-runtime policy to one normal
// model resolution. It is deliberately shared by implementation, pipeline,
// review, and dry-run validation so a model that is safe to show is also safe
// to execute.
func (r *runner) resolveModelForRuntime(stage string, issue beads.Issue, explicitModel, effectiveRuntime string) (modelroute.Resolution, error) {
	sourceRuntime, _ := r.normalRuntimeFor(issue)
	source := r.modelRequestForRuntime(stage, issue.Labels, explicitModel, sourceRuntime)
	if r.opts.RuntimeEquivalent != "" && sourceRuntime != effectiveRuntime {
		target := r.modelRequestForRuntime(stage, issue.Labels, explicitModel, effectiveRuntime)
		return modelroute.ResolveEquivalent(source, target)
	}
	// In all non-equivalent cases effectiveRuntime equals the normal runtime.
	// The equal-runtime equivalent case preserves exact native choices because
	// no cross-runtime translation is necessary.
	if sourceRuntime == effectiveRuntime {
		return modelroute.Resolve(source)
	}
	return modelroute.Resolve(r.modelRequestForRuntime(stage, issue.Labels, explicitModel, effectiveRuntime))
}

func (r *runner) implementationStage(issue beads.Issue) string {
	if issue.HasLabel(epicreview.LabelDocs) {
		return modelroute.StageDocs
	}
	return modelroute.StageImplement
}

// validateEquivalentModels fails the run before any item in this boundary is
// launched. It prevents a mixed wave from partially dispatching before a
// later ambiguous native model produces an unsafe conversion failure.
func (r *runner) validateEquivalentModels(items []sched.Item) error {
	if r.opts.RuntimeEquivalent == "" {
		return nil
	}
	for _, item := range items {
		effective, _ := r.effectiveRuntimeFor(item.Issue)
		if _, err := r.resolveModelForRuntime(r.implementationStage(item.Issue), item.Issue, "", effective); err != nil {
			return fmt.Errorf("bead %s cannot use --runtime-equivalent %s: %w", item.Issue.ID, effective, err)
		}
	}
	return nil
}

func scopedSigningTransportError(rt runtime.Runtime, socket string) error {
	if socket == "" || rt.Capabilities().ScopedSigningSocket {
		return nil
	}
	return fmt.Errorf(
		"runtime %s cannot isolate the koryph scoped signing socket on this host; no private key, operator SSH agent, or broad Unix-socket access was exposed",
		rt.Name(),
	)
}
