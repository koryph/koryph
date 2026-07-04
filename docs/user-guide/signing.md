<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Commit & Artifact Signing

Koryph supports vault-backed SSH signing for commits and cosign blob
signing for artifacts. The private key **never leaves the vault or SSH
agent** — it is held in memory only, never written to disk, never logged.

Signing is opt-in per project. When `signing.required` is `true` the engine
enforces it at three points: repo git config is applied before dispatch, the
signing key must be loaded before any wave runs, and every commit on a branch
is signature-verified before merge.

## Two agents: operator vs. dispatched

`koryph signing enable` loads the signing key into **two** SSH agents:

1. **Your ambient agent** (`SSH_AUTH_SOCK`) — so your own `git commit -s` signs
   as usual.
2. **The koryph scoped signing agent** — a dedicated `ssh-agent` koryph starts
   (socket under a private per-user temp dir) that holds **only** the signing
   key.

Dispatched agents run headless with `--permission-mode dontAsk` on untrusted
issue text, so koryph never hands them your ambient agent — that typically
carries your personal and production SSH keys, which an injected agent could
use to push or authenticate anywhere. Instead, each dispatched agent's
`SSH_AUTH_SOCK` points at the **scoped** agent, and its whole environment is
built from a credential-free allowlist (see
[IDE integration](../ide-integration.md#3-how-plugin-issued-commands-interoperate-with-koryph-accounts)):
tokens like `GH_TOKEN`, `VAULT_TOKEN`, `AWS_*` and the ambient socket are
dropped. The agent can sign its commits with the signing key and nothing else.

The engine only **verifies** the scoped agent holds the key at run start (it
never touches the vault itself); if it does not, the run fails closed with a
`koryph signing enable` hint. If your vault provider ignores `SSH_AUTH_SOCK`
when loading (so the scoped agent stays empty), `koryph signing enable` reports
it immediately — use the `ssh-add` fallback (`agent_load: []`) in that case.

---

## Configure your vault once

Instead of passing `--provider` and `--vault-name` flags to every
`koryph signing setup` run, configure defaults in one place and skip the
flags entirely on subsequent calls.

### Project-level default (recommended for teams)

Add a `vault` block to `koryph.project.json`:

```json
{
  "vault": {
    "provider":  "protonpass",
    "container": "Engineering"
  }
}
```

`container` is the provider-native grouping — a Proton Pass vault name,
1Password vault, file directory, KeePassXC database/group path, or
HashiCorp/OpenBao KV mount. It acts as the default `--vault-name` for
public-key resolution and the default storage location for new keys.

With this block in place you can run:

```sh
koryph signing setup \
  --project koryph \
  --key-ref "pass://SHARE/ITEM" \
  --identity you@example.com
```

and `--provider protonpass` + `--vault-name Engineering` are filled in
automatically.

### Machine-level default (per operator)

Add a `vault` block to `~/.koryph/config.json` to set a default for all
projects that have no project-level vault block:

```json
{
  "vault": {
    "provider":  "onepassword",
    "container": "Personal"
  }
}
```

### Resolution order

Every command that stores or fetches a secret walks this ladder (first
non-empty wins):

1. **Explicit flag** (`--provider` / `--vault-provider`)
2. **`vault` block** in `koryph.project.json`
3. **`signing` block** in `koryph.project.json` — `provider` + `vault_name`
   (legacy proxy; keeps existing projects working without migration)
4. **`vault` block** in `~/.koryph/config.json`
5. OS-appropriate default (`keychain` on macOS, `encrypted-file` elsewhere)

---

## The signing block (`koryph.project.json`)

```json
{
  "signing": {
    "required":   true,
    "mode":       "ssh",
    "provider":   "protonpass",
    "key_ref":    "pass://share-id/item-id",
    "vault_name": "Engineering",
    "item_title": "SSH Signing Key",
    "identity":   "you@example.com",
    "public_key": "ssh-ed25519 AAAA...",
    "artifacts":  false
  }
}
```

| Field        | Required | Description |
|-------------|----------|-------------|
| `required`  | yes      | Fail closed: repo config applied at run setup, agent must hold the key, merge verifies all commits. |
| `mode`      | no       | `ssh` (default) or `gitsign` (sigstore keyless). |
| `provider`  | ssh only | Vault backend: `protonpass` · `onepassword` · `encrypted-file` · `keychain` (macOS) · `file` · `command` · `aws_secretsmanager` · `azure_keyvault` · `gcp_secretmanager` · `keepassxc` · `openbao` · `vault`. |
| `key_ref`   | varies   | Provider-specific reference: a `pass://` URI, `op://vault/item/field`, filesystem path, or the `{ref}` value consumed by a `command` template. Also used for public-key resolution when neither `--public-key` nor `--vault-name/--item-title` is given. |
| `vault_name`| no       | Provenance: vault name used to resolve `public_key` via `--vault-name`. |
| `item_title`| no       | Provenance: item title used to resolve `public_key` via `--item-title`. |
| `identity`  | yes      | Signer email; becomes the principal in `.allowed_signers`. |
| `public_key`| ssh only | SSH public key literal (`ssh-ed25519 AAAA…`). Captured deterministically at setup; written to `user.signingkey` and `.allowed_signers`. |
| `artifacts` | no       | Enable `koryph sign blob` (cosign) for release artifacts. |

---

## Vault providers

Templates live in `~/.koryph/vault.json` (built-in defaults are used when
the file is absent). Edit templates there to absorb CLI flag changes without a
code update.

Each provider exposes up to four template slots:

| Slot           | Purpose |
|---------------|---------|
| `fetch`        | Print a secret (private key material) to stdout. Used for cosign artifact signing and file-provider agent loading. |
| `agent_load`   | Load SSH keys into the system agent (e.g. `pass-cli ssh-agent load`). |
| `view`         | Print the vault item JSON to stdout (URI selector). Used by `koryph signing setup` to resolve the public key via `--key-ref`. |
| `view_by_title`| Like `view` but selects by `--vault-name` / `--item-title`. |

### Provider matrix

Quick-reference for all ten providers. Full template defaults and override
examples follow in the per-provider sections below.

| Provider | Binary | Auth model | Headless caveats | Template slots |
|----------|--------|------------|-----------------|----------------|
| `protonpass` | `pass-cli` | Proton Pass account (`pass-cli login`) | Requires an active session. Alternative: run Proton Pass as the agent (`pass-cli ssh-agent start --socket-path …`; set `agent_load: []` in `vault.json`). | `fetch`, `agent_load`, `view`, `view_by_title` |
| `onepassword` | `op` | 1Password CLI (`op signin`) | Use `OP_SERVICE_ACCOUNT_TOKEN` or device trust for CI. `op read` returns raw field values — supply `--public-key` explicitly at setup. | `fetch` |
| `encrypted-file` | — (Go, all platforms) | Passphrase (`KORYPH_PASSPHRASE` env or `/dev/tty`) | Set `KORYPH_PASSPHRASE` for non-interactive CI; see trade-offs above. | — (built-in) |
| `keychain` | `security` (darwin only) | macOS Keychain (user session) | Interactive Keychain unlock required; prefer `file`+`--apple-use-keychain` for fully headless macOS CI. | — (built-in) |
| `file` | — | OS file permissions | Fully headless; reads `key_ref` path directly — no CLI or login required. Plaintext keys get a WARN posture. | — (built-in) |
| `command` | User-supplied | User-supplied | Headless support depends entirely on the template command. | `fetch` (user-supplied) |
| `aws_secretsmanager` | `aws` | AWS credential chain (env vars, `~/.aws/credentials`, IAM role, EC2 instance profile) | IAM roles and instance profiles work headlessly in CI — no `aws configure` needed. | `fetch` |
| `azure_keyvault` | `az` | Azure CLI (`az login`) or managed identity / workload identity | Use managed identity or `az login --service-principal` for CI; no interactive login required. | `fetch` |
| `gcp_secretmanager` | `gcloud` | gcloud auth or Application Default Credentials (ADC) | Use a service account key file or workload identity for CI (`GOOGLE_APPLICATION_CREDENTIALS`). | `fetch` |
| `keepassxc` | `keepassxc-cli` | Master password (interactive) or key file | Configure the database for key-file-only auth (no master password) and supply `--key-file` in the template to remove the interactive prompt. | `fetch` |
| `openbao` | `bao` | `VAULT_TOKEN` + `VAULT_ADDR` env vars (or `bao login`) | Fully headless when env vars are set. | `fetch` |
| `vault` | `vault` | `VAULT_TOKEN` + `VAULT_ADDR` env vars (or `vault login`) | Fully headless when env vars are set. | `fetch` |

---

### Proton Pass (`protonpass`)

**Default templates** (from `DefaultVault()`):

```json
{
  "providers": {
    "protonpass": {
      "fetch":         ["pass-cli", "item", "view", "{ref}"],
      "agent_load":    ["pass-cli", "ssh-agent", "load"],
      "view":          ["pass-cli", "item", "view", "{ref}", "--output", "json"],
      "view_by_title": ["pass-cli", "item", "view",
                        "--vault-name", "{vault}",
                        "--item-title", "{title}",
                        "--output", "json"],
      "login_hint":    "pass-cli login"
    }
  }
}
```

The `agent_load` template loads all SSH keys from Proton Pass into the system
SSH agent at once. The repo-level `user.signingkey` then selects which key
signs commits in that specific repo — each project pins its own `public_key`
independently (see [Per-project keys](#per-project-keys)).

Alternatively, run Proton Pass as the agent (`pass-cli ssh-agent start
--socket-path ...`) and set `SSH_AUTH_SOCK` to its socket; set `agent_load`
to `[]` in `vault.json` to skip the load step.

### 1Password (`onepassword`)

Uses `op read op://vault/item/field`. No native `agent_load`; koryph
fetches the key and pipes it to `ssh-add -t 3600 -` (memory only, max 1 h).
Provide the public key explicitly via `--public-key` (1Password's `op read`
returns raw field values, not structured JSON).

---

## No-vault path — signing without a password manager

You do not need Proton Pass, 1Password, or a cloud vault to use commit
signing. Koryph provides two built-in providers that store key material
locally with security equivalent to standard `~/.ssh` practice.

### Posture ladder

| Provider | Posture | Doctor / status |
|----------|---------|-----------------|
| Any vault-backed provider | **OK** | No note |
| `keychain` (macOS) | **OK** | No note — macOS Keychain is the guard |
| `encrypted-file` / passphrase-protected OpenSSH key | **OK** with info note | "same posture as a passphrase-protected ~/.ssh key" |
| `file` (plaintext, no passphrase) | **WARN** | "key is stored unencrypted on disk" + migration hint |

### Quick start (no vault)

```bash
# Generate a key — prompts for passphrase twice (non-empty required).
# Defaults to keychain on macOS, encrypted-file on Linux.
koryph signing keygen --project myproject --identity you@example.com

# Wire the generated key into the project policy.
koryph signing setup --project myproject \
  --provider <provider> --key-ref <path shown by keygen> \
  --identity you@example.com \
  --public-key @<path>.pub

# Load into agent + configure git.
koryph signing enable --project myproject
```

### macOS Keychain (`keychain`, darwin-only)

`key_ref` is an account name stored in macOS Keychain under service `koryph`.

- **Fetch**: `security find-generic-password -s koryph -a {ref} -w`
- **Store**: `security -i` (stdin) — password is never in argv/ps

**Keychain vs `--apple-use-keychain`:** the `keychain` provider stores the
_private key material_ in the Keychain. The `--apple-use-keychain` flag on
`ssh-add` caches a _passphrase_ in the Keychain (for passphrase-protected
files). These are complementary — `koryph signing keygen` uses both on macOS
when the `file` or `encrypted-file` provider is chosen:

```
ssh-add --apple-use-keychain /path/to/key  # caches passphrase → no prompt on reboot
```

Add to `~/.ssh/config` to make this persist automatically:

```
Host *
    UseKeychain yes
    AddKeysToAgent yes
```

### Encrypted-file (`encrypted-file`, all platforms)

`key_ref` is a path to a passphrase-encrypted blob written by
[filippo.io/age](https://age-encryption.org/) using a scrypt recipient.
The private key is never stored in plaintext — the age layer is the
single encryption at rest.

**Passphrase lookup order:**

1. `KORYPH_PASSPHRASE` environment variable — for CI/automated use.
   Trade-off: the variable is visible to child processes; prefer a vault
   provider for fully automated production deployments.
2. `/dev/tty` prompt with echo disabled — for interactive use.

**Store:** atomic write to `{key_ref}` with mode `0600`. Temp file +
rename so partial writes are never visible.

### File (`file`)

`key_ref` is a filesystem path read directly — no template invoked.

### AWS Secrets Manager (`aws_secretsmanager`)

Uses the AWS CLI (`aws`). Auth is ambient — the standard AWS credential chain
is used (environment variables, `~/.aws/credentials`, EC2 instance profile,
etc.). No `agent_load` or `view` template; koryph fetches the secret value
and holds it in memory only.

**Default template:**

```json
{
  "providers": {
    "aws_secretsmanager": {
      "fetch": ["aws", "secretsmanager", "get-secret-value",
                "--secret-id", "{ref}",
                "--query", "SecretString",
                "--output", "text"],
      "login_hint": "aws configure"
    }
  }
}
```

`{ref}` is the secret ARN or name (e.g.
`arn:aws:secretsmanager:us-east-1:123456789012:secret:my-secret` or just
`my-secret` when the region and account are configured).

**Minimum IAM permission** on the target secret resource:

| Action | Purpose |
|--------|---------|
| `secretsmanager:GetSecretValue` | Retrieve the secret string |

Example least-privilege policy:

```json
{
  "Effect": "Allow",
  "Action": "secretsmanager:GetSecretValue",
  "Resource": "arn:aws:secretsmanager:REGION:ACCOUNT:secret:my-secret-*"
}
```

If the secret is encrypted with a customer-managed KMS key, also grant
`kms:Decrypt` on that key.

---

### Azure Key Vault (`azure_keyvault`)

Uses the Azure CLI (`az`). Auth is ambient — run `az login` (or rely on
managed identity / workload identity in CI) before use.

**Default template:**

```json
{
  "providers": {
    "azure_keyvault": {
      "fetch": ["az", "keyvault", "secret", "show",
                "--id", "{ref}",
                "--query", "value",
                "-o", "tsv"],
      "login_hint": "az login"
    }
  }
}
```

`{ref}` is the secret ID URI:
`https://VAULT-NAME.vault.azure.net/secrets/SECRET-NAME` (the version segment
is optional; omitting it returns the current version).

**Minimum permission** — assign the built-in RBAC role or access policy:

| Model | Role / Permission |
|-------|------------------|
| Azure RBAC (recommended) | `Key Vault Secrets User` |
| Legacy access policy | `Get` on Secrets |

The `Key Vault Secrets User` role grants only `Microsoft.KeyVault/vaults/secrets/getSecret/action`
and `Microsoft.KeyVault/vaults/secrets/readMetadata/action` — no list, set, or
delete permissions.

---

### GCP Secret Manager (`gcp_secretmanager`)

Uses the Google Cloud CLI (`gcloud`). Auth is ambient — run
`gcloud auth login` (or `gcloud auth application-default login` for ADC) and
set a default project with `gcloud config set project PROJECT`.

**Default template:**

```json
{
  "providers": {
    "gcp_secretmanager": {
      "fetch": ["gcloud", "secrets", "versions", "access", "latest",
                "--secret", "{ref}"],
      "login_hint": "gcloud auth login"
    }
  }
}
```

`{ref}` is the secret name. Accepted forms:

| Form | Example |
|------|---------|
| Short name (default project configured) | `my-secret` |
| Fully-qualified resource name | `projects/my-project/secrets/my-secret` |

**Minimum IAM role** on the target secret resource:

| Role | Purpose |
|------|---------|
| `roles/secretmanager.secretAccessor` | Access secret version payloads |

Grant at the secret level for least privilege:

```bash
gcloud secrets add-iam-policy-binding my-secret \
  --member="serviceAccount:SA@PROJECT.iam.gserviceaccount.com" \
  --role="roles/secretmanager.secretAccessor"
```

---

### KeePassXC (`keepassxc`)

Uses `keepassxc-cli`. No ambient auth — `{ref}` is the entry path within the
database (e.g. `Engineering/GitHub Token`).

**Default template:**

```json
{
  "providers": {
    "keepassxc": {
      "fetch": ["keepassxc-cli", "show",
                "--key-file", "/path/to/database.keyx",
                "--attributes", "Password",
                "/path/to/database.kdbx",
                "{ref}"],
      "login_hint": "keepassxc-cli --key-file /path/to/database.keyx /path/to/database.kdbx"
    }
  }
}
```

**Prerequisites:**

- KeePassXC installed with the CLI component (`keepassxc-cli` on `$PATH`).
- A KeePass database (`.kdbx`) accessible at the configured path.

**Headless constraint:** `keepassxc-cli` prompts for the master password
interactively when the database requires one. For fully headless operation:

1. Configure the database for **key-file-only** authentication (no master
   password) — KeePassXC → Database → Database Security → Remove master
   password, keep the key file.
2. Supply `--key-file /path/to/database.keyx` in the template.

The `/path/to/` values in the default template are placeholders. Override
them in `~/.koryph/vault.json`:

```json
{
  "providers": {
    "keepassxc": {
      "fetch": ["keepassxc-cli", "show",
                "--key-file", "~/.keepass/database.keyx",
                "--attributes", "Password",
                "~/.keepass/passwords.kdbx",
                "{ref}"]
    }
  }
}
```

**SSH private keys stored as KeePassXC file attachments:** override `fetch`
to use `attachment-export`:

```json
{
  "providers": {
    "keepassxc": {
      "fetch": ["keepassxc-cli", "attachment-export",
                "--key-file", "~/.keepass/database.keyx",
                "~/.keepass/passwords.kdbx",
                "{ref}",
                "private_key",
                "-"]
    }
  }
}
```

`{ref}` is the entry path; `private_key` is the attachment filename; `-`
writes the attachment to stdout.

---

### OpenBao (`openbao`)

Uses the OpenBao CLI (`bao`). Auth is ambient — `VAULT_TOKEN` and `VAULT_ADDR`
env vars must be set, or run `bao login` before use.

**Default template:**

```json
{
  "providers": {
    "openbao": {
      "fetch": ["bao", "kv", "get", "-field=value", "{ref}"],
      "login_hint": "bao login"
    }
  }
}
```

`{ref}` is the KV secret path (e.g. `secret/myapp` or
`kv/myapp/credentials`). The `value` field is retrieved by default;
override `fetch` in `vault.json` to use a different field name.

**Prerequisites:**

- OpenBao installed (`bao` on `$PATH`).
- `VAULT_ADDR` pointing to your OpenBao server.
- A valid token in `VAULT_TOKEN` or obtained via `bao login`.
- The KV secrets engine mounted at the path prefix in `{ref}`.

**Minimum policy** on the target secret:

```hcl
path "secret/data/myapp" {
  capabilities = ["read"]
}
```

Override the field name for a non-default secret layout:

```json
{ "providers": { "openbao": { "fetch": ["bao", "kv", "get", "-field=password", "{ref}"] } } }
```

---

### HashiCorp Vault (`vault`)

Uses the HashiCorp Vault CLI (`vault`). Auth is ambient — `VAULT_TOKEN` and
`VAULT_ADDR` env vars must be set, or run `vault login` before use.

**Default template:**

```json
{
  "providers": {
    "vault": {
      "fetch": ["vault", "kv", "get", "-field=value", "{ref}"],
      "login_hint": "vault login"
    }
  }
}
```

`{ref}` is the KV secret path (e.g. `secret/myapp` or
`kv/myapp/credentials`). The `value` field is retrieved by default;
override `fetch` in `vault.json` to use a different field name.

**Prerequisites:**

- HashiCorp Vault CLI installed (`vault` on `$PATH`).
- `VAULT_ADDR` pointing to your Vault server.
- A valid token in `VAULT_TOKEN` or obtained via `vault login`.
- The KV secrets engine (v1 or v2) mounted at the path prefix in `{ref}`.

**Minimum policy** on the target secret:

```hcl
path "secret/data/myapp" {
  capabilities = ["read"]
}
```

Override the field name for a non-default secret layout:

```json
{ "providers": { "vault": { "fetch": ["vault", "kv", "get", "-field=password", "{ref}"] } } }
```

---

### Command (`command`)

Supply any argv template in `vault.json`; `{ref}` is substituted with
`signing.key_ref`:

```json
{ "providers": { "command": { "fetch": ["my-tool", "get", "{ref}"] } } }
```

---

## Operator runbook — deterministic key association

`koryph signing setup` **requires exactly one** public-key source. The delta
heuristic (diffing `ssh-add -L` before/after agent load) has been removed.

### Form 1: by vault-name and item-title (recommended)

```bash
# 1. Log in to vault
pass-cli login

# 2. Write the signing policy — public key resolved from vault item JSON
koryph signing setup \
  --project my-project \
  --provider protonpass \
  --vault-name "Engineering" \
  --item-title "SSH Signing Key" \
  --identity you@example.com

# 3. Load keys into agent + apply repo git config
koryph signing enable --project my-project

# 4. Commit .allowed_signers so verification works on every clone
git add .allowed_signers && git commit -s -m "chore: add signing identity"

# 5. Check status (shows key source, SHA256 fingerprint, agent readiness)
koryph signing status --project my-project
```

Koryph calls `pass-cli item view --vault-name "Engineering" --item-title
"SSH Signing Key" --output json`, parses the JSON, and scans **all** string
values at every nesting depth for exactly one SSH public key shaped value.
Zero or multiple distinct keys → setup fails with a clear error.

### Form 2: by URI

```bash
koryph signing setup \
  --project my-project \
  --provider protonpass \
  --key-ref "pass://SHARE_ID/ITEM_ID" \
  --identity you@example.com
```

Koryph calls `pass-cli item view "pass://SHARE_ID/ITEM_ID" --output json`
and extracts the public key from the JSON. The `key_ref` is also saved as
the private key reference (used for artifact signing with `--artifacts`).

### Form 3: explicit public key literal or file

```bash
# Inline literal
koryph signing setup \
  --project my-project \
  --provider file \
  --key-ref /path/to/private.pem \
  --public-key "ssh-ed25519 AAAA..." \
  --identity you@example.com

# From file (use @-prefix)
koryph signing setup \
  --project my-project \
  --provider file \
  --key-ref /path/to/private.pem \
  --public-key @~/.ssh/id_ed25519.pub \
  --identity you@example.com
```

---

## `koryph signing status` output

```
project:         my-project
required:        true
mode:            ssh
provider:        protonpass
key source:      vault-name="Engineering" item-title="SSH Signing Key"
identity:        you@example.com
artifacts:       false
pubkey fp:       SHA256:abc...
agent ready:     yes
repo gpg.format: ssh
repo signingkey: key::ssh-ed25519 AAAA...
repo gpgsign:    true
allowed_signers: /path/to/repo/.allowed_signers (present)
repo allowedSignersFile: /path/to/repo/.allowed_signers
```

The **key source** line shows which selector was used at setup time
(`vault-name+item-title`, `key-ref=pass://...`, or `literal`).
The **pubkey fp** is the SHA256 fingerprint of the configured public key
(format: `SHA256:<base64>`, matching `ssh-keygen -lf`).

---

## Per-project keys

Each project registered with `koryph project add` independently pins its own
public key in its `koryph.project.json`:

```json
// Project A
{ "signing": { "public_key": "ssh-ed25519 AAAA...KEY-A", "vault_name": "TeamA", ... } }

// Project B
{ "signing": { "public_key": "ssh-ed25519 AAAA...KEY-B", "vault_name": "TeamB", ... } }
```

`pass-cli ssh-agent load` (or `pass-cli ssh-agent start`) may load many keys
into the system SSH agent at once. The **repo-level `user.signingkey`** set
by `koryph signing enable` selects which key signs commits in each specific
repo — so working across projects with different signing keys Just Works.

No koryph code assumes a single global signing key. Each `koryph signing
setup` / `enable` / `verify` operates strictly on the project it was asked
about.

---

## Merge-time verification semantics

Before rebasing and merging, koryph runs
`koryph signing verify --project ID --branch feature/abc`, which calls
`git log --format='%H %G?' base..branch`. Verification happens **before**
the rebase so a bad-signature commit never touches the target branch.

| `%G?` | Meaning |
|-------|---------|
| `G`   | Good — merge proceeds |
| `N`   | No signature — **blocked** |
| `B`   | Bad signature — **blocked** |
| `U`   | Valid sig, key not in `allowed_signers` — **blocked** |
| `E`   | Cannot verify (missing key / no `allowed_signers`) — **blocked** |

### Interaction with the landing method (`merge_method`)

Landing preserves these signatures by refusing to rewrite them. koryph lands
via a local `git merge --ff-only` + push (never a GitHub-native merge button),
because a merge commit adds an *unsigned* commit and squash/rebase merges
rewrite the SHAs and committer identity — destroying the signatures verified
above. Consequently, when `required` is `true`, any `merge_method` other than
`ff` (e.g. `squash`, whether set in project config or passed as `koryph land
--method` / `koryph merge --squash`) is **refused with a clear error** — only
`ff` keeps the signed commits byte-for-byte. See
[Landing an opened PR](running-waves.md#landing-an-opened-pr-fast-forward-only).

---

## Artifact signing with cosign

Enable `"artifacts": true` in the signing block, then:

```bash
koryph sign blob --project my-project dist/release.tar.gz
# → dist/release.tar.gz.sig
```

Koryph fetches the private key, passes it to `cosign sign-blob` via
`env://KORYPH_COSIGN_KEY` (child env only, never on disk), and writes
`<path>.sig`. Encrypted keys read their passphrase from `COSIGN_PASSWORD`.

Artifact signing requires `key_ref` to be set (the `fetch` template needs a
URI to retrieve private key material). Use Form 2 (`--key-ref`) or Form 3
(`--key-ref` + `--public-key`) when enabling `--artifacts`.

This is keyed signing for artifacts *your* managed project produces. To
verify koryph's own downloaded releases (checksums, keyless cosign bundle,
SLSA provenance, SBOMs), see
[Verifying a release](supply-chain.md).

---

## Gitsign — keyless alternative

Set `"mode": "gitsign"` for Sigstore keyless signing (no vault, no SSH key):

```json
{ "signing": { "required": true, "mode": "gitsign", "identity": "you@example.com" } }
```

Koryph configures `gpg.format x509`, `gpg.x509.program gitsign`, and
`commit.gpgsign true`. The first signature on a machine opens a browser for
OIDC; subsequent signatures are silent.

---

## Applies to any managed project

The signing policy is per-project, not per-agent. Any project registered with
`koryph project add` — regardless of language or toolchain — can enable
vault-backed signing by following the operator runbook above.
