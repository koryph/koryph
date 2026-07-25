// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/signing"
	"github.com/koryph/koryph/internal/textx"
	"github.com/koryph/koryph/internal/worktree"
)

var verifySignatures = signing.Verify

// ValidateGateEvidence validates the self-describing fields before an engine
// admits persisted bytes into its trusted ledger. Freshness against git refs
// and the configured gate is checked separately by EvidenceCurrent.
func ValidateGateEvidence(evidence *GateEvidence) error {
	if evidence == nil {
		return errors.New("gate evidence is nil")
	}
	if evidence.Schema != GateEvidenceSchema {
		return fmt.Errorf("unsupported gate evidence schema %q", evidence.Schema)
	}
	for name, sha := range map[string]string{
		"candidate_sha": evidence.CandidateSHA,
		"base_sha":      evidence.BaseSHA,
	} {
		decoded, err := hex.DecodeString(strings.TrimSpace(sha))
		if err != nil || (len(decoded) != 20 && len(decoded) != 32) {
			return fmt.Errorf("gate evidence %s is not a full commit SHA", name)
		}
	}
	for name, digest := range map[string]string{
		"diff_digest":        evidence.DiffDigest,
		"gate_config_digest": evidence.GateConfigDigest,
		"command_digest":     evidence.CommandDigest,
	} {
		raw := strings.TrimPrefix(strings.TrimSpace(digest), "sha256:")
		decoded, err := hex.DecodeString(raw)
		if !strings.HasPrefix(strings.TrimSpace(digest), "sha256:") || err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("gate evidence %s is not a sha256 digest", name)
		}
	}
	if strings.TrimSpace(evidence.EngineVersion) == "" ||
		strings.TrimSpace(evidence.BuildIdentity) == "" {
		return errors.New("gate evidence lacks engine version or build identity")
	}
	if evidence.CompletedAt.IsZero() {
		return errors.New("gate evidence lacks completion time")
	}
	return nil
}

type gateConfigKey struct {
	Gate                []string     `json:"gate"`
	Protected           []string     `json:"protected"`
	RequireSigned       bool         `json:"require_signed"`
	RequireConventional bool         `json:"require_conventional"`
	Reconcilers         []Reconciler `json:"reconcilers"`
	Prepare             []string     `json:"prepare"`
}

func validationDigests(o Opts) (configDigest, commandDigest string, err error) {
	commandDigest, err = digestJSON(append([]string(nil), o.Gate...))
	if err != nil {
		return "", "", err
	}
	configDigest, err = digestJSON(gateConfigKey{
		Gate:                append([]string(nil), o.Gate...),
		Protected:           append([]string(nil), o.Extra...),
		RequireSigned:       o.RequireSigned,
		RequireConventional: o.RequireConventional,
		Reconcilers:         append([]Reconciler(nil), o.Reconcilers...),
		Prepare:             append([]string(nil), o.Prepare...),
	})
	return configDigest, commandDigest, err
}

func digestJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func gitCommit(ctx context.Context, dir, ref string) (string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: dir, Name: "git", Args: []string{"rev-parse", "--verify", ref + "^{commit}"},
	})
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(res.Stdout)
	if len(sha) != 40 && len(sha) != 64 {
		return "", fmt.Errorf("git ref %q resolved to invalid commit %q", ref, sha)
	}
	return sha, nil
}

func diffDigest(ctx context.Context, dir, base, candidate string) (string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: dir, Name: "git",
		Args: []string{"diff", "--binary", "--full-index", base + "..." + candidate},
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(res.Stdout))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func buildGateEvidence(
	ctx context.Context,
	o Opts,
	wt *worktree.Info,
	baseSHA string,
) (*GateEvidence, error) {
	if strings.TrimSpace(o.EngineVersion) == "" || strings.TrimSpace(o.BuildIdentity) == "" {
		return nil, errors.New("validation evidence requires engine version and exact build identity")
	}
	candidateSHA, err := gitCommit(ctx, wt.Path, o.Branch)
	if err != nil {
		return nil, err
	}
	diff, err := diffDigest(ctx, wt.Path, baseSHA, candidateSHA)
	if err != nil {
		return nil, err
	}
	configDigest, commandDigest, err := validationDigests(o)
	if err != nil {
		return nil, err
	}
	return &GateEvidence{
		Schema: GateEvidenceSchema, CandidateSHA: candidateSHA, BaseSHA: baseSHA,
		DiffDigest: diff, GateConfigDigest: configDigest, CommandDigest: commandDigest,
		EngineVersion: strings.TrimSpace(o.EngineVersion),
		BuildIdentity: strings.TrimSpace(o.BuildIdentity),
		CompletedAt:   time.Now().UTC(),
	}, nil
}

func buildValidationCheckpoint(
	ctx context.Context,
	o Opts,
	wt *worktree.Info,
	baseSHA string,
	reconciled []string,
	reconcileRounds int,
	prepared bool,
) (*ValidationCheckpoint, error) {
	if strings.TrimSpace(o.EngineVersion) == "" || strings.TrimSpace(o.BuildIdentity) == "" {
		return nil, errors.New("validation checkpoint requires engine version and exact build identity")
	}
	candidateSHA, err := gitCommit(ctx, wt.Path, o.Branch)
	if err != nil {
		return nil, err
	}
	configDigest, commandDigest, err := validationDigests(o)
	if err != nil {
		return nil, err
	}
	return &ValidationCheckpoint{
		CandidateSHA: candidateSHA, BaseSHA: baseSHA,
		GateConfigDigest: configDigest, CommandDigest: commandDigest,
		EngineVersion:   strings.TrimSpace(o.EngineVersion),
		BuildIdentity:   strings.TrimSpace(o.BuildIdentity),
		Reconciled:      append([]string(nil), reconciled...),
		ReconcileRounds: reconcileRounds,
		Prepared:        prepared,
	}, nil
}

func validationCheckpointCurrent(
	ctx context.Context,
	o Opts,
	wt *worktree.Info,
	checkpoint *ValidationCheckpoint,
) (bool, error) {
	if checkpoint == nil || checkpoint.CompletedAt.IsZero() ||
		checkpoint.EngineVersion != strings.TrimSpace(o.EngineVersion) ||
		checkpoint.BuildIdentity != strings.TrimSpace(o.BuildIdentity) {
		return false, nil
	}
	configDigest, commandDigest, err := validationDigests(o)
	if err != nil {
		return false, err
	}
	if checkpoint.GateConfigDigest != configDigest ||
		checkpoint.CommandDigest != commandDigest {
		return false, nil
	}
	if dirty, err := candidateWorktreeDirty(ctx, wt.Path); err != nil || dirty {
		return false, err
	}
	candidateSHA, err := gitCommit(ctx, wt.Path, o.Branch)
	if err != nil || candidateSHA != checkpoint.CandidateSHA {
		return false, err
	}
	baseSHA, err := gitCommit(ctx, wt.Path, checkpoint.BaseSHA)
	return err == nil && baseSHA == checkpoint.BaseSHA, err
}

func buildGateEvidenceFromCheckpoint(
	ctx context.Context,
	o Opts,
	wt *worktree.Info,
	checkpoint *ValidationCheckpoint,
) (*GateEvidence, error) {
	evidence, err := buildGateEvidence(ctx, o, wt, checkpoint.BaseSHA)
	if err != nil {
		return nil, err
	}
	evidence.CompletedAt = checkpoint.CompletedAt
	if evidence.CandidateSHA != checkpoint.CandidateSHA ||
		evidence.BaseSHA != checkpoint.BaseSHA ||
		evidence.GateConfigDigest != checkpoint.GateConfigDigest ||
		evidence.CommandDigest != checkpoint.CommandDigest ||
		evidence.EngineVersion != checkpoint.EngineVersion ||
		evidence.BuildIdentity != checkpoint.BuildIdentity {
		return nil, errors.New("successful-gate checkpoint changed before evidence construction")
	}
	return evidence, nil
}

func persistGateEvidence(path string, evidence *GateEvidence) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("validation evidence path is required")
	}
	if err := ValidateGateEvidence(evidence); err != nil {
		return err
	}
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(path, append(data, '\n'), 0o600)
}

// EvidenceCurrent performs the read-only authentication used by engine resume.
// It never lands, rebases, or updates the local default branch.
func EvidenceCurrent(ctx context.Context, o Opts, evidence *GateEvidence) (bool, error) {
	if ValidateGateEvidence(evidence) != nil ||
		evidence.EngineVersion != strings.TrimSpace(o.EngineVersion) ||
		evidence.BuildIdentity != strings.TrimSpace(o.BuildIdentity) {
		return false, nil
	}
	configDigest, commandDigest, err := validationDigests(o)
	if err != nil {
		return false, err
	}
	if evidence.GateConfigDigest != configDigest || evidence.CommandDigest != commandDigest {
		return false, nil
	}
	list, err := worktree.List(ctx, o.RepoRoot)
	if err != nil {
		return false, err
	}
	var wt *worktree.Info
	for i := range list {
		if list[i].Branch == o.Branch {
			wt = &list[i]
			break
		}
	}
	if wt == nil {
		return false, nil
	}
	if dirty, err := candidateWorktreeDirty(ctx, wt.Path); err != nil || dirty {
		return false, err
	}
	def := o.DefaultBranch
	if def == "" {
		def = "main"
	}
	hasRemote, err := remoteExists(ctx, o.RepoRoot)
	if err != nil {
		return false, err
	}
	if hasRemote {
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.RepoRoot, Name: "git", Args: []string{"fetch", "origin", def},
		}); err != nil {
			return false, err
		}
		remoteSHA, err := gitCommit(ctx, o.RepoRoot, "origin/"+def)
		if err != nil || remoteSHA != evidence.BaseSHA {
			return false, err
		}
	}
	baseSHA, err := gitCommit(ctx, o.RepoRoot, def)
	if err != nil || baseSHA != evidence.BaseSHA {
		return false, err
	}
	candidateSHA, err := gitCommit(ctx, wt.Path, o.Branch)
	if err != nil || candidateSHA != evidence.CandidateSHA {
		return false, err
	}
	diff, err := diffDigest(ctx, wt.Path, baseSHA, candidateSHA)
	return err == nil && diff == evidence.DiffDigest, err
}

// landValidated performs only the short, evidence-authenticated mutation path.
// Its caller already holds the merge slot and has resolved the candidate
// worktree. No rebase, prepare command, or gate may occur here.
func landValidated(
	ctx context.Context,
	o Opts,
	wt *worktree.Info,
	def string,
) (Result, error) {
	evidence := o.Validated
	if evidence == nil {
		return Result{Status: StatusError}, errors.New("validated landing requires gate evidence")
	}
	if err := ValidateGateEvidence(evidence); err != nil {
		return Result{Status: StatusEvidenceStale, GateOutput: err.Error()}, nil
	}
	if strings.TrimSpace(evidence.EngineVersion) == "" ||
		strings.TrimSpace(evidence.EngineVersion) != strings.TrimSpace(o.EngineVersion) ||
		strings.TrimSpace(evidence.BuildIdentity) == "" ||
		strings.TrimSpace(evidence.BuildIdentity) != strings.TrimSpace(o.BuildIdentity) {
		return Result{Status: StatusEvidenceStale, GateOutput: "gate evidence schema, engine version, or build identity changed"}, nil
	}
	configDigest, commandDigest, err := validationDigests(o)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	if configDigest != evidence.GateConfigDigest || commandDigest != evidence.CommandDigest {
		return Result{Status: StatusEvidenceStale, GateOutput: "gate configuration or command digest changed"}, nil
	}

	if dirty, err := candidateWorktreeDirty(ctx, wt.Path); err != nil {
		return Result{Status: StatusError}, err
	} else if dirty {
		return Result{Status: StatusDirty, GateOutput: "candidate worktree changed after validation; work preserved"}, nil
	}
	candidateSHA, err := gitCommit(ctx, wt.Path, o.Branch)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	if candidateSHA != evidence.CandidateSHA {
		return Result{Status: StatusEvidenceStale, GateOutput: "candidate branch moved after validation"}, nil
	}
	diff, err := diffDigest(ctx, wt.Path, evidence.BaseSHA, candidateSHA)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	if diff != evidence.DiffDigest {
		return Result{Status: StatusEvidenceStale, GateOutput: "candidate diff changed after validation"}, nil
	}

	hasRemote, err := remoteExists(ctx, o.RepoRoot)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	remoteSHA := ""
	if hasRemote {
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.RepoRoot, Name: "git", Args: []string{"fetch", "origin", def},
		}); err != nil {
			return Result{Status: StatusError}, err
		}
		remoteSHA, err = gitCommit(ctx, o.RepoRoot, "origin/"+def)
		if err != nil {
			return Result{Status: StatusError}, err
		}
	}
	if _, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.RepoRoot, Name: "git", Args: []string{"checkout", def},
	}); err != nil {
		return Result{Status: StatusError}, err
	}
	localSHA, err := gitCommit(ctx, o.RepoRoot, def)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	localContainsCandidate, ancestorErr := commitAncestor(
		ctx, o.RepoRoot, evidence.CandidateSHA, localSHA,
	)
	if ancestorErr != nil {
		return Result{Status: StatusError}, ancestorErr
	}
	if localContainsCandidate {
		// A prior landing attempt completed the local ff but lost its push
		// response (or the engine died before applying the result). Resume only
		// the missing publication/cleanup; never rerun the gate or dispatch a
		// model. The immutable evidence and diff were authenticated above.
		result := Result{Status: StatusMerged, MergedSHA: candidateSHA}
		if hasRemote {
			remoteContainsCandidate, err := commitAncestor(
				ctx, o.RepoRoot, evidence.CandidateSHA, remoteSHA,
			)
			if err != nil {
				return Result{Status: StatusError}, err
			}
			if remoteContainsCandidate {
				result.Pushed = true
			} else if o.Push {
				// Recovery authority may publish exactly CandidateSHA, never a
				// stronger local default containing unrelated, ungated commits.
				if localSHA != evidence.CandidateSHA {
					return Result{
						Status: StatusRecoveryUnsafe,
						GateOutput: "local default contains the validated candidate plus " +
							"additional commits that have no gate/review evidence",
					}, nil
				}
				if remoteSHA != evidence.BaseSHA {
					// The first landing completed its local ff, then its push
					// failed while origin advanced independently. Revalidation
					// can proceed only from the old evidence base. Rewind local
					// default solely when it is EXACTLY CandidateSHA, origin is
					// a descendant of the old base, the root worktree is clean,
					// and the candidate is still retained on its own branch.
					// Any stronger local state is operator work and must park.
					remoteAdvancedFromBase, err := commitAncestor(
						ctx, o.RepoRoot, evidence.BaseSHA, remoteSHA,
					)
					if err != nil {
						return Result{Status: StatusError}, err
					}
					if !remoteAdvancedFromBase {
						return Result{
							Status: StatusRecoveryUnsafe,
							GateOutput: "remote default diverged after local landing, but local default " +
								"cannot be safely restored to the validated base",
						}, nil
					}
					if dirty, err := trackedWorktreeDirty(ctx, o.RepoRoot); err != nil {
						return Result{Status: StatusError}, err
					} else if dirty {
						return Result{
							Status: StatusRecoveryUnsafe,
							GateOutput: "remote default advanced after local landing, but the local " +
								"default worktree has uncommitted state",
						}, nil
					}
					if _, err := execx.MustSucceed(ctx, execx.Cmd{
						Dir: o.RepoRoot, Name: "git",
						Args: []string{"reset", "--hard", evidence.BaseSHA},
					}); err != nil {
						return Result{Status: StatusError}, fmt.Errorf(
							"restore local %s to validated base for revalidation: %w", def, err,
						)
					}
					retainedSHA, err := gitCommit(ctx, wt.Path, o.Branch)
					if err != nil {
						return Result{Status: StatusError}, err
					}
					if retainedSHA != evidence.CandidateSHA {
						return Result{
							Status:     StatusRecoveryUnsafe,
							GateOutput: "candidate branch was not retained during local-default restoration",
						}, nil
					}
					return Result{
						Status: StatusEvidenceStale,
						GateOutput: "remote default advanced after partial push; local default " +
							"restored to validated base for engine-owned revalidation",
					}, nil
				}
				if _, err := execx.MustSucceed(ctx, execx.Cmd{
					Dir: o.RepoRoot, Name: "git", Args: []string{"push", "origin", def},
				}); err != nil {
					return result, fmt.Errorf("push origin %s after local landing: %w", def, err)
				}
				result.Pushed = true
			}
		}
		cleanupValidatedLanding(ctx, o, wt, &result)
		return result, nil
	}
	if hasRemote {
		if remoteSHA != evidence.BaseSHA {
			if localSHA != evidence.BaseSHA {
				return Result{
					Status: StatusRecoveryUnsafe,
					GateOutput: "remote default advanced after validation while local default " +
						"was neither the validated base nor the exact retained candidate",
				}, nil
			}
			return Result{Status: StatusEvidenceStale, GateOutput: "remote default branch advanced without matching local landing"}, nil
		}
		ff, err := gitRun(ctx, o.RepoRoot, "merge", "--ff-only", "origin/"+def)
		if err != nil {
			return Result{Status: StatusError}, err
		}
		if ff.ExitCode != 0 {
			return Result{Status: StatusEvidenceStale, GateOutput: "local default branch diverged after validation"}, nil
		}
	}
	baseSHA, err := gitCommit(ctx, o.RepoRoot, def)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	if baseSHA != evidence.BaseSHA || candidateSHA != evidence.CandidateSHA {
		return Result{Status: StatusEvidenceStale, GateOutput: "candidate or default branch moved after validation"}, nil
	}
	diff, err = diffDigest(ctx, wt.Path, baseSHA, candidateSHA)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	if diff != evidence.DiffDigest {
		return Result{Status: StatusEvidenceStale, GateOutput: "candidate diff changed after validation"}, nil
	}

	// Re-run the cheap fail-closed checks at the mutation boundary. This does
	// not repeat the expensive gate and closes races in protected paths,
	// signatures, and commit policy.
	if res, ok, err := preflight(ctx, o, wt, def); !ok {
		return res, err
	}
	if o.RequireSigned {
		bad, err := verifySignatures(ctx, wt.Path, def, o.Branch)
		if err != nil {
			return Result{Status: StatusError}, err
		}
		if len(bad) > 0 {
			return Result{
				Status: StatusUnsigned,
				GateOutput: "unsigned or unverifiable commits after validation:\n" +
					strings.Join(bad, "\n"),
			}, nil
		}
	}

	if o.OpenPR {
		return openPR(ctx, o, def, hasRemote)
	}
	if o.Squash {
		return Result{Status: StatusEvidenceStale, GateOutput: "validated landing does not permit SHA-rewriting squash"}, nil
	}
	ff, err := gitRun(ctx, o.RepoRoot, "merge", "--ff-only", o.Branch)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	if ff.ExitCode != 0 {
		return Result{
			Status:     StatusEvidenceStale,
			GateOutput: textx.Tail(ff.Stdout+ff.Stderr, 2000),
		}, nil
	}
	result := Result{Status: StatusMerged, MergedSHA: candidateSHA}
	if o.Push && hasRemote {
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.RepoRoot, Name: "git", Args: []string{"push", "origin", def},
		}); err != nil {
			return result, fmt.Errorf("push origin %s: %w", def, err)
		}
		result.Pushed = true
	}
	cleanupValidatedLanding(ctx, o, wt, &result)
	return result, nil
}

// trackedWorktreeDirty protects state that `git reset --hard` would destroy.
// Untracked files are intentionally ignored: reset --hard leaves them intact,
// and the engine's own ignored/private run state may live under the root.
func trackedWorktreeDirty(ctx context.Context, dir string) (bool, error) {
	result, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: dir, Name: "git",
		Args: []string{"status", "--porcelain", "--untracked-files=no"},
	})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(result.Stdout) != "", nil
}

func commitAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	result, err := gitRun(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		return false, err
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("test commit ancestry %s..%s: %s",
			ancestor, descendant, strings.TrimSpace(result.Stderr))
	}
}

func cleanupValidatedLanding(ctx context.Context, o Opts, wt *worktree.Info, result *Result) {
	if o.KeepWorktree {
		return
	}
	if err := worktree.Remove(ctx, wt.Path, false); err != nil {
		result.GateOutput = "cleanup-warning: worktree kept: " + err.Error()
	} else if err := worktree.DeleteBranch(ctx, o.RepoRoot, o.Branch); err != nil {
		result.GateOutput = "cleanup-warning: branch kept: " + err.Error()
	}
}
