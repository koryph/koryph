<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Security

koryph orchestrates autonomous agents with shell access against your
repositories, under your accounts. That makes security a design input, not
a checklist item — and it makes honesty about limits part of the design.
This page covers three things: how to report a vulnerability, how the
koryph project keeps its own supply chain trustworthy, and what koryph does
(and deliberately does not claim to do) to protect the machines and
accounts it operates.

## Reporting a vulnerability

**Report privately via GitHub Security Advisories:**
[github.com/koryph/koryph/security/advisories/new](https://github.com/koryph/koryph/security/advisories/new)
— never in a public issue. Response is best-effort (this is an open-source
project without a paid security team; there is no SLA). The current policy
and supported versions live in
[SECURITY.md](https://github.com/koryph/koryph/blob/main/SECURITY.md).

## How the koryph project secures itself

Everything below is enforced by automation you can inspect in the repo —
the same "hygiene as code" posture koryph applies to your projects is
applied to koryph first:

- **Every commit is signed and signed-off.** SSH-signed commits and DCO
  sign-off are required by committed branch rulesets
  (`.github/rulesets/`), local hooks, and CI — for maintainers and bots
  alike.
- **Every release is verifiable.** Releases are draft-until-complete and
  immutable: SHA-256 checksums, a keyless cosign signature (Sigstore
  Fulcio/Rekor), SPDX SBOMs per artifact, and SLSA build provenance all
  attach before anything publishes. [Verify one yourself](user-guide/supply-chain.md)
  — trust the math, not us.
- **CI is least-privilege and pinned.** Workflows run with read-only
  default tokens, job-scoped write permissions, and SHA-pinned actions.
- **Scorecard, scanners, and secret hygiene.** An
  [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/koryph/koryph)
  workflow runs weekly and publishes results; gitleaks and
  private-key detection run in pre-commit; GitHub secret scanning and push
  protection are enabled; Dependabot watches the module graph and CI
  actions.
- **Licensing is auditable.** Apache-2.0 throughout, REUSE-compliant, with
  SPDX headers on every file — enforced by pre-commit and the gate.

## How koryph protects your machine and accounts

koryph's product-side security model rests on a few load-bearing choices:

- **No SaaS, no telemetry, no server.** Everything runs on your machine
  against your git remotes and your AI subscriptions. Nothing phones home;
  observability data stays under `~/.koryph/` unless *you* export it.
- **Fail-closed identity.** Every project pins the account its agents run
  under, and identity is verified before any dispatch — never inherited
  from whatever happens to be logged into the shell. Ambiguity is an error,
  not a guess.
- **Credential-free agent environments.** Dispatched agents get an
  explicitly constructed, allowlisted environment. Your tokens and keys are
  not in it.
- **Vault-served signing keys.** Signing keys resolve on demand from Proton
  Pass, 1Password, macOS Keychain, or an encrypted file — never plaintext
  on disk by default. See [Signing](user-guide/signing.md).
- **Worktree isolation and protected paths.** Agents work in their own git
  worktrees; merges that touch CI workflows, hooks, or policy files are
  refused outright, so an agent cannot rewrite the rules it runs under.
  A human lands those changes deliberately.
- **Guard hooks as defense-in-depth.** Shipped hooks confine agents to
  their worktree, deny orchestrator-only operations (pushing, merging,
  closing beads), and screen for prompt-injection-shaped commands.
- **Hygiene for your repos, as code.** [Posture profiles](user-guide/postures.md)
  and [repo IaC](user-guide/zero-to-shipped.md#stage-4-pin-repository-hygiene-protect)
  let you pin branch protection, required checks, signed commits, and
  secret scanning on the repositories koryph manages — diff-first, with
  snapshots and rollback.

## What we do not claim

Honesty is part of the security posture:

- **Hooks and guards are controls, not a sandbox.** They raise the cost of
  agent misbehavior; they are not a security boundary against a determined
  adversary. If your threat model requires hard isolation, run koryph
  inside a VM or container of your own.
- **You are the operator.** You own the sandboxing decision, the
  credentials you make available, the actions your agents take, and the
  costs they incur. Read the
  [DISCLAIMER](https://github.com/koryph/koryph/blob/main/DISCLAIMER.md)
  before running autonomous agents against anything you care about.
- **Alpha runtimes are refused, not risk-managed.** Dispatch to runtimes
  koryph cannot identity-verify and meter is blocked fail-closed — see
  [AI runtimes: support status](user-guide/runtimes.md).

## See also

- [Verifying a release](user-guide/supply-chain.md) — checksums, cosign,
  SLSA, SBOM, step by step.
- [Signing](user-guide/signing.md) — vault-served commit signing.
- [Posture profiles](user-guide/postures.md) — repo hygiene as code.
- [Community & contributing](community.md) — where security fits in the
  contribution flow.
