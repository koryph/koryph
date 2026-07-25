// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func artifactOpts(t *testing.T, repo, claudeBin, artifactDir string) Opts {
	t.Helper()
	gitIn(t, repo, "checkout", "agent/x1")
	o := baseOpts(t, repo, claudeBin)
	o.OutPath = ""
	o.ArtifactDir = artifactDir
	o.CandidateSHA = gitOutput(t, repo, "rev-parse", "agent/x1")
	o.BaseSHA = gitOutput(t, repo, "rev-parse", "main")
	return o
}

func TestReviewHistoryIsCumulativeImmutableAndStable(t *testing.T) {
	repo := reviewRepo(t)
	dir := t.TempDir()

	firstEnvelope := `{"type":"result","result":"{\"blocking\":false,\"findings\":[{\"severity\":\"major\",\"file\":\"feature.go\",\"line\":1,\"summary\":\"missing guard\"}]}"}`
	first := Review(context.Background(), artifactOpts(t, repo, fakeClaude(t, firstEnvelope), dir))
	if first.Degraded || !first.Blocking || len(first.BlockingHistory) != 1 {
		t.Fatalf("first verdict = %+v", first)
	}
	firstBytes, err := os.ReadFile(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.BlockingHistory[0].ID

	secondEnvelope := `{"type":"result","result":"{\"blocking\":false,\"prior_findings\":[{\"id\":\"` +
		firstID + `\",\"status\":\"resolved\",\"evidence\":\"feature.go:1 now validates\"}],` +
		`\"findings\":[{\"severity\":\"blocking\",\"file\":\"feature.go\",\"line\":1,\"summary\":\"missing authorization\"}]}"}`
	secondOpts := artifactOpts(t, repo, fakeClaude(t, secondEnvelope), dir)
	secondOpts.PriorVerdictPaths = []string{first.ArtifactPath}
	second := Review(context.Background(), secondOpts)
	if second.Degraded || !second.Blocking || len(second.BlockingHistory) != 2 {
		t.Fatalf("second verdict = %+v", second)
	}
	if second.ArtifactPath == first.ArtifactPath {
		t.Fatal("distinct review content reused an immutable artifact path")
	}
	after, err := os.ReadFile(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(firstBytes) {
		t.Fatal("later review mutated the first immutable artifact")
	}

	ids := []string{second.BlockingHistory[0].ID, second.BlockingHistory[1].ID}
	thirdEnvelope := `{"type":"result","result":"{\"blocking\":false,\"prior_findings\":[` +
		`{\"id\":\"` + ids[0] + `\",\"status\":\"resolved\",\"evidence\":\"focused regression A\"},` +
		`{\"id\":\"` + ids[1] + `\",\"status\":\"resolved\",\"evidence\":\"focused regression B\"}],\"findings\":[]}"}`
	capture := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("KORYPH_TEST_REVIEW_STDIN", capture)
	thirdOpts := artifactOpts(t, repo, fakeClaude(t, thirdEnvelope), dir)
	// Loading only the newest artifact must retain the complete first+second
	// blocker set; callers do not need to replay the whole chain forever.
	thirdOpts.PriorVerdictPaths = []string{second.ArtifactPath}
	third := Review(context.Background(), thirdOpts)
	if third.Degraded || third.Blocking || len(third.BlockingHistory) != 2 {
		t.Fatalf("third verdict = %+v", third)
	}
	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !strings.Contains(string(prompt), id) {
			t.Errorf("repair prompt omitted cumulative blocker %s:\n%s", id, prompt)
		}
	}

	var artifact Artifact
	raw, err := os.ReadFile(third.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.HistoryDigest != third.HistoryDigest ||
		len(artifact.PriorArtifactDigests) != 1 ||
		artifact.PriorArtifactDigests[0] != second.ArtifactDigest {
		t.Fatalf("third artifact chain = %+v", artifact)
	}
}

func TestReviewRejectsTamperedHistoryDigest(t *testing.T) {
	repo := reviewRepo(t)
	dir := t.TempDir()
	envelope := `{"type":"result","result":"{\"blocking\":true,\"findings\":[{\"severity\":\"blocking\",\"summary\":\"guard missing\"}]}"}`
	first := Review(context.Background(), artifactOpts(t, repo, fakeClaude(t, envelope), dir))
	if first.Degraded {
		t.Fatal(first.Reason)
	}

	raw, err := os.ReadFile(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var artifact Artifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	artifact.History[0].Summary = "tampered"
	tampered, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	tamperedPath := filepath.Join(t.TempDir(), "tampered.json")
	if err := os.WriteFile(tamperedPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	clean := `{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`
	o := artifactOpts(t, repo, fakeClaude(t, clean), dir)
	o.PriorVerdictPaths = []string{tamperedPath}
	got := Review(context.Background(), o)
	if !got.Degraded || !strings.Contains(got.Reason, "identity or digest") {
		t.Fatalf("tampered history verdict = %+v", got)
	}
}

func TestReviewRejectsArtifactWithStrippedSchema(t *testing.T) {
	repo := reviewRepo(t)
	dir := t.TempDir()
	envelope := `{"type":"result","result":"{\"blocking\":true,\"findings\":[{\"severity\":\"blocking\",\"summary\":\"guard missing\"}]}"}`
	first := Review(context.Background(), artifactOpts(t, repo, fakeClaude(t, envelope), dir))
	if first.Degraded {
		t.Fatal(first.Reason)
	}

	raw, err := os.ReadFile(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var artifact map[string]any
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	delete(artifact, "schema")
	tampered, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	tamperedPath := filepath.Join(t.TempDir(), "schema-stripped.json")
	if err := os.WriteFile(tamperedPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	clean := `{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`
	o := artifactOpts(t, repo, fakeClaude(t, clean), dir)
	o.PriorVerdictPaths = []string{tamperedPath}
	got := Review(context.Background(), o)
	if !got.Degraded || !strings.Contains(got.Reason, "neither a versioned artifact nor a legacy verdict") {
		t.Fatalf("schema-stripped history verdict = %+v", got)
	}
}

func TestReviewImmutableOutPathRefusesDifferentVerdict(t *testing.T) {
	repo := reviewRepo(t)
	path := filepath.Join(t.TempDir(), "general-review-v1.json")
	firstEnvelope := `{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`
	firstOpts := baseOpts(t, repo, fakeClaude(t, firstEnvelope))
	firstOpts.OutPath = path
	first := Review(context.Background(), firstOpts)
	if first.Degraded {
		t.Fatal(first.Reason)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	secondEnvelope := `{"type":"result","result":"{\"blocking\":true,\"findings\":[{\"severity\":\"blocking\",\"summary\":\"new blocker\"}]}"}`
	secondOpts := baseOpts(t, repo, fakeClaude(t, secondEnvelope))
	secondOpts.OutPath = path
	second := Review(context.Background(), secondOpts)
	if !second.Degraded || !strings.Contains(second.Reason, "already exists with different content") {
		t.Fatalf("overwrite verdict = %+v", second)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("immutable OutPath content changed")
	}
}

func TestReviewContentAddressedArtifactConcurrentReplay(t *testing.T) {
	repo := reviewRepo(t)
	dir := t.TempDir()
	envelope := `{"type":"result","result":"{\"blocking\":false,\"findings\":[]}"}`
	bin := fakeClaude(t, envelope)
	base := artifactOpts(t, repo, bin, dir)

	const workers = 6
	results := make(chan Verdict, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- Review(context.Background(), base)
		}()
	}
	wg.Wait()
	close(results)

	path := ""
	for result := range results {
		if result.Degraded {
			t.Fatalf("concurrent replay degraded: %+v", result)
		}
		if path == "" {
			path = result.ArtifactPath
		} else if result.ArtifactPath != path {
			t.Fatalf("identical review produced %q and %q", path, result.ArtifactPath)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
