<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Release Bot: GitHub App provisioning

The koryph release pipeline uses a **GitHub App** (the "release bot") to open
and update Release PRs. This document explains why an App is required, how to
perform the one-time setup, and how to replicate it across new projects.

---

## Why a GitHub App, not a PAT?

The release pipeline uses
[release-please](https://github.com/googleapis/release-please) to maintain a
_Release PR_ — a pull request that accumulates the changelog and version bump
for the next release.

When a human-authored PAT opens a pull request, the token's owner becomes the
PR author. GitHub's branch-protection rules prevent authors from approving
their own pull requests. This means:

- The operator who owns the PAT **cannot approve** the Release PR.
- Approvals must come from a different account, adding friction.
- If the repo enforces required reviewers (which koryph-managed repos do),
  the Release PR is permanently blocked unless a second human dismisses the
  author restriction.

A **GitHub App** solves this cleanly:

| Mechanism | PR author | Operator can approve? |
|-----------|-----------|----------------------|
| PAT | The PAT owner | **No** — author cannot self-approve |
| GitHub App | The App (bot identity) | **Yes** — operator is not the author |

The App only needs two permissions (`Contents: write`, `Pull requests: write`)
and has no webhook — it is a narrow-scope, no-inbound-listener identity.

---

## Prerequisites

- **`gh`** (GitHub CLI) installed and authenticated (`gh auth status`).
- **`python3`** on `$PATH` (used by the bootstrap redirect catcher).
- **Browser access** to [github.com](https://github.com) from the machine
  running the script (only needed for `--bootstrap`).
- The repository must allow Actions to create and approve pull requests.
  (The `--attach` command enables this automatically.)

---

## One-time bootstrap (one click)

GitHub does not allow Apps to be created purely via API — the
[App Manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
reduces creation to exactly **one browser click**.

```bash
scripts/provision-release-bot.sh --bootstrap
```

What happens:

1. A local HTTP server starts on port 3737 to catch the redirect.
2. Your browser opens with a form pre-filled with the App manifest (name,
   permissions, redirect URL).
3. The form auto-submits to `github.com/settings/apps/new`. You see GitHub's
   confirmation page.
4. **Click "Create GitHub App"** — this is the single human action.
5. GitHub redirects back to `localhost:3737/callback` with a one-time code.
6. The script exchanges the code for an App ID and private key PEM via the
   GitHub API and stores them in `~/.config/koryph/release-bot/`.
7. GitHub opens the installation page so you can install the App on your
   account or organisation. Choose "All repositories" or specific repos.

The credentials are stored at:

```
~/.config/koryph/release-bot/app-id          # plain text App ID
~/.config/koryph/release-bot/private-key.pem # RSA private key (mode 600)
```

The script is **idempotent** — re-running when credentials already exist
prints their location and exits without creating a second App.

### Custom name or port

```bash
scripts/provision-release-bot.sh --bootstrap \
  --name my-release-bot \
  --port 4242
```

---

## Attaching a repository (zero clicks)

Once the App is bootstrapped and installed on the account that owns the repo,
attach each repository with a single command:

```bash
scripts/provision-release-bot.sh --attach koryph/koryph
```

This command performs five idempotent steps:

| Step | API call | Notes |
|------|----------|-------|
| Resolve installation ID | `GET /user/installations` | Finds the installation for the repo's owner |
| Add repo to installation | `PUT /user/installations/{iid}/repositories/{rid}` | Skipped if already present |
| Set `RELEASE_BOT_APP_ID` secret | `gh secret set` | Skipped if already present |
| Set `RELEASE_BOT_PRIVATE_KEY` secret | `gh secret set` | Skipped if already present |
| Enable Actions PR-approval toggle | `PUT /repos/{o}/{r}/actions/permissions/workflow` | Skipped if already `true` |

The last step — `can_approve_pull_request_reviews=true` — is the IaC capture
of a setting that would otherwise require a manual click in the GitHub UI under
_Settings → Actions → General → Allow GitHub Actions to create and approve pull
requests_.

### Overriding credentials

If credentials are stored in a non-default location:

```bash
scripts/provision-release-bot.sh --attach koryph/other-repo \
  --app-id 12345 \
  --key-file /path/to/private-key.pem
```

---

## Verifying configuration (no mutations)

```bash
# Check local bootstrap credentials only
scripts/provision-release-bot.sh --check

# Check a specific repository
scripts/provision-release-bot.sh --check koryph/koryph
```

The `--check` command reports any missing secrets or misconfigured settings
and exits with a non-zero status if drift is detected. It never mutates
anything, making it suitable for CI-level doctor checks.

---

## How the workflow uses the App

The release workflow uses
[`actions/create-github-app-token`](https://github.com/actions/create-github-app-token)
to mint a short-lived installation token for each run:

```yaml
- uses: actions/create-github-app-token@v1
  id: app-token
  with:
    app-id: ${{ secrets.RELEASE_BOT_APP_ID }}
    private-key: ${{ secrets.RELEASE_BOT_PRIVATE_KEY }}
```

**Fallback:** when `RELEASE_BOT_APP_ID` or `RELEASE_BOT_PRIVATE_KEY` are
absent, the workflow falls back to `GITHUB_TOKEN`. Repositories without the
bot still work — they just inherit the PAT self-approval limitation (close/
reopen the Release PR with a different token to trigger checks).

---

## Replicating to new projects

For each new project managed by koryph:

1. **Bootstrap once per GitHub account** — if you already ran `--bootstrap`
   for your account, skip this step. The App is installed account-wide.

2. **Install on the account** — if the App is not yet installed on the new
   project's account:

   ```bash
   open "https://github.com/settings/apps/<app-name>/installations"
   ```

   Click "Install" and grant access to the target repo.

3. **Attach the repo:**

   ```bash
   scripts/provision-release-bot.sh --attach <owner>/<new-repo>
   ```

4. **Verify:**

   ```bash
   scripts/provision-release-bot.sh --check <owner>/<new-repo>
   ```

No further action is required. The next Release PR opened by release-please
will be authored by the App and immediately approvable by the operator.

---

## Troubleshooting

### "Timed out waiting for the GitHub redirect"

The bootstrap server waited 120 seconds without receiving the redirect. Causes:

- The browser didn't open (headless environment): copy the URL printed to the
  terminal and open it manually.
- You did not click "Create GitHub App": click the button on the GitHub page.
- Port 3737 is in use: use `--port <other-port>`.

### "Could not find an installation for '<owner>'"

The App is not installed on that account/organisation. Open:

```
https://github.com/settings/apps/<app-name>/installations
```

Click "Install" on the target account, then re-run `--attach`.

### "Code exchange failed"

Manifest codes are **single-use** and expire quickly. Re-run `--bootstrap` to
get a new code.

### "Failed to enable Actions PR approval"

The authenticated `gh` token may lack admin access to the repository. Ensure
you are authenticated as a repository admin:

```bash
gh auth status
gh auth login   # re-authenticate if needed
```

---

## Security notes

- The private key PEM is stored at mode `600` under
  `~/.config/koryph/release-bot/private-key.pem`. Do not commit it to a
  repository.
- Repository secrets (`RELEASE_BOT_APP_ID`, `RELEASE_BOT_PRIVATE_KEY`) are
  encrypted at rest by GitHub and never exposed in workflow logs.
- The App has no webhook — it cannot receive inbound events, minimising its
  attack surface.
- Installation tokens minted by `actions/create-github-app-token` are
  short-lived (1 hour max) and scoped to the repository.
