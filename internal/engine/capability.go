// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/runtimeconfig"
	"github.com/koryph/koryph/internal/version"
	"github.com/koryph/koryph/internal/worktree"
)

const capabilityRetryLimit = 1

// capabilityEvidenceHash binds a hold to non-secret facts that can repair it.
// Mutable tracker status and UpdatedAt are intentionally absent: reopening a
// bead without repairing anything must be a zero-dispatch no-op.
func (r *runner) capabilityEvidenceHash(ctx context.Context, issue beads.Issue, hold ledger.CapabilityHold) string {
	cfg, _ := json.Marshal(r.cfg)
	probeHash := ""
	if hold.ProbePassed {
		probeHash = hold.ProbeHash
	}
	parts := []string{
		issue.ID,
		worktree.BranchFor(issue.ID),
		r.branchHead(ctx, worktree.BranchFor(issue.ID)),
		r.baseCommit(ctx),
		r.effectiveRuntimeName(issue),
		runtimeBinaryFingerprint(r.effectiveRuntimeName(issue)),
		string(cfg),
		EngineVersion,
		version.Build(),
		version.Commit(),
		digestCapabilityPart(issue.Notes),
		hold.OperatorHash,
		probeHash,
	}
	return digestCapabilityPart(strings.Join(parts, "\x00"))
}

func runtimeBinaryFingerprint(name string) string {
	bin := name
	switch name {
	case "claude":
		if override := os.Getenv(runtimeconfig.EnvClaudeBin); override != "" {
			bin = override
		}
	case "codex":
		if override := os.Getenv(runtimeconfig.EnvCodexBin); override != "" {
			bin = override
		}
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return name + ":missing"
	}
	info, err := os.Stat(path)
	if err != nil {
		return name + ":unreadable"
	}
	return fmt.Sprintf("%s:%s:%d:%d", name, path, info.Size(), info.ModTime().UnixNano())
}

func (r *runner) effectiveRuntimeName(issue beads.Issue) string {
	name, _ := r.effectiveRuntimeFor(issue)
	return name
}

func digestCapabilityPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// filterCapabilityHolds removes unchanged held beads before sched.BuildWave.
// Changed evidence is remembered in memory and consumed only by dispatchBead,
// so a candidate deferred by capacity does not spend its one retry.
func (r *runner) filterCapabilityHolds(ctx context.Context, issues []beads.Issue) []beads.Issue {
	out := make([]beads.Issue, 0, len(issues))
	if r.capabilityRetryEvidence == nil {
		r.capabilityRetryEvidence = map[string]string{}
	}
	for _, issue := range issues {
		hold, ok, err := r.store.LoadCapabilityHold(issue.ID)
		if err != nil {
			r.progress("bead %s: capability hold unreadable; dispatch denied: %v", issue.ID, err)
			continue
		}
		if !ok {
			out = append(out, issue)
			continue
		}
		evidence := r.capabilityEvidenceHash(ctx, issue, hold)
		if evidence == hold.EvidenceHash || hold.RetryCount >= hold.RetryLimit {
			delete(r.capabilityRetryEvidence, issue.ID)
			r.progress("bead %s: capability %s remains held (unchanged evidence; zero backend dispatch)", issue.ID, hold.Capability)
			continue
		}
		r.capabilityRetryEvidence[issue.ID] = evidence
		out = append(out, issue)
	}
	return out
}

func (r *runner) consumeCapabilityRetry(beadID string) bool {
	evidence := r.capabilityRetryEvidence[beadID]
	if evidence == "" {
		_, held, err := r.store.LoadCapabilityHold(beadID)
		return err == nil && !held
	}
	ok, err := r.store.ConsumeCapabilityRetry(beadID, evidence)
	if err != nil {
		r.progress("bead %s: capability retry evidence could not be persisted: %v", beadID, err)
		return false
	}
	delete(r.capabilityRetryEvidence, beadID)
	return ok
}

func (r *runner) capabilityDispatchAllowed(beadID string, origin dispatchOrigin) bool {
	if origin == dispatchOriginRequeue {
		return true
	}
	return r.consumeCapabilityRetry(beadID)
}
