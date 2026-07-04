// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/koryph/koryph/internal/forge"
)

// gitlabSecretsSvc implements [forge.SecretsService] for GitLab using the
// GitLab REST API v4.
//
// GitLab's equivalent of GitHub Actions secrets are CI/CD variables.  They
// are managed per-project or per-group.  Variable values are never returned
// by the list endpoint — only keys are exposed, matching the contract of
// [forge.SecretsService.ListRepo] and [forge.SecretsService.ListOrg].
//
// Variable visibility on SetRepo defaults to "env_var" (environment variable)
// with env_scope "*" (all environments), which is the broadest and most
// compatible setting for CI use.
//
// Self-managed instances are served by KORYPH_GITLAB_HOST (via [prAPIBase]).
type gitlabSecretsSvc struct{}

// ---------- SecretsService methods -------------------------------------------

// ListRepo returns the keys (never the values) of CI/CD variables set on
// "owner/repo".
func (s *gitlabSecretsSvc) ListRepo(ctx context.Context, owner, repo string) ([]string, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/variables?per_page=100",
		prAPIBase(), glPIDPath(owner, repo))
	return s.listVarKeys(ctx, apiURL, fmt.Sprintf("gitlab secrets: list repo vars %s/%s", owner, repo))
}

// ListOrg returns the keys of group-level CI/CD variables for the given group.
// GitLab groups are the equivalent of GitHub organisations.
func (s *gitlabSecretsSvc) ListOrg(ctx context.Context, org string) ([]string, error) {
	apiURL := fmt.Sprintf("%s/groups/%s/variables?per_page=100",
		prAPIBase(), glURLEscape(org))
	return s.listVarKeys(ctx, apiURL, fmt.Sprintf("gitlab secrets: list group vars %s", org))
}

// SetRepo creates or updates a CI/CD variable on the project.
// If the variable already exists (HTTP 400 with duplicate key) the call
// falls back to a PUT update.
func (s *gitlabSecretsSvc) SetRepo(ctx context.Context, owner, repo, name, value string) error {
	base := fmt.Sprintf("%s/projects/%s/variables", prAPIBase(), glPIDPath(owner, repo))
	return s.upsertVar(ctx, base, name, value,
		fmt.Sprintf("gitlab secrets: set repo var %q on %s/%s", name, owner, repo))
}

// SetOrg creates or updates a group-level CI/CD variable.
// The repos parameter is not used — GitLab group variables apply to all
// projects in the group (scoping by project requires individual project variables).
func (s *gitlabSecretsSvc) SetOrg(ctx context.Context, org, name, value string, _ []string) error {
	base := fmt.Sprintf("%s/groups/%s/variables", prAPIBase(), glURLEscape(org))
	return s.upsertVar(ctx, base, name, value,
		fmt.Sprintf("gitlab secrets: set group var %q on %s", name, org))
}

// ---------- helpers ----------------------------------------------------------

// glCIVariable is the subset of a GitLab CI variable the list endpoint returns.
type glCIVariable struct {
	Key              string `json:"key"`
	VariableType     string `json:"variable_type"`
	Value            string `json:"value"`
	Protected        bool   `json:"protected"`
	Masked           bool   `json:"masked"`
	Raw              bool   `json:"raw"`
	EnvironmentScope string `json:"environment_scope"`
}

// listVarKeys fetches variable records from apiURL and returns the key names.
func (s *gitlabSecretsSvc) listVarKeys(ctx context.Context, apiURL, errPrefix string) ([]string, error) {
	raw, err := glExpect(ctx, http.MethodGet, apiURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", errPrefix, err)
	}
	var vars []glCIVariable
	if err := json.Unmarshal(raw, &vars); err != nil {
		return nil, fmt.Errorf("%s: parse response: %w", errPrefix, err)
	}
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.Key)
	}
	return names, nil
}

// upsertVar tries to create a variable; if GitLab returns 400 (variable
// already exists), it falls back to a PUT update on the specific key.
func (s *gitlabSecretsSvc) upsertVar(ctx context.Context, baseURL, name, value, errPrefix string) error {
	payload, err := json.Marshal(map[string]string{
		"key":               name,
		"value":             value,
		"variable_type":     "env_var",
		"environment_scope": "*",
	})
	if err != nil {
		return fmt.Errorf("%s: marshal payload: %w", errPrefix, err)
	}

	body, code, err := glDo(ctx, http.MethodPost, baseURL, payload)
	if err != nil {
		return fmt.Errorf("%s: %w", errPrefix, err)
	}
	if code == http.StatusCreated || code == http.StatusOK {
		return nil
	}
	// GitLab returns 400 when the variable already exists; fall back to PUT.
	if code == http.StatusBadRequest {
		if isAlreadyExists(body) {
			updatePayload, err := json.Marshal(map[string]string{
				"value":             value,
				"variable_type":     "env_var",
				"environment_scope": "*",
			})
			if err != nil {
				return fmt.Errorf("%s: marshal update payload: %w", errPrefix, err)
			}
			putURL := baseURL + "/" + name
			if _, err := glExpect(ctx, http.MethodPut, putURL, updatePayload, http.StatusOK); err != nil {
				return fmt.Errorf("%s (update): %w", errPrefix, err)
			}
			return nil
		}
	}
	return fmt.Errorf("%s: HTTP %d: %s", errPrefix, code, strings.TrimSpace(string(body)))
}

// isAlreadyExists checks whether a GitLab 400 body indicates a duplicate-key
// error.  GitLab returns {"message":{"key":["has already been taken"]}}.
func isAlreadyExists(body []byte) bool {
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, "already been taken") || strings.Contains(msg, "already exists")
}

// Compile-time interface check.
var _ forge.SecretsService = (*gitlabSecretsSvc)(nil)
