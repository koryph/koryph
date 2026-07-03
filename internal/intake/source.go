// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package intake

import "context"

// SourceIssue is a provider-neutral issue record returned by a Source.
type SourceIssue struct {
	Number int
	Title  string
	Body   string
	Labels []string // label names
	Author string   // login / handle
}

// Source abstracts an issue-tracker provider for intake.
type Source interface {
	// List returns open issues that carry the trigger label.
	List(ctx context.Context, owner, repo, label string, limit int) ([]SourceIssue, error)
	// Comment posts a text comment on an issue identified by number.
	Comment(ctx context.Context, owner, repo string, number int, body string) error
	// Provenance returns the canonical external-ref key for an issue, scoped
	// to the specific owner/repo so keys are globally unique across multiple
	// configured sources. Example: "gh-acme/widgets#42". The key is also used
	// as a bead label for backward-compatible label-based deduplication.
	Provenance(owner, repo string, number int) string
}
