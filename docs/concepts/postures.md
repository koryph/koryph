<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Hygiene: postures and vaults

*This page expands the [Concepts overview](index.md). See
[Posture profiles](../user-guide/postures.md) and
[Signing](../user-guide/signing.md) for the commands that operate it.*

## The idea

Repository security lives in dozens of settings scattered across a forge's UI:
branch-protection rules, required checks, signed-commit enforcement, scanner
toggles, Actions permissions. Click-configured, they drift — nobody remembers
the exact state, no two repos match, and a regression is invisible until it
lets something through. And the secrets those settings depend on — signing keys
above all — end up as plaintext files in `~/.ssh` because that was the path of
least resistance.

koryph makes both things **configuration**. A **posture** is a named, versioned
bundle of rulesets, repo settings, and scanner presets that koryph can *diff*
against a live repo and *apply* to it. Secrets resolve through a **vault** layer
— fetched on demand from a real secret manager, never plaintext by default.

## In koryph

A posture is checked, diffed, and applied by name:

```bash
koryph posture list                       # available profiles
koryph posture check oss-solo-maintainer  # compare live repo; exit 1 on drift
koryph posture diff  oss-solo-maintainer  # same, always exit 0
koryph posture apply oss-solo-maintainer  # diff, then apply the changes
```

The built-in `oss-solo-maintainer` profile is the opinionated default:
branch-protection and signed-commit rulesets, hardened repo settings, and
Actions permissions, parameterized by things like the CI checks a PR must pass.
Because `check` exits non-zero on drift, it drops straight into CI as a
regression guard.

Signing keys come from the vault layer rather than a bare file. koryph supports
Proton Pass, 1Password, macOS Keychain, and a passphrase-encrypted file among
others; enabling signing loads the key into the SSH agent and applies the git
config:

```bash
koryph signing enable --project koryph    # Proton Pass serves the key here
```

A key protected by a passphrase on disk is treated as exactly what it is — the
same posture as a normal `~/.ssh` key — worth an informational note, not an
alarm. The posture layer reports honestly instead of crying wolf.

## The failure mode it prevents

Configuration drift and secret sprawl. Without a versioned posture, branch
protection quietly weakens over time — a rule disabled "just to merge this one
thing" and never restored — and you discover the gap only when an unsigned or
unreviewed commit lands on `main`. Without a vault layer, signing keys
accumulate as plaintext on disk, one laptop backup away from exposure. A posture
you can diff turns "is this repo still locked down?" into a command with a
deterministic answer, and on-demand vault fetches keep the keys out of your
filesystem entirely.

## Operate it

- [Posture profiles](../user-guide/postures.md) — listing, diffing, and applying.
- [Signing](../user-guide/signing.md) — vault providers and enabling signed
  commits (which the [green gate](worktrees.md) requires).
