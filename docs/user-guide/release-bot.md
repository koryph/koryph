<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Release Bot: GitHub App provisioning

The koryph release pipeline uses a **GitHub App** (the "release bot") to open
and update Release PRs. This document explains why an App is required, how to
perform the one-time setup, and how to replicate it across projects and
organisations.

All provisioning is done with the **`koryph bot`** command family — no repo
clone, no Python, and no separate scripts required.

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

The App needs only two permissions (`Contents: write`, `Pull requests: write`)
and has **no webhook** — it is a narrow-scope, no-inbound-listener identity.

---

## Prerequisites

- **`koryph`** installed (`brew install koryph/tap/koryph` or from source).
- **`gh`** (GitHub CLI) installed and authenticated (`gh auth status`).
- **Browser access** to [github.com](https://github.com) from the machine
  running the command (only needed for `bot create`; use `--headless` on
  remote machines).

---

## Three replication scenarios

Choose the scenario that matches your ownership model:

| Scenario | Command | Who can install |
|----------|---------|----------------|
| Personal account (default) | `koryph bot create` | Owning account only |
| Guest org (repo admin, not org owner) | `koryph bot create --public` | Any repo admin (scoped) |
| Org you own | `koryph bot create --org <org>` | Within the org |

---

## Scenario A — Personal account (private bot)

One bot, one account.  The operator who creates the App can install it on any
repo they own.

### Step 1 — Create the App (one browser click)

```bash
koryph bot create --name mylogin-release-bot
```

What happens:

1. `koryph bot create` starts a local redirect catcher on a random port.
2. Your browser opens with a form pre-filled with the App manifest.
3. The form auto-submits to `github.com/settings/apps/new`.
4. **Click "Create GitHub App"** — this is the single human action.
5. GitHub redirects back to the local server with a one-time code.
6. koryph exchanges the code for the App ID and private key PEM.
7. Credentials are saved to `~/.koryph/bots/mylogin-release-bot.json` (mode 0600).

The PEM is **never printed** to the terminal.

> **Headless machines:** if the browser cannot open automatically, pass
> `--headless` and visit the URL printed to the terminal from another device.
>
> ```bash
> koryph bot create --name mylogin-release-bot --headless
> ```

### Step 2 — Install the App (one browser click)

```bash
koryph bot install --name mylogin-release-bot
```

This prints the installation URL and opens it in your browser.  Select the
repositories you want to grant access to, then click **Install**.

### Step 3 — Wire the bot to a project

```bash
koryph release setup --project <id> --bot mylogin-release-bot
```

### Step 4 — Verify

```bash
koryph doctor --project <id>
```

---

## Scenario B — Guest org (public bot, repo-admin installs)

You administer repos in an organisation you do not own.  A **public** bot can
be installed by any repo admin in any org — scoped to the repos they select.

```bash
# Create a public bot (any repo admin can install it)
koryph bot create --name mylogin-release-bot --public

# Install: visit the URL and select the guest-org repos you administer
koryph bot install --name mylogin-release-bot
```

On the installation page, select the **guest organisation** from the account
dropdown, then choose the specific repos you administer.  GitHub creates a
**repo-scoped installation** — it does not grant access to the entire org.

#### Approval-request behaviour

If the guest org has a policy that requires admin approval for third-party app
installs, your install click sends an **approval request** to the org owner.
The bot activates only after the org owner approves.  Check with the org owner
if the install appears to be pending.

---

## Scenario C — Org you own (org-private bot)

```bash
# Create an org-owned private bot
koryph bot create --name myorg-release-bot --org myorg

# Install within the org
koryph bot install --name myorg-release-bot
```

The bot is owned by the org and can only be installed within that org.

---

## Listing bots

```bash
koryph bot list
```

```
mylogin-release-bot            app_id=12345      owner=mylogin           private
myorg-release-bot              app_id=67890      owner=myorg             private
```

---

## Credential storage

Credentials are stored at `~/.koryph/bots/<name>.json` (mode 0600):

```json
{
  "name": "mylogin-release-bot",
  "app_id": 12345,
  "slug": "mylogin-release-bot",
  "owner": "mylogin",
  "public": false,
  "pem": "<RSA private key — never printed to terminal>"
}
```

The PEM field is the **only secret** — the other fields are safe to inspect or
copy.  Never commit this file to a repository.

---

## How the workflow uses the App

The release workflow mints a short-lived installation token using
[`actions/create-github-app-token`](https://github.com/actions/create-github-app-token):

```yaml
- uses: actions/create-github-app-token@v1
  id: app-token
  with:
    app-id: ${{ secrets.RELEASE_BOT_APP_ID }}
    private-key: ${{ secrets.RELEASE_BOT_PRIVATE_KEY }}
```

**Fallback:** when `RELEASE_BOT_APP_ID` or `RELEASE_BOT_PRIVATE_KEY` are
absent, the workflow falls back to `GITHUB_TOKEN`.  Repositories without the
bot still work — they just inherit the PAT self-approval limitation (close/
reopen the Release PR with a different token to trigger checks).

---

## Troubleshooting

### "Timed out waiting for the GitHub callback"

`koryph bot create` waited for the redirect without receiving it (default
timeout: 5 minutes).  Causes:

- The browser did not open: use `--headless` and visit the URL manually.
- You did not click **Create GitHub App** on the GitHub page.

Re-run `koryph bot create`; each run generates a fresh one-time code.

### "Code exchange failed (HTTP 404)"

Manifest codes are **single-use** and expire quickly.  Re-run
`koryph bot create` to get a new code.

### "Bot already exists"

A credential file already exists at `~/.koryph/bots/<name>.json`.  Either:

- Delete the file and re-run to provision a new App, **or**
- Use `--name` with a different name to create a parallel App.

### Permission denied during installation

If the org requires admin approval, your install click submits a request that
the org owner must approve.  The bot will not be active until the org owner
approves the request via GitHub.

---

## Security notes

- The private key PEM is stored at mode `0600` in `~/.koryph/bots/`.
  Do not commit this file to a repository.
- Repository secrets (`RELEASE_BOT_APP_ID`, `RELEASE_BOT_PRIVATE_KEY`) are
  encrypted at rest by GitHub and never exposed in workflow logs.
- The App has **no webhook** — it cannot receive inbound events, minimising
  its attack surface.
- Installation tokens minted by `actions/create-github-app-token` are
  short-lived (≤ 1 hour) and scoped to the repository.
- Only `contents: write` and `pull_requests: write` are requested — no org
  permissions, no admin access.
