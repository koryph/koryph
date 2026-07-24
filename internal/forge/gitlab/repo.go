// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/koryph/koryph/internal/forge"
)

// gitlabRepoSvc implements [forge.RepoService] for GitLab using the GitLab
// REST API v4.
//
// Authentication: reads KORYPH_GITLAB_TOKEN for the private access token
// (same env var as [gitlabPRSvc]).
//
// The primary endpoint is GET/PUT /projects/:id; project path is encoded
// as "owner%2Frepo" via [glPIDPath].
//
// Feature-level mappings:
//
//   - AllowMergeCommit → merge_method == "merge"
//   - AllowRebaseMerge → merge_method == "rebase_merge" or "ff"
//   - AllowSquashMerge → squash_option != "never" (and non-empty)
//
// VulnAlerts and ActionsWorkflow return [forge.ErrUnsupported] — these are
// GitHub-specific features with no GitLab equivalent.
//
// Self-managed instances are served by KORYPH_GITLAB_HOST (via [prAPIBase]).
type gitlabRepoSvc struct{}

func (s *gitlabRepoSvc) DetectCurrent(_ context.Context) (string, error) {
	return "", forge.ErrUnsupported
}

// ---------- RepoService methods -----------------------------------------------

// Get fetches current settings for "owner/repo".
func (s *gitlabRepoSvc) Get(ctx context.Context, owner, repo string) (*forge.RepoSettings, error) {
	raw, err := s.GetRaw(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var proj glProjectRaw
	if err := json.Unmarshal(raw, &proj); err != nil {
		return nil, fmt.Errorf("gitlab repo: parse project %s/%s: %w", owner, repo, err)
	}
	return proj.toRepoSettings(raw), nil
}

// Update applies the high-level [forge.RepoSettings] fields to the project.
// Callers that need fine-grained per-field patching should use [PatchRaw].
func (s *gitlabRepoSvc) Update(ctx context.Context, owner, repo string, settings *forge.RepoSettings) error {
	body := map[string]any{}

	// Map merge strategy: GitLab's merge_method is mutually exclusive.
	// Preference order: rebase > merge > fast-forward.
	switch {
	case settings.AllowRebaseMerge:
		body["merge_method"] = "rebase_merge"
	case settings.AllowMergeCommit:
		body["merge_method"] = "merge"
	default:
		body["merge_method"] = "ff" // fast-forward only
	}

	// Map squash option.
	if settings.AllowSquashMerge {
		body["squash_option"] = "default_on"
	} else {
		body["squash_option"] = "never"
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("gitlab repo: marshal update %s/%s: %w", owner, repo, err)
	}
	return s.PatchRaw(ctx, owner, repo, payload)
}

// GetRaw fetches the full provider-native project JSON.
func (s *gitlabRepoSvc) GetRaw(ctx context.Context, owner, repo string) (json.RawMessage, error) {
	apiURL := fmt.Sprintf("%s/projects/%s", prAPIBase(), glPIDPath(owner, repo))
	raw, err := glExpect(ctx, http.MethodGet, apiURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab repo: get project %s/%s: %w", owner, repo, err)
	}
	return raw, nil
}

// PatchRaw applies a raw JSON body to the project settings endpoint via PUT.
// GitLab's projects API uses PUT (not PATCH) for partial updates — the
// endpoint ignores unrecognised keys so callers may send partial objects.
func (s *gitlabRepoSvc) PatchRaw(ctx context.Context, owner, repo string, payload json.RawMessage) error {
	apiURL := fmt.Sprintf("%s/projects/%s", prAPIBase(), glPIDPath(owner, repo))
	if _, err := glExpect(ctx, http.MethodPut, apiURL, payload, http.StatusOK); err != nil {
		return fmt.Errorf("gitlab repo: patch project %s/%s: %w", owner, repo, err)
	}
	return nil
}

// VulnAlerts returns [forge.ErrUnsupported] — GitLab does not have Dependabot
// vulnerability alerts; use GitLab's Security Dashboard instead.
func (s *gitlabRepoSvc) VulnAlerts(_ context.Context, _, _ string) (bool, error) {
	return false, forge.ErrUnsupported
}

// SetVulnAlerts returns [forge.ErrUnsupported] — GitLab does not have Dependabot.
func (s *gitlabRepoSvc) SetVulnAlerts(_ context.Context, _, _ string, _ bool) error {
	return forge.ErrUnsupported
}

// ActionsWorkflow returns [forge.ErrUnsupported] — GitHub Actions only.
func (s *gitlabRepoSvc) ActionsWorkflow(_ context.Context, _, _ string) (json.RawMessage, error) {
	return nil, forge.ErrUnsupported
}

// SetActionsWorkflow returns [forge.ErrUnsupported] — GitHub Actions only.
func (s *gitlabRepoSvc) SetActionsWorkflow(_ context.Context, _, _ string, _ json.RawMessage) error {
	return forge.ErrUnsupported
}

func (s *gitlabRepoSvc) ListFiles(_ context.Context, _, _, _ string) ([]string, error) {
	return nil, forge.ErrUnsupported
}

func (s *gitlabRepoSvc) ReadFile(_ context.Context, _, _, _ string) ([]byte, error) {
	return nil, forge.ErrUnsupported
}

// ---------- GitLab REST API types for projects --------------------------------

// glProjectRaw is the subset of GitLab project API fields koryph reads.
type glProjectRaw struct {
	MergeMethod                  string `json:"merge_method"`
	SquashOption                 string `json:"squash_option"`
	RemoveSourceBranchAfterMerge bool   `json:"remove_source_branch_after_merge"`
}

// toRepoSettings converts a GitLab project to the forge-neutral
// [forge.RepoSettings] type.  raw is stored in [forge.RepoSettings.RawFull].
//
// squash_option mapping:
//
//	"never"       → AllowSquashMerge=false (squash disabled)
//	"default_off" → AllowSquashMerge=true  (squash available, unchecked by default per MR)
//	"default_on"  → AllowSquashMerge=true  (squash available, checked by default per MR)
//	"always"      → AllowSquashMerge=true  (squash enforced)
//
// Both "default_off" and "default_on" map to AllowSquashMerge=true so that a
// Get→Update round-trip does not silently disable squash that was merely
// defaulted-off at the MR level.
func (p *glProjectRaw) toRepoSettings(raw json.RawMessage) *forge.RepoSettings {
	allowMerge := p.MergeMethod == "merge"
	// ff (fast-forward) and rebase_merge are both rebase-style on GitLab.
	allowRebase := p.MergeMethod == "rebase_merge" || p.MergeMethod == "ff"
	// Only "never" (or empty/unknown) means squash is disabled.
	allowSquash := p.SquashOption != "never" && p.SquashOption != ""
	return &forge.RepoSettings{
		AllowMergeCommit: allowMerge,
		AllowSquashMerge: allowSquash,
		AllowRebaseMerge: allowRebase,
		RawFull:          raw,
	}
}

// Compile-time interface check.
var _ forge.RepoService = (*gitlabRepoSvc)(nil)
