// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package intake

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
)

// --- fakes -----------------------------------------------------------------

// fakeStore is an in-memory beadStore. extRefs/labels seed dedup hits keyed by
// the exact query string; the *Err fields inject failures; created records every
// Create call so tests can assert the shared loop's create payload.
type fakeStore struct {
	extRefs map[string][]beads.Issue
	labels  map[string][]beads.Issue

	extErr    error
	labelErr  error
	createErr error

	extQueries   []string
	labelQueries []string
	created      []beads.CreateInput
}

func (s *fakeStore) ListByExternalRef(_ context.Context, ref string) ([]beads.Issue, error) {
	s.extQueries = append(s.extQueries, ref)
	if s.extErr != nil {
		return nil, s.extErr
	}
	return s.extRefs[ref], nil
}

func (s *fakeStore) ListByLabel(_ context.Context, label string) ([]beads.Issue, error) {
	s.labelQueries = append(s.labelQueries, label)
	if s.labelErr != nil {
		return nil, s.labelErr
	}
	return s.labels[label], nil
}

func (s *fakeStore) Create(_ context.Context, in beads.CreateInput) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	s.created = append(s.created, in)
	return "bead-new", nil
}

// fakeSource is a provider-neutral Source stub. Provenance mirrors the real
// providers' "<prefix>-<owner>/<repo>#<n>" shape; Comment records calls and can
// be made to fail.
type fakeSource struct {
	commentErr  error
	commentedTo []int
}

func (f *fakeSource) List(context.Context, string, string, string, int) ([]SourceIssue, error) {
	return nil, nil // ingest receives issues directly; List is unused here.
}

func (f *fakeSource) Comment(_ context.Context, _, _ string, number int, _ string) error {
	f.commentedTo = append(f.commentedTo, number)
	return f.commentErr
}

func (f *fakeSource) Provenance(owner, repo string, number int) string {
	return provKeyFor(owner, repo, number)
}

func provKeyFor(owner, repo string, number int) string {
	return "fake-" + owner + "/" + repo + "#" + strconv.Itoa(number)
}

func describeNoop(iss SourceIssue) string { return "body: " + iss.Title }

// --- dedup table -----------------------------------------------------------

func TestIngestDedupPaths(t *testing.T) {
	const owner, repo = "acme", "widgets"
	iss := SourceIssue{Number: 5, Title: "add feature"}
	primaryKey := provKeyFor(owner, repo, 5) // "fake-acme/widgets#5"
	legacyKey := func(n int) string { return "fake-legacy-" + strconv.Itoa(n) }

	cases := []struct {
		name        string
		store       *fakeStore
		legacyKey   func(int) string
		wantSkipped bool
		wantCreated bool
	}{
		{
			name:        "new issue is ingested",
			store:       &fakeStore{},
			wantCreated: true,
		},
		{
			name:        "dedup via external-ref",
			store:       &fakeStore{extRefs: map[string][]beads.Issue{primaryKey: {{ID: "cx-1"}}}},
			wantSkipped: true,
		},
		{
			name:        "dedup via label fallback",
			store:       &fakeStore{labels: map[string][]beads.Issue{primaryKey: {{ID: "cx-2"}}}},
			wantSkipped: true,
		},
		{
			name:        "dedup via legacy external-ref (GitHub only)",
			store:       &fakeStore{extRefs: map[string][]beads.Issue{"fake-legacy-5": {{ID: "cx-3"}}}},
			legacyKey:   legacyKey,
			wantSkipped: true,
		},
		{
			name:        "dedup via legacy label (GitHub only)",
			store:       &fakeStore{labels: map[string][]beads.Issue{"fake-legacy-5": {{ID: "cx-4"}}}},
			legacyKey:   legacyKey,
			wantSkipped: true,
		},
		{
			name:        "legacy key not consulted when option is nil",
			store:       &fakeStore{extRefs: map[string][]beads.Issue{"fake-legacy-5": {{ID: "cx-5"}}}},
			legacyKey:   nil, // JIRA/Linear: legacy dedup disabled → issue is ingested
			wantCreated: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ingest(context.Background(), tc.store, &fakeSource{}, owner, repo,
				[]SourceIssue{iss},
				ingestOptions{errPrefix: "intake", legacyKey: tc.legacyKey},
				describeNoop)
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if tc.wantSkipped {
				if len(res.Skipped) != 1 || len(res.Ingested) != 0 {
					t.Fatalf("want 1 skipped/0 ingested, got %d/%d", len(res.Skipped), len(res.Ingested))
				}
				if res.Skipped[0].Reason != "already ingested" {
					t.Fatalf("skip reason = %q", res.Skipped[0].Reason)
				}
			}
			if tc.wantCreated {
				if len(res.Ingested) != 1 || len(res.Skipped) != 0 {
					t.Fatalf("want 1 ingested/0 skipped, got %d/%d", len(res.Ingested), len(res.Skipped))
				}
				if len(tc.store.created) != 1 {
					t.Fatalf("want 1 create, got %d", len(tc.store.created))
				}
				got := tc.store.created[0]
				if got.ExternalRef != primaryKey {
					t.Fatalf("create ExternalRef = %q, want %q", got.ExternalRef, primaryKey)
				}
				// Mandatory labels + the qualified provenance key.
				if !containsAll(got.Labels, primaryKey, labelIntake, labelNoDispatch) {
					t.Fatalf("create labels missing mandatory set: %v", got.Labels)
				}
			}
			// The legacy key must never be queried when the option is nil.
			if tc.legacyKey == nil {
				for _, q := range append(tc.store.extQueries, tc.store.labelQueries...) {
					if strings.HasPrefix(q, "fake-legacy-") {
						t.Fatalf("legacy key queried despite nil option: %q", q)
					}
				}
			}
		})
	}
}

// --- dry-run ----------------------------------------------------------------

func TestIngestDryRunMutatesNothing(t *testing.T) {
	store := &fakeStore{}
	res, err := ingest(context.Background(), store, &fakeSource{}, "acme", "widgets",
		[]SourceIssue{{Number: 1, Title: "a"}, {Number: 2, Title: "b"}},
		ingestOptions{errPrefix: "intake", DryRun: true}, describeNoop)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ingested) != 2 {
		t.Fatalf("dry-run ingested = %d, want 2", len(res.Ingested))
	}
	if len(store.created) != 0 {
		t.Fatalf("dry-run must not create beads, got %d", len(store.created))
	}
	for _, it := range res.Ingested {
		if it.Reason != "would ingest (dry-run)" {
			t.Fatalf("dry-run reason = %q", it.Reason)
		}
	}
}

// --- comment-back -----------------------------------------------------------

func TestIngestCommentBack(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		src := &fakeSource{}
		res, err := ingest(context.Background(), &fakeStore{}, src, "acme", "widgets",
			[]SourceIssue{{Number: 7, Title: "x"}},
			ingestOptions{errPrefix: "intake", CommentBack: true}, describeNoop)
		if err != nil {
			t.Fatal(err)
		}
		if res.Ingested[0].Reason != "commented" {
			t.Fatalf("reason = %q, want commented", res.Ingested[0].Reason)
		}
		if len(src.commentedTo) != 1 || src.commentedTo[0] != 7 {
			t.Fatalf("comment targets = %v, want [7]", src.commentedTo)
		}
	})

	t.Run("failure is non-fatal", func(t *testing.T) {
		src := &fakeSource{commentErr: errors.New("boom")}
		res, err := ingest(context.Background(), &fakeStore{}, src, "acme", "widgets",
			[]SourceIssue{{Number: 8, Title: "y"}},
			ingestOptions{errPrefix: "intake", CommentBack: true}, describeNoop)
		if err != nil {
			t.Fatalf("comment failure must not fail the run: %v", err)
		}
		if len(res.Ingested) != 1 {
			t.Fatalf("issue still ingested despite comment failure: %d", len(res.Ingested))
		}
		if !strings.HasPrefix(res.Ingested[0].Reason, "comment-back failed: ") {
			t.Fatalf("reason = %q", res.Ingested[0].Reason)
		}
	})
}

// --- error prefixes ---------------------------------------------------------

func TestIngestErrorsCarryPrefix(t *testing.T) {
	issues := []SourceIssue{{Number: 3, Title: "z"}}

	t.Run("dedupe check error", func(t *testing.T) {
		store := &fakeStore{extErr: errors.New("db down")}
		_, err := ingest(context.Background(), store, &fakeSource{}, "acme", "widgets", issues,
			ingestOptions{errPrefix: "intake/jira"}, describeNoop)
		if err == nil || !strings.HasPrefix(err.Error(), "intake/jira: dedupe check for #3: ") {
			t.Fatalf("err = %v, want intake/jira dedupe-check prefix", err)
		}
	})

	t.Run("label fallback error", func(t *testing.T) {
		store := &fakeStore{labelErr: errors.New("db down")}
		_, err := ingest(context.Background(), store, &fakeSource{}, "acme", "widgets", issues,
			ingestOptions{errPrefix: "intake/linear"}, describeNoop)
		if err == nil || !strings.HasPrefix(err.Error(), "intake/linear: dedupe label fallback for #3: ") {
			t.Fatalf("err = %v, want intake/linear label-fallback prefix", err)
		}
	})

	t.Run("create error", func(t *testing.T) {
		store := &fakeStore{createErr: errors.New("no disk")}
		_, err := ingest(context.Background(), store, &fakeSource{}, "acme", "widgets", issues,
			ingestOptions{errPrefix: "intake"}, describeNoop)
		if err == nil || !strings.HasPrefix(err.Error(), "intake: create bead for #3: ") {
			t.Fatalf("err = %v, want intake create-bead prefix", err)
		}
	})
}

func containsAll(haystack []string, needles ...string) bool {
	set := make(map[string]bool, len(haystack))
	for _, h := range haystack {
		set[h] = true
	}
	for _, n := range needles {
		if !set[n] {
			return false
		}
	}
	return true
}
