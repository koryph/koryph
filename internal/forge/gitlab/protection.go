// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/koryph/koryph/internal/forge"
)

// gitlabProtectionSvc implements [forge.ProtectionService] for GitLab using
// the GitLab REST API v4.
//
// GitLab does not have a unified "ruleset" concept; instead it exposes three
// orthogonal protection APIs that this service aggregates behind the
// [forge.ProtectionService] interface:
//
//  1. Protected branches — /projects/:id/protected_branches
//  2. Push rules        — /projects/:id/push_rule (singleton resource)
//  3. Approval rules    — /projects/:id/approval_rules
//
// # ID scheme
//
// The opaque Ruleset.ID encodes the resource type as a prefix so [Get],
// [Update], and [Delete] can route to the correct API without extra
// round-trips:
//
//   - "pb:<url-encoded-branch-name>" → protected branch
//   - "push-rules"                   → project push rule singleton
//   - "ar:<numeric-id>"              → approval rule
//
// # Group-level targets
//
// The [forge.ProtectionService] contract allows a bare group/org name as
// target (no "/" separator).  GitLab group-level push rules require GitLab
// Premium; this service returns [forge.ErrUnsupported] for those paths.
//
// Self-managed instances are served by KORYPH_GITLAB_HOST (via [prAPIBase]).
type gitlabProtectionSvc struct{}

// idPrefixPB is the Ruleset.ID prefix for protected branches.
const idPrefixPB = "pb:"

// idPushRules is the fixed Ruleset.ID for the push-rule singleton.
const idPushRules = "push-rules"

// idPrefixAR is the Ruleset.ID prefix for approval rules.
const idPrefixAR = "ar:"

// ---------- ProtectionService methods -----------------------------------------

// List returns all protection resources for the project.  It aggregates
// protected branches, push rules (if any exist), and approval rules into a
// single slice of [forge.Ruleset] values.
//
// target must be "owner/repo" — group-level targets return [forge.ErrUnsupported].
func (s *gitlabProtectionSvc) List(ctx context.Context, target string) ([]forge.Ruleset, error) {
	if !strings.Contains(target, "/") {
		return nil, fmt.Errorf("gitlab protection: list %q: %w: group-level protection not yet supported", target, forge.ErrUnsupported)
	}
	owner, repo := splitTarget(target)

	var out []forge.Ruleset

	// 1. Protected branches.
	pbList, err := s.listProtectedBranches(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	out = append(out, pbList...)

	// 2. Push rule singleton — omit from list when absent (404).
	pr, err := s.getPushRule(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	if pr != nil {
		out = append(out, *pr)
	}

	// 3. Approval rules.
	arList, err := s.listApprovalRules(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	out = append(out, arList...)

	return out, nil
}

// Get returns one protection resource by its opaque ID.
//
// target must be "owner/repo".
func (s *gitlabProtectionSvc) Get(ctx context.Context, target, id string) (*forge.Ruleset, error) {
	if !strings.Contains(target, "/") {
		return nil, fmt.Errorf("gitlab protection: get %q: %w: group-level protection not yet supported", target, forge.ErrUnsupported)
	}
	owner, repo := splitTarget(target)

	switch {
	case strings.HasPrefix(id, idPrefixPB):
		branch := strings.TrimPrefix(id, idPrefixPB)
		return s.getProtectedBranch(ctx, owner, repo, branch)
	case id == idPushRules:
		rs, err := s.getPushRule(ctx, owner, repo)
		if err != nil {
			return nil, err
		}
		if rs == nil {
			return nil, fmt.Errorf("gitlab protection: get push-rules for %s: no push rule configured", target)
		}
		return rs, nil
	case strings.HasPrefix(id, idPrefixAR):
		arID := strings.TrimPrefix(id, idPrefixAR)
		return s.getApprovalRule(ctx, owner, repo, arID)
	default:
		return nil, fmt.Errorf("gitlab protection: get %q for %s: unrecognised ID format", id, target)
	}
}

// Create creates a new protection resource and returns it with its assigned ID.
//
// The resource type is inferred from rs.Name:
//   - name starting with "pb:" → protected branch (branch name is the suffix)
//   - name == "push-rules"     → project push rule singleton
//   - any other name           → approval rule
//
// target must be "owner/repo".
func (s *gitlabProtectionSvc) Create(ctx context.Context, target string, rs *forge.Ruleset) (*forge.Ruleset, error) {
	if !strings.Contains(target, "/") {
		return nil, fmt.Errorf("gitlab protection: create %q: %w: group-level protection not yet supported", target, forge.ErrUnsupported)
	}
	owner, repo := splitTarget(target)

	switch {
	case strings.HasPrefix(rs.Name, idPrefixPB):
		return s.createProtectedBranch(ctx, owner, repo, rs)
	case rs.Name == idPushRules:
		return s.createPushRule(ctx, owner, repo, rs)
	default:
		return s.createApprovalRule(ctx, owner, repo, rs)
	}
}

// Update replaces an existing protection resource.  rs.ID must be set.
//
// target must be "owner/repo".
func (s *gitlabProtectionSvc) Update(ctx context.Context, target string, rs *forge.Ruleset) error {
	if !strings.Contains(target, "/") {
		return fmt.Errorf("gitlab protection: update %q: %w: group-level protection not yet supported", target, forge.ErrUnsupported)
	}
	owner, repo := splitTarget(target)

	switch {
	case strings.HasPrefix(rs.ID, idPrefixPB):
		return s.updateProtectedBranch(ctx, owner, repo, rs)
	case rs.ID == idPushRules:
		return s.updatePushRule(ctx, owner, repo, rs)
	case strings.HasPrefix(rs.ID, idPrefixAR):
		arID := strings.TrimPrefix(rs.ID, idPrefixAR)
		return s.updateApprovalRule(ctx, owner, repo, arID, rs)
	default:
		return fmt.Errorf("gitlab protection: update %q for %s: unrecognised ID format", rs.ID, target)
	}
}

// Delete removes a protection resource permanently.
//
// target must be "owner/repo".
func (s *gitlabProtectionSvc) Delete(ctx context.Context, target, id string) error {
	if !strings.Contains(target, "/") {
		return fmt.Errorf("gitlab protection: delete %q: %w: group-level protection not yet supported", target, forge.ErrUnsupported)
	}
	owner, repo := splitTarget(target)

	switch {
	case strings.HasPrefix(id, idPrefixPB):
		branch := strings.TrimPrefix(id, idPrefixPB)
		return s.deleteProtectedBranch(ctx, owner, repo, branch)
	case id == idPushRules:
		return s.deletePushRule(ctx, owner, repo)
	case strings.HasPrefix(id, idPrefixAR):
		arID := strings.TrimPrefix(id, idPrefixAR)
		return s.deleteApprovalRule(ctx, owner, repo, arID)
	default:
		return fmt.Errorf("gitlab protection: delete %q for %s: unrecognised ID format", id, target)
	}
}

// ---------- Protected branches -----------------------------------------------

// glProtectedBranch is the GitLab protected-branch API response shape.
type glProtectedBranch struct {
	Name             string `json:"name"`
	PushAccessLevels []struct {
		AccessLevel            int    `json:"access_level"`
		AccessLevelDescription string `json:"access_level_description"`
	} `json:"push_access_levels"`
	MergeAccessLevels []struct {
		AccessLevel            int    `json:"access_level"`
		AccessLevelDescription string `json:"access_level_description"`
	} `json:"merge_access_levels"`
	AllowForcePush            bool `json:"allow_force_push"`
	CodeOwnerApprovalRequired bool `json:"code_owner_approval_required"`
}

func (s *gitlabProtectionSvc) listProtectedBranches(ctx context.Context, owner, repo string) ([]forge.Ruleset, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/protected_branches?per_page=100",
		prAPIBase(), glPIDPath(owner, repo))
	raw, err := glExpect(ctx, http.MethodGet, apiURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab protection: list protected branches %s/%s: %w", owner, repo, err)
	}
	var branches []glProtectedBranch
	if err := json.Unmarshal(raw, &branches); err != nil {
		return nil, fmt.Errorf("gitlab protection: parse protected branches: %w", err)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("gitlab protection: parse protected branches array: %w", err)
	}

	out := make([]forge.Ruleset, 0, len(branches))
	for i, b := range branches {
		var rawItem []byte
		if i < len(items) {
			rawItem = items[i]
		}
		out = append(out, forge.Ruleset{
			ID:   idPrefixPB + b.Name,
			Name: idPrefixPB + b.Name,
			Raw:  rawItem,
		})
	}
	return out, nil
}

func (s *gitlabProtectionSvc) getProtectedBranch(ctx context.Context, owner, repo, branch string) (*forge.Ruleset, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/protected_branches/%s",
		prAPIBase(), glPIDPath(owner, repo), url.PathEscape(branch))
	raw, err := glExpect(ctx, http.MethodGet, apiURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab protection: get protected branch %q for %s/%s: %w", branch, owner, repo, err)
	}
	return &forge.Ruleset{
		ID:   idPrefixPB + branch,
		Name: idPrefixPB + branch,
		Raw:  raw,
	}, nil
}

// createProtectedBranch POSTs a new protected-branch record.
// rs.Name must be "pb:<branch-name>"; rs.Raw is the POST body.
func (s *gitlabProtectionSvc) createProtectedBranch(ctx context.Context, owner, repo string, rs *forge.Ruleset) (*forge.Ruleset, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/protected_branches",
		prAPIBase(), glPIDPath(owner, repo))
	raw, err := glExpect(ctx, http.MethodPost, apiURL, rs.Raw, http.StatusCreated)
	if err != nil {
		return nil, fmt.Errorf("gitlab protection: create protected branch %q for %s/%s: %w", rs.Name, owner, repo, err)
	}
	var created glProtectedBranch
	if err := json.Unmarshal(raw, &created); err != nil {
		return nil, fmt.Errorf("gitlab protection: parse created protected branch: %w", err)
	}
	return &forge.Ruleset{
		ID:   idPrefixPB + created.Name,
		Name: idPrefixPB + created.Name,
		Raw:  raw,
	}, nil
}

// updateProtectedBranch deletes then re-creates the protected branch.
// GitLab does not provide a PATCH/PUT for protected branches; delete + create
// is the official workaround.
//
// Safety: the existing branch config is fetched before deletion.  If the
// re-create step fails, a best-effort restore is attempted using the
// previously captured config so the branch is not left permanently unprotected.
func (s *gitlabProtectionSvc) updateProtectedBranch(ctx context.Context, owner, repo string, rs *forge.Ruleset) error {
	branch := strings.TrimPrefix(rs.ID, idPrefixPB)

	// Capture existing config before any destructive step.
	prior, fetchErr := s.getProtectedBranch(ctx, owner, repo, branch)

	if err := s.deleteProtectedBranch(ctx, owner, repo, branch); err != nil {
		return fmt.Errorf("gitlab protection: update protected branch %q (delete step): %w", branch, err)
	}

	_, createErr := s.createProtectedBranch(ctx, owner, repo, rs)
	if createErr != nil {
		// Attempt rollback using the prior config.
		if fetchErr == nil && prior != nil && prior.Raw != nil {
			restoreRS := &forge.Ruleset{Name: prior.Name, Raw: prior.Raw}
			if _, restoreErr := s.createProtectedBranch(ctx, owner, repo, restoreRS); restoreErr != nil {
				return fmt.Errorf("gitlab protection: update protected branch %q failed and rollback failed: create: %w; restore: %v",
					branch, createErr, restoreErr)
			}
			return fmt.Errorf("gitlab protection: update protected branch %q (create step, rolled back): %w",
				branch, createErr)
		}
		return fmt.Errorf("gitlab protection: update protected branch %q (create step, branch is now unprotected): %w",
			branch, createErr)
	}
	return nil
}

func (s *gitlabProtectionSvc) deleteProtectedBranch(ctx context.Context, owner, repo, branch string) error {
	apiURL := fmt.Sprintf("%s/projects/%s/protected_branches/%s",
		prAPIBase(), glPIDPath(owner, repo), url.PathEscape(branch))
	if _, err := glExpect(ctx, http.MethodDelete, apiURL, nil, http.StatusNoContent); err != nil {
		return fmt.Errorf("gitlab protection: delete protected branch %q for %s/%s: %w", branch, owner, repo, err)
	}
	return nil
}

// ---------- Push rules -------------------------------------------------------

func (s *gitlabProtectionSvc) getPushRule(ctx context.Context, owner, repo string) (*forge.Ruleset, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/push_rule",
		prAPIBase(), glPIDPath(owner, repo))
	raw, code, err := glDo(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab protection: get push rule %s/%s: %w", owner, repo, err)
	}
	if code == http.StatusNotFound {
		return nil, nil // no push rule configured; caller treats nil as absent
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("gitlab protection: get push rule %s/%s: HTTP %d: %s", owner, repo, code, strings.TrimSpace(string(raw)))
	}
	return &forge.Ruleset{
		ID:   idPushRules,
		Name: idPushRules,
		Raw:  raw,
	}, nil
}

// createPushRule POSTs a new push rule.  GitLab uses POST to create the
// singleton; subsequent modifications use PUT.
func (s *gitlabProtectionSvc) createPushRule(ctx context.Context, owner, repo string, rs *forge.Ruleset) (*forge.Ruleset, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/push_rule",
		prAPIBase(), glPIDPath(owner, repo))
	raw, err := glExpect(ctx, http.MethodPost, apiURL, rs.Raw, http.StatusCreated)
	if err != nil {
		return nil, fmt.Errorf("gitlab protection: create push rule for %s/%s: %w", owner, repo, err)
	}
	return &forge.Ruleset{
		ID:   idPushRules,
		Name: idPushRules,
		Raw:  raw,
	}, nil
}

// updatePushRule applies changes to the existing push rule via PUT.
func (s *gitlabProtectionSvc) updatePushRule(ctx context.Context, owner, repo string, rs *forge.Ruleset) error {
	apiURL := fmt.Sprintf("%s/projects/%s/push_rule",
		prAPIBase(), glPIDPath(owner, repo))
	if _, err := glExpect(ctx, http.MethodPut, apiURL, rs.Raw, http.StatusOK); err != nil {
		return fmt.Errorf("gitlab protection: update push rule for %s/%s: %w", owner, repo, err)
	}
	return nil
}

// deletePushRule removes the project push rule.
func (s *gitlabProtectionSvc) deletePushRule(ctx context.Context, owner, repo string) error {
	apiURL := fmt.Sprintf("%s/projects/%s/push_rule",
		prAPIBase(), glPIDPath(owner, repo))
	if _, err := glExpect(ctx, http.MethodDelete, apiURL, nil, http.StatusNoContent); err != nil {
		return fmt.Errorf("gitlab protection: delete push rule for %s/%s: %w", owner, repo, err)
	}
	return nil
}

// ---------- Approval rules ---------------------------------------------------

// glApprovalRule is the GitLab approval-rule API response shape.
type glApprovalRule struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	ApprovalsRequired int    `json:"approvals_required"`
}

func (s *gitlabProtectionSvc) listApprovalRules(ctx context.Context, owner, repo string) ([]forge.Ruleset, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/approval_rules",
		prAPIBase(), glPIDPath(owner, repo))
	raw, err := glExpect(ctx, http.MethodGet, apiURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab protection: list approval rules %s/%s: %w", owner, repo, err)
	}
	var rules []glApprovalRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("gitlab protection: parse approval rules: %w", err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("gitlab protection: parse approval rules array: %w", err)
	}

	out := make([]forge.Ruleset, 0, len(rules))
	for i, r := range rules {
		var rawItem []byte
		if i < len(items) {
			rawItem = items[i]
		}
		out = append(out, forge.Ruleset{
			ID:   fmt.Sprintf("%s%d", idPrefixAR, r.ID),
			Name: r.Name,
			Raw:  rawItem,
		})
	}
	return out, nil
}

func (s *gitlabProtectionSvc) getApprovalRule(ctx context.Context, owner, repo, arID string) (*forge.Ruleset, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/approval_rules/%s",
		prAPIBase(), glPIDPath(owner, repo), url.PathEscape(arID))
	raw, err := glExpect(ctx, http.MethodGet, apiURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab protection: get approval rule %s for %s/%s: %w", arID, owner, repo, err)
	}
	var rule glApprovalRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		return nil, fmt.Errorf("gitlab protection: parse approval rule %s: %w", arID, err)
	}
	return &forge.Ruleset{
		ID:   fmt.Sprintf("%s%d", idPrefixAR, rule.ID),
		Name: rule.Name,
		Raw:  raw,
	}, nil
}

// createApprovalRule POSTs a new approval rule.
func (s *gitlabProtectionSvc) createApprovalRule(ctx context.Context, owner, repo string, rs *forge.Ruleset) (*forge.Ruleset, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/approval_rules",
		prAPIBase(), glPIDPath(owner, repo))
	raw, err := glExpect(ctx, http.MethodPost, apiURL, rs.Raw, http.StatusCreated, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab protection: create approval rule %q for %s/%s: %w", rs.Name, owner, repo, err)
	}
	var created glApprovalRule
	if err := json.Unmarshal(raw, &created); err != nil {
		return nil, fmt.Errorf("gitlab protection: parse created approval rule: %w", err)
	}
	return &forge.Ruleset{
		ID:   fmt.Sprintf("%s%d", idPrefixAR, created.ID),
		Name: created.Name,
		Raw:  raw,
	}, nil
}

// updateApprovalRule replaces an existing approval rule via PUT.
func (s *gitlabProtectionSvc) updateApprovalRule(ctx context.Context, owner, repo, arID string, rs *forge.Ruleset) error {
	apiURL := fmt.Sprintf("%s/projects/%s/approval_rules/%s",
		prAPIBase(), glPIDPath(owner, repo), url.PathEscape(arID))
	if _, err := glExpect(ctx, http.MethodPut, apiURL, rs.Raw, http.StatusOK); err != nil {
		return fmt.Errorf("gitlab protection: update approval rule %s for %s/%s: %w", arID, owner, repo, err)
	}
	return nil
}

// deleteApprovalRule removes an approval rule by ID.
func (s *gitlabProtectionSvc) deleteApprovalRule(ctx context.Context, owner, repo, arID string) error {
	apiURL := fmt.Sprintf("%s/projects/%s/approval_rules/%s",
		prAPIBase(), glPIDPath(owner, repo), url.PathEscape(arID))
	if _, err := glExpect(ctx, http.MethodDelete, apiURL, nil, http.StatusNoContent); err != nil {
		return fmt.Errorf("gitlab protection: delete approval rule %s for %s/%s: %w", arID, owner, repo, err)
	}
	return nil
}

// ---------- helpers -----------------------------------------------------------

// splitTarget splits "owner/repo" into (owner, repo).
// Callers should validate the "/" is present before calling.
func splitTarget(target string) (owner, repo string) {
	parts := strings.SplitN(target, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return target, ""
}

// Compile-time interface check.
var _ forge.ProtectionService = (*gitlabProtectionSvc)(nil)
