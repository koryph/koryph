<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Contributing to Koryph

Thank you for your interest in contributing! Please read this document before
opening a pull request.

## Licensing

All contributions are accepted under the
[Apache License, Version 2.0](LICENSE). By submitting a pull request you
agree that your contribution is licensed to the project under Apache-2.0 and
that you retain no additional rights beyond what Apache-2.0 grants.

## Developer Certificate of Origin (DCO 1.1)

Every commit **must** carry a `Signed-off-by` trailer:

```
Signed-off-by: Your Real Name <you@example.com>
```

Add it automatically with `git commit -s`. By signing off you certify that:

> By making a contribution to this project, I certify that the contribution
> is made under the terms of the
> [Developer Certificate of Origin 1.1](https://developercertificate.org)
> — in particular, that you have the right to submit the work and that it
> may be licensed under Apache-2.0.

Commits without a `Signed-off-by` trailer will not be merged.

## Commit Signing (Required)

Every commit must be **cryptographically signed**. Unsigned commits will not
be merged. Two accepted methods:

**SSH-key signing** (recommended for local development):

```bash
git config gpg.format ssh
git config user.signingkey ~/.ssh/id_ed25519.pub
git config commit.gpgsign true
```

**Keyless signing with [gitsign](https://github.com/sigstore/gitsign)**
(sigstore, CI-friendly):

```bash
gitsign init   # configures the repo automatically
```

Verify your commit is signed: `git log --show-signature -1`.

## Commit Message Format

Use [Conventional Commits](https://www.conventionalcommits.org/):

```
type(scope): subject in imperative mood, lowercase, ≤72 chars
```

Accepted types: `feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `ci`,
`build`, `perf`, `style`.

Example: `feat(gate): add retry logic for worktree bootstrap`

## Pull Request Flow

1. Fork the repository and create a feature branch off `main`.
2. Make your changes, committing early and often.
3. Run the green gate (see below) — all checks must pass locally before
   opening a PR.
4. Open a pull request against `main`. Describe what and why; link any
   related issues.
5. Maintainers may request changes or decline the PR. Please respond within
   30 days or the PR may be closed as stale.

## Green Gate (Run Before Every PR)

```bash
# Formatting — output must be empty
test -z "$(gofmt -l .)"

# Build
go build ./...

# Vet
go vet ./...

# Tests
go test ./...
```

A CI check enforces these on every pull request. Fix all failures before
requesting review.

## Code Review

- Be respectful and constructive in review comments.
- Maintainers have final say on design decisions.
- Security-sensitive changes require two maintainer approvals.

## Documentation

Every pull request that **adds or changes user-visible behavior** must update
the relevant chapter(s) of the `docs/` mkdocs book **in the same PR**. This
includes new flags, changed defaults, new commands, renamed concepts, and any
behavioral change a user could observe.

In the PR description, include one of:

- A list of the `docs/` files you updated (e.g. `docs/getting-started.md`,
  `docs/reference/gate.md`), or
- The explicit statement: **"no user-visible surface"** — confirming the
  change is internal only and no docs update is needed.

PRs that omit this statement, or that change user-facing behavior without a
corresponding docs update, will not be merged until both gaps are addressed.
