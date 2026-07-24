// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/phasecontrol"
)

// processPhaseRequests is the orchestrator side of the worker control bridge.
// The worker-authored phase directory is untrusted; an authoritative response
// journal lives one level above worker access and makes restart replay safe.
func (r *runner) processPhaseRequests(ctx context.Context, sl *ledger.Slot) bool {
	phaseDir := r.store.PhaseDir(r.run.RunID, sl.PhaseID)
	requests, err := phasecontrol.ListRequests(phaseDir)
	if err != nil {
		logPhaseRequest(r.run.RunID, r.opts.ProjectID, sl.PhaseID, "", "", phasecontrol.ResponseFailed, "invalid phase control directory")
		return false
	}
	pending := false
	journalDir := r.phaseControlJournalDir(sl.PhaseID)
	for _, envelope := range requests {
		if envelope.Err != nil {
			logPhaseRequest(r.run.RunID, r.opts.ProjectID, sl.PhaseID, envelope.ID, "", phasecontrol.ResponseRejected, "malformed request")
			continue
		}
		req := envelope.Request
		digest, err := phasecontrol.Digest(req)
		if err != nil {
			continue
		}
		if previous, err := phasecontrol.ReadResponseDir(journalDir, req.ID); err == nil {
			if previous.RequestDigest != digest {
				previous = phaseResponse(req, digest, phasecontrol.ResponseRejected, "request id was reused with different content")
			}
			_ = phasecontrol.PublishResponse(phaseDir, previous)
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			logPhaseRequest(r.run.RunID, r.opts.ProjectID, sl.PhaseID, req.ID, req.Operation, phasecontrol.ResponseFailed, "cannot read response journal")
			continue
		}

		var resp phasecontrol.Response
		if req.Operation == phasecontrol.OperationRuntimeCanary {
			var ready bool
			resp, ready = r.pollRuntimeCanaryRequest(ctx, sl, req, digest)
			if !ready {
				pending = true
				continue
			}
		} else {
			resp = r.handlePhaseRequest(ctx, sl, req, digest)
		}
		if err := phasecontrol.WriteResponseDir(journalDir, resp, 0o600); err != nil {
			logPhaseRequest(r.run.RunID, r.opts.ProjectID, sl.PhaseID, req.ID, req.Operation, phasecontrol.ResponseFailed, "cannot persist response journal")
			continue
		}
		if err := phasecontrol.PublishResponse(phaseDir, resp); err != nil {
			logPhaseRequest(r.run.RunID, r.opts.ProjectID, sl.PhaseID, req.ID, req.Operation, phasecontrol.ResponseFailed, "cannot publish response")
			continue
		}
		logPhaseRequest(r.run.RunID, r.opts.ProjectID, sl.PhaseID, req.ID, req.Operation, resp.State, resp.Detail)
	}
	return pending
}

func (r *runner) handlePhaseRequest(ctx context.Context, sl *ledger.Slot, req phasecontrol.Request, digest string) phasecontrol.Response {
	if req.PhaseID != sl.PhaseID {
		return phaseResponse(req, digest, phasecontrol.ResponseRejected, "request does not belong to this phase")
	}
	switch req.Operation {
	case phasecontrol.OperationLabelAdd:
		if req.Runtime != "" {
			return phaseResponse(req, digest, phasecontrol.ResponseRejected, "label request contains unsupported arguments")
		}
		if err := phasecontrol.ValidateSchedulingLabel(req.Label); err != nil {
			return phaseResponse(req, digest, phasecontrol.ResponseRejected, err.Error())
		}
		beadID := sl.BeadID
		if beadID == "" {
			beadID = sl.PhaseID
		}
		if err := r.adapter.AddLabel(ctx, beadID, req.Label); err != nil {
			return phaseResponse(req, digest, phasecontrol.ResponseFailed, "orchestrator could not update bead metadata")
		}
		if issue, ok := r.issues[beadID]; ok && !containsString(issue.Labels, req.Label) {
			issue.Labels = append(issue.Labels, req.Label)
			r.issues[beadID] = issue
		}
		return phaseResponse(req, digest, phasecontrol.ResponseApplied, "added "+req.Label+" to the current bead")
	default:
		return phaseResponse(req, digest, phasecontrol.ResponseRejected, "operation is not available in this binary")
	}
}

func phaseResponse(req phasecontrol.Request, digest, state, detail string) phasecontrol.Response {
	return phasecontrol.Response{
		SchemaVersion: phasecontrol.SchemaVersion,
		ID:            req.ID,
		RequestDigest: digest,
		State:         state,
		Detail:        phasecontrol.SanitizeDetail(detail),
		CompletedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func (r *runner) phaseControlJournalDir(phaseID string) string {
	sum := sha256.Sum256([]byte(phaseID))
	return filepath.Join(r.store.RunDir(r.run.RunID), ".phase-control", hex.EncodeToString(sum[:]))
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
