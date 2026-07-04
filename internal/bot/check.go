// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/signing"
)

// CheckLevel classifies a check finding's severity.
type CheckLevel string

const (
	CheckOK   CheckLevel = "ok"
	CheckWarn CheckLevel = "warn"
	CheckFail CheckLevel = "fail"
)

// CheckFinding is one result from the bot validator chain.
type CheckFinding struct {
	// Check is the validator name (e.g. "jwt-valid", "installation-exists").
	Check string
	// Level is ok|warn|fail.
	Level CheckLevel
	// Message is a human-readable description.
	Message string
	// Remediation, when non-empty, is the exact command the operator should
	// run to fix the issue.
	Remediation string
}

// CheckOptions configures 'koryph bot check'.
type CheckOptions struct {
	// Name selects the bot to check (required).
	Name string
	// Repo is the optional "owner/repo" to validate against.
	// When empty, only the local credentials and app identity are checked.
	Repo string
	// GHBin overrides the gh CLI binary path (tests inject a stub).
	GHBin string
}

// Check runs the bot validator chain and returns all findings. The chain
// short-circuits: if the JWT cannot be minted the subsequent network-dependent
// validators are skipped (they would all fail for the same root cause).
//
// Validators in order:
//  1. jwt-valid       — PEM parses; JWT minted; GET /app confirms app_id match
//  2. installation-exists — GET /app/installations lists ≥ 1 installation
//  3. installation-covers — installation covers the target repo (if --repo set);
//     when repository_selection=selected, verifies via installation token
//  4. secrets-present — RELEASE_BOT_APP_ID + RELEASE_BOT_PRIVATE_KEY present
//     at repo OR org level (if --repo); 403 on org check degrades to warn
//  5. toggle-on       — Actions can_approve_pull_request_reviews enabled (if --repo)
//  6. caller-workflow — any .github/workflows/*.yml calls release-train.yml
//     (local ./ or koryph/koryph@ reference); message names what it looked for
func Check(ctx context.Context, cfg *Config, opts CheckOptions) ([]CheckFinding, error) {
	ghBin := opts.GHBin
	if ghBin == "" {
		ghBin = "gh"
	}

	var findings []CheckFinding

	// 1. JWT validity + app identity.
	jwt, jwtFinding := checkJWT(ctx, cfg)
	findings = append(findings, jwtFinding)
	if jwtFinding.Level == CheckFail {
		// Cannot proceed to network checks without a valid JWT.
		return findings, nil
	}

	// 2. Installation exists.
	installs, installFinding := checkInstallations(ctx, jwt)
	findings = append(findings, installFinding)
	if installFinding.Level == CheckFail {
		return findings, nil
	}

	if opts.Repo == "" {
		// Repo-scoped checks are optional.
		return findings, nil
	}

	owner, _, err := splitOwnerRepo(opts.Repo)
	if err != nil {
		return findings, fmt.Errorf("bot check: %w", err)
	}

	// 3. Installation covers the target repo (owner-match + repo-selection check).
	findings = append(findings, checkInstallationCovers(ctx, jwt, installs, owner, opts))

	// 4. Secrets present (best-effort; degrade gracefully on gh errors).
	findings = append(findings, checkSecrets(opts.Repo, ghBin, opts.Name)...)

	// 5. Actions toggle.
	findings = append(findings, checkActionsToggle(opts.Repo, ghBin, opts.Name))

	// 6. Caller workflow present.
	findings = append(findings, checkCallerWorkflow(ctx, cfg, opts.Repo, ghBin))

	return findings, nil
}

// --- individual validators --------------------------------------------------

// appIdentity is the relevant subset of GET /app response.
type appIdentity struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// checkJWT resolves the key (vault or inline), mints a JWT, calls GET /app,
// and verifies the app_id matches.
func checkJWT(ctx context.Context, cfg *Config) (string, CheckFinding) {
	jwt, err := MintJWTCtx(ctx, cfg)
	if err != nil {
		remediation := fmt.Sprintf("koryph bot create --name %s   # re-provision if the PEM is corrupt", cfg.Name)
		if ve, ok := err.(*VaultErr); ok { //nolint:errorlint
			remediation = ve.Remediation
		}
		return "", CheckFinding{
			Check:       "jwt-valid",
			Level:       CheckFail,
			Message:     fmt.Sprintf("cannot mint JWT: %v", err),
			Remediation: remediation,
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/app", http.NoBody)
	if err != nil {
		return "", CheckFinding{
			Check:   "jwt-valid",
			Level:   CheckFail,
			Message: fmt.Sprintf("build GET /app request: %v", err),
		}
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", CheckFinding{
			Check:   "jwt-valid",
			Level:   CheckFail,
			Message: fmt.Sprintf("GET /app: %v", err),
		}
	}
	defer resp.Body.Close()

	body := make([]byte, 1<<16) // cap at 64 KiB
	n, _ := resp.Body.Read(body)
	body = body[:n]

	if resp.StatusCode != http.StatusOK {
		return "", CheckFinding{
			Check:       "jwt-valid",
			Level:       CheckFail,
			Message:     fmt.Sprintf("GET /app returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))),
			Remediation: fmt.Sprintf("koryph bot create --name %s   # re-provision if the app was deleted", cfg.Name),
		}
	}

	var identity appIdentity
	if err := json.Unmarshal(body, &identity); err != nil {
		return "", CheckFinding{
			Check:   "jwt-valid",
			Level:   CheckFail,
			Message: fmt.Sprintf("parse GET /app response: %v", err),
		}
	}

	if identity.ID != cfg.AppID {
		return "", CheckFinding{
			Check: "jwt-valid",
			Level: CheckFail,
			Message: fmt.Sprintf("app_id mismatch: stored=%d GitHub=%d (slug=%s) — credentials may be stale",
				cfg.AppID, identity.ID, identity.Slug),
			Remediation: fmt.Sprintf("koryph bot create --name %s   # re-provision with correct credentials", cfg.Name),
		}
	}

	return jwt, CheckFinding{
		Check:   "jwt-valid",
		Level:   CheckOK,
		Message: fmt.Sprintf("JWT valid; app_id=%d slug=%s", cfg.AppID, identity.Slug),
	}
}

// checkInstallations calls GET /app/installations and returns the list.
func checkInstallations(ctx context.Context, jwt string) ([]installation, CheckFinding) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/app/installations", http.NoBody)
	if err != nil {
		return nil, CheckFinding{
			Check:   "installation-exists",
			Level:   CheckFail,
			Message: fmt.Sprintf("build installations request: %v", err),
		}
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, CheckFinding{
			Check:   "installation-exists",
			Level:   CheckFail,
			Message: fmt.Sprintf("GET /app/installations: %v", err),
		}
	}
	defer resp.Body.Close()

	body := make([]byte, 1<<20) // cap at 1 MiB
	n, _ := resp.Body.Read(body)
	body = body[:n]

	if resp.StatusCode != http.StatusOK {
		return nil, CheckFinding{
			Check:   "installation-exists",
			Level:   CheckFail,
			Message: fmt.Sprintf("GET /app/installations returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))),
		}
	}

	var installs []installation
	if err := json.Unmarshal(body, &installs); err != nil {
		return nil, CheckFinding{
			Check:   "installation-exists",
			Level:   CheckFail,
			Message: fmt.Sprintf("parse installations: %v", err),
		}
	}

	if len(installs) == 0 {
		return nil, CheckFinding{
			Check:       "installation-exists",
			Level:       CheckFail,
			Message:     "no installations found — the app has not been installed on any account",
			Remediation: "koryph bot install --name <name>   # then choose repos in the browser",
		}
	}

	owners := make([]string, 0, len(installs))
	for _, i := range installs {
		owners = append(owners, i.Account.Login)
	}
	return installs, CheckFinding{
		Check:   "installation-exists",
		Level:   CheckOK,
		Message: fmt.Sprintf("%d installation(s): %s", len(installs), strings.Join(owners, ", ")),
	}
}

// checkInstallationCovers verifies that one of the installations covers the
// target repo.
//
//   - repository_selection == "all" (or empty): any repo under the owner is
//     covered; return OK immediately.
//   - repository_selection == "selected": mint an installation token and list
//     the repos the installation can actually access to give a real verdict.
func checkInstallationCovers(ctx context.Context, jwt string, installs []installation, owner string, opts CheckOptions) CheckFinding {
	for _, i := range installs {
		if !strings.EqualFold(i.Account.Login, owner) {
			continue
		}
		// Owner matches. Inspect repository_selection.
		sel := strings.ToLower(i.RepositorySelection)
		if sel == "" || sel == "all" {
			return CheckFinding{
				Check:   "installation-covers",
				Level:   CheckOK,
				Message: fmt.Sprintf("installation %d covers %q (all repositories)", i.ID, owner),
			}
		}
		// "selected" — must verify that the specific repo is in the covered list.
		return checkInstallationCoversRepo(ctx, jwt, i.ID, owner, opts)
	}
	return CheckFinding{
		Check:   "installation-covers",
		Level:   CheckFail,
		Message: fmt.Sprintf("no installation covers owner %q", owner),
		Remediation: fmt.Sprintf("koryph bot attach --name %s --repo %s   # adds the repo to the installation",
			opts.Name, opts.Repo),
	}
}

// checkInstallationCoversRepo checks that a specific repo is accessible under
// the installation when repository_selection is "selected". It mints a
// short-lived installation token and lists the repos it covers.
func checkInstallationCoversRepo(ctx context.Context, jwt string, iid int64, owner string, opts CheckOptions) CheckFinding {
	instToken, err := mintInstallationToken(ctx, jwt, iid)
	if err != nil {
		// Degrade gracefully — we know the owner matches, just can't verify repo.
		return CheckFinding{
			Check: "installation-covers",
			Level: CheckWarn,
			Message: fmt.Sprintf(
				"installation %d covers %q (repository_selection=selected; "+
					"cannot verify %s is in covered list: %v)",
				iid, owner, opts.Repo, err),
		}
	}

	covered, err := listInstallationRepos(ctx, instToken)
	if err != nil {
		return CheckFinding{
			Check: "installation-covers",
			Level: CheckWarn,
			Message: fmt.Sprintf(
				"installation %d covers %q (repository_selection=selected; "+
					"cannot list covered repos: %v)",
				iid, owner, err),
		}
	}

	for _, r := range covered {
		if strings.EqualFold(r, opts.Repo) {
			return CheckFinding{
				Check:   "installation-covers",
				Level:   CheckOK,
				Message: fmt.Sprintf("installation %d covers %s (repository_selection=selected)", iid, opts.Repo),
			}
		}
	}
	return CheckFinding{
		Check: "installation-covers",
		Level: CheckFail,
		Message: fmt.Sprintf(
			"installation %d covers %q (selected) but %s is not in the covered repository list",
			iid, owner, opts.Repo),
		Remediation: fmt.Sprintf("koryph bot attach --name %s --repo %s   # adds the repo to the installation",
			opts.Name, opts.Repo),
	}
}

// mintInstallationToken calls POST /app/installations/{id}/access_tokens with
// the app JWT and returns the short-lived installation token.
func mintInstallationToken(ctx context.Context, jwt string, iid int64) (string, error) {
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", iid)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST /app/installations/%d/access_tokens: %w", iid, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 1<<16)
	n, _ := resp.Body.Read(body)
	body = body[:n]

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("POST /app/installations/%d/access_tokens returned HTTP %d",
			iid, resp.StatusCode)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.Token == "" {
		return "", fmt.Errorf("parse installation token response: %w", err)
	}
	return tok.Token, nil
}

// listInstallationRepos calls GET /installation/repositories with an
// installation token and returns the full_name of every accessible repo.
func listInstallationRepos(ctx context.Context, instToken string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/installation/repositories", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build repos request: %w", err)
	}
	req.Header.Set("Authorization", "token "+instToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /installation/repositories: %w", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 1<<20)
	n, _ := resp.Body.Read(body)
	body = body[:n]

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /installation/repositories returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse repos response: %w", err)
	}
	names := make([]string, 0, len(result.Repositories))
	for _, r := range result.Repositories {
		names = append(names, r.FullName)
	}
	return names, nil
}

// checkSecrets checks that RELEASE_BOT_APP_ID and RELEASE_BOT_PRIVATE_KEY
// are present in the repo's Actions secrets OR the org's Actions secrets.
//
// Search order: repo-level secrets first, then org-level secrets for any that
// are still absent (e.g. provisioned via 'koryph bot attach --org-secrets').
// A 403 on the org-level check (which requires read:org / org-admin scope) is
// tolerated: the finding degrades to WARN naming the exact permission needed.
func checkSecrets(ownerRepo, ghBin, botName string) []CheckFinding {
	owner, _, splitErr := splitOwnerRepo(ownerRepo)
	if splitErr != nil {
		// Defensive only — caller already validated.
		return []CheckFinding{{
			Check:   "secrets-present",
			Level:   CheckWarn,
			Message: fmt.Sprintf("internal: splitOwnerRepo: %v", splitErr),
		}}
	}

	const (
		keyAppID  = "RELEASE_BOT_APP_ID"
		keyAppKey = "RELEASE_BOT_PRIVATE_KEY"
	)
	required := []string{keyAppID, keyAppKey}

	// --- Repo-level secrets ---
	names := make(map[string]bool)
	repoOut, repoErr := exec.Command(ghBin, "secret", "list", //nolint:gosec
		"--repo", ownerRepo, "--jq", ".[].name").Output()
	if repoErr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(repoOut)), "\n") {
			if l := strings.TrimSpace(line); l != "" {
				names[l] = true
			}
		}
	}

	// --- Org-level secrets (checked when any required secret is missing) ---
	orgForbidden := false
	orgForbiddenMsg := ""
	if !names[keyAppID] || !names[keyAppKey] {
		orgCmd := exec.Command(ghBin, "api", //nolint:gosec
			"/orgs/"+owner+"/actions/secrets",
			"--jq", ".secrets[].name")
		orgOut, orgErr := orgCmd.CombinedOutput()
		if orgErr == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(orgOut)), "\n") {
				if l := strings.TrimSpace(line); l != "" {
					names[l] = true
				}
			}
		} else if ghOutputIs403(orgOut) {
			orgForbidden = true
			orgForbiddenMsg = "org-level check skipped (HTTP 403 — " +
				"requires read:org scope or org-admin role; " +
				"run: gh auth refresh -h github.com -s read:org)"
		}
		// Other org errors: silently skip (best-effort; repo result still used).
	}

	// Cannot read repo secrets at all — warn and bail.
	if repoErr != nil && len(names) == 0 {
		return []CheckFinding{{
			Check:   "secrets-present",
			Level:   CheckWarn,
			Message: fmt.Sprintf("%s: cannot read secrets (gh api error — likely no admin access; best-effort check skipped)", ownerRepo),
		}}
	}

	var findings []CheckFinding
	for _, want := range required {
		switch {
		case names[want]:
			findings = append(findings, CheckFinding{
				Check:   "secrets-present",
				Level:   CheckOK,
				Message: fmt.Sprintf("%s: %s present", ownerRepo, want),
			})
		case orgForbidden:
			findings = append(findings, CheckFinding{
				Check:   "secrets-present",
				Level:   CheckWarn,
				Message: fmt.Sprintf("%s: %s not found at repo level; %s", ownerRepo, want, orgForbiddenMsg),
			})
		default:
			findings = append(findings, CheckFinding{
				Check:       "secrets-present",
				Level:       CheckFail,
				Message:     fmt.Sprintf("%s: %s missing", ownerRepo, want),
				Remediation: fmt.Sprintf("koryph bot attach --name %s --repo %s", botName, ownerRepo),
			})
		}
	}
	return findings
}

// checkActionsToggle checks that can_approve_pull_request_reviews is enabled.
func checkActionsToggle(ownerRepo, ghBin, botName string) CheckFinding {
	out, err := exec.Command(ghBin, "api", //nolint:gosec
		"/repos/"+ownerRepo+"/actions/permissions/workflow",
		"--jq", ".can_approve_pull_request_reviews").Output()
	if err != nil {
		return CheckFinding{
			Check:   "toggle-on",
			Level:   CheckWarn,
			Message: fmt.Sprintf("%s: cannot read Actions permissions (best-effort check skipped)", ownerRepo),
		}
	}
	if strings.TrimSpace(string(out)) == "true" {
		return CheckFinding{
			Check:   "toggle-on",
			Level:   CheckOK,
			Message: fmt.Sprintf("%s: Actions can_approve_pull_request_reviews: enabled", ownerRepo),
		}
	}
	return CheckFinding{
		Check:       "toggle-on",
		Level:       CheckFail,
		Message:     fmt.Sprintf("%s: Actions can_approve_pull_request_reviews: disabled", ownerRepo),
		Remediation: fmt.Sprintf("koryph bot attach --name %s --repo %s   # enables the toggle", botName, ownerRepo),
	}
}

// checkCallerWorkflow verifies that at least one workflow file in
// .github/workflows/ contains a 'uses:' reference to release-train.yml
// (via the local ./ form or a koryph/koryph@ cross-repo reference).
//
// This intentionally does NOT hardcode a specific file name: release-please,
// semantic-release, and other callers use different filenames. The check
// passes as long as any workflow in the directory delegates to release-train.yml.
func checkCallerWorkflow(_ context.Context, _ *Config, ownerRepo, ghBin string) CheckFinding {
	const lookingFor = "uses: referencing release-train.yml (./ or koryph/koryph@ prefix)"

	// List workflow files in .github/workflows/.
	listOut, err := exec.Command(ghBin, "api", //nolint:gosec
		"/repos/"+ownerRepo+"/contents/.github/workflows",
		"--jq", `[.[] | select(.type == "file") | .name] | .[]`,
	).Output()
	if err != nil {
		return CheckFinding{
			Check: "caller-workflow",
			Level: CheckWarn,
			Message: fmt.Sprintf("%s: cannot list .github/workflows (%v); "+
				"looked for %s", ownerRepo, err, lookingFor),
		}
	}

	names := splitLines(string(listOut))
	if len(names) == 0 {
		return CheckFinding{
			Check: "caller-workflow",
			Level: CheckWarn,
			Message: fmt.Sprintf("%s: no workflow files found in .github/workflows "+
				"(run `koryph release setup`); looked for %s", ownerRepo, lookingFor),
		}
	}

	// Fetch content of each .yml/.yaml file and scan for a release-train.yml reference.
	for _, name := range names {
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		b64Out, err := exec.Command(ghBin, "api", //nolint:gosec
			"/repos/"+ownerRepo+"/contents/.github/workflows/"+name,
			"--jq", ".content",
		).Output()
		if err != nil {
			continue // skip inaccessible or deleted file
		}
		// GitHub returns the file content as base64 with embedded newlines.
		b64 := strings.ReplaceAll(strings.TrimSpace(string(b64Out)), "\n", "")
		decoded, decErr := base64.StdEncoding.DecodeString(b64)
		if decErr != nil {
			continue
		}
		if containsReleaseTrain(string(decoded)) {
			return CheckFinding{
				Check: "caller-workflow",
				Level: CheckOK,
				Message: fmt.Sprintf("%s: %s calls release-train.yml "+
					"(looked for %s)", ownerRepo, name, lookingFor),
			}
		}
	}

	return CheckFinding{
		Check: "caller-workflow",
		Level: CheckWarn,
		Message: fmt.Sprintf("%s: no workflow found that calls release-train.yml "+
			"(run `koryph release setup` if release infra is not yet configured); "+
			"looked for %s", ownerRepo, lookingFor),
	}
}

// containsReleaseTrain reports whether the workflow YAML content contains a
// uses: line that references release-train.yml via the local ./ form or a
// koryph/koryph@ cross-repo reference.
func containsReleaseTrain(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "uses:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(trimmed, "uses:"))
		// Local form:       uses: ./.github/workflows/release-train.yml
		// Cross-repo form:  uses: koryph/koryph/.github/workflows/release-train.yml@v1
		if strings.HasSuffix(val, "release-train.yml") ||
			strings.Contains(val, "release-train.yml@") {
			return true
		}
	}
	return false
}

// splitLines splits a newline-delimited string into non-empty trimmed lines.
func splitLines(s string) []string {
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	return lines
}

// CredentialFinding is a lightweight offline-only finding for use by
// koryph doctor (no network calls). It verifies that the stored PEM can
// produce a valid JWT structure (parse + RSA sign with test data).
type CredentialFinding struct {
	Name    string
	Level   CheckLevel
	Message string
}

// CheckCredentials performs an offline-only credential check on all stored
// bots. It is called by koryph doctor to surface corrupted credential files
// and plaintext-key posture warnings without making any network calls.
//
// For inline-mode bots (Provider == "") the PEM is validated directly.
// For pointer-mode bots (Provider set) the credential pointer is validated
// (provider and key_ref present); no vault fetch is performed.
func CheckCredentials() ([]CredentialFinding, error) {
	names, err := List()
	if err != nil {
		return nil, err
	}
	var findings []CredentialFinding
	for _, name := range names {
		cfg, err := Load(name)
		if err != nil {
			findings = append(findings, CredentialFinding{
				Name:    name,
				Level:   CheckFail,
				Message: fmt.Sprintf("load error: %v", err),
			})
			continue
		}
		findings = append(findings, credentialFindingsFor(cfg)...)
	}
	return findings, nil
}

// CredentialFindingsFor checks one specific bot's offline credential validity.
// Returns nil slice when no bot with that name exists.
func CredentialFindingsFor(name string) ([]CredentialFinding, error) {
	cfg, err := Load(name)
	if err != nil {
		return nil, err
	}
	return credentialFindingsFor(cfg), nil
}

// credentialFindingsFor runs the offline credential + posture checks for one bot.
func credentialFindingsFor(cfg *Config) []CredentialFinding {
	var findings []CredentialFinding

	if cfg.IsPointer() {
		// Pointer mode: verify that provider and key_ref are present.
		if cfg.KeyRef == "" {
			findings = append(findings, CredentialFinding{
				Name:    cfg.Name,
				Level:   CheckFail,
				Message: fmt.Sprintf("pointer-mode bot %q is missing key_ref — re-run `koryph bot vault-migrate --name %s`", cfg.Name, cfg.Name),
			})
		} else {
			// Check posture of the pointer (provider classification).
			posture := botPosture(cfg)
			msg := fmt.Sprintf("credentials ok (app_id=%d owner=%s provider=%s)", cfg.AppID, cfg.Owner, cfg.Provider)
			level := CheckOK
			if posture.Note != "" {
				msg += " — " + posture.Note
			}
			findings = append(findings, CredentialFinding{
				Name:    cfg.Name,
				Level:   level,
				Message: msg,
			})
		}
		return findings
	}

	// Inline mode: validate the PEM.
	if err := ValidatePEM(cfg); err != nil {
		findings = append(findings, CredentialFinding{
			Name:    cfg.Name,
			Level:   CheckFail,
			Message: fmt.Sprintf("invalid PEM: %v — run `koryph bot create --name %s` to re-provision", err, cfg.Name),
		})
		return findings
	}

	findings = append(findings, CredentialFinding{
		Name:    cfg.Name,
		Level:   CheckOK,
		Message: fmt.Sprintf("credentials ok (app_id=%d owner=%s)", cfg.AppID, cfg.Owner),
	})

	// Posture check: WARN when the key is stored as plaintext.
	// Keys encrypted with encrypted-file or stored in Keychain are the same
	// posture as a passphrase-protected ~/.ssh key — no warning for those.
	posture := botPosture(cfg)
	if !posture.Level.PostureOK() {
		findings = append(findings, CredentialFinding{
			Name:  cfg.Name,
			Level: CheckWarn,
			Message: fmt.Sprintf(
				"%s: private key stored as plaintext in %s — "+
					"migrate to a secure provider with `koryph bot vault-migrate --name %s`",
				cfg.Name, BotPath(cfg.Name), cfg.Name),
		})
	}

	return findings
}

// botPosture classifies the security posture of a bot's key storage.
// It reuses the signing package's posture classifier by mapping the bot
// config fields to a signing.Config.
func botPosture(cfg *Config) signing.PostureResult {
	sigCfg := &signing.Config{
		Provider: cfg.Provider,
		KeyRef:   cfg.KeyRef,
	}
	// For inline mode (Provider==""), KeyRef is empty and ClassifyPosture returns
	// PosturePlaintext ("no provider").
	// For pointer mode, ClassifyPosture inspects Provider correctly.
	return signing.ClassifyPosture(sigCfg)
}

// PrintCheckResults renders check findings to w in a human-readable format.
// Returns the worst exit code: 0 ok / 1 warn / 2 fail.
func PrintCheckResults(w io.Writer, findings []CheckFinding) int {
	code := 0
	for _, f := range findings {
		mark := "✓"
		switch f.Level {
		case CheckWarn:
			mark = "!"
			if code < 1 {
				code = 1
			}
		case CheckFail:
			mark = "✗"
			code = 2
		}
		fmt.Fprintf(w, "  %s %-26s %s\n", mark, "["+string(f.Check)+"]", f.Message)
		if f.Remediation != "" {
			fmt.Fprintf(w, "    → %s\n", f.Remediation)
		}
	}
	return code
}
