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

## Configure your vault once

`koryph bot create` stores the App's private key in a vault. To avoid
passing `--vault-provider` on every invocation, configure a default in
`~/.koryph/config.json` (machine-wide) or `koryph.project.json` (per project):

**Machine-wide** (`~/.koryph/config.json`):
```json
{
  "vault": {
    "provider":  "protonpass",
    "container": "Engineering"
  }
}
```

**Per-project** (`koryph.project.json`):
```json
{
  "vault": {
    "provider":  "onepassword",
    "container": "Personal"
  }
}
```

With either block in place, `koryph bot create` picks up the provider
automatically and you can skip `--vault-provider`.

See [Commit & Artifact Signing — Configure your vault once](signing.md#configure-your-vault-once)
for the full resolution order and per-provider `container` semantics.

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

### Step 3 — Attach the bot to a repository

```bash
koryph bot attach --name mylogin-release-bot --repo OWNER/REPO
```

This performs all wiring in a single idempotent command:

1. Mints a short-lived JWT from the stored PEM (no `gh` dependency for
   App-auth calls).
2. Resolves the App installation that covers `OWNER` via the GitHub App API.
3. Adds `OWNER/REPO` to the installation (idempotent).
4. Sets `RELEASE_BOT_APP_ID` and `RELEASE_BOT_PRIVATE_KEY` as per-repo
   Actions secrets.
5. Enables the `can_approve_pull_request_reviews` toggle on the repository.

For org-level secrets (shared across repos in the same org), pass
`--org-secrets`:

```bash
koryph bot attach --name mylogin-release-bot --repo OWNER/REPO --org-secrets
```

### Step 4 — Wire the bot to a project

```bash
koryph release setup --project <id>
```

### Step 5 — Verify

```bash
# Full validator chain: JWT validity, installation, secrets, toggle, workflow
koryph bot check --name mylogin-release-bot --repo OWNER/REPO

# Per-project doctor check (also checks bot credentials offline)
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

# Wire the specific repo
koryph bot attach --name mylogin-release-bot --repo GUEST-ORG/MY-REPO
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

# Wire a specific repo (sets per-repo secrets by default;
# use --org-secrets to set org-level secrets shared across repos)
koryph bot attach --name myorg-release-bot --repo myorg/my-repo
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

Add `--check` for a quick offline PEM validity check:

```bash
koryph bot list --check
```

For a full live identity check against the GitHub API, use `koryph bot check`:

```bash
koryph bot check --name mylogin-release-bot
```

---

## Checking bot health

`koryph bot check` runs a validator chain with precise remediation per failure:

| Check | What it validates |
|-------|------------------|
| `jwt-valid` | PEM parses; JWT minted; `GET /app` confirms `app_id` match |
| `installation-exists` | At least one installation found for the app |
| `installation-covers` | Installation covers the target `OWNER` (with `--repo`) |
| `secrets-present` | `RELEASE_BOT_APP_ID` + `RELEASE_BOT_PRIVATE_KEY` present |
| `toggle-on` | `can_approve_pull_request_reviews` is enabled |
| `caller-workflow` | `.github/workflows/release.yml` present in the repo |

```bash
# Identity check only (no --repo):
koryph bot check --name mylogin-release-bot

# Full check against a specific repo:
koryph bot check --name mylogin-release-bot --repo OWNER/REPO
```

Exit codes: `0` all ok / `1` warnings / `2` failures.

---

## `koryph doctor` integration

`koryph doctor --project <id>` automatically checks bot health when the project
has a release block configured:

- **`release-bot-secrets`**: verifies `RELEASE_BOT_APP_ID` and
  `RELEASE_BOT_PRIVATE_KEY` are present on the project's GitHub repo.
- **`actions-approval`**: verifies `can_approve_pull_request_reviews` is enabled.
- **`bot-credentials`**: offline PEM validity check for all stored bots in
  `~/.koryph/bots/`.

The bot-credentials check never makes a network call — it surfaces corrupted
credential files before the operator tries to use them.

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

---

## Fallback ladder

When a GitHub App cannot be installed (org policy, personal-account restrictions,
or simply not worth the setup for a low-frequency project), the release pipeline
degrades gracefully through a ladder of supported alternatives. Choose the
rung that fits your constraints.

| Rung | Mechanism | Checks fire automatically? | Operator can self-approve? | Per-release action required |
|------|-----------|---------------------------|---------------------------|----------------------------|
| **1 — Bot (preferred)** | GitHub App (`RELEASE_BOT_APP_ID` + `RELEASE_BOT_PRIVATE_KEY`) | Yes — App is not GITHUB_TOKEN | Yes — operator is not the PR author | None |
| **2 — Bot-less + kick** | GITHUB_TOKEN fallback | No — use `koryph release kick` | Yes | `koryph release kick --repo OWNER/REPO` |
| **3 — PAT as token** | Fine-grained PAT in `token:` action input | Yes — PAT is a real actor | **No** — PAT owner becomes PR author; cannot self-approve | None, but needs a second reviewer (or no required-approval ruleset) |
| **4 — Admin-merge** | Admin bypass of branch-protection on merge | N/A — admin bypasses check requirement | N/A | Admin merge via GitHub UI or `gh pr merge --admin` |

### Rung 1 — GitHub App (preferred)

See the sections above. Run `koryph bot create` + `koryph bot attach` then
`koryph release setup --bot`. The Release PR is authored by the App identity;
checks fire on every push; the operator approves and merges normally.

### Rung 2 — Bot-less with `koryph release kick`

No bot installed. The caller workflow falls back to `GITHUB_TOKEN` — release-please
opens and updates the Release PR normally, but GitHub's platform rule prevents
`GITHUB_TOKEN`-triggered events from firing workflows. Close+reopen under a
real actor's token to unblock:

```bash
koryph release kick --repo OWNER/REPO
# auto-detects the PR by the "autorelease: pending" label

koryph release kick --repo OWNER/REPO --pr 42
# explicit PR number (guard relaxed to a warning)

koryph release kick --repo OWNER/REPO --wait
# blocks until all check conclusions arrive (default timeout: 10 min)
```

`koryph doctor --project ID` reports **"bot-less: kick required per release"**
when secrets are absent, so the operator knows exactly what to do.

**Trade-off:** one manual step per release cycle. Suitable for infrequent
releases where bot setup overhead is not justified.

### Rung 3 — Fine-grained PAT as the action token

Set a fine-grained PAT (with `Contents: write` + `Pull requests: write`
permissions on the repo) as a repository secret, then pass it as the `token:`
input to `actions/create-github-app-token` — or, more directly, supply it as
the workflow `GITHUB_TOKEN` override via environment:

```yaml
# In the caller workflow (manual customization):
jobs:
  release:
    uses: koryph/koryph/.github/workflows/release-train.yml@main
    with:
      ...
    secrets:
      token: ${{ secrets.MY_PAT_SECRET }}
```

**Checks fire automatically** because the PAT is a real actor. **However**:
the PAT owner becomes the PR author. GitHub's branch-protection rules prevent
an author from approving their own PR — so:

- Without a required-approval ruleset → fine (no approver needed).
- With a required reviewer count ≥ 1 → a **second reviewer** must approve,
  or the approval requirement must be waived for release-bot PRs.

**Trade-off:** no per-release manual step, but the self-approval limitation
means either a second reviewer is required or the required-approval rule must
be relaxed/bypassed for release-bot PRs. The PAT must also be rotated
periodically (fine-grained PATs expire).

### Rung 4 — Admin-merge escape hatch

An admin (anyone with the "Bypass pull request requirements" permission on the
repository) can merge the Release PR without satisfying checks or approval
rules. This is an emergency escape hatch, not a workflow:

```bash
gh pr merge --repo OWNER/REPO 42 --admin --squash
```

**Trade-off:** bypasses all branch-protection requirements. Only appropriate
when a release is time-critical and no other rung is available.

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

### `koryph bot check` reports "no installation found"

Run `koryph bot install --name <name>` to open the GitHub installation page,
then select the repositories to grant access to.  After installing, re-run
`koryph bot attach`.

### `koryph bot check` reports `secrets-present: warn`

The check requires repository admin access to read secret names via the GitHub
API.  If you have admin access and the secrets are still missing, run:

```bash
koryph bot attach --name <name> --repo OWNER/REPO
```

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
- JWTs minted by `koryph bot attach` and `koryph bot check` are also
  short-lived (10-minute expiry), used only for the GitHub App API, and never
  stored or logged.
