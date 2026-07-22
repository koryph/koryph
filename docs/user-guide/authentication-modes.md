<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Authentication Modes

Every account koryph dispatches under has an **auth mode**, set per project
in the registry record. The default asks nothing of you — it is the same
OAuth-subscription behavior koryph has always had. The other two modes exist
so a machine authenticated only by a long-lived credential (one that never
ran `claude auth login`) is recognized and dispatchable instead of reporting
"not logged in".

---

## The three modes

| `auth_mode` | Credential | Billing | Identity check |
|---|---|---|---|
| `subscription` (default) | Claude OAuth login (`~/.claude.json` / Keychain) | Claude subscription | `oauthAccount.emailAddress` must match `expected_identity` |
| `oauth-token` | long-lived `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) | **Claude subscription** — billed exactly like `subscription` | credential fingerprint + a live check against Anthropic |
| `api-key` | long-lived `ANTHROPIC_API_KEY` | **pay-per-token** | credential fingerprint + a live check against Anthropic |

`subscription` is always the default: `auth_mode` is empty on every project
registered before this feature existed, and an empty value behaves exactly
as before. Nothing about an existing subscription project changes, and
koryph never infers `api-key` mode from an ambient `ANTHROPIC_API_KEY`
sitting in your shell — entering pay-per-token billing always requires an
explicit `--auth-mode api-key` from you (see [Adopt never silently switches
billing](#adopt-never-silently-switches-billing) below).

`oauth-token` exists for machines that only ran `claude setup-token` and
never `claude auth login` — a common shape for CI runners and headless
boxes. It still bills the flat-rate subscription; only the *identity check*
differs from `subscription` mode, because there is no `.claude.json` to
read on such a machine.

---

## Why identity verification differs

A bare API key or long-lived token is workspace-scoped — there is no email
address to extract and compare, the way `subscription` mode does with
`oauthAccount.emailAddress`. For `api-key` and `oauth-token` accounts, **the
credential's fingerprint is the identity**:

1. **Resolve** the credential (vault fetch or named env var — see below). An
   empty or unresolvable credential refuses dispatch and names the exact
   fix (fill the vault item / export the named var).
2. **Fingerprint check.** koryph computes `sha256(credential)` and compares
   a non-secret, truncated hex prefix (`sha256:<16 hex chars>`) against the
   `identity_fingerprint` recorded when the account was registered. A
   mismatch means the key or token was swapped since enrollment — dispatch
   refuses closed. Only the truncated hash is ever persisted; the
   credential itself never is.
3. **Liveness probe.** koryph makes one free `GET /v1/models` call against
   Anthropic — an `api-key` account authenticates with `x-api-key`, an
   `oauth-token` account with `Authorization: Bearer` — to confirm the
   credential is still valid before any agent is dispatched.

Any failure at any step is fail-closed — the same posture `subscription`
mode has always had. koryph never dispatches on an account it could not
verify.

---

## Registering an account under a non-subscription mode

Both onboarding front doors — `koryph adopt` and `koryph project add` —
accept the same three flag families:

```sh
koryph adopt --auth-mode api-key \
  --credential-source env --credential-env KORYPH_ANTHROPIC_KEY

koryph project add /path/to/repo \
  --account personal --identity "ci runner" \
  --auth-mode oauth-token \
  --credential-source vault --credential-provider protonpass \
  --credential-ref "Claude setup-token"
```

| Flag | Meaning |
|---|---|
| `--auth-mode` | `subscription` (default) \| `api-key` \| `oauth-token` |
| `--credential-source` | `vault` \| `env` |
| `--credential-provider` | vault provider name (with `--credential-source vault`) |
| `--credential-ref` | vault item reference (with `--credential-source vault`) |
| `--credential-env` | purpose-named env var holding the credential (with `--credential-source env`) |

`--account`/`--identity` are still accepted for `api-key`/`oauth-token`
accounts, but for these two modes `--identity` is no longer required to look
like an email — it becomes a free-form display label (`expected_identity`
in the registry), never itself verified. It is the fingerprint, not this
label, that koryph checks at dispatch.

### The anti-footgun: never the canonical env var name

`--credential-env` (and the registry's `credential.env_var`) must **not** be
named `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` — those are the
*canonical* names koryph injects the **resolved** value under in the
dispatched agent's environment. Naming the source var the same as the
injected var would let a dispatched agent's own ambient environment satisfy
its own credential lookup, defeating the point of resolving it explicitly.
koryph refuses this shape both at registration and again at dispatch.

The ambient `ANTHROPIC_API_KEY` in *your own* shell is still always
stripped from every dispatched agent's environment — the same
credential-free allowlist [Signing](signing.md) describes. A credential
reaches the child process only via this explicit vault/named-var
resolution, never by inheritance.

---

## Adopt never silently switches billing

`koryph adopt`'s discovery step recognizes an ambient `CLAUDE_CODE_OAUTH_TOKEN`
or `ANTHROPIC_API_KEY` in your environment and reports it as a candidate —
but it treats the two very differently:

- **`CLAUDE_CODE_OAUTH_TOKEN`** is subscription-billed, so a resolvable
  token is offered as a verified candidate with no extra flag required.
- **`ANTHROPIC_API_KEY`** bills per token, so adopt only *reports* it — "no
  OAuth login found; an `ANTHROPIC_API_KEY` is present — enable api-key auth
  with `--auth-mode api-key` (this bills per token)" — and requires the
  explicit flag before it will register the account. Auto-flipping billing
  on a bare ambient key would break the subscription-first default (see
  [Governors](../concepts/governors.md)).

---

## Billing and quota by mode

| `auth_mode` | Billing | Quota accounting |
|---|---|---|
| `subscription` | subscription | 5h/weekly calibrated-% windows, as in [Billing & quota](billing-and-quota.md) |
| `oauth-token` | subscription | same 5h/weekly windows — usage is read from the local transcript scan, which does not distinguish how you authenticated |
| `api-key` | pay-per-token, from wave 1 | no subscription window applies; governed advisory unless a rolling-$ ceiling is configured (below) |

An `api-key` account has no `/usage` percentage to calibrate against, so it
does not participate in the 5h/weekly governor ladder at all. The enforced
spend caps for an `api-key` account today are the per-run `--budget` flag
and the per-agent `per_agent_max_usd` cap (see [Per-agent budget
caps](billing-and-quota.md#per-agent-budget-caps-and-the-turn-boundary-nuance)).
A persistent per-account rolling-$ ceiling is designed (spend tracked
against a configurable window, feeding the same warn→throttle→stop ladder
subscription accounts use) but not yet wired into the governor's per-wave
gate — until it is, an `api-key` account runs *advisory*: measured and
logged, never blocked, the same posture an uncalibrated subscription account
has.

---

## Where this lives

The three new fields — `auth_mode`, `credential`, `identity_fingerprint` —
are registry-record fields, never checked into `koryph.project.json`, same
as the rest of the account triple. See [Account
model](projects-and-accounts.md#account-model) for the full field reference,
and [People: accounts and personas](../concepts/accounts.md) for how
identity verification fits the bigger picture.
