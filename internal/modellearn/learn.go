// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package modellearn closes the escalation feedback loop (koryph-qf6.6): it
// mines run ledgers for beads that only merged after their final attempt
// escalated to a stronger tier (koryph-qf6.4), aggregates that evidence by
// the similarity features frozen on each slot (koryph-qf6.3 — area label and
// size bucket), and recommends a starting tier for FUTURE beads that share
// those features — so similar work stops re-paying the failed-retry tax on a
// model the history says is too weak.
//
// The actuator is deliberately a bead label, not a routing-table entry:
// Apply writes `model:<tier>` (plus a `model-learned:<date>` provenance
// marker) onto matching ready beads, which the existing modelroute label
// precedence honors with zero dispatch-path changes. That keeps the learned
// state human-auditable (visible in `bd show`), human-overridable (any
// pre-existing `model:*` label wins — Apply never touches a bead that
// already carries one, which also makes re-apply idempotent), and durable
// across machines (labels sync through the beads DB; the run ledgers this
// package reads are gitignored and machine-local).
package modellearn

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/quota"
)

// DefaultMinEvidence is the minimum count of escalated-then-merged beads a
// (area, size) bucket needs before Recommend proposes a tier for it. Two is
// the smallest count that is a pattern rather than an incident.
const DefaultMinEvidence = 2

// ProvenancePrefix is the label prefix Apply stamps alongside the routing
// label so a learned `model:<tier>` is distinguishable from a human one.
const ProvenancePrefix = "model-learned:"

// Evidence is one merged bead's contribution to the dataset: its frozen
// similarity features and whether it needed an escalation to merge.
type Evidence struct {
	BeadID    string
	RunID     string
	Escalated bool     // merged only after the final-attempt escalation
	Tier      string   // the tier the merging attempt ran on (slot.Model)
	FromTier  string   // the tier it escalated FROM ("" when not escalated)
	Areas     []string // area:* labels frozen on the slot at first dispatch
	Size      string   // quota size bucket frozen on the slot
}

// Recommendation proposes a starting tier for beads matching (Area, Size).
type Recommendation struct {
	Area        string
	Size        string
	Tier        string   // recommended starting tier
	Evidence    int      // escalated-then-merged beads behind it
	CleanMerges int      // counter-evidence: merges that never needed it
	Beads       []string // the evidence beads, for the operator's audit
}

// Applied records one label write Apply performed.
type Applied struct {
	BeadID string
	Area   string
	Size   string
	Tier   string
}

// Collect scans every run ledger under the store and returns one Evidence
// row per merged bead that carries frozen similarity features. Slots that
// predate the koryph-qf6.3 feature fields contribute nothing (their features
// are unknowable), and non-merged slots contribute nothing (the learner's
// success signal is "escalation MERGED the bead"; a bead that failed even on
// the stronger tier is not evidence the stronger tier helps). Beads seen in
// multiple runs count once, preferring the newest run (ListRuns is
// newest-first). Unreadable individual runs are skipped, not fatal.
func Collect(store *ledger.Store) ([]Evidence, error) {
	runs, err := store.ListRuns()
	if err != nil {
		return nil, err
	}
	var evs []Evidence
	seen := map[string]bool{}
	for _, runID := range runs {
		run, err := store.LoadRun(runID)
		if err != nil {
			continue
		}
		for _, sl := range run.Slots {
			if sl == nil || seen[sl.PhaseID] {
				continue
			}
			if len(sl.BeadLabels) == 0 && sl.SizeClass == "" {
				continue // predates feature persistence
			}
			switch sl.Status {
			case ledger.SlotMerged, ledger.SlotDone, ledger.SlotPROpened:
			default:
				continue
			}
			seen[sl.PhaseID] = true
			evs = append(evs, Evidence{
				BeadID:    sl.PhaseID,
				RunID:     runID,
				Escalated: escalated(sl.ModelWhy),
				Tier:      sl.Model,
				FromTier:  fromTierOf(sl.ModelWhy),
				Areas:     areasOf(sl.BeadLabels),
				Size:      sl.SizeClass,
			})
		}
	}
	return evs, nil
}

// Recommend aggregates evidence into per-(area, size) recommendations. A
// bucket earns one when its escalated-then-merged count reaches minEvidence
// (<=0 means DefaultMinEvidence) AND strictly exceeds its clean base-tier
// merges — an area that usually merges fine on the cheap tier keeps its
// default no matter how loud two outliers are. The recommended tier is the
// strongest tier the escalations landed on (opus, under today's
// RecoveryUpgrade policy). Output is sorted by (area, size) for stable
// display and deterministic apply order.
func Recommend(evs []Evidence, minEvidence int) []Recommendation {
	if minEvidence <= 0 {
		minEvidence = DefaultMinEvidence
	}
	type key struct{ area, size string }
	type agg struct {
		escalated, clean int
		tier             string
		beads            []string
	}
	buckets := map[key]*agg{}
	for _, ev := range evs {
		for _, area := range ev.Areas {
			k := key{area, ev.Size}
			b := buckets[k]
			if b == nil {
				b = &agg{}
				buckets[k] = b
			}
			switch {
			case ev.Escalated:
				b.escalated++
				b.beads = append(b.beads, ev.BeadID)
				if b.tier == "" || ev.Tier == modelroute.TierOpus {
					b.tier = ev.Tier
				}
			case ev.Tier == modelroute.TierHaiku || ev.Tier == modelroute.TierSonnet:
				b.clean++
			}
		}
	}
	var recs []Recommendation
	for k, b := range buckets {
		if b.escalated >= minEvidence && b.escalated > b.clean && b.tier != "" {
			sort.Strings(b.beads)
			recs = append(recs, Recommendation{
				Area: k.area, Size: k.size, Tier: b.tier,
				Evidence: b.escalated, CleanMerges: b.clean, Beads: b.beads,
			})
		}
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Area != recs[j].Area {
			return recs[i].Area < recs[j].Area
		}
		return recs[i].Size < recs[j].Size
	})
	return recs
}

// BeadWriter is the one write Apply needs — satisfied by *beads.Adapter and
// the engine's WorkSource.
type BeadWriter interface {
	AddLabel(ctx context.Context, id, label string) error
}

// Apply stamps each matching frontier bead with the recommended
// `model:<tier>` routing label plus a `model-learned:<date>` provenance
// marker, and returns the applied set alongside an updated copy of the
// frontier whose matched issues carry the new labels (so a caller holding
// the slice — the engine's wave boundary — routes on them immediately,
// without a re-fetch).
//
// A bead matches a recommendation when it is a dispatchable type
// (task/bug/chore), carries the recommendation's area label, buckets to the
// recommendation's size, and has NO existing `model:*` label — the
// precedence rule that keeps humans (and prior applications: re-apply is a
// no-op) authoritative over the learner. Label-write failures skip the bead
// and are tallied in failed; they never abort the pass.
func Apply(ctx context.Context, w BeadWriter, issues []beads.Issue, recs []Recommendation, date string) (applied []Applied, updated []beads.Issue, failed int) {
	updated = make([]beads.Issue, len(issues))
	copy(updated, issues)
	if len(recs) == 0 {
		return nil, updated, 0
	}
	for i := range updated {
		iss := &updated[i]
		if !dispatchableType(iss.IssueType) || hasModelLabel(iss.Labels) {
			continue
		}
		size := quota.SizeOf(len(iss.Description))
		for _, rec := range recs {
			if rec.Size != size || !hasLabel(iss.Labels, rec.Area) {
				continue
			}
			if err := w.AddLabel(ctx, iss.ID, "model:"+rec.Tier); err != nil {
				failed++
				break
			}
			// Provenance is best-effort on top of a routing label that already
			// landed — a failure here must not undo or double-apply routing.
			if err := w.AddLabel(ctx, iss.ID, ProvenancePrefix+date); err != nil {
				failed++
			}
			iss.Labels = append(iss.Labels, "model:"+rec.Tier, ProvenancePrefix+date)
			applied = append(applied, Applied{BeadID: iss.ID, Area: rec.Area, Size: rec.Size, Tier: rec.Tier})
			break // one routing label per bead; later recs see hasModelLabel
		}
	}
	return applied, updated, failed
}

// escalated matches the same substring the TUI's ↑ marker and the engine's
// write-back key on, so every consumer agrees on what counts as escalated.
func escalated(modelWhy string) bool {
	return strings.Contains(strings.ToLower(modelWhy), "escalat")
}

// fromTierOf parses the origin tier out of an escalation rationale of the
// form "escalated from <tier> after ..." (engine requeueSlot, koryph-qf6.4).
func fromTierOf(modelWhy string) string {
	rest, ok := strings.CutPrefix(modelWhy, "escalated from ")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, ' '); i > 0 {
		return rest[:i]
	}
	return rest
}

// areasOf filters a frozen label set down to its area:* labels.
func areasOf(labels []string) []string {
	var out []string
	for _, l := range labels {
		if strings.HasPrefix(l, "area:") {
			out = append(out, l)
		}
	}
	return out
}

// hasModelLabel reports whether any label carries the model: routing prefix —
// plain (model:opus) or stage-scoped (model:implement:opus) both count, since
// either one means a human or a prior apply already chose.
func hasModelLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, "model:") {
			return true
		}
	}
	return false
}

func hasLabel(labels []string, want string) bool {
	return slices.Contains(labels, want)
}

// dispatchableType mirrors the scheduler's eligibility: only these types are
// ever loop-dispatched, so only they benefit from a routing label.
func dispatchableType(t string) bool {
	switch t {
	case "task", "bug", "chore":
		return true
	}
	return false
}
