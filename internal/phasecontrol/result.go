// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package phasecontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/plan"
)

const (
	ResultSchemaVersion = 1
	ResultFileName      = "result.json"
	maxEvidenceBytes    = 1 << 20
)

// DispatchContext is the trusted, orchestrator-authored identity used to bind
// a terminal result to exactly one implementation dispatch.
type DispatchContext struct {
	RunID     string
	PhaseID   string
	Attempt   int
	SessionID string
	BaseSHA   string
}

// FocusedTestEvidence is one worker-owned, focused validation result. The full
// project gate is deliberately absent: the validation service owns it.
type FocusedTestEvidence struct {
	Command    string `json:"command"`
	ExitStatus int    `json:"exit_status"`
	LogPath    string `json:"log_path"`
	LogDigest  string `json:"log_digest,omitempty"`
}

// EvidenceReference is one typed proof for an acceptance criterion. File
// references are canonicalized and digested by Complete; focused-test
// references must name one successful FocusedTests command.
type EvidenceReference struct {
	Kind    string `json:"kind"` // file | focused-test
	Path    string `json:"path,omitempty"`
	Command string `json:"command,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

// AcceptanceEvidence binds a stable acceptance criterion to concrete files or
// focused checks. The atomic-acceptance unit strengthens this shape further.
type AcceptanceEvidence struct {
	CriterionID string              `json:"criterion_id"`
	References  []EvidenceReference `json:"references"`
}

// Evidence is the structured input supplied to `koryph phase complete`.
type Evidence struct {
	FocusedTests []FocusedTestEvidence `json:"focused_tests,omitempty"`
	Acceptance   []AcceptanceEvidence  `json:"acceptance,omitempty"`
}

// ResultManifest is the only terminal-success artifact accepted by the
// engine. Every identity field is derived from the dispatch manifest and live
// worktree; the worker supplies only the evidence file path.
type ResultManifest struct {
	SchemaVersion  int      `json:"schema_version"`
	State          string   `json:"state"`
	RunID          string   `json:"run_id"`
	PhaseID        string   `json:"phase_id"`
	Attempt        int      `json:"attempt"`
	Generation     string   `json:"dispatch_generation"`
	BaseSHA        string   `json:"base_sha"`
	CandidateSHA   string   `json:"candidate_sha"`
	CommitCount    int      `json:"commit_count"`
	WorktreeClean  bool     `json:"worktree_clean"`
	SummaryPath    string   `json:"summary_path"`
	SummaryDigest  string   `json:"summary_digest"`
	EvidencePath   string   `json:"evidence_path"`
	EvidenceDigest string   `json:"evidence_digest"`
	Evidence       Evidence `json:"evidence"`
	CompletedAt    string   `json:"completed_at"`
}

// CompleteOptions contains trusted dispatch context plus live filesystem
// locations. None of the identity fields are accepted from CLI flags.
type CompleteOptions struct {
	PhaseDir     string
	Worktree     string
	SummaryPath  string
	EvidencePath string
	Dispatch     DispatchContext
}

// ValidationContext is the live engine view against which a result is checked.
type ValidationContext struct {
	PhaseDir      string
	Worktree      string
	Dispatch      DispatchContext
	CandidateSHA  string
	CommitCount   int
	WorktreeClean bool
	// ExpectedCriteria is the strict, issue-owned acceptance contract. The
	// result matrix must contain every ID exactly once and no other IDs.
	ExpectedCriteria []plan.Criterion
}

// CandidateState is the live Git identity observed relative to one dispatch
// base.
type CandidateState struct {
	SHA         string
	CommitCount int
	Clean       bool
}

// DispatchGeneration returns a stable, opaque generation identity. Session ID
// makes two dispatches of the same attempt distinct; base SHA prevents a
// manifest from surviving a rebase.
func DispatchGeneration(c DispatchContext) string {
	h := sha256.New()
	for _, part := range []string{
		strings.TrimSpace(c.RunID),
		strings.TrimSpace(c.PhaseID),
		strconv.Itoa(c.Attempt),
		strings.TrimSpace(c.SessionID),
		strings.TrimSpace(c.BaseSHA),
	} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ResultPath(phaseDir string) string {
	return filepath.Join(phaseDir, ResultFileName)
}

// Complete validates the structured evidence and live git state, then
// atomically writes the terminal result. A dirty/commitless branch never gets
// a success artifact.
func Complete(ctx context.Context, o CompleteOptions) (ResultManifest, error) {
	if err := validateCompleteOptions(o); err != nil {
		return ResultManifest{}, err
	}
	base, err := gitOutput(ctx, o.Worktree, "rev-parse", o.Dispatch.BaseSHA+"^{commit}")
	if err != nil {
		return ResultManifest{}, fmt.Errorf("phase complete: resolve dispatch base: %w", err)
	}
	if base != strings.TrimSpace(o.Dispatch.BaseSHA) {
		return ResultManifest{}, errors.New("phase complete: dispatch base is not a canonical commit SHA")
	}
	state, err := InspectCandidate(ctx, o.Worktree, base)
	if err != nil {
		return ResultManifest{}, fmt.Errorf("phase complete: inspect candidate: %w", err)
	}
	if state.CommitCount == 0 {
		return ResultManifest{}, errors.New("phase complete: candidate has no commits beyond the dispatch base")
	}
	if !state.Clean {
		return ResultManifest{}, errors.New("phase complete: worktree is not clean")
	}

	summary, err := fsx.ReadRegularConfined(
		o.SummaryPath, maxEvidenceBytes, o.PhaseDir, o.Worktree,
	)
	if err != nil {
		return ResultManifest{}, fmt.Errorf("phase complete: summary: %w", err)
	}
	evidence, _, _, err := loadEvidence(
		o.EvidencePath, o.PhaseDir, o.Worktree,
	)
	if err != nil {
		return ResultManifest{}, fmt.Errorf("phase complete: evidence: %w", err)
	}
	evidence, evidencePath, evidenceDigest, err := snapshotEvidence(
		o.PhaseDir, o.Worktree, o.Dispatch, evidence,
	)
	if err != nil {
		return ResultManifest{}, fmt.Errorf("phase complete: snapshot evidence: %w", err)
	}

	result := ResultManifest{
		SchemaVersion:  ResultSchemaVersion,
		State:          "done",
		RunID:          strings.TrimSpace(o.Dispatch.RunID),
		PhaseID:        strings.TrimSpace(o.Dispatch.PhaseID),
		Attempt:        o.Dispatch.Attempt,
		Generation:     DispatchGeneration(o.Dispatch),
		BaseSHA:        base,
		CandidateSHA:   state.SHA,
		CommitCount:    state.CommitCount,
		WorktreeClean:  true,
		SummaryPath:    summary.Path,
		SummaryDigest:  summary.Digest,
		EvidencePath:   evidencePath,
		EvidenceDigest: evidenceDigest,
		Evidence:       evidence,
		CompletedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := fsx.WriteJSONAtomic(ResultPath(o.PhaseDir), result); err != nil {
		return ResultManifest{}, fmt.Errorf("phase complete: write result: %w", err)
	}
	return result, nil
}

func snapshotEvidence(
	phaseDir string,
	worktree string,
	dispatch DispatchContext,
	evidence Evidence,
) (Evidence, string, string, error) {
	for i := range evidence.FocusedTests {
		test := &evidence.FocusedTests[i]
		source, err := fsx.ReadRegularConfined(
			test.LogPath, maxEvidenceBytes, phaseDir, worktree,
		)
		if err != nil {
			return Evidence{}, "", "", err
		}
		if source.Digest != test.LogDigest {
			return Evidence{}, "", "", errors.New("focused test log changed during completion")
		}
		snapshotPath := filepath.Join(phaseDir, "result-log-"+source.Digest+".log")
		if err := fsx.WriteAtomicNoClobber(snapshotPath, source.Data, 0o600); err != nil &&
			!errors.Is(err, os.ErrExist) {
			return Evidence{}, "", "", err
		}
		snapshot, err := fsx.ReadRegularConfined(
			snapshotPath, maxEvidenceBytes, phaseDir,
		)
		if err != nil || snapshot.Digest != source.Digest {
			return Evidence{}, "", "", errors.New("focused test log snapshot differs from authenticated source")
		}
		test.LogPath = snapshot.Path
		test.LogDigest = snapshot.Digest
	}

	generation := DispatchGeneration(dispatch)
	evidencePath := filepath.Join(phaseDir, "result-evidence-"+generation+".json")
	if err := fsx.WriteJSONAtomicNoClobberPerm(evidencePath, evidence, 0o600); err != nil &&
		!errors.Is(err, os.ErrExist) {
		return Evidence{}, "", "", err
	}
	read, err := fsx.ReadRegularConfined(evidencePath, maxEvidenceBytes, phaseDir)
	if err != nil {
		return Evidence{}, "", "", err
	}
	var persisted Evidence
	if err := json.Unmarshal(read.Data, &persisted); err != nil {
		return Evidence{}, "", "", err
	}
	left, _ := json.Marshal(evidence)
	right, _ := json.Marshal(persisted)
	if string(left) != string(right) {
		return Evidence{}, "", "", errors.New("existing canonical evidence differs from current completion")
	}
	return persisted, read.Path, read.Digest, nil
}

// InspectCandidate returns the full candidate SHA, commit count relative to
// the dispatch base, and live cleanliness used by both the completion command
// and engine validation.
func InspectCandidate(ctx context.Context, worktree, baseSHA string) (CandidateState, error) {
	candidate, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return CandidateState{}, fmt.Errorf("candidate HEAD: %w", err)
	}
	if _, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: worktree, Name: "git",
		Args: []string{"merge-base", "--is-ancestor", strings.TrimSpace(baseSHA), candidate},
	}); err != nil {
		return CandidateState{}, errors.New("dispatch base is not an ancestor of candidate HEAD")
	}
	countText, err := gitOutput(ctx, worktree, "rev-list", "--count", strings.TrimSpace(baseSHA)+".."+candidate)
	if err != nil {
		return CandidateState{}, fmt.Errorf("candidate commits: %w", err)
	}
	count, err := strconv.Atoi(countText)
	if err != nil {
		return CandidateState{}, fmt.Errorf("parse candidate commit count: %w", err)
	}
	status, err := gitOutput(ctx, worktree, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return CandidateState{}, fmt.Errorf("worktree status: %w", err)
	}
	return CandidateState{SHA: candidate, CommitCount: count, Clean: status == ""}, nil
}

func LoadResult(phaseDir string) (ResultManifest, error) {
	read, err := fsx.ReadRegularConfined(ResultFileName, maxEvidenceBytes, phaseDir)
	if err != nil {
		return ResultManifest{}, err
	}
	var result ResultManifest
	if err := json.Unmarshal(read.Data, &result); err != nil {
		return ResultManifest{}, err
	}
	return result, nil
}

// ValidateResult proves a terminal artifact still describes the current slot,
// candidate, base, and evidence. It is intentionally strict: stale or
// malformed manifests are preserved for diagnosis but never grandfathered.
func ValidateResult(result ResultManifest, want ValidationContext) error {
	switch {
	case result.SchemaVersion != ResultSchemaVersion:
		return fmt.Errorf("unsupported result schema version %d", result.SchemaVersion)
	case result.State != "done":
		return fmt.Errorf("result state is %q, want done", result.State)
	case result.RunID != strings.TrimSpace(want.Dispatch.RunID):
		return errors.New("result run does not match current run")
	case result.PhaseID != strings.TrimSpace(want.Dispatch.PhaseID):
		return errors.New("result phase does not match current phase")
	case result.Attempt != want.Dispatch.Attempt:
		return errors.New("result attempt does not match current attempt")
	case result.Generation != DispatchGeneration(want.Dispatch):
		return errors.New("result dispatch generation is stale")
	case result.BaseSHA != strings.TrimSpace(want.Dispatch.BaseSHA):
		return errors.New("result base SHA does not match dispatch base")
	case result.CandidateSHA != strings.TrimSpace(want.CandidateSHA):
		return errors.New("result candidate SHA does not match live branch")
	case result.CommitCount <= 0 || result.CommitCount != want.CommitCount:
		return errors.New("result commit count does not match nonzero live commit count")
	case !result.WorktreeClean || !want.WorktreeClean:
		return errors.New("result requires a clean live worktree")
	}
	summary, err := fsx.ReadRegularConfined(
		result.SummaryPath, maxEvidenceBytes, want.PhaseDir, want.Worktree,
	)
	if err != nil || summary.Path != filepath.Clean(result.SummaryPath) ||
		summary.Digest != result.SummaryDigest {
		return errors.New("result summary digest does not match")
	}
	evidence, evidencePath, digest, err := loadEvidence(
		result.EvidencePath, want.PhaseDir, want.Worktree,
	)
	if err != nil || evidencePath != filepath.Clean(result.EvidencePath) ||
		digest != result.EvidenceDigest {
		return errors.New("result evidence digest does not match")
	}
	left, _ := json.Marshal(evidence)
	right, _ := json.Marshal(result.Evidence)
	if string(left) != string(right) {
		return errors.New("result embedded evidence does not match evidence file")
	}
	if err := validateAcceptanceMatrix(evidence.Acceptance, want.ExpectedCriteria); err != nil {
		return err
	}
	return nil
}

func validateCompleteOptions(o CompleteOptions) error {
	switch {
	case strings.TrimSpace(o.PhaseDir) == "":
		return errors.New("phase complete: phase directory is required")
	case strings.TrimSpace(o.Worktree) == "":
		return errors.New("phase complete: worktree is required")
	case strings.TrimSpace(o.Dispatch.RunID) == "":
		return errors.New("phase complete: run id is required")
	case strings.TrimSpace(o.Dispatch.PhaseID) == "":
		return errors.New("phase complete: phase id is required")
	case o.Dispatch.Attempt <= 0:
		return errors.New("phase complete: positive attempt is required")
	case strings.TrimSpace(o.Dispatch.SessionID) == "":
		return errors.New("phase complete: session id is required")
	case strings.TrimSpace(o.Dispatch.BaseSHA) == "":
		return errors.New("phase complete: base SHA is required")
	case strings.TrimSpace(o.SummaryPath) == "":
		return errors.New("phase complete: summary path is required")
	case strings.TrimSpace(o.EvidencePath) == "":
		return errors.New("phase complete: evidence path is required")
	}
	return nil
}

func loadEvidence(path, phaseDir, worktree string) (Evidence, string, string, error) {
	read, err := fsx.ReadRegularConfined(path, maxEvidenceBytes, phaseDir, worktree)
	if err != nil {
		return Evidence{}, "", "", err
	}
	var evidence Evidence
	if err := json.Unmarshal(read.Data, &evidence); err != nil {
		return Evidence{}, "", "", fmt.Errorf("invalid JSON: %w", err)
	}
	if len(evidence.Acceptance) == 0 {
		return Evidence{}, "", "", errors.New("acceptance evidence matrix is required")
	}
	for i := range evidence.FocusedTests {
		test := &evidence.FocusedTests[i]
		test.Command = strings.TrimSpace(test.Command)
		if test.Command == "" {
			return Evidence{}, "", "", errors.New("focused test command is required")
		}
		if test.ExitStatus != 0 {
			return Evidence{}, "", "", fmt.Errorf("focused test %q failed with exit status %d", test.Command, test.ExitStatus)
		}
		logRead, err := fsx.ReadRegularConfined(
			test.LogPath, maxEvidenceBytes, phaseDir, worktree,
		)
		if err != nil {
			return Evidence{}, "", "", fmt.Errorf("focused test log: %w", err)
		}
		test.LogPath = logRead.Path
		test.LogDigest = logRead.Digest
	}
	successfulTests := make(map[string]bool, len(evidence.FocusedTests))
	for _, test := range evidence.FocusedTests {
		successfulTests[test.Command] = true
	}
	seen := map[string]bool{}
	for i := range evidence.Acceptance {
		item := &evidence.Acceptance[i]
		item.CriterionID = strings.TrimSpace(item.CriterionID)
		if item.CriterionID == "" || seen[item.CriterionID] {
			return Evidence{}, "", "", errors.New("acceptance criterion ids must be nonempty and unique")
		}
		seen[item.CriterionID] = true
		if len(item.References) == 0 {
			return Evidence{}, "", "", fmt.Errorf("acceptance criterion %s has no evidence", item.CriterionID)
		}
		for j := range item.References {
			ref := &item.References[j]
			ref.Kind = strings.TrimSpace(ref.Kind)
			switch ref.Kind {
			case "file":
				if strings.TrimSpace(ref.Command) != "" {
					return Evidence{}, "", "", fmt.Errorf("acceptance criterion %s file reference contains a command", item.CriterionID)
				}
				// Worker-authored relative file references are worktree
				// relative; phase-local paths remain available explicitly or
				// as the fallback root.
				refRead, err := fsx.ReadRegularConfined(
					ref.Path, maxEvidenceBytes, worktree, phaseDir,
				)
				if err != nil {
					return Evidence{}, "", "", fmt.Errorf("acceptance criterion %s file reference: %w", item.CriterionID, err)
				}
				ref.Path = refRead.Path
				ref.Digest = refRead.Digest
			case "focused-test":
				ref.Command = strings.TrimSpace(ref.Command)
				if ref.Command == "" || !successfulTests[ref.Command] {
					return Evidence{}, "", "", fmt.Errorf("acceptance criterion %s references an unknown or unsuccessful focused test", item.CriterionID)
				}
				if strings.TrimSpace(ref.Path) != "" || strings.TrimSpace(ref.Digest) != "" {
					return Evidence{}, "", "", fmt.Errorf("acceptance criterion %s focused-test reference contains file fields", item.CriterionID)
				}
			default:
				return Evidence{}, "", "", fmt.Errorf("acceptance criterion %s has unsupported evidence kind %q", item.CriterionID, ref.Kind)
			}
		}
	}
	return evidence, read.Path, read.Digest, nil
}

func validateAcceptanceMatrix(got []AcceptanceEvidence, expected []plan.Criterion) error {
	if len(expected) == 0 {
		return errors.New("result cannot be accepted without strict expected acceptance criteria")
	}
	if len(got) != len(expected) {
		return fmt.Errorf("result acceptance matrix has %d criteria, want %d", len(got), len(expected))
	}
	seen := make(map[string]bool, len(got))
	for _, item := range got {
		if seen[item.CriterionID] {
			return fmt.Errorf("result acceptance criterion %s is duplicated", item.CriterionID)
		}
		seen[item.CriterionID] = true
	}
	for _, criterion := range expected {
		if !seen[criterion.ID] {
			return fmt.Errorf("result acceptance criterion %s is missing", criterion.ID)
		}
	}
	for id := range seen {
		found := false
		for _, criterion := range expected {
			if criterion.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("result acceptance criterion %s is unknown", id)
		}
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{Dir: dir, Name: "git", Args: args})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}
