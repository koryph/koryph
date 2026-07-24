// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/forge"
	ghpkg "github.com/koryph/koryph/internal/forge/github"
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

	// BotSvc, SecretsSvc, and RepoSvc route provider operations through the
	// forge seam. Nil values use the registered GitHub provider for backwards
	// compatibility with package callers.
	BotSvc     forge.BotService
	SecretsSvc forge.SecretsService
	RepoSvc    forge.RepoService
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
	// RepositorySelection is "all" when the app can access every repo in the
	// account, or "selected" when access is limited to an explicit list.
	RepositorySelection string `json:"repository_selection"`
}

// Attach implements 'koryph bot attach'. It is idempotent on all steps.
//
//  1. Resolve the private key (vault or inline) and mint an app JWT.
//  2. GET /app/installations (authenticated as the app) to find the
//     installation that covers the target owner (no gh dependency here).
//  3. Add the repo to the installation via gh api (user's auth token).
//  4. Set repository (or org) secrets via gh secret set.
//  5. Enable the Actions can_approve_pull_request_reviews toggle.
//
// Actions secrets (RELEASE_BOT_APP_ID, RELEASE_BOT_PRIVATE_KEY) always
// receive the operational PEM — this is inherent to the
// create-github-app-token action and unchanged by vault storage.
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

	if opts.BotSvc == nil || opts.SecretsSvc == nil || opts.RepoSvc == nil {
		provider := ghpkg.New()
		if opts.BotSvc == nil {
			opts.BotSvc = provider.Bot()
		}
		if opts.SecretsSvc == nil {
			opts.SecretsSvc = provider.Secrets()
		}
		if opts.RepoSvc == nil {
			opts.RepoSvc = provider.Repo()
		}
	}

	// Step 1: resolve key (vault fetch or inline) then mint JWT.
	resolvedPEM, err := ResolveKey(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("bot attach: resolve key: %w", err)
	}
	jwt, err := mintJWTFrom(resolvedPEM, cfg.AppID, time.Now())
	if err != nil {
		return nil, fmt.Errorf("bot attach: mint JWT: %w", err)
	}
	fmt.Fprintf(out, "  ✓ JWT minted (app_id %d)\n", cfg.AppID)

	// Step 2: resolve installation ID via the GitHub App API (Bearer JWT).
	iid, err := resolveInstallation(ctx, jwt, owner)
	if err != nil {
		return nil, fmt.Errorf("bot attach: %w", err)
	}
	fmt.Fprintf(out, "  ✓ installation %d covers %q\n", iid, owner)

	// Step 3: resolve repo ID and add to installation.
	// addRepoToInstallation detects a 403 (OAuth scope insufficient for the
	// /user/installations family) and returns skipped=true in that case — the
	// warning + remediations are printed inside the helper.
	rid, repoAdded, skipped403, err := addRepoToInstallation(ctx, opts.Repo, iid, opts.BotSvc, opts.Name, out)
	if err != nil {
		return nil, fmt.Errorf("bot attach: %w", err)
	}
	switch {
	case skipped403:
		// Warning + remediations already printed by addRepoToInstallation.
	case repoAdded:
		fmt.Fprintf(out, "  ✓ %s added to installation %d\n", opts.Repo, iid)
	default:
		fmt.Fprintf(out, "  ✓ %s already in installation %d\n", opts.Repo, iid)
	}

	// Step 4: set secrets (uses the resolved PEM — Action secrets always need
	// the operational PEM copy, regardless of where it is stored locally).
	//
	secrets, err := setSecrets(ctx, cfg, resolvedPEM, opts, out)
	if err != nil {
		return nil, fmt.Errorf("bot attach: %w", err)
	}

	// Step 5: enable Actions PR-approval toggle.
	toggled, err := ensureActionsApproval(ctx, opts.Repo, opts.RepoSvc, out)
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
// Returns (repoID, wasAdded, skipped403, err).
//
// skipped403 is true when the PUT request returned HTTP 403 (the caller's
// OAuth token lacks the read:user scope required by the /user/installations
// family). In that case the warning and two remediations are written to out
// and the function returns nil — Attach continues with secrets and toggle.
func addRepoToInstallation(ctx context.Context, ownerRepo string, iid int64, svc forge.BotService, botName string, out io.Writer) (int64, bool, bool, error) {
	attachment, err := svc.AttachRepository(ctx, ownerRepo, iid)
	if err != nil {
		if ghOutputIs403([]byte(err.Error())) {
			// The /user/installations family requires the read:user OAuth scope,
			// which gh's default token may not have. Print remediations and
			// continue — secrets and toggle can still be configured.
			fmt.Fprintf(out, "  ⚠ cannot add %s to installation %d (HTTP 403 — OAuth token lacks read:user scope)\n",
				ownerRepo, iid)
			fmt.Fprintf(out, "    Remediation 1: gh auth refresh -h github.com -s read:user\n")
			fmt.Fprintf(out, "                   then retry: koryph bot attach --name %s --repo %s\n",
				botName, ownerRepo)
			fmt.Fprintf(out, "    Remediation 2: org Settings → GitHub Apps → configure %s → Repository access\n",
				botName)
			return attachment.RepositoryID, false, true, nil // skipped — not a fatal error
		}
		return attachment.RepositoryID, false, false, fmt.Errorf("add %s to installation %d: %w",
			ownerRepo, iid, err)
	}
	return attachment.RepositoryID, attachment.Added, false, nil
}

// setSecrets writes RELEASE_BOT_APP_ID and RELEASE_BOT_PRIVATE_KEY either
// as per-repo secrets (default) or as org-level selected-repo secrets
// (--org-secrets). resolvedPEM is the operational PEM material obtained via
// ResolveKey; it is passed directly to GitHub Actions secrets regardless of
// where the key is stored locally (vault, encrypted file, or inline).
func setSecrets(ctx context.Context, cfg *Config, resolvedPEM string, opts AttachOptions, out io.Writer) ([]string, error) {
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
			{"RELEASE_BOT_PRIVATE_KEY", resolvedPEM},
		} {
			if err := opts.SecretsSvc.SetOrg(ctx, owner, s.name, s.val, []string{repo}); err != nil {
				return nil, fmt.Errorf("set org secret %s: %w", s.name, err)
			}
			fmt.Fprintf(out, "  ✓ org secret %s set on %s\n", s.name, owner)
			written = append(written, s.name)
		}
	} else {
		// Per-repo secrets.
		for _, s := range []struct{ name, val string }{
			{"RELEASE_BOT_APP_ID", appIDVal},
			{"RELEASE_BOT_PRIVATE_KEY", resolvedPEM},
		} {
			if err := opts.SecretsSvc.SetRepo(ctx, owner, repo, s.name, s.val); err != nil {
				return nil, fmt.Errorf("set repo secret %s on %s: %w", s.name, opts.Repo, err)
			}
			fmt.Fprintf(out, "  ✓ repo secret %s set on %s/%s\n", s.name, owner, repo)
			written = append(written, s.name)
		}
	}
	return written, nil
}

// ensureActionsApproval enables the can_approve_pull_request_reviews toggle
// if it is not already on. Returns true when the toggle was changed (false
// when it was already enabled — idempotent).
func ensureActionsApproval(ctx context.Context, ownerRepo string, svc forge.RepoService, out io.Writer) (bool, error) {
	owner, repo, err := splitOwnerRepo(ownerRepo)
	if err != nil {
		return false, err
	}
	current, err := svc.ActionsWorkflow(ctx, owner, repo)
	if err == nil {
		var permissions struct {
			CanApprove bool `json:"can_approve_pull_request_reviews"`
		}
		if json.Unmarshal(current, &permissions) == nil && permissions.CanApprove {
			fmt.Fprintf(out, "  ✓ Actions can_approve_pull_request_reviews already enabled\n")
			return false, nil
		}
	}
	if err := svc.SetActionsWorkflow(ctx, owner, repo, json.RawMessage(`{"can_approve_pull_request_reviews":true}`)); err != nil {
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

// ghOutputIs403 reports whether the combined output of a gh command indicates
// an HTTP 403 response from the GitHub API.
func ghOutputIs403(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "HTTP 403") ||
		strings.Contains(s, "status 403") ||
		strings.Contains(s, " 403 ")
}
