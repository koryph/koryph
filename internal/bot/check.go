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
//  3. installation-covers — installation covers the target repo (if --repo set)
//  4. secrets-present — RELEASE_BOT_APP_ID + RELEASE_BOT_PRIVATE_KEY present (if --repo)
//  5. toggle-on       — Actions can_approve_pull_request_reviews enabled (if --repo)
//  6. caller-workflow — .github/workflows/release.yml present in repo (if --repo)
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

	// 3. Installation covers the target owner.
	findings = append(findings, checkInstallationCovers(installs, owner, opts))

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

// checkJWT mints a JWT, calls GET /app, and verifies the app_id matches.
func checkJWT(ctx context.Context, cfg *Config) (string, CheckFinding) {
	jwt, err := MintJWT(cfg)
	if err != nil {
		return "", CheckFinding{
			Check:       "jwt-valid",
			Level:       CheckFail,
			Message:     fmt.Sprintf("cannot mint JWT: %v", err),
			Remediation: fmt.Sprintf("koryph bot create --name %s   # re-provision if the PEM is corrupt", cfg.Name),
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

// checkInstallationCovers verifies that one of the installations covers owner.
func checkInstallationCovers(installs []installation, owner string, opts CheckOptions) CheckFinding {
	for _, i := range installs {
		if strings.EqualFold(i.Account.Login, owner) {
			return CheckFinding{
				Check:   "installation-covers",
				Level:   CheckOK,
				Message: fmt.Sprintf("installation %d covers %q", i.ID, owner),
			}
		}
	}
	return CheckFinding{
		Check:   "installation-covers",
		Level:   CheckFail,
		Message: fmt.Sprintf("no installation covers owner %q", owner),
		Remediation: fmt.Sprintf("koryph bot attach --name %s --repo %s   # adds the repo to the installation",
			opts.Name, opts.Repo),
	}
}

// checkSecrets checks that RELEASE_BOT_APP_ID and RELEASE_BOT_PRIVATE_KEY
// are present in the repo's Actions secrets. Degrades to warn on gh errors.
func checkSecrets(ownerRepo, ghBin, botName string) []CheckFinding {
	out, err := exec.Command(ghBin, "secret", "list", //nolint:gosec
		"--repo", ownerRepo, "--jq", ".[].name").Output()
	if err != nil {
		// Likely missing admin access or gh not authenticated — warn but don't fail.
		return []CheckFinding{{
			Check:   "secrets-present",
			Level:   CheckWarn,
			Message: fmt.Sprintf("%s: cannot read secrets (gh api error — likely no admin access; best-effort check skipped)", ownerRepo),
		}}
	}

	names := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			names[l] = true
		}
	}

	var findings []CheckFinding
	for _, want := range []string{"RELEASE_BOT_APP_ID", "RELEASE_BOT_PRIVATE_KEY"} {
		if names[want] {
			findings = append(findings, CheckFinding{
				Check:   "secrets-present",
				Level:   CheckOK,
				Message: fmt.Sprintf("%s: %s present", ownerRepo, want),
			})
		} else {
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

// checkCallerWorkflow checks that .github/workflows/release.yml exists in the
// repository. Uses gh api to avoid requiring a local clone.
func checkCallerWorkflow(_ context.Context, _ *Config, ownerRepo, ghBin string) CheckFinding {
	_, err := exec.Command(ghBin, "api", //nolint:gosec
		"/repos/"+ownerRepo+"/contents/.github/workflows/release.yml",
		"--jq", ".path").Output()
	if err != nil {
		return CheckFinding{
			Check:   "caller-workflow",
			Level:   CheckWarn,
			Message: fmt.Sprintf("%s: .github/workflows/release.yml not found or not accessible (run `koryph release setup` if release infra is not yet configured)", ownerRepo),
		}
	}
	return CheckFinding{
		Check:   "caller-workflow",
		Level:   CheckOK,
		Message: fmt.Sprintf("%s: .github/workflows/release.yml present", ownerRepo),
	}
}

// CredentialFinding is a lightweight offline-only finding for use by
// koryph doctor (no network calls). It verifies that the stored PEM can
// produce a valid JWT structure (parse + RSA sign with test data).
type CredentialFinding struct {
	Name    string
	Level   CheckLevel
	Message string
}

// CheckCredentials performs an offline-only PEM validity check on all stored
// bots. It is called by koryph doctor to surface corrupted credential files
// without making any network calls.
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
		if err := ValidatePEM(cfg); err != nil {
			findings = append(findings, CredentialFinding{
				Name:    name,
				Level:   CheckFail,
				Message: fmt.Sprintf("invalid PEM: %v — run `koryph bot create --name %s` to re-provision", err, name),
			})
			continue
		}
		findings = append(findings, CredentialFinding{
			Name:    name,
			Level:   CheckOK,
			Message: fmt.Sprintf("credentials ok (app_id=%d owner=%s)", cfg.AppID, cfg.Owner),
		})
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
	if err := ValidatePEM(cfg); err != nil {
		return []CredentialFinding{{
			Name:    name,
			Level:   CheckFail,
			Message: fmt.Sprintf("invalid PEM: %v — run `koryph bot create --name %s` to re-provision", err, name),
		}}, nil
	}
	return []CredentialFinding{{
		Name:    name,
		Level:   CheckOK,
		Message: fmt.Sprintf("credentials ok (app_id=%d owner=%s)", cfg.AppID, cfg.Owner),
	}}, nil
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
