<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Long-lived credential auth modes: API-key and OAuth-token accounts (2026-07-21)

Status: PROPOSED — design-doc-first, awaiting operator approval before
decomposition. No beads filed yet (see §12).
Origin: operator direction (2026-07-21) — "koryph needs to support long-lived
API keys for authentication to Claude as well. Lack of this support makes it
appear a user is not logged in. The user will need to supply an API key
somehow." Operator decisions on the three design forks: support **both** auth
modes per account (pay-per-token `ANTHROPIC_API_KEY` and subscription
`CLAUDE_CODE_OAUTH_TOKEN`); **both** key sources (vault-served with a
named-env-var fallback); write the design first, then decompose.

## 1. Problem

koryph verifies *identity* exactly one way: it reads the resolved Claude
profile's `.claude.json` and requires an `oauthAccount.emailAddress`. That
single check is the chokepoint every dispatch, onboard, and validate path
funnels through — `account.Verify` (`internal/account/account.go:207-228`),
wrapped by `account.VerifyExpected` (`account.go:233-245`), reached via
`claude.VerifyIdentity` (`internal/runtime/claude/claude.go:111-117`) from
`internal/dispatch/cli.go:71`, `internal/engine/run.go:268`, and
`internal/onboard/validate.go:86`.

```go
// internal/account/account.go:197-222 (condensed)
type claudeConfig struct {
    OAuthAccount struct {
        EmailAddress     string `json:"emailAddress"`
        OrganizationName string `json:"organizationName"`
    } `json:"oauthAccount"`
}
// ...
if cfg.OAuthAccount.EmailAddress == "" {
    return Identity{}, fmt.Errorf("... has no oauthAccount.emailAddress (not logged in?) — refusing dispatch")
}
```

A user authenticated by a long-lived credential — `ANTHROPIC_API_KEY`, or a
`CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token` — never ran
`claude auth login`, so `.claude.json` has no `oauthAccount` block (on macOS
the OAuth credential lives in Keychain, not a JSON file, and a key/token in
the environment writes nothing at all). The check fails closed with the
literal message "(not logged in?)". This is the reported symptom: a validly
authenticated machine "appears not logged in" and every wave refuses to
dispatch.

The presence of a credential in the environment is **never consulted for
identity**. `ANTHROPIC_API_KEY` participates today only in *billing*, and even
there only as a break-glass fallback (see §3).

### What already exists (the pre-wiring)

This is not a greenfield feature. Three of the four layers are built:

- **Billing mode enum + per-slot stamping + child-env injection.**
  `account.BillingMode` (`internal/account/types.go:33-39`:
  `BillingSubscription`/`BillingAPIKey`); the resolved key is injected as
  `ANTHROPIC_API_KEY` into the child *only* under api-key billing
  (`account.go:143-145`); the dispatch `Spec` carries `Billing` + `APIKey`
  (never logged) (`internal/dispatch/types.go:67-68`).
- **A generic vault layer that already serves API tokens.**
  `signing.FetchSecret(ctx, provider, ref) ([]byte, error)`
  (`internal/signing/vault.go:268`) returns arbitrary secret material and is
  used in production for a GitLab API token (`internal/bot/gitlab.go:146`)
  and a GitHub App key (`internal/bot/resolve.go:31`). Providers:
  protonpass, onepassword, file, encrypted-file, keychain, command,
  aws_secretsmanager, azure_keyvault, gcp_secretmanager, keepassxc, openbao,
  vault (`internal/signing/config.go:31-53`).
- **The interface already anticipates this mode.** The `Runtime` contract doc
  reads: *"a pure API-key runtime (future): 'verified' reduces to 'the
  configured key env var is non-empty'"* (`internal/runtime/runtime.go:85-88`).

### The three gaps (what this design closes)

1. **Identity is OAuth-only.** The api-key path today is a *billing fallback
   at governor-stop*, riding on top of a still-OAuth-verified account
   (`internal/engine/wave.go:600-609`) — not an auth mode. There is no
   account whose *identity* is established by a key.
2. **Key source & the anti-footgun.** The key is resolved from a
   **named operator env var** (`os.Getenv(r.rec.APIKeyEnvVar)`,
   `wave.go:603`), and the design deliberately **forbids** naming that var
   `ANTHROPIC_API_KEY` (`docs/user-guide/billing-and-quota.md:230-232`); the
   ambient `ANTHROPIC_API_KEY` is stripped from every child env because it is
   not on the allowlist (`internal/account/account.go:29-42`). The vault is
   generic but the api-key path does not call it. This anti-footgun must
   survive: a credential is injected only from a vault reference or a
   purpose-named var, never inherited.
3. **Quota is subscription-shaped.** The governor is built entirely on two
   subscription plan windows (5-hour + weekly) with USD ceilings calibrated
   from the Claude app's `/usage` percentages (`internal/quota/types.go`,
   `internal/quota/usage.go`; `Window.Fraction()` returns `1.0` when
   `CeilingUSD <= 0`, `types.go:137-143`). A pay-per-token account has no such
   window, so it reads as *permanently at stop* — backwards. Per-slot cost
   tracking already exists (`ParseResultCost`/`total_cost_usd`;
   `runner.runCostUSD`/`projectedRunCostUSD`, `wave.go:615-645`; `--budget`
   and `per_agent_max_usd` caps), but the governor has no rolling-$ mode.

## 2. Invariants (the correctness contract)

Everything below preserves the existing posture. New clauses are marked NEW.

- **I1 — identity is verified fail-closed before any dispatch.** Unchanged as
  a *requirement*; what "verified" *means* becomes auth-mode-specific (§5).
  No mode may dispatch on an unverified account.
- **I2 — no ambient credential inheritance.** NEW-restated. The child env is
  still built from the allowlist (`account.go:29-42`); the ambient
  `ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN` are never forwarded. A
  credential reaches the child **only** as an explicit, resolved injection
  from a vault reference or a purpose-named env var — never the canonical
  ambient name.
- **I3 — subscription-first stays the default.** NEW. `auth_mode` defaults to
  `subscription`. Nothing about an existing subscription project changes; the
  new modes are opt-in per account and never inferred silently into a billing
  change (see §8 on adopt).
- **I4 — one injected credential, canonical name, correct precedence.** NEW.
  api-key mode injects `ANTHROPIC_API_KEY`; oauth-token mode injects
  `CLAUDE_CODE_OAUTH_TOKEN` and must *not* also set `ANTHROPIC_API_KEY` (which
  outranks the token in the CLI's precedence and would silently switch to API
  billing). koryph dispatches `claude -p` (headless), where `ANTHROPIC_API_KEY`
  is always honored without a prompt — so injection is deterministic.
- **I5 — pay-per-token is never entered by accident.** The `api-key` billing
  shape requires an explicit `auth_mode: api-key` (or the existing
  `--allow-api-spend` fallback path, unchanged). An `oauth-token` account
  bills the subscription exactly like `subscription`.

## 3. Current billing/quota state (for grounding)

The only path that turns on api-key billing today is a triple-AND at governor
stop (`internal/engine/wave.go:600-609`): `--allow-api-spend` (run flag) AND
`api_fallback == "explicit"` (registry) AND a non-empty `api_key_env_var`.
This is a *break-glass on top of a verified subscription account* and is
**retained unchanged** — it remains the way a subscription account spills to
API at a hard stop. This design adds a *parallel* first-class mode; it does
not remove the fallback.

## 4. The account model (registry `Record`)

Auth/billing lives in the machine-local registry record
(`~/.koryph/registry.d/<id>.json`, `internal/registry/types.go:41-141`), never
in the checked-in `koryph.project.json` — matching the precedent that
`koryph.project.json` carries no account fields (`internal/project/project.go`
has only the vault/signing blocks). New fields on `Record` (and the
per-runtime `RuntimeAccount`, `types.go:296-312`):

```jsonc
{
  "auth_mode": "subscription",          // NEW: "subscription" | "api-key" | "oauth-token"; default "subscription"
  "credential": {                        // NEW: only for api-key / oauth-token modes
    "source": "vault",                   // "vault" | "env"
    "provider": "protonpass",            // when source=vault: any signing.VaultProviders value
    "key_ref": "Anthropic API Key",      // when source=vault: item reference passed to FetchSecret
    "env_var": "KORYPH_ANTHROPIC_KEY"    // when source=env: purpose-named var; MUST NOT be ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN
  },
  "identity_fingerprint": "sha256:ab34…", // NEW: see §5; replaces the email identity for non-subscription modes
  "expected_identity": "you@example.com", // unchanged; required only for auth_mode=subscription
  // existing billing fields unchanged: api_fallback, api_key_env_var, billing_guard, quota_profile, batch_policy
}
```

- `subscription` mode ignores `credential`/`identity_fingerprint` and behaves
  exactly as today (`expected_identity` = the OAuth email).
- `api-key` and `oauth-token` modes use `credential` (both sources supported,
  per the operator decision) and `identity_fingerprint`; `expected_identity`
  is optional and no longer forced to contain `@` (relax
  `onboard.validateRegisterOpts`, `internal/onboard/register.go:66-68`).

## 5. Identity verification (the chokepoint, branched by mode)

`account.Verify` gains an auth-mode switch. The `subscription` branch is the
current code verbatim (regression-guarded). The non-subscription branch makes
"verified" mean **the credential resolves and is live, and it is the same
credential this account was enrolled with**:

1. **Resolve** the credential (§6) — a vault fetch or a named-env-var read.
   Empty/unresolvable → refuse, message names the exact fix (fill the vault
   item / export the named var).
2. **Fingerprint check (fail-closed identity).** Compute
   `sha256(credential)` and compare a non-secret prefix against the record's
   `identity_fingerprint`. A mismatch means the key/token was swapped —
   refuse dispatch. This preserves the "identity verified fail-closed"
   invariant without an email and without storing the secret: enrollment
   records the fingerprint, dispatch re-derives and compares. (A truncated
   SHA-256 prefix is safe to persist and to show in `koryph doctor`.)
3. **Liveness probe.** Validate the credential against Anthropic with the
   cheapest call — `GET /v1/models` (free; a 200 confirms auth, a 401 an
   invalid/expired credential). api-key → `x-api-key: <key>`; oauth-token →
   `Authorization: Bearer <token>` plus `anthropic-beta: oauth-2025-04-20`
   (OAuth tokens are Bearer, not `x-api-key`). This reuses the existing
   `internal/anthro` HTTP client seam. The probe is cached briefly (per
   validate/run), not run per slot.

There is no derivable human identity for a bare key (workspace-scoped
`wrkspc_…`, resolvable only with an Admin key), so the **fingerprint is the
identity** for these modes, optionally labeled by a user-supplied
`expected_identity` string (free-form, no `@` requirement) for display.

`claude.VerifyIdentity`/`AuthCheck` (`internal/runtime/claude/claude.go:99-117`)
delegate to the branched `account.Verify` unchanged in shape.

## 6. Credential resolution (both sources, one seam)

A single new function is the extension seam:

```go
// internal/account: resolves the credential for a non-subscription account.
// Returns (canonicalEnvVarName, value, error). Never logs value.
func ResolveCredential(ctx, rec) (envVar string, value string, err error)
```

- `source: "vault"` → `signing.FetchSecret(ctx, provider, key_ref)`
  (`internal/signing/vault.go:268`) — the same call `internal/bot` already
  uses. Memory-only; the obs span logs `provider` + `key_ref`, never the value
  (`vault.go:290-297`).
- `source: "env"` → read the purpose-named var; reject `ANTHROPIC_API_KEY` and
  `CLAUDE_CODE_OAUTH_TOKEN` as names (mirrors the batch client's refusal,
  `internal/anthro/client.go:104-105`).
- The canonical env var returned is `ANTHROPIC_API_KEY` for api-key mode,
  `CLAUDE_CODE_OAUTH_TOKEN` for oauth-token mode. `ChildEnvSpec` gains the
  resolved value + canonical name; `ChildEnv` (`account.go:134-157`) injects
  exactly that one variable and nothing else changes about the allowlist.

## 7. Billing & quota by mode

| auth_mode | Billing | Quota accounting |
|---|---|---|
| `subscription` | subscription (unchanged) | 5h/weekly calibrated-% windows (unchanged) |
| `oauth-token` | **subscription** | 5h/weekly windows apply (usage still measured by the transcript scan, which reads `~/.claude/projects/*.jsonl` regardless of auth method) |
| `api-key` | **pay-per-token** | **rolling-$ budget** (NEW) |

For `api-key`, the governor cannot use `/usage` percentages (there is no plan
window). The rolling-$ mode drives the ladder off *absolute spend*:

- The per-slot settled cost already exists (`total_cost_usd` via
  `ParseResultCost`; `runner.runCostUSD`/`projectedRunCostUSD`,
  `wave.go:615-645`) and the per-run `--budget` and per-agent
  `per_agent_max_usd` caps already bound it.
- Add a per-account rolling-$ ceiling (e.g. `daily_usd` / `window_usd`) so the
  governor's warn→throttle→stop ladder reads `spent$ / ceiling$` instead of
  the calibrated-% `Window.Fraction()`. A pure-API account with no ceiling
  configured is *advisory* (not fail-closed-at-stop), reusing the existing
  "no usage source → governor advisory" escape hatch (`wave.go:591-593`).
- MVP scope: per-run `--budget` + `per_agent_max_usd` are the enforced caps;
  the persistent rolling-$ window is a fast follow (§Open questions).

## 8. Adopt / onboard / discovery

- **Discovery** (`internal/account/discover.go`) recognizes only OAuth
  `.claude.json` today, so an API-key machine yields one *unverified* personal
  candidate and `adopt.Detect` marks claude un-authed
  (`internal/adopt/detect.go:51-55`). Add: detect a resolvable non-subscription
  credential (a `CLAUDE_CODE_OAUTH_TOKEN` in env, or an operator-declared
  vault/named-var) and surface it as a verified candidate under the chosen
  `auth_mode`.
- **adopt never silently switches billing (I3/I5).** Auto-detecting a bare
  ambient `ANTHROPIC_API_KEY` and flipping to pay-per-token would violate
  subscription-first. adopt *reports* "no OAuth login found; an
  `ANTHROPIC_API_KEY` is present — enable api-key auth with
  `--auth-mode api-key` (this bills per token)" and requires the explicit
  flag. `oauth-token` mode, being subscription-billed, can be offered more
  freely.
- **Registration** (`onboard.validateRegisterOpts`,
  `register.go:66-68`) relaxes the `@`-in-identity requirement for
  non-subscription modes and records `auth_mode`, `credential`, and
  `identity_fingerprint`.
- New CLI surface: `--auth-mode subscription|api-key|oauth-token`,
  `--credential-source vault|env`, `--credential-provider`, `--credential-ref`,
  `--credential-env` on `koryph adopt` / `project add`, mirrored in the
  `/koryph-adopt` runbook.

## 9. Extension seam

Two seams keep this open for future providers (Bedrock/Vertex/Foundry, other
runtimes):

1. **`ResolveCredential(rec) → (envVar, value)`** (§6) — adding a mode is a
   new case returning a different canonical env var (e.g.
   `CLAUDE_CODE_USE_BEDROCK`).
2. **Auth-mode-keyed verification strategy** in `account.Verify` (§5) — a map
   from `auth_mode` to a `verify(rec) (Identity, error)` func. Today:
   `subscription` (email), `api-key` / `oauth-token` (fingerprint + live
   probe).

The Claude adapter (`internal/runtime/claude`) stays the only runtime; the
runtime-neutral `Billing`/credential fields on `DispatchSpec`
(`internal/runtime/spec.go`) already exist to carry this.

## 10. Docs to update (on implementation, not now)

`docs/concepts/accounts.md` (identity narrative gains an api-key/oauth-token
branch), `docs/concepts/governors.md:19-54` and
`docs/user-guide/billing-and-quota.md` (subscription-first framing gains the
explicit-opt-in api-key auth mode + rolling-$ accounting),
`docs/user-guide/projects-and-accounts.md` (new registry fields),
`docs/user-guide/signing.md:29-33` (the "credential-free environment" framing
gains the "one deliberately-injected credential in api-key/oauth-token mode"
caveat), and a new short `docs/user-guide/` chapter "Authentication modes."

## 11. Acceptance criteria

1. A machine with **only** `ANTHROPIC_API_KEY` (no `claude auth login`) is
   adopted with `--auth-mode api-key --credential-source env|vault`, passes
   `koryph validate`, and dispatches a wave that bills per token.
2. A machine with **only** `CLAUDE_CODE_OAUTH_TOKEN` is adopted with
   `--auth-mode oauth-token`, passes validate, and dispatches on the
   subscription with the 5h/weekly windows intact.
3. The ambient `ANTHROPIC_API_KEY` is still stripped from the child env; the
   injected value comes only from the vault/named-var resolution (existing
   `TestCommandEnvSubscriptionOmitsAPIKey` stays green; a new test asserts the
   resolved value *is* injected in api-key mode and the ambient is not the
   source).
4. Swapping the key/token (fingerprint mismatch) fails closed at dispatch with
   a clear message.
5. `auth_mode: subscription` behavior is byte-for-byte unchanged (regression
   guard over `account.Verify` and `ChildEnv`).
6. An api-key account with no rolling-$ ceiling is governed advisory, not
   read as permanently at stop; with a ceiling, the ladder fires on
   `spent$/ceiling$`.
7. `koryph doctor` reports the auth mode, credential source, and fingerprint
   prefix (never the secret) for each account.

## 12. Decomposition (after approval)

Deferred — this is the `/koryph-design` STOP point. On approval, `/koryph-plan`
decomposes into footprinted beads, roughly:

- `area:registry` — `Record`/`RuntimeAccount` fields + migration.
- `area:govern` (or `area:account`) — `ResolveCredential`, the
  auth-mode-keyed `Verify` strategy, fingerprint identity, the `/v1/models`
  liveness probe seam.
- `area:quota` — rolling-$ accounting mode + governor ladder branch.
- `area:dispatch` / `area:engine` — `ChildEnvSpec` canonical-name injection;
  `billingFor` co-existence with the new mode.
- `area:adopt` / `area:onboard` — discovery of non-subscription credentials,
  the explicit `--auth-mode` flags, relaxed identity validation, no silent
  billing switch.
- `area:cli` — flag surface on `adopt`/`project add`.
- `area:docs` (`fp:docs-nav`) — the doc updates in §10.

Cross-cutting: `refactor-core` risk is low (the changes are additive branches,
not wave-loop rewrites), but the `account.Verify` and `ChildEnv` edits are on
the account-safety hot path and should carry the security-review label.

## Open questions

1. **Identity = fingerprint** (proposed) vs a purely user-supplied label.
   Fingerprint gives a real fail-closed swap-detection; confirm that's wanted
   over a looser label.
2. **Rolling-$ persistence.** MVP enforces per-run `--budget` +
   `per_agent_max_usd` only; a persistent per-account rolling window
   (`$/day`) is a fast follow. Ship MVP first, or design the persistent window
   up front?
3. **Batch client unification.** `internal/anthro` already resolves a named
   var and refuses the ambient key; should api-key *accounts* feed the batch
   path from the same `ResolveCredential` (vault-capable) rather than its own
   `--key-env`? (Recommend yes, as a follow-up.)
4. **oauth-token liveness probe.** Confirm `GET /v1/models` with
   `Authorization: Bearer` + `anthropic-beta: oauth-2025-04-20` is the sanctioned
   validity check for a `setup-token` credential (vs a `claude /status` scrape).
