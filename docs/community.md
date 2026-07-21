<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Community & contributing

koryph is Apache-2.0 open source, built in the open at
[github.com/koryph/koryph](https://github.com/koryph/koryph) — by a
factory that [builds itself](developer-guide/self-hosting.md). Here is
everything you need to report, ask, fix, and contribute.

## Filing an issue

**[Open an issue →](https://github.com/koryph/koryph/issues/new/choose)**

Issue forms keep reports actionable and routable — blank issues are
disabled on purpose:

- **Bug report** — needs your `koryph version`, exact reproduction
  commands, expected vs actual behavior, and relevant output. Redact
  identities, emails, and keys from logs before pasting.
- **Feature request** — the problem you can't solve today, the behavior
  you propose, and alternatives you considered.
- **Documentation** — a wrong, stale, or missing page on this site; the
  fastest fix is a link to the page plus what you expected to find.

The one exception: **security vulnerabilities never go in public
issues** — use
[private advisories](https://github.com/koryph/koryph/security/advisories/new)
instead (see [Security](security.md)).

## Contributing code

The full rules live in
[CONTRIBUTING.md](https://github.com/koryph/koryph/blob/main/CONTRIBUTING.md);
the short version:

- **DCO sign-off on every commit** (`git commit -s`) — your certification
  that you may contribute the change under Apache-2.0.
- **Cryptographically signed commits** — SSH-key signing or keyless
  gitsign; unsigned commits are refused by the rulesets.
- **Conventional Commits** — commit messages drive versioning.
- **The green gate** — `make gate` (format, build, vet, test, lint) must
  pass before a PR; user-visible changes update the matching `docs/`
  chapter in the same PR.
- Security-sensitive changes get a second maintainer review.
- **One social rule** — be kind, and argue about the work. The
  [Code of Conduct](https://github.com/koryph/koryph/blob/main/CODE_OF_CONDUCT.md)
  is one page: kindness by default, critique code never people, merit
  decides, and skill buys no exemptions.

New to the codebase? Start with
[Architecture](architecture.md), then the
[package map](developer-guide/packages.md) and
[editor setup](developer-guide/ide-setup.md).

## Where help is most wanted

- **Runtime adapters** — support for Codex, Cursor, Gemini, Grok Builder,
  and friends is [alpha](user-guide/runtimes.md); adapters are the
  highest-leverage contribution there is. Open a feature request naming
  the runtime before you start so work isn't duplicated.
- **Forge providers** — GitHub and GitLab ship today; the
  [forge seam](user-guide/forges.md) is built for more.
- **Vault providers** — the [signing](user-guide/signing.md) vault layer
  welcomes additional backends.
- **Docs** — you're reading the product; every stale sentence is a bug.

## Licensing

- **Apache-2.0**, and only Apache-2.0 — for the code, the docs, and every
  contribution ([LICENSE](https://github.com/koryph/koryph/blob/main/LICENSE)).
  The repo is [REUSE](https://reuse.software/)-compliant: every file
  carries an SPDX header, so license provenance is machine-checkable.
- Submitting a PR constitutes agreement to license the contribution under
  Apache-2.0; no CLA, no copyright assignment.
- koryph orchestrates autonomous agents — read the
  [DISCLAIMER](https://github.com/koryph/koryph/blob/main/DISCLAIMER.md)
  (no warranty, indemnification, autonomous-agent risk) before running it
  against repositories you care about.

## The wider ecosystem

- Work is tracked in [beads](https://github.com/gastownhall/beads) — the
  same open task substrate koryph dispatches from; koryph's own backlog is
  beads in this repo.
- Design documents live in
  [`docs/designs/`](https://github.com/koryph/koryph/tree/main/docs/designs)
  in the repo — the roadmap in the open, deliberately excluded from this
  published book.
- Curious how koryph relates to the rest of the 2026 agent-orchestration
  world? See [How koryph compares](compare.md).
