// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/forge"
)

// fakeGH writes a shell script to a temp dir and returns the path.
// The script receives all gh args via "$@" and switches on key patterns.
// The --jq flag is effectively ignored: the script returns pre-filtered output.
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	content := "#!/bin/sh\n" + script
	if err := os.WriteFile(bin, []byte(content), 0o755); err != nil {
		t.Fatalf("fakeGH: write: %v", err)
	}
	return bin
}

// encodeWorkflow base64-encodes workflow YAML content the way GitHub does
// (StdEncoding, no embedded newlines — the real API adds newlines but our
// decoder strips them first so either form works in tests).
func encodeWorkflow(t *testing.T, yaml string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(yaml))
}

type fakeRepoService struct{ bin string }

func (s fakeRepoService) DetectCurrent(context.Context) (string, error) {
	return "", forge.ErrUnsupported
}
func (s fakeRepoService) Get(context.Context, string, string) (*forge.RepoSettings, error) {
	return nil, forge.ErrUnsupported
}
func (s fakeRepoService) Update(context.Context, string, string, *forge.RepoSettings) error {
	return forge.ErrUnsupported
}
func (s fakeRepoService) GetRaw(context.Context, string, string) (json.RawMessage, error) {
	return nil, forge.ErrUnsupported
}
func (s fakeRepoService) PatchRaw(context.Context, string, string, json.RawMessage) error {
	return forge.ErrUnsupported
}
func (s fakeRepoService) VulnAlerts(context.Context, string, string) (bool, error) {
	return false, forge.ErrUnsupported
}
func (s fakeRepoService) SetVulnAlerts(context.Context, string, string, bool) error {
	return forge.ErrUnsupported
}
func (s fakeRepoService) ActionsWorkflow(context.Context, string, string) (json.RawMessage, error) {
	return nil, forge.ErrUnsupported
}
func (s fakeRepoService) SetActionsWorkflow(context.Context, string, string, json.RawMessage) error {
	return forge.ErrUnsupported
}
func (s fakeRepoService) ListFiles(_ context.Context, owner, repo, path string) ([]string, error) {
	out, err := exec.Command(s.bin, "api", "/repos/"+owner+"/"+repo+"/contents/"+path).Output() //nolint:gosec
	if err != nil {
		return nil, err
	}
	return splitLines(string(out)), nil
}
func (s fakeRepoService) ReadFile(_ context.Context, owner, repo, path string) ([]byte, error) {
	out, err := exec.Command(s.bin, "api", "/repos/"+owner+"/"+repo+"/contents/"+path).Output() //nolint:gosec
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", ""))
}

type fakeSecretsService struct{ bin string }

func (s fakeSecretsService) ListRepo(_ context.Context, owner, repo string) ([]string, error) {
	out, err := exec.Command(s.bin, "secret", "list", "--repo", owner+"/"+repo).Output() //nolint:gosec
	return splitLines(string(out)), err
}
func (s fakeSecretsService) ListOrg(_ context.Context, org string) ([]string, error) {
	out, err := exec.Command(s.bin, "api", "/orgs/"+org+"/actions/secrets").CombinedOutput() //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	return splitLines(string(out)), nil
}
func (fakeSecretsService) SetRepo(context.Context, string, string, string, string) error {
	return forge.ErrUnsupported
}
func (fakeSecretsService) SetOrg(context.Context, string, string, string, []string) error {
	return forge.ErrUnsupported
}

type fakeBotService struct{ bin string }

func (fakeBotService) CurrentUser(context.Context) (string, error) { return "", forge.ErrUnsupported }
func (fakeBotService) ExchangeManifest(context.Context, string) (forge.BotConfig, error) {
	return forge.BotConfig{}, forge.ErrUnsupported
}
func (fakeBotService) ListInstallations(context.Context, string) ([]forge.Installation, error) {
	return nil, forge.ErrUnsupported
}
func (fakeBotService) MintInstallationToken(context.Context, string, int64) (string, error) {
	return "", forge.ErrUnsupported
}
func (s fakeBotService) AttachRepository(_ context.Context, ownerRepo string, installationID int64) (forge.RepositoryAttachment, error) {
	out, err := exec.Command(s.bin, "api", "/repos/"+ownerRepo).Output() //nolint:gosec
	if err != nil {
		return forge.RepositoryAttachment{}, err
	}
	var id int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &id); err != nil {
		return forge.RepositoryAttachment{}, err
	}
	cmd := exec.Command(s.bin, "api", "-X", "PUT", fmt.Sprintf("/user/installations/%d/repositories/%d", installationID, id)) //nolint:gosec
	if out, err := cmd.CombinedOutput(); err != nil {
		return forge.RepositoryAttachment{RepositoryID: id}, fmt.Errorf("%w: %s", err, out)
	}
	return forge.RepositoryAttachment{RepositoryID: id, Added: true}, nil
}
func (fakeBotService) SetSecrets(context.Context, forge.BotConfig, string) error {
	return forge.ErrUnsupported
}

// --------------------------------------------------------------------------
// checkCallerWorkflow tests
// --------------------------------------------------------------------------

// TestCheckCallerWorkflow_ReleasePleasePasses verifies that a repo with a
// release-please.yml that contains `uses: ./.github/workflows/release-train.yml`
// passes the caller-workflow check, even though the file is not named release.yml.
func TestCheckCallerWorkflow_ReleasePleasePasses(t *testing.T) {
	const ownerRepo = "acme/myapp"
	yaml := `name: Release
on:
  push:
    branches: [main]
jobs:
  release:
    uses: ./.github/workflows/release-train.yml
    secrets: inherit
`
	encoded := encodeWorkflow(t, yaml)

	ghBin := fakeGH(t, `
ARGS="$*"
# List .github/workflows directory
if echo "$ARGS" | grep -q "contents/.github/workflows"; then
  if echo "$ARGS" | grep -q "release-please"; then
    # Return base64 content of release-please.yml
    echo '`+encoded+`'
    exit 0
  fi
  # List directory: return one file name
  echo "release-please.yml"
  exit 0
fi
echo "unexpected: $ARGS" >&2
exit 1
`)

	cfg := &Config{Name: "test-bot", AppID: 42}
	finding := checkCallerWorkflow(t.Context(), cfg, ownerRepo, fakeRepoService{bin: ghBin})
	if finding.Level != CheckOK {
		t.Errorf("expected CheckOK, got %s: %s", finding.Level, finding.Message)
	}
	if !strings.Contains(finding.Message, "release-please.yml") {
		t.Errorf("expected file name in message, got: %q", finding.Message)
	}
	if !strings.Contains(finding.Message, "release-train.yml") {
		t.Errorf("expected 'release-train.yml' in message, got: %q", finding.Message)
	}
}

// TestCheckCallerWorkflow_NoCallerFound verifies WARN when no workflow calls release-train.yml.
func TestCheckCallerWorkflow_NoCallerFound(t *testing.T) {
	const ownerRepo = "acme/myapp"
	yaml := `name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`
	encoded := encodeWorkflow(t, yaml)

	ghBin := fakeGH(t, `
ARGS="$*"
if echo "$ARGS" | grep -q "contents/.github/workflows"; then
  if echo "$ARGS" | grep -q "ci.yml"; then
    echo '`+encoded+`'
    exit 0
  fi
  echo "ci.yml"
  exit 0
fi
echo "unexpected: $ARGS" >&2
exit 1
`)

	cfg := &Config{Name: "test-bot", AppID: 42}
	finding := checkCallerWorkflow(t.Context(), cfg, ownerRepo, fakeRepoService{bin: ghBin})
	if finding.Level != CheckWarn {
		t.Errorf("expected CheckWarn, got %s: %s", finding.Level, finding.Message)
	}
	if !strings.Contains(finding.Message, "looked for") {
		t.Errorf("expected 'looked for' in message, got: %q", finding.Message)
	}
}

// TestCheckCallerWorkflow_CrossRepoRef verifies the koryph/koryph@ form passes.
func TestCheckCallerWorkflow_CrossRepoRef(t *testing.T) {
	const ownerRepo = "acme/myapp"
	yaml := `name: Release
on:
  push: {branches: [main]}
jobs:
  release:
    uses: koryph/koryph/.github/workflows/release-train.yml@v1
    secrets: inherit
`
	encoded := encodeWorkflow(t, yaml)

	ghBin := fakeGH(t, `
ARGS="$*"
if echo "$ARGS" | grep -q "contents/.github/workflows"; then
  if echo "$ARGS" | grep -q "release.yml"; then
    echo '`+encoded+`'
    exit 0
  fi
  echo "release.yml"
  exit 0
fi
echo "unexpected: $ARGS" >&2
exit 1
`)

	cfg := &Config{Name: "test-bot", AppID: 42}
	finding := checkCallerWorkflow(t.Context(), cfg, ownerRepo, fakeRepoService{bin: ghBin})
	if finding.Level != CheckOK {
		t.Errorf("cross-repo ref: expected CheckOK, got %s: %s", finding.Level, finding.Message)
	}
}

// --------------------------------------------------------------------------
// containsReleaseTrain unit tests (no gh binary needed)
// --------------------------------------------------------------------------

func TestContainsReleaseTrain(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "local uses",
			content: "    uses: ./.github/workflows/release-train.yml\n",
			want:    true,
		},
		{
			name:    "cross-repo uses with tag",
			content: "    uses: koryph/koryph/.github/workflows/release-train.yml@v2\n",
			want:    true,
		},
		{
			name:    "no uses at all",
			content: "runs-on: ubuntu-latest\n",
			want:    false,
		},
		{
			name:    "uses different workflow",
			content: "    uses: ./.github/workflows/ci.yml\n",
			want:    false,
		},
		{
			name:    "uses: with leading spaces stripped",
			content: "\t\t    uses: ./.github/workflows/release-train.yml",
			want:    true,
		},
	}
	for _, tc := range cases {
		got := containsReleaseTrain(tc.content)
		if got != tc.want {
			t.Errorf("%s: containsReleaseTrain=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// checkSecrets tests
// --------------------------------------------------------------------------

// TestCheckSecrets_OrgLevelPass verifies that secrets found only at org level
// (not repo level) produce CheckOK findings — the expected path when the repo
// was attached with --org-secrets.
func TestCheckSecrets_OrgLevelPass(t *testing.T) {
	const ownerRepo = "acme/myapp"

	// Repo level returns empty list; org level has both secrets.
	ghBin := fakeGH(t, `
ARGS="$*"
if echo "$ARGS" | grep -q "secret list"; then
  # Repo-level secrets: empty
  exit 0
fi
if echo "$ARGS" | grep -q "actions/secrets"; then
  # Org-level secrets: both present
  printf 'RELEASE_BOT_APP_ID\nRELEASE_BOT_PRIVATE_KEY\n'
  exit 0
fi
echo "unexpected: $ARGS" >&2
exit 1
`)

	findings := checkSecrets(t.Context(), ownerRepo, fakeSecretsService{bin: ghBin}, "test-bot")

	var appIDFound, keyFound bool
	for _, f := range findings {
		if f.Check != "secrets-present" {
			t.Errorf("unexpected check name: %q", f.Check)
		}
		if f.Level != CheckOK {
			t.Errorf("expected CheckOK for %q, got %s: %s", f.Message, f.Level, f.Message)
		}
		if strings.Contains(f.Message, "RELEASE_BOT_APP_ID") {
			appIDFound = true
		}
		if strings.Contains(f.Message, "RELEASE_BOT_PRIVATE_KEY") {
			keyFound = true
		}
	}
	if !appIDFound || !keyFound {
		t.Errorf("expected findings for both secrets; got: %v", findings)
	}
}

// TestCheckSecrets_OrgForbiddenSkips verifies that a 403 on the org-level
// check produces a CheckWarn with a message naming the required permission.
func TestCheckSecrets_OrgForbiddenSkips(t *testing.T) {
	const ownerRepo = "acme/myapp"

	// Repo level returns empty; org level returns a 403-style message.
	ghBin := fakeGH(t, `
ARGS="$*"
if echo "$ARGS" | grep -q "secret list"; then
  # Repo-level secrets: empty
  exit 0
fi
if echo "$ARGS" | grep -q "actions/secrets"; then
  echo "gh: HTTP 403: Must have admin rights" >&2
  exit 1
fi
echo "unexpected: $ARGS" >&2
exit 1
`)

	findings := checkSecrets(t.Context(), ownerRepo, fakeSecretsService{bin: ghBin}, "test-bot")

	if len(findings) == 0 {
		t.Fatal("expected findings, got none")
	}
	for _, f := range findings {
		if f.Level != CheckWarn {
			t.Errorf("expected CheckWarn (org 403 degrade), got %s: %s", f.Level, f.Message)
		}
		if !strings.Contains(f.Message, "403") {
			t.Errorf("expected '403' in message to name the error, got: %q", f.Message)
		}
		if !strings.Contains(strings.ToLower(f.Message), "read:org") {
			t.Errorf("expected 'read:org' permission named in message, got: %q", f.Message)
		}
	}
}

// TestCheckSecrets_RepoPresentSkipsOrg verifies that when both secrets exist at
// repo level the org check is not attempted (the fake gh exits 1 on API calls
// so the test would fail if the org endpoint is reached).
func TestCheckSecrets_RepoPresentSkipsOrg(t *testing.T) {
	const ownerRepo = "acme/myapp"

	ghBin := fakeGH(t, `
ARGS="$*"
if echo "$ARGS" | grep -q "secret list"; then
  printf 'RELEASE_BOT_APP_ID\nRELEASE_BOT_PRIVATE_KEY\n'
  exit 0
fi
# If we ever reach here, the org check was called when it shouldn't be.
echo "unexpected: $ARGS" >&2
exit 1
`)

	findings := checkSecrets(t.Context(), ownerRepo, fakeSecretsService{bin: ghBin}, "test-bot")
	for _, f := range findings {
		if f.Level != CheckOK {
			t.Errorf("expected CheckOK (both at repo level), got %s: %s", f.Level, f.Message)
		}
	}
}

// --------------------------------------------------------------------------
// checkInstallationCovers tests (with httptest for the "selected" path)
// --------------------------------------------------------------------------

// TestCheckInstallationCovers_All verifies the fast path: repository_selection=all.
func TestCheckInstallationCovers_All(t *testing.T) {
	installs := []installation{{
		ID:                  1234,
		RepositorySelection: "all",
		Account: struct {
			Login string `json:"login"`
		}{Login: "acme"},
	}}
	opts := CheckOptions{Name: "test-bot", Repo: "acme/myapp"}
	// jwt is unused when repository_selection=all (no HTTP call is made).
	finding := checkInstallationCovers(t.Context(), "fake-jwt", installs, "acme", opts)
	if finding.Level != CheckOK {
		t.Errorf("expected CheckOK for all-repos installation, got %s: %s", finding.Level, finding.Message)
	}
}

// TestCheckInstallationCovers_SelectedCovers verifies that a "selected"
// installation that includes the target repo passes.
func TestCheckInstallationCovers_SelectedCovers(t *testing.T) {
	// Stand up a fake GitHub API server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"test-inst-token"}`))
		case r.URL.Path == "/installation/repositories":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repositories":[{"full_name":"acme/myapp"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Patch the GitHub API base URL used by mintInstallationToken and
	// listInstallationRepos. We do this by providing a JWT that, when used
	// against the test server, returns the right fixtures.
	// Since those functions hard-code api.github.com, we can't redirect them
	// in unit tests without a transport hook. Instead, test the helpers directly.
	t.Skip("requires HTTP transport override for GitHub API base URL — covered by e2e; skipping unit isolation")
}

// TestCheckInstallationCovers_SelectedMissing verifies that a "selected"
// installation that does NOT include the target repo fails.
func TestCheckInstallationCovers_SelectedMissing(t *testing.T) {
	t.Skip("requires HTTP transport override for GitHub API base URL — covered by e2e; skipping unit isolation")
}

// TestCheckInstallationCovers_NoOwnerMatch verifies the fail path when no
// installation covers the requested owner.
func TestCheckInstallationCovers_NoOwnerMatch(t *testing.T) {
	installs := []installation{{
		ID:                  5,
		RepositorySelection: "all",
		Account: struct {
			Login string `json:"login"`
		}{Login: "other-org"},
	}}
	opts := CheckOptions{Name: "test-bot", Repo: "acme/myapp"}
	finding := checkInstallationCovers(t.Context(), "jwt", installs, "acme", opts)
	if finding.Level != CheckFail {
		t.Errorf("expected CheckFail for no-owner-match, got %s: %s", finding.Level, finding.Message)
	}
	if finding.Remediation == "" {
		t.Error("expected Remediation to be set")
	}
}

// --------------------------------------------------------------------------
// addRepoToInstallation 403 tests
// --------------------------------------------------------------------------

// TestAddRepoToInstallation_403Skips verifies that a 403 from the PUT endpoint
// causes addRepoToInstallation to return skipped=true without an error, and
// that the two remediations are printed to the out writer.
func TestAddRepoToInstallation_403Skips(t *testing.T) {
	const (
		ownerRepo = "acme/myapp"
		iid       = int64(7)
		botName   = "my-bot"
	)

	ghBin := fakeGH(t, `
ARGS="$*"
# Resolve repo ID
if echo "$ARGS" | grep -q "/repos/"; then
  echo "999"
  exit 0
fi
# Check existing repos in installation — not found
if echo "$ARGS" | grep -q "user/installations"; then
  if echo "$ARGS" | grep -qE "^api /user/installations.*repositories$"; then
    echo ""
    exit 0
  fi
  # PUT — return 403
  echo "HTTP 403: Must have read:user scope" >&2
  exit 1
fi
echo "unexpected: $ARGS" >&2
exit 1
`)

	var out strings.Builder
	rid, repoAdded, skipped, err := addRepoToInstallation(
		t.Context(), ownerRepo, iid, fakeBotService{bin: ghBin}, botName, &out)

	if err != nil {
		t.Fatalf("expected nil error on 403 skip, got: %v", err)
	}
	if !skipped {
		t.Errorf("expected skipped=true")
	}
	if repoAdded {
		t.Errorf("expected repoAdded=false when skipped")
	}
	if rid == 0 {
		t.Errorf("expected rid to be set (parsed from repo ID response)")
	}
	msg := out.String()
	if !strings.Contains(msg, "403") {
		t.Errorf("expected '403' in output, got: %q", msg)
	}
	if !strings.Contains(msg, "read:user") {
		t.Errorf("expected 'read:user' remediation in output, got: %q", msg)
	}
	if !strings.Contains(msg, "gh auth refresh") {
		t.Errorf("expected 'gh auth refresh' remediation in output, got: %q", msg)
	}
	if !strings.Contains(msg, "Repository access") {
		t.Errorf("expected UI-path remediation (Repository access) in output, got: %q", msg)
	}
}
