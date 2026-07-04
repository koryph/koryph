// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/koryph/koryph/internal/forge"
)

// githubSecretsSvc implements [forge.SecretsService] for GitHub using the gh
// CLI.  Repository and organisation secrets are managed via `gh secret`.
//
// The gh binary path is controlled by the KORYPH_GH_BIN environment variable.
type githubSecretsSvc struct{}

func (s *githubSecretsSvc) ghBin() string {
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		return v
	}
	return "gh"
}

// run executes a gh command and returns combined output and an error.
func (s *githubSecretsSvc) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, s.ghBin(), args...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	return out, err
}

// ListRepo returns the names (never the values) of secrets set on owner/repo.
func (s *githubSecretsSvc) ListRepo(_ context.Context, owner, repo string) ([]string, error) {
	ownerRepo := owner + "/" + repo
	out, err := exec.Command(s.ghBin(), "secret", "list", //nolint:gosec
		"--repo", ownerRepo,
		"--json", "name",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("github secrets: list repo secrets %s: %w", ownerRepo, err)
	}
	var items []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("github secrets: parse repo secrets: %w", err)
	}
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Name
	}
	return names, nil
}

// ListOrg returns the names of org-level secrets for the given organisation.
func (s *githubSecretsSvc) ListOrg(_ context.Context, org string) ([]string, error) {
	out, err := exec.Command(s.ghBin(), "secret", "list", //nolint:gosec
		"--org", org,
		"--json", "name",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("github secrets: list org secrets %s: %w", org, err)
	}
	var items []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("github secrets: parse org secrets: %w", err)
	}
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Name
	}
	return names, nil
}

// SetRepo creates or updates a repository secret.
func (s *githubSecretsSvc) SetRepo(ctx context.Context, owner, repo, name, value string) error {
	ownerRepo := owner + "/" + repo
	out, err := s.run(ctx,
		"secret", "set", name,
		"--repo", ownerRepo,
		"--body", value,
	)
	if err != nil {
		return fmt.Errorf("github secrets: set repo secret %q on %s: %w\n%s", name, ownerRepo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetOrg creates or updates an org-level secret. When repos is non-empty the
// secret is restricted to those repository names; nil or empty means the
// provider's default visibility (typically "all selected repositories").
func (s *githubSecretsSvc) SetOrg(ctx context.Context, org, name, value string, repos []string) error {
	args := []string{
		"secret", "set", name,
		"--org", org,
		"--body", value,
		"--visibility", "selected",
	}
	for _, r := range repos {
		args = append(args, "--repos", r)
	}
	if len(repos) == 0 {
		// No specific repos → use all-repos visibility.
		args = []string{
			"secret", "set", name,
			"--org", org,
			"--body", value,
			"--visibility", "all",
		}
	}
	out, err := s.run(ctx, args...)
	if err != nil {
		return fmt.Errorf("github secrets: set org secret %q on %s: %w\n%s", name, org, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Compile-time interface check.
var _ forge.SecretsService = (*githubSecretsSvc)(nil)
