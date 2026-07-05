<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# CI asset installation: koryph installs the pipelines it needs (2026-07-04)

Status: approved; epic + children filed from §4.
Origin: operator direction — "Does koryph install the git service
specific ci actions that it needs? If not, this should be enabled."
Addendum to docs/designs/2026-07-forge-providers.md (§ CIService).

## 1. Today's gap

`forge.CIService.Render(kind)` ships kinds `docs`, `release`, `caller`,
`scanner` for both providers — and has **zero consumers**. The only
pipeline koryph installs today is the release caller workflow, via
`koryph release setup`'s own embedded GitHub template that predates and
bypasses the forge seam. There is no renderable **gate** pipeline (the
build/test workflow that makes the green gate a required check) and no
generic installer verb at all. A koryph project on GitLab gets nothing.

## 2. Shape

- **New kind `gate`** in CIService: the forge-native CI pipeline that
  runs the project's gate command on push/PR (GitHub: a workflow with a
  `make gate`-style job; GitLab: a `.gitlab-ci.yml` stage). The gate
  command is a template input (default `make gate`), NOT hardcoded —
  koryph is language-agnostic; the project's Makefile owns the meaning.
- **New verb `koryph ci setup [--kind gate|scanner|all]`**: resolves the
  project's forge, renders the requested kinds via `forge.CI().Render`,
  writes them to the forge-native path (`.github/workflows/…` /
  `.gitlab-ci.yml` include), and prints commit guidance — same UX as
  `koryph release setup`. Idempotent; `koryph ci check` reports drift
  between installed assets and current Render output.
- **`koryph release setup` routes through the seam**: the embedded
  template moves into the GitHub provider as its `release`/`caller`
  Render implementation; release setup calls `forge.CI()` like everyone
  else. GitLab release setup falls out of the same call.
- **Doctor**: `ci-assets` check — gate pipeline present + current;
  WARN with the exact `koryph ci setup` remediation.
- rdc (docs publishing setup) stays its own epic — the docs workflow
  needs Pages/DNS orchestration beyond asset rendering — but consumes
  the same installer plumbing (`ci setup` internals exported for it).

## 3. Non-goals

- No CI *execution* features (log streaming, re-runs) — install only.
- No auto-commit: assets are written to the working tree; committing is
  the operator's (or agent's) act, same as release setup.
- No forced adoption: `ci setup` is offered, never implied, by
  `koryph project add` output.

## 4. Sequencing

C1 gate kind in both providers (fp:go:forge) → C2 `koryph ci setup` /
`ci check` (area:cli:ops) ∥ C3 release-setup seam routing (area:build +
area:cli:release + fp:go:forge — serialized with C1 by the shared forge
token, ordered C1→C3 by dependency) ∥ C4 doctor ci-assets check
(area:doctor) → C5 docs (area:docs + fp:docs-nav). Width 3 after C1.
