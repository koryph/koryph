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
	"strings"

	"github.com/koryph/koryph/internal/forge"
)

// githubProtectionSvc implements [forge.ProtectionService] for GitHub using
// the gh CLI.
//
// The target parameter distinguishes repo-level from org-level operations:
//   - "owner/repo" (contains "/") → repository-level rulesets endpoint
//   - "orgname"   (no "/")        → organisation-level rulesets endpoint
//
// The gh binary path is controlled by the KORYPH_GH_BIN environment variable.
type githubProtectionSvc struct{}

func (s *githubProtectionSvc) ghBin() string {
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		return v
	}
	return "gh"
}

// ghAPI executes the gh CLI with the given arguments, optionally writing
// input to a temp file and passing it via --input.
// Returns (stdout, exitCode, spawnError).  A non-zero exit code is NOT a
// spawn error — callers must check code separately.
func (s *githubProtectionSvc) ghAPI(ctx context.Context, args []string, input []byte) ([]byte, int, error) {
	finalArgs := args
	if input != nil {
		f, err := os.CreateTemp("", "koryph-prot-*.json")
		if err != nil {
			return nil, -1, fmt.Errorf("github protection: create temp: %w", err)
		}
		defer os.Remove(f.Name()) //nolint:errcheck
		if _, err := f.Write(input); err != nil {
			f.Close()
			return nil, -1, fmt.Errorf("github protection: write temp: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, -1, fmt.Errorf("github protection: close temp: %w", err)
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
		return nil, -1, fmt.Errorf("github protection: exec gh: %w: %s", runErr, errb.String())
	}
	return out.Bytes(), 0, nil
}

// rulesetsEndpoint returns the appropriate rulesets API endpoint for target.
// target with a "/" is treated as "owner/repo"; otherwise as an org name.
func rulesetsEndpoint(target string) string {
	if strings.Contains(target, "/") {
		return "repos/" + target + "/rulesets"
	}
	return "orgs/" + target + "/rulesets"
}

// rulesetEndpoint returns the endpoint for a single ruleset by numeric ID.
func rulesetEndpoint(target, id string) string {
	if strings.Contains(target, "/") {
		return fmt.Sprintf("repos/%s/rulesets/%s", target, id)
	}
	return fmt.Sprintf("orgs/%s/rulesets/%s", target, id)
}

// List returns all rulesets for the target.  Each Ruleset.Raw holds the full
// provider-native JSON, as returned by the individual GET endpoint (not the
// list endpoint, which returns summary objects).
func (s *githubProtectionSvc) List(ctx context.Context, target string) ([]forge.Ruleset, error) {
	endpoint := rulesetsEndpoint(target)
	raw, code, err := s.ghAPI(ctx, []string{"api", endpoint}, nil)
	if err != nil {
		return nil, fmt.Errorf("github protection: list rulesets: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("github protection: list rulesets for %q: gh exited %d: %s", target, code, strings.TrimSpace(string(raw)))
	}

	var items []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("github protection: parse ruleset list: %w", err)
	}

	// Fetch full JSON for each item so Ruleset.Raw is populated.
	out := make([]forge.Ruleset, 0, len(items))
	for _, it := range items {
		id := fmt.Sprintf("%d", it.ID)
		rs, err := s.Get(ctx, target, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *rs)
	}
	return out, nil
}

// Get returns one ruleset by its provider-opaque ID (numeric string for
// GitHub).  The returned Ruleset.Raw holds the full provider-native JSON.
func (s *githubProtectionSvc) Get(ctx context.Context, target, id string) (*forge.Ruleset, error) {
	endpoint := rulesetEndpoint(target, id)
	raw, code, err := s.ghAPI(ctx, []string{"api", endpoint}, nil)
	if err != nil {
		return nil, fmt.Errorf("github protection: fetch ruleset %s: %w", id, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("github protection: fetch ruleset %s for %q: gh exited %d: %s", id, target, code, strings.TrimSpace(string(raw)))
	}

	var meta struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("github protection: parse ruleset %s: %w", id, err)
	}
	return &forge.Ruleset{
		ID:   fmt.Sprintf("%d", meta.ID),
		Name: meta.Name,
		Raw:  raw,
	}, nil
}

// Create posts a new ruleset to GitHub and returns the created ruleset with
// its server-assigned ID.  rs.Raw holds the full desired-state JSON payload.
func (s *githubProtectionSvc) Create(ctx context.Context, target string, rs *forge.Ruleset) (*forge.Ruleset, error) {
	endpoint := rulesetsEndpoint(target)
	respRaw, code, err := s.ghAPI(ctx, []string{"api", "-X", "POST", endpoint}, rs.Raw)
	if err != nil {
		return nil, fmt.Errorf("github protection: create ruleset %q: %w", rs.Name, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("github protection: create ruleset %q for %q: gh exited %d: %s", rs.Name, target, code, strings.TrimSpace(string(respRaw)))
	}

	var created struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respRaw, &created); err != nil {
		return nil, fmt.Errorf("github protection: parse created ruleset: %w", err)
	}
	return &forge.Ruleset{
		ID:   fmt.Sprintf("%d", created.ID),
		Name: created.Name,
		Raw:  respRaw,
	}, nil
}

// Update replaces an existing ruleset.  rs.ID must be the numeric string ID
// returned by [List] or [Get].  rs.Raw is sent as the PUT body.
func (s *githubProtectionSvc) Update(ctx context.Context, target string, rs *forge.Ruleset) error {
	endpoint := rulesetEndpoint(target, rs.ID)
	_, code, err := s.ghAPI(ctx, []string{"api", "-X", "PUT", endpoint}, rs.Raw)
	if err != nil {
		return fmt.Errorf("github protection: update ruleset %q: %w", rs.Name, err)
	}
	if code != 0 {
		return fmt.Errorf("github protection: update ruleset %q for %q: gh exited %d", rs.Name, target, code)
	}
	return nil
}

// Delete removes a ruleset by ID.
func (s *githubProtectionSvc) Delete(ctx context.Context, target, id string) error {
	endpoint := rulesetEndpoint(target, id)
	_, code, err := s.ghAPI(ctx, []string{"api", "-X", "DELETE", endpoint}, nil)
	if err != nil {
		return fmt.Errorf("github protection: delete ruleset %s: %w", id, err)
	}
	if code != 0 {
		return fmt.Errorf("github protection: delete ruleset %s for %q: gh exited %d", id, target, code)
	}
	return nil
}

// Compile-time interface check.
var _ forge.ProtectionService = (*githubProtectionSvc)(nil)
