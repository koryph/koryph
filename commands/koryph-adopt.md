---
name: koryph-adopt
description: Adopt a repo into koryph management (or repair this one) via the koryph adopt wizard
---
<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Adopt a repository into koryph management using the `koryph adopt` wizard:

$ARGUMENTS

The target is the path in the arguments; with no arguments, the current
repo. On an already-adopted repo this is a safe repair/no-op pass —
re-running `adopt` re-checks every onboarding step and re-validates.

Do this:

1. **Preflight.** `koryph version` must work. If koryph itself is missing,
   install it first (macOS: `brew install koryph/tap/koryph`;
   otherwise https://koryph.build/user-guide/installation/) — ask before
   running any installer.
2. **Preview, never guess.** Run `koryph adopt <root> --dry-run --json` and
   read `steps[]`. Summarize the plan for the user in one short block:
   what is already `done`, what is `needed` (with each step's `why`), what
   is `offer`ed, and anything `blocked`. Get their go-ahead before writing
   anything.
3. **Resolve the fail-closed values up front.** Non-interactive runs accept
   only unambiguous derivations, so check the dry-run plan and ask the user
   for whichever of these it could not derive:
   - account: `--account <profile> --identity <email>` (needed when zero or
     several verified Claude accounts were found),
   - gate: `--gate "cmd1;;cmd2"` (needed when no build/test commands could
     be inferred) — confirm inferred gate commands with the user even when
     derivation succeeded; a wrong gate green-lights broken merges,
   - forge: `--forge github|gitlab` (needed when the remote matches no
     known host).
   - auth mode: default is `subscription` (OAuth login) and is never
     switched automatically. If the machine has no OAuth login but a
     resolvable long-lived credential, ask the user before opting in —
     `--auth-mode api-key` bills **pay-per-token**, not the subscription;
     `--auth-mode oauth-token` stays subscription-billed. Either needs
     `--credential-source vault|env` plus `--credential-provider`/
     `--credential-ref` (vault) or `--credential-env` (env).
4. **Execute.** Run `koryph adopt <root> --yes --json` plus the flags from
   step 3. Installs that need `sudo` are never run by the wizard — relay
   the exact command to the user to run themselves, then re-run adopt.
5. **Interpret the result.** Exit 0 with `koryph validate` green means
   adopted. Non-zero: report each remaining `blocked` step and its
   remediation verbatim, fix what you can with the user, and re-run —
   adopt is idempotent.
6. **Hand off the deferred offers.** Adoption leaves signing and repo
   posture as opt-ins; tell the user the commands (`koryph signing keygen`
   or `koryph signing setup`, `koryph posture apply oss-solo-maintainer`)
   and offer to run them only if asked.
7. Finish by reporting the project id and the next step:
   `/koryph-plan` a design (or `/koryph-import` existing TODOs), then
   `koryph run --project <id> --once --dry-run`.

Do **not** hand-edit `koryph.project.json`, run `bd init` directly, or
install system packages without the user's explicit consent — the wizard
owns those steps and asks for exactly the consent it needs.
