// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package intake

import (
	"context"
	"fmt"

	"github.com/koryph/koryph/internal/beads"
)

// beadStore is the subset of *beads.Adapter that the shared ingest loop needs.
// Declaring it as an interface lets table tests drive ingest against a fake
// store without a real `bd` binary.
type beadStore interface {
	ListByExternalRef(ctx context.Context, ref string) ([]beads.Issue, error)
	ListByLabel(ctx context.Context, label string) ([]beads.Issue, error)
	Create(ctx context.Context, in beads.CreateInput) (string, error)
}

// ingestOptions carries the per-provider knobs for the shared ingest loop. The
// only behavioural differences between the GitHub, JIRA, and Linear intake paths
// are captured here (plus the describe closure passed to ingest).
type ingestOptions struct {
	// errPrefix is prepended to every wrapped error emitted inside the loop
	// (e.g. "intake", "intake/jira", "intake/linear") so each provider's error
	// messages are preserved verbatim.
	errPrefix string
	// DryRun prints intent and mutates nothing.
	DryRun bool
	// CommentBack posts the new bead ID back on each ingested issue.
	CommentBack bool
	// legacyKey, when non-nil, produces the pre-v1 unqualified provenance key
	// (e.g. "gh-42") for an issue number so beads created before repo
	// qualification are not re-ingested. Only GitHub sets it; nil disables the
	// backward-compat check entirely. This makes the previously GitHub-only
	// legacy dedup an explicit, opt-in behaviour rather than an implicit
	// divergence between the three loops.
	legacyKey func(number int) string
}

// ingest is the single shared intake loop behind Run, RunJIRA, and RunLinear.
// The caller fetches issues via src.List (so each provider keeps its exact
// list-error message and List argument shape) and hands them here; ingest then
// dedupes against the bead store, honours dry-run, files one planning bead per
// new issue, and optionally comments back.
//
// All provider variation is confined to (a) opts — error prefix, dry-run and
// comment-back flags, and the optional legacy-dedup key — and (b) describe, the
// closure that builds each bead's description with a provider-specific
// provenance footer.
func ingest(
	ctx context.Context,
	bd beadStore,
	src Source,
	owner, repo string,
	issues []SourceIssue,
	opts ingestOptions,
	describe func(SourceIssue) string,
) (*Result, error) {
	res := &Result{Owner: owner, Repo: repo}
	for _, iss := range issues {
		provKey := src.Provenance(owner, repo, iss.Number)

		// Idempotency: primary lookup via external-ref (the canonical dedup
		// key), then the provenance label for beads created before external-ref
		// was introduced.
		existing, derr := bd.ListByExternalRef(ctx, provKey)
		if derr != nil {
			return nil, fmt.Errorf("%s: dedupe check for #%d: %w", opts.errPrefix, iss.Number, derr)
		}
		if len(existing) == 0 {
			existing, derr = bd.ListByLabel(ctx, provKey)
			if derr != nil {
				return nil, fmt.Errorf("%s: dedupe label fallback for #%d: %w", opts.errPrefix, iss.Number, derr)
			}
		}
		// Backward-compat: also check the pre-v1 unqualified key when the
		// provider supplies one (GitHub only) so beads created by older koryph
		// intake runs are not re-ingested.
		if len(existing) == 0 && opts.legacyKey != nil {
			oldKey := opts.legacyKey(iss.Number)
			existing, _ = bd.ListByExternalRef(ctx, oldKey)
			if len(existing) == 0 {
				existing, _ = bd.ListByLabel(ctx, oldKey)
			}
		}
		if len(existing) > 0 {
			res.Skipped = append(res.Skipped, Item{
				Number: iss.Number,
				Title:  iss.Title,
				BeadID: existing[0].ID,
				Reason: "already ingested",
			})
			continue
		}

		item := Item{Number: iss.Number, Title: iss.Title}
		if opts.DryRun {
			item.Reason = "would ingest (dry-run)"
			res.Ingested = append(res.Ingested, item)
			continue
		}

		id, cerr := bd.Create(ctx, beads.CreateInput{
			Title:       iss.Title,
			Description: describe(iss),
			Labels:      []string{provKey, labelIntake, labelNoDispatch},
			Priority:    priorityFor(iss),
			IssueType:   issueTypeFor(iss),
			ExternalRef: provKey,
		})
		if cerr != nil {
			return nil, fmt.Errorf("%s: create bead for #%d: %w", opts.errPrefix, iss.Number, cerr)
		}
		item.BeadID = id

		if opts.CommentBack {
			body := fmt.Sprintf("Tracked as bead %s for planning.", id)
			if gerr := src.Comment(ctx, owner, repo, iss.Number, body); gerr != nil {
				// Non-fatal: the bead already exists; record the miss.
				item.Reason = "comment-back failed: " + gerr.Error()
			} else {
				item.Reason = "commented"
			}
		}
		res.Ingested = append(res.Ingested, item)
	}
	return res, nil
}

// legacyProvenancer is an optional interface implemented by Sources that can
// also produce the pre-v1 unqualified key (e.g. "gh-42") for backward-compatible
// deduplication of beads created before repo qualification was introduced.
type legacyProvenancer interface {
	legacyProvenance(number int) string
}
