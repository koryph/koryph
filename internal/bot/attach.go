// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// AttachOptions configures a 'koryph bot attach' run.
type AttachOptions struct {
	// Name is the bot credential name (required; must match a stored bot).
	Name string
	// Repo is the "owner/repo" slug to attach (required).
	Repo string
	// OrgSecrets, when true, sets RELEASE_BOT_APP_ID and
	// RELEASE_BOT_PRIVATE_KEY as organisation-level Actions secrets with
	// selected-repos visibility (the named repo is added to the visibility
	// list). When false (the default), the secrets are set on the specific
	// repo only.
	OrgSecrets bool
	// Out receives progress messages.
	Out io.Writer

	// GHBin overrides the gh CLI binary path (tests inject a stub).
	GHBin string
}

// AttachResult summarises what 'koryph bot attach' did.
type AttachResult struct {
	// InstallationID is the GitHub App installation that covers Owner.
	InstallationID int64
	// RepoID is the GitHub repository numeric ID.
	RepoID int64
	// RepoAdded is true when the repo was newly added to the installation
	// (false when it was already present — idempotent).
	RepoAdded bool
	// SecretsSet is the list of secret names that were written (or verified).
	SecretsSet []string
	// ToggleEnabled is true when the Actions PR-approval toggle was enabled
	// (false when it was already on — idempotent).
	ToggleEnabled bool
}

// installation is the GitHub App installation API shape we care about.
type installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
}

// Attach implements 'koryph bot attach'. It is idempotent on all steps.
//
//  1. Mint an app JWT from the stored PEM.
//  2. GET /app/installations (authenticated as the app) to find the
//     installation that covers the target owner (no gh dependency here).
//  3. Add the repo to the installation via gh api (user's auth token).
//  4. Set repository (or org) secrets via gh secret set.
//  5. Enable the Actions can_approve_pull_request_reviews toggle.
func Attach(ctx context.Context, cfg *Config, opts AttachOptions) (*AttachResult, error) {
	if opts.Repo == "" {
		return nil, fmt.Errorf("bot attach: --repo is required")
	}
	owner, _, err := splitOwnerRepo(opts.Repo)
	if err != nil {
		return nil, fmt.Errorf("bot attach: %w", err)
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	ghBin := opts.GHBin
	if ghBin == "" {
		ghBin = "gh"
	}

	// Step 1: mint JWT.
	jwt, err := MintJWT(cfg)
	if err != nil {
		return nil, fmt.Errorf("bot attach: mint JWT: %w", err)
	}
	fmt.Fprintf(out, "  ✓ JWT minted from stored PEM (app_id %d)\n", cfg.AppID)

	// Step 2: resolve installation ID via the GitHub App API (Bearer JWT).
	iid, err := resolveInstallation(ctx, jwt, owner)
	if err != nil {
		return nil, fmt.Errorf("bot attach: %w", err)
	}
	fmt.Fprintf(out, "  ✓ installation %d covers %q\n", iid, owner)

	// Step 3: resolve repo ID and add to installation.
	rid, repoAdded, err := addRepoToInstallation(ctx, opts.Repo, iid, ghBin)
	if err != nil {
		return nil, fmt.Errorf("bot attach: %w", err)
	}
	if repoAdded {
		fmt.Fprintf(out, "  ✓ %s added to installation %d\n", opts.Repo, iid)
	} else {
		fmt.Fprintf(out, "  ✓ %s already in installation %d\n", opts.Repo, iid)
	}

	// Step 4: set secrets.
	secrets, err := setSecrets(ctx, cfg, opts, ghBin, out)
	if err != nil {
		return nil, fmt.Errorf("bot attach: %w", err)
	}

	// Step 5: enable Actions PR-approval toggle.
	toggled, err := ensureActionsApproval(opts.Repo, ghBin, out)
	if err != nil {
		return nil, fmt.Errorf("bot attach: %w", err)
	}

	return &AttachResult{
		InstallationID: iid,
		RepoID:         rid,
		RepoAdded:      repoAdded,
		SecretsSet:     secrets,
		ToggleEnabled:  toggled,
	}, nil
}

// resolveInstallation calls GET /app/installations (authenticated as the GitHub
// App via the app JWT) and returns the installation ID whose account.login
// matches owner. The call does not go through gh — it uses the raw GitHub API
// with Bearer JWT authentication, satisfying the "no gh dependency for
// app-auth calls" requirement in the task design doc.
func resolveInstallation(ctx context.Context, jwt, owner string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/app/installations", http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("build installations request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET /app/installations: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET /app/installations returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var installs []installation
	if err := json.Unmarshal(body, &installs); err != nil {
		return 0, fmt.Errorf("parse installations response: %w", err)
	}
	for _, i := range installs {
		if strings.EqualFold(i.Account.Login, owner) {
			return i.ID, nil
		}
	}
	return 0, fmt.Errorf(
		"no installation found for owner %q — install the app first: koryph bot install --name <name>",
		owner)
}

// addRepoToInstallation resolves the GitHub repository numeric ID, checks
// whether it is already included in the installation, and adds it if not.
// Returns (repoID, wasAdded, err).
func addRepoToInstallation(_ context.Context, ownerRepo string, iid int64, ghBin string) (int64, bool, error) {
	// Resolve repo ID.
	ridOut, err := runGHBin(ghBin, "api", "/repos/"+ownerRepo, "--jq", ".id")
	if err != nil {
		return 0, false, fmt.Errorf("resolve repo ID for %s: %w", ownerRepo, err)
	}
	var rid int64
	if _, err := fmt.Sscanf(strings.TrimSpace(ridOut), "%d", &rid); err != nil || rid == 0 {
		return 0, false, fmt.Errorf("parse repo ID %q: %w", strings.TrimSpace(ridOut), err)
	}

	// Check whether the repo is already in the installation.
	alreadyOut, _ := runGHBin(ghBin, "api",
		fmt.Sprintf("/user/installations/%d/repositories", iid),
		"--jq", fmt.Sprintf(".repositories[] | select(.id == %d) | .id", rid))
	var existing int64
	fmt.Sscanf(strings.TrimSpace(alreadyOut), "%d", &existing) //nolint:errcheck
	if existing == rid {
		return rid, false, nil // already present — idempotent
	}

	// Add the repo to the installation.
	_, err = runGHBin(ghBin, "api", "-X", "PUT",
		fmt.Sprintf("/user/installations/%d/repositories/%d", iid, rid))
	if err != nil {
		return 0, false, fmt.Errorf("add %s to installation %d: %w", ownerRepo, iid, err)
	}
	return rid, true, nil
}

// setSecrets writes RELEASE_BOT_APP_ID and RELEASE_BOT_PRIVATE_KEY either
// as per-repo secrets (default) or as org-level selected-repo secrets
// (--org-secrets). Returns the list of secret names written.
func setSecrets(_ context.Context, cfg *Config, opts AttachOptions, ghBin string, out io.Writer) ([]string, error) {
	owner, repo, err := splitOwnerRepo(opts.Repo)
	if err != nil {
		return nil, err
	}
	appIDVal := fmt.Sprintf("%d", cfg.AppID)

	var written []string

	if opts.OrgSecrets {
		// Org-level secrets (selected-repos visibility).
		for _, s := range []struct{ name, val string }{
			{"RELEASE_BOT_APP_ID", appIDVal},
			{"RELEASE_BOT_PRIVATE_KEY", cfg.PEM},
		} {
			if err := setOrgSecret(ghBin, owner, repo, s.name, s.val); err != nil {
				return nil, fmt.Errorf("set org secret %s: %w", s.name, err)
			}
			fmt.Fprintf(out, "  ✓ org secret %s set on %s\n", s.name, owner)
			written = append(written, s.name)
		}
	} else {
		// Per-repo secrets.
		for _, s := range []struct{ name, val string }{
			{"RELEASE_BOT_APP_ID", appIDVal},
			{"RELEASE_BOT_PRIVATE_KEY", cfg.PEM},
		} {
			if err := setRepoSecret(ghBin, opts.Repo, s.name, s.val); err != nil {
				return nil, fmt.Errorf("set repo secret %s on %s: %w", s.name, opts.Repo, err)
			}
			fmt.Fprintf(out, "  ✓ repo secret %s set on %s/%s\n", s.name, owner, repo)
			written = append(written, s.name)
		}
	}
	return written, nil
}

// setRepoSecret sets a per-repository Actions secret via gh secret set.
func setRepoSecret(ghBin, ownerRepo, name, val string) error {
	cmd := exec.Command(ghBin, "secret", "set", name, "--repo", ownerRepo, //nolint:gosec
		"--body", val)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh secret set %s --repo %s: %w\n%s", name, ownerRepo, err, string(out))
	}
	return nil
}

// setOrgSecret sets an organisation-level Actions secret with selected-repos
// visibility, adding ownerRepo to the secret's repository list.
func setOrgSecret(ghBin, org, repo, name, val string) error {
	// Set the secret at org level with selected visibility.
	cmd := exec.Command(ghBin, "secret", "set", name, //nolint:gosec
		"--org", org,
		"--visibility", "selected",
		"--repos", repo,
		"--body", val)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh secret set %s --org %s: %w\n%s", name, org, err, string(out))
	}
	return nil
}

// ensureActionsApproval enables the can_approve_pull_request_reviews toggle
// if it is not already on. Returns true when the toggle was changed (false
// when it was already enabled — idempotent).
func ensureActionsApproval(ownerRepo, ghBin string, out io.Writer) (bool, error) {
	// Check current state.
	current, err := runGHBin(ghBin, "api",
		"/repos/"+ownerRepo+"/actions/permissions/workflow",
		"--jq", ".can_approve_pull_request_reviews")
	if err == nil && strings.TrimSpace(current) == "true" {
		fmt.Fprintf(out, "  ✓ Actions can_approve_pull_request_reviews already enabled\n")
		return false, nil
	}

	// Enable the toggle.
	_, err = runGHBin(ghBin, "api", "-X", "PUT",
		"/repos/"+ownerRepo+"/actions/permissions/workflow",
		"-F", "can_approve_pull_request_reviews=true")
	if err != nil {
		return false, fmt.Errorf("enable Actions PR-approval on %s: %w", ownerRepo, err)
	}
	fmt.Fprintf(out, "  ✓ Actions can_approve_pull_request_reviews enabled on %s\n", ownerRepo)
	return true, nil
}

// splitOwnerRepo splits "owner/repo" into its two parts.
func splitOwnerRepo(s string) (owner, repo string, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid owner/repo %q (expected OWNER/REPO)", s)
	}
	return parts[0], parts[1], nil
}

// runGHBin runs the gh CLI with the given args and returns combined output.
func runGHBin(bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...) //nolint:gosec
	out, err := cmd.Output()
	if err != nil {
		combined, _ := cmd.CombinedOutput()
		return "", fmt.Errorf("gh %s: %w\n%s", strings.Join(args, " "), err, string(combined))
	}
	return string(out), nil
}
