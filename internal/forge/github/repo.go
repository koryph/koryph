// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/koryph/koryph/internal/forge"
)

// githubRepoSvc implements [forge.RepoService] for GitHub using the gh CLI.
//
// All methods use explicit owner/repo form for the endpoint so the binary
// can be invoked from any working directory.  The gh binary path is
// controlled by the KORYPH_GH_BIN environment variable (default: "gh").
type githubRepoSvc struct{}

func (s *githubRepoSvc) ghBin() string {
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		return v
	}
	return "gh"
}

// ghAPI executes the gh CLI with the given arguments, optionally writing
// input to a temp file and passing it via --input.
// Returns (stdout, exitCode, spawnError).  A non-zero exit code is NOT a
// spawn error — callers must check code separately.
func (s *githubRepoSvc) ghAPI(ctx context.Context, args []string, input []byte) ([]byte, int, error) {
	finalArgs := args
	if input != nil {
		f, err := os.CreateTemp("", "koryph-repo-*.json")
		if err != nil {
			return nil, -1, fmt.Errorf("github repo: create temp: %w", err)
		}
		defer os.Remove(f.Name()) //nolint:errcheck
		if _, err := f.Write(input); err != nil {
			f.Close()
			return nil, -1, fmt.Errorf("github repo: write temp: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, -1, fmt.Errorf("github repo: close temp: %w", err)
		}
		finalArgs = append(append([]string{}, args...), "--input", f.Name())
	}
	cmd := exec.CommandContext(ctx, s.ghBin(), finalArgs...) //nolint:gosec
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if runErr := cmd.Run(); runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return out.Bytes(), ee.ExitCode(), nil
		}
		return nil, -1, fmt.Errorf("github repo: exec gh: %w: %s", runErr, errb.String())
	}
	return out.Bytes(), 0, nil
}

// Get fetches current repository settings.
func (s *githubRepoSvc) Get(ctx context.Context, owner, repo string) (*forge.RepoSettings, error) {
	raw, err := s.GetRaw(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var full struct {
		AllowMergeCommit bool `json:"allow_merge_commit"`
		AllowSquashMerge bool `json:"allow_squash_merge"`
		AllowRebaseMerge bool `json:"allow_rebase_merge"`
	}
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, fmt.Errorf("github repo: parse settings: %w", err)
	}
	return &forge.RepoSettings{
		AllowMergeCommit: full.AllowMergeCommit,
		AllowSquashMerge: full.AllowSquashMerge,
		AllowRebaseMerge: full.AllowRebaseMerge,
		RawFull:          raw,
	}, nil
}

// Update applies the high-level RepoSettings fields to the repository.
// Callers that need fine-grained section patching should use [PatchRaw].
func (s *githubRepoSvc) Update(ctx context.Context, owner, repo string, settings *forge.RepoSettings) error {
	payload, err := json.Marshal(map[string]interface{}{
		"allow_merge_commit": settings.AllowMergeCommit,
		"allow_squash_merge": settings.AllowSquashMerge,
		"allow_rebase_merge": settings.AllowRebaseMerge,
	})
	if err != nil {
		return fmt.Errorf("github repo: marshal settings: %w", err)
	}
	endpoint := fmt.Sprintf("repos/%s/%s", owner, repo)
	_, code, err := s.ghAPI(ctx, []string{"api", "-X", "PATCH", endpoint}, payload)
	if err != nil {
		return fmt.Errorf("github repo: update settings: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("github repo: update settings: gh exited %d", code)
	}
	return nil
}

// GetRaw fetches the full provider-native repository JSON from the GitHub API.
func (s *githubRepoSvc) GetRaw(ctx context.Context, owner, repo string) (json.RawMessage, error) {
	endpoint := fmt.Sprintf("repos/%s/%s", owner, repo)
	raw, code, err := s.ghAPI(ctx, []string{"api", endpoint}, nil)
	if err != nil {
		return nil, fmt.Errorf("github repo: fetch repo: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("github repo: fetch repo: gh exited %d", code)
	}
	return raw, nil
}

// PatchRaw applies a raw JSON PATCH to the primary repository metadata endpoint.
func (s *githubRepoSvc) PatchRaw(ctx context.Context, owner, repo string, payload json.RawMessage) error {
	endpoint := fmt.Sprintf("repos/%s/%s", owner, repo)
	_, code, err := s.ghAPI(ctx, []string{"api", "-X", "PATCH", endpoint}, payload)
	if err != nil {
		return fmt.Errorf("github repo: patch raw: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("github repo: patch raw: gh exited %d", code)
	}
	return nil
}

// VulnAlerts reports whether Dependabot vulnerability alerts are enabled.
// GitHub returns HTTP 204 when enabled and HTTP 404 when disabled.
func (s *githubRepoSvc) VulnAlerts(ctx context.Context, owner, repo string) (bool, error) {
	endpoint := fmt.Sprintf("repos/%s/%s/vulnerability-alerts", owner, repo)
	_, code, err := s.ghAPI(ctx, []string{"api", endpoint}, nil)
	if err != nil {
		return false, fmt.Errorf("github repo: check vuln alerts: %w", err)
	}
	return code == 0, nil // 204 → enabled; 404 → disabled
}

// SetVulnAlerts enables or disables Dependabot vulnerability alerts.
func (s *githubRepoSvc) SetVulnAlerts(ctx context.Context, owner, repo string, enabled bool) error {
	endpoint := fmt.Sprintf("repos/%s/%s/vulnerability-alerts", owner, repo)
	method := "DELETE"
	if enabled {
		method = "PUT"
	}
	_, code, err := s.ghAPI(ctx, []string{"api", "-X", method, endpoint}, nil)
	if err != nil {
		return fmt.Errorf("github repo: set vuln alerts: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("github repo: set vuln alerts: gh exited %d", code)
	}
	return nil
}

// ActionsWorkflow fetches the actions workflow permissions JSON.
func (s *githubRepoSvc) ActionsWorkflow(ctx context.Context, owner, repo string) (json.RawMessage, error) {
	endpoint := fmt.Sprintf("repos/%s/%s/actions/permissions/workflow", owner, repo)
	raw, code, err := s.ghAPI(ctx, []string{"api", endpoint}, nil)
	if err != nil {
		return nil, fmt.Errorf("github repo: fetch actions workflow: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("github repo: fetch actions workflow: gh exited %d", code)
	}
	return raw, nil
}

// SetActionsWorkflow applies raw JSON actions workflow permissions.
func (s *githubRepoSvc) SetActionsWorkflow(ctx context.Context, owner, repo string, payload json.RawMessage) error {
	endpoint := fmt.Sprintf("repos/%s/%s/actions/permissions/workflow", owner, repo)
	_, code, err := s.ghAPI(ctx, []string{"api", "-X", "PUT", endpoint}, payload)
	if err != nil {
		return fmt.Errorf("github repo: set actions workflow: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("github repo: set actions workflow: gh exited %d", code)
	}
	return nil
}

// Compile-time interface check.
var _ forge.RepoService = (*githubRepoSvc)(nil)
