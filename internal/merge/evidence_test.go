// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/worktree"
)

func validationFixture(t *testing.T, gate string) (repoPath, worktreePath string, opts Opts) {
	t.Helper()
	isolateGit(t)
	repo := initRepo(t)
	wt := worktreeOn(t, repo, "agent/evidence")
	commitIn(t, wt.Path, "candidate.txt", "candidate\n", "feat(test): candidate")
	phase := t.TempDir()
	opts = Opts{
		RepoRoot: repo, Branch: "agent/evidence", DefaultBranch: "main",
		Gate: []string{gate}, SlotOwner: "validator", Slot: &fakeSlot{},
		ValidationPhaseDir: phase, ValidateOnly: true, KeepWorktree: true,
		EngineVersion: "0.test", BuildIdentity: "commit:test",
		EvidencePath: filepath.Join(phase, "gate-evidence.json"),
	}
	return repo, wt.Path, opts
}

func TestValidateThenLandRunsOneGateAndUsesShortSlot(t *testing.T) {
	starts := filepath.Join(t.TempDir(), "gate-starts")
	repo, wt, opts := validationFixture(t, "echo gate >>"+shellTestQuote(starts))
	mainBefore := headOf(t, repo, "main")

	validated, err := Merge(context.Background(), opts)
	if err != nil || validated.Status != StatusValidated || validated.Evidence == nil {
		t.Fatalf("Validate = (%+v, %v)", validated, err)
	}
	if got := headOf(t, repo, "main"); got != mainBefore {
		t.Fatalf("validation moved main: %s != %s", got, mainBefore)
	}
	slot := opts.Slot.(*fakeSlot)
	if slot.acquired != 1 || slot.released != 1 {
		t.Fatalf("validation base slot = %d/%d, want 1/1", slot.acquired, slot.released)
	}
	var persisted GateEvidence
	raw, err := os.ReadFile(opts.EvidencePath)
	if err != nil || json.Unmarshal(raw, &persisted) != nil {
		t.Fatalf("persisted evidence: %v", err)
	}
	if persisted.CandidateSHA != headOf(t, wt, "HEAD") ||
		persisted.BaseSHA != mainBefore ||
		persisted.BuildIdentity != opts.BuildIdentity {
		t.Fatalf("persisted evidence = %+v", persisted)
	}

	landing := opts
	landing.ValidateOnly = false
	landing.Validated = validated.Evidence
	landing.Push = false
	landing.Slot = &fakeSlot{}
	landed, err := Merge(context.Background(), landing)
	if err != nil || landed.Status != StatusMerged {
		t.Fatalf("Land = (%+v, %v)", landed, err)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "gate") != 1 {
		t.Fatalf("gate starts = %q, %v; want one", data, err)
	}
	if got := headOf(t, repo, "main"); got != validated.Evidence.CandidateSHA {
		t.Fatalf("landed main = %s, want %s", got, validated.Evidence.CandidateSHA)
	}
}

func TestValidatedLandingRejectsMovedBaseWithoutRegating(t *testing.T) {
	starts := filepath.Join(t.TempDir(), "gate-starts")
	repo, _, opts := validationFixture(t, "echo gate >>"+shellTestQuote(starts))
	validated, err := Merge(context.Background(), opts)
	if err != nil || validated.Status != StatusValidated {
		t.Fatalf("Validate = (%+v, %v)", validated, err)
	}
	commitIn(t, repo, "main-moved.txt", "moved\n", "chore(test): move base")
	moved := headOf(t, repo, "main")

	landing := opts
	landing.ValidateOnly = false
	landing.Validated = validated.Evidence
	landing.Slot = &fakeSlot{}
	res, err := Merge(context.Background(), landing)
	if err != nil || res.Status != StatusEvidenceStale {
		t.Fatalf("Land moved base = (%+v, %v), want evidence stale", res, err)
	}
	if got := headOf(t, repo, "main"); got != moved {
		t.Fatalf("stale landing moved main: %s != %s", got, moved)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "gate") != 1 {
		t.Fatalf("gate starts after stale landing = %q, %v; want one", data, err)
	}
}

func TestValidateFailureRunsGateExactlyOnce(t *testing.T) {
	starts := filepath.Join(t.TempDir(), "gate-starts")
	_, _, opts := validationFixture(t, "echo gate >>"+shellTestQuote(starts)+"; false")
	res, err := Merge(context.Background(), opts)
	if err != nil || res.Status != StatusGateFailed {
		t.Fatalf("Validate failure = (%+v, %v)", res, err)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "gate") != 1 {
		t.Fatalf("failed gate starts = %q, %v; want one", data, err)
	}
	if _, err := os.Stat(opts.EvidencePath); !os.IsNotExist(err) {
		t.Fatalf("failed validation persisted evidence: %v", err)
	}
}

func TestPostGateTailFailuresResumeCheckpointWithoutRegating(t *testing.T) {
	tests := []struct {
		name      string
		configure func(t *testing.T, opts *Opts)
	}{
		{
			name: "signature infrastructure",
			configure: func(t *testing.T, opts *Opts) {
				opts.RequireSigned = true
				original := verifySignatures
				calls := 0
				verifySignatures = func(
					context.Context, string, string, string,
				) ([]string, error) {
					calls++
					if calls == 2 {
						return nil, errors.New("transient signature verifier failure")
					}
					return nil, nil
				}
				t.Cleanup(func() { verifySignatures = original })
			},
		},
		{
			name: "evidence construction infrastructure",
			configure: func(t *testing.T, _ *Opts) {
				original := buildValidationEvidence
				calls := 0
				buildValidationEvidence = func(
					ctx context.Context,
					opts Opts,
					wt *worktree.Info,
					checkpoint *ValidationCheckpoint,
				) (*GateEvidence, error) {
					calls++
					if calls == 1 {
						return nil, errors.New("transient evidence construction failure")
					}
					return original(ctx, opts, wt, checkpoint)
				}
				t.Cleanup(func() { buildValidationEvidence = original })
			},
		},
		{
			name: "evidence persistence infrastructure",
			configure: func(t *testing.T, _ *Opts) {
				original := persistValidationEvidence
				calls := 0
				persistValidationEvidence = func(path string, evidence *GateEvidence) error {
					calls++
					if calls == 1 {
						return errors.New("transient evidence persistence failure")
					}
					return original(path, evidence)
				}
				t.Cleanup(func() { persistValidationEvidence = original })
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			starts := filepath.Join(t.TempDir(), "gate-starts")
			_, _, opts := validationFixture(
				t, "echo gate >>"+shellTestQuote(starts),
			)
			tc.configure(t, &opts)

			first, err := Merge(t.Context(), opts)
			if err == nil || first.ValidationCheckpoint == nil {
				t.Fatalf("first post-gate failure = (%+v, %v)", first, err)
			}
			opts.ValidationCheckpoint = first.ValidationCheckpoint
			second, err := Merge(t.Context(), opts)
			if err != nil || second.Status != StatusValidated {
				t.Fatalf("checkpoint retry = (%+v, %v)", second, err)
			}
			data, err := os.ReadFile(starts)
			if err != nil || strings.Count(string(data), "gate") != 1 {
				t.Fatalf("gate starts = %q, %v; want exactly one", data, err)
			}
		})
	}
}

func TestValidateGateEvidenceRejectsMalformedIdentity(t *testing.T) {
	valid := &GateEvidence{
		Schema:           GateEvidenceSchema,
		CandidateSHA:     strings.Repeat("a", 40),
		BaseSHA:          strings.Repeat("b", 40),
		DiffDigest:       "sha256:" + strings.Repeat("1", 64),
		GateConfigDigest: "sha256:" + strings.Repeat("2", 64),
		CommandDigest:    "sha256:" + strings.Repeat("3", 64),
		EngineVersion:    "test", BuildIdentity: "binary=test",
		CompletedAt: time.Now().UTC(),
	}
	if err := ValidateGateEvidence(valid); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
	for _, mutate := range []func(*GateEvidence){
		func(e *GateEvidence) { e.Schema = "unknown" },
		func(e *GateEvidence) { e.CandidateSHA = "short" },
		func(e *GateEvidence) { e.DiffDigest = "sha256:short" },
		func(e *GateEvidence) { e.CompletedAt = time.Time{} },
	} {
		changed := *valid
		mutate(&changed)
		if err := ValidateGateEvidence(&changed); err == nil {
			t.Fatalf("malformed evidence accepted: %+v", changed)
		}
	}
}

func TestValidatedLandingRecoversLocalMergeAfterPushFailure(t *testing.T) {
	isolateGit(t)
	repo, bare := initRepoWithRemote(t)
	wt := worktreeOn(t, repo, "agent/partial-push")
	commitIn(t, wt.Path, "candidate.txt", "candidate\n", "feat(test): candidate")
	candidate := headOf(t, wt.Path, "HEAD")
	starts := filepath.Join(t.TempDir(), "gate-starts")
	evidenceDir := t.TempDir()
	opts := Opts{
		RepoRoot: repo, Branch: "agent/partial-push", DefaultBranch: "main",
		Gate:               []string{"echo gate >>" + shellTestQuote(starts)},
		ValidationPhaseDir: evidenceDir, ValidateOnly: true, KeepWorktree: true,
		EngineVersion: "0.test", BuildIdentity: "commit:test",
		EvidencePath: filepath.Join(evidenceDir, "gate-evidence.json"),
		SlotOwner:    "validator", Slot: &fakeSlot{},
	}
	validated, err := Merge(t.Context(), opts)
	if err != nil || validated.Status != StatusValidated {
		t.Fatalf("validate = (%+v, %v)", validated, err)
	}

	marker := filepath.Join(t.TempDir(), "rejected-once")
	hook := filepath.Join(bare, "hooks", "pre-receive")
	script := "#!/bin/sh\n" +
		"if [ ! -f " + shellTestQuote(marker) + " ]; then\n" +
		"  touch " + shellTestQuote(marker) + "\n" +
		"  echo transient push failure >&2\n" +
		"  exit 1\n" +
		"fi\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	landing := opts
	landing.ValidateOnly = false
	landing.Validated = validated.Evidence
	landing.Push = true
	landing.Slot = &fakeSlot{}
	first, err := Merge(t.Context(), landing)
	if err == nil || first.Status != StatusMerged || first.MergedSHA != candidate {
		t.Fatalf("first partial landing = (%+v, %v)", first, err)
	}
	if got := headOf(t, repo, "main"); got != candidate {
		t.Fatalf("local main = %s, want locally landed %s", got, candidate)
	}

	second, err := Merge(t.Context(), landing)
	if err != nil || second.Status != StatusMerged || !second.Pushed {
		t.Fatalf("resumed landing = (%+v, %v)", second, err)
	}
	remote := strings.Fields(mustGit(t, repo, "ls-remote", "origin", "refs/heads/main"))
	if len(remote) == 0 || remote[0] != candidate {
		t.Fatalf("remote main = %v, want %s", remote, candidate)
	}

	// Recovery remains idempotent after the published default branch advances:
	// ancestry, not equality with CandidateSHA, proves that this candidate was
	// already landed.
	commitIn(t, repo, "later.txt", "later\n", "chore(test): advance landed default")
	mustGit(t, repo, "push", "origin", "main")
	third, err := Merge(t.Context(), landing)
	if err != nil || third.Status != StatusMerged || !third.Pushed ||
		third.MergedSHA != candidate {
		t.Fatalf("resumed landing after default advanced = (%+v, %v)", third, err)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "gate") != 1 {
		t.Fatalf("gate ran again during push recovery: %q, %v", data, err)
	}
}

func TestValidatedLandingRefusesUngatedCommitAfterLocalCandidate(t *testing.T) {
	isolateGit(t)
	repo, _ := initRepoWithRemote(t)
	wt := worktreeOn(t, repo, "agent/ungated-local")
	commitIn(t, wt.Path, "candidate.txt", "candidate\n", "feat(test): candidate")
	evidenceDir := t.TempDir()
	opts := Opts{
		RepoRoot: repo, Branch: "agent/ungated-local", DefaultBranch: "main",
		Gate: []string{"true"}, ValidationPhaseDir: evidenceDir,
		ValidateOnly: true, KeepWorktree: true,
		EngineVersion: "0.test", BuildIdentity: "commit:test",
		EvidencePath: filepath.Join(evidenceDir, "gate-evidence.json"),
		SlotOwner:    "validator", Slot: &fakeSlot{},
	}
	validated, err := Merge(t.Context(), opts)
	if err != nil || validated.Status != StatusValidated {
		t.Fatalf("validate = (%+v, %v)", validated, err)
	}
	mustGit(t, repo, "merge", "--ff-only", validated.Evidence.CandidateSHA)
	commitIn(t, repo, "ungated.txt", "operator\n", "chore(test): ungated local")
	localTip := headOf(t, repo, "main")

	landing := opts
	landing.ValidateOnly = false
	landing.Validated = validated.Evidence
	landing.Push = true
	landing.Slot = &fakeSlot{}
	result, err := Merge(t.Context(), landing)
	if err != nil || result.Status != StatusRecoveryUnsafe {
		t.Fatalf("unsafe local recovery = (%+v, %v)", result, err)
	}
	if got := headOf(t, repo, "main"); got != localTip {
		t.Fatalf("unsafe recovery changed local main: got %s want %s", got, localTip)
	}
	remote := strings.Fields(mustGit(t, repo, "ls-remote", "origin", "refs/heads/main"))
	if len(remote) == 0 || remote[0] != validated.Evidence.BaseSHA {
		t.Fatalf("unsafe recovery pushed ungated local tip: %v", remote)
	}
}

func TestValidatedLandingRestoresExactPartialLandingForAdvancedOriginRevalidation(t *testing.T) {
	isolateGit(t)
	repo, bare := initRepoWithRemote(t)
	wt := worktreeOn(t, repo, "agent/advanced-origin")
	commitIn(t, wt.Path, "candidate.txt", "candidate\n", "feat(test): candidate")
	originalCandidate := headOf(t, wt.Path, "HEAD")
	starts := filepath.Join(t.TempDir(), "gate-starts")
	evidenceDir := t.TempDir()
	opts := Opts{
		RepoRoot: repo, Branch: "agent/advanced-origin", DefaultBranch: "main",
		Gate:               []string{"echo gate >>" + shellTestQuote(starts)},
		ValidationPhaseDir: t.TempDir(), ValidateOnly: true, KeepWorktree: true,
		EngineVersion: "0.test", BuildIdentity: "commit:test",
		EvidencePath: filepath.Join(evidenceDir, "gate-evidence.json"),
		SlotOwner:    "validator", Slot: &fakeSlot{},
	}
	firstEvidence, err := Merge(t.Context(), opts)
	if err != nil || firstEvidence.Status != StatusValidated {
		t.Fatalf("first validation = (%+v, %v)", firstEvidence, err)
	}

	marker := filepath.Join(t.TempDir(), "rejected-once")
	hook := filepath.Join(bare, "hooks", "pre-receive")
	script := "#!/bin/sh\n" +
		"if [ ! -f " + shellTestQuote(marker) + " ]; then\n" +
		"  touch " + shellTestQuote(marker) + "\n" +
		"  echo transient push failure >&2\n" +
		"  exit 1\n" +
		"fi\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	landing := opts
	landing.ValidateOnly = false
	landing.Validated = firstEvidence.Evidence
	landing.Push = true
	landing.Slot = &fakeSlot{}
	partial, err := Merge(t.Context(), landing)
	if err == nil || partial.Status != StatusMerged {
		t.Fatalf("partial landing = (%+v, %v)", partial, err)
	}
	if got := headOf(t, repo, "main"); got != originalCandidate {
		t.Fatalf("local main after partial landing = %s, want %s", got, originalCandidate)
	}

	externalRoot := t.TempDir()
	mustGit(t, externalRoot, "clone", "-q", bare, "external")
	external := filepath.Join(externalRoot, "external")
	mustGit(t, external, "config", "user.email", "external@example.com")
	mustGit(t, external, "config", "user.name", "External Writer")
	mustGit(t, external, "config", "commit.gpgsign", "false")
	commitIn(t, external, "remote.txt", "remote\n", "chore(test): advance origin")
	advancedRemote := headOf(t, external, "HEAD")
	mustGit(t, external, "push", "-q", "origin", "main")

	stale, err := Merge(t.Context(), landing)
	if err != nil || stale.Status != StatusEvidenceStale {
		t.Fatalf("advanced-origin recovery = (%+v, %v), want evidence stale", stale, err)
	}
	if got := headOf(t, repo, "main"); got != firstEvidence.Evidence.BaseSHA {
		t.Fatalf("restored local main = %s, want old base %s", got, firstEvidence.Evidence.BaseSHA)
	}
	if got := headOf(t, wt.Path, "agent/advanced-origin"); got != originalCandidate {
		t.Fatalf("retained candidate branch = %s, want %s", got, originalCandidate)
	}

	opts.Slot = &fakeSlot{}
	secondEvidence, err := Merge(t.Context(), opts)
	if err != nil || secondEvidence.Status != StatusValidated {
		t.Fatalf("revalidation = (%+v, %v)", secondEvidence, err)
	}
	if secondEvidence.Evidence.BaseSHA != advancedRemote ||
		secondEvidence.Evidence.CandidateSHA == originalCandidate {
		t.Fatalf("revalidation evidence = %+v, remote=%s original=%s",
			secondEvidence.Evidence, advancedRemote, originalCandidate)
	}
	landing.Validated = secondEvidence.Evidence
	landing.Slot = &fakeSlot{}
	landed, err := Merge(t.Context(), landing)
	if err != nil || landed.Status != StatusMerged || !landed.Pushed {
		t.Fatalf("revalidated landing = (%+v, %v)", landed, err)
	}
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "gate") != 2 {
		t.Fatalf("gate starts = %q, %v; want one per old/new tuple", data, err)
	}
}

func TestValidatedLandingParksWhenAdvancedLocalCannotBeSafelyRestored(t *testing.T) {
	isolateGit(t)
	repo, bare := initRepoWithRemote(t)
	wt := worktreeOn(t, repo, "agent/unsafe-recovery")
	commitIn(t, wt.Path, "candidate.txt", "candidate\n", "feat(test): candidate")
	opts := Opts{
		RepoRoot: repo, Branch: "agent/unsafe-recovery", DefaultBranch: "main",
		Gate: []string{"true"}, ValidationPhaseDir: t.TempDir(),
		ValidateOnly: true, KeepWorktree: true,
		EngineVersion: "0.test", BuildIdentity: "commit:test",
		EvidencePath: filepath.Join(t.TempDir(), "gate-evidence.json"),
		SlotOwner:    "validator", Slot: &fakeSlot{},
	}
	validated, err := Merge(t.Context(), opts)
	if err != nil || validated.Status != StatusValidated {
		t.Fatalf("validation = (%+v, %v)", validated, err)
	}
	landing := opts
	landing.ValidateOnly = false
	landing.Validated = validated.Evidence
	landing.Slot = &fakeSlot{}
	landed, err := Merge(t.Context(), landing)
	if err != nil || landed.Status != StatusMerged {
		t.Fatalf("local landing = (%+v, %v)", landed, err)
	}
	commitIn(t, repo, "local-later.txt", "local\n", "chore(test): advance local")
	localAdvanced := headOf(t, repo, "main")

	externalRoot := t.TempDir()
	mustGit(t, externalRoot, "clone", "-q", bare, "external")
	external := filepath.Join(externalRoot, "external")
	mustGit(t, external, "config", "user.email", "external@example.com")
	mustGit(t, external, "config", "user.name", "External Writer")
	mustGit(t, external, "config", "commit.gpgsign", "false")
	commitIn(t, external, "remote.txt", "remote\n", "chore(test): advance origin")
	mustGit(t, external, "push", "-q", "origin", "main")

	landing.Push = true
	landing.Slot = &fakeSlot{}
	unsafe, err := Merge(t.Context(), landing)
	if err != nil || unsafe.Status != StatusRecoveryUnsafe {
		t.Fatalf("unsafe recovery = (%+v, %v)", unsafe, err)
	}
	if got := headOf(t, repo, "main"); got != localAdvanced {
		t.Fatalf("unsafe recovery moved local default: %s != %s", got, localAdvanced)
	}
}
