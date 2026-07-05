<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# CI pipeline setup

`koryph ci setup` renders and installs forge-native CI pipeline assets into
your project. It is the canonical way to give koryph a green gate — the
build/test workflow that must pass before any branch is merged.

---

## What gets installed

`koryph ci setup` installs **pipeline asset files** (CI workflow or config
fragments) by rendering them from koryph's embedded templates and writing
them to the forge-native path. Assets are idempotent: re-running over an
already-current file is a no-op.

### Gate pipeline

The **gate** kind is installed by default (`--kind gate`). It runs the
project's [green gate command](#the-gate-command-contract) on every push and
pull/merge request, keeping the default branch and all open PRs/MRs green.

| Forge | Installed path |
|-------|----------------|
| GitHub | `.github/workflows/koryph-gate.yml` |
| GitLab | `.koryph/ci/koryph-gate.yml` ¹ |

¹ GitLab CI uses an *include* fragment (see [GitLab: adding the include
entry](#gitlab-adding-the-include-entry)).

### Scanner pipeline (optional)

The **scanner** kind (`--kind scanner`) installs a dependency/vulnerability
scanner pipeline. It is optional — run `--kind all` to install both gate
and scanner, or `--kind scanner` to install only the scanner.

| Forge | Installed path |
|-------|----------------|
| GitHub | `.github/workflows/koryph-scanner.yml` |
| GitLab | `.koryph/ci/koryph-scanner.yml` ¹ |

---

## Quickstart

```sh
# Install the gate pipeline only (default)
koryph ci setup --project myproject

# Install both gate and scanner
koryph ci setup --project myproject --kind all

# Override the gate command (e.g. for a non-Makefile build)
koryph ci setup --project myproject --gate-cmd "go test ./..."

# Install only the scanner
koryph ci setup --project myproject --kind scanner
```

After running `ci setup`, koryph prints commit guidance:

```
  installed    .github/workflows/koryph-gate.yml

Remaining HUMAN steps:
  1. Review the installed file(s) above.
  2. git add <paths above> && git commit -s -m 'ci: install koryph CI assets'
  3. Push and open a PR — GitHub will run the gate workflow on every push and PR.
```

koryph never commits CI assets automatically; committing is always the
operator's (or agent's) act — the same principle as `koryph release setup`.

---

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--project ID` | — | Project to install into (required, or pass the ID as a positional argument) |
| `--kind gate\|scanner\|all` | `gate` | CI asset kind(s) to install |
| `--gate-cmd CMD` | `make gate` | Override the gate command; see [The gate command contract](#the-gate-command-contract) |

---

## The gate command contract

The gate command is the single shell command that embodies "the branch is
green." koryph renders it verbatim into the CI pipeline; the pipeline
checks out the repository, then runs:

```sh
<gate-cmd>
```

No language runtime, package manager, or build tool is installed by the
generated pipeline itself — it is **toolchain-neutral by design**. Your
project's Makefile (or whatever owns the gate command) is responsible for
its own prerequisites.

### Default: `make gate`

Unless overridden with `--gate-cmd`, koryph uses `make gate`. The koryph
project itself defines `make gate` in its top-level Makefile; your project
should do the same. A typical `gate` target runs formatting checks, linting,
builds, and the full test suite.

### Overriding for non-Makefile projects

Pass `--gate-cmd` to use a different command:

```sh
# Go project without a Makefile
koryph ci setup --project myproject --gate-cmd "go build ./... && go vet ./... && go test ./..."

# Python project
koryph ci setup --project myproject --gate-cmd "pytest"
```

The override is written into the rendered pipeline header (as a comment) so
you can always see which command the installed file uses. To change it later,
re-run `koryph ci setup --gate-cmd <new-cmd>` and commit the updated file.

---

## Checking for drift

`koryph ci check` compares installed CI assets against the current render
output and exits 1 if any asset is missing or has drifted:

```sh
# Check the gate pipeline
koryph ci check --project myproject

# Check all installed kinds
koryph ci check --project myproject --kind all
```

Example output (all current):

```
  ok           .github/workflows/koryph-gate.yml
ci check: all CI assets are current.
```

Example output (drift detected):

```
  DRIFT        .github/workflows/koryph-gate.yml
koryph: ci check: drift detected — run `koryph ci setup --project myproject` to update
```

Run `koryph ci setup` to resolve drift; `ci check` is suitable for a
pre-dispatch gate or a periodic CI step.

---

## Release pipeline routing

`koryph release setup` also installs CI assets — the caller workflow that
drives the release train. It routes through the same forge seam internally:
`forge.CI().Render("caller")`. The caller workflow is therefore a first-class
CI kind, available (for reference or manual install) as `--kind caller` on
`koryph ci setup`.

Practically: run `koryph release setup` to wire the release pipeline, and
`koryph ci setup` to wire the gate pipeline. They are independent and compose
freely.

---

## Forge-agnostic posture

koryph CI assets are designed to be **forge-agnostic**. The same verb and
flags work on GitHub and GitLab; only the rendered output differs.

| | GitHub | GitLab |
|---|---|---|
| Gate kind | `.github/workflows/koryph-gate.yml` | `.koryph/ci/koryph-gate.yml` |
| Scanner kind | `.github/workflows/koryph-scanner.yml` | `.koryph/ci/koryph-scanner.yml` |
| Trigger (push) | `on: push` | `rules: CI_PIPELINE_SOURCE == "push"` |
| Trigger (PR/MR) | `on: pull_request` | `rules: CI_PIPELINE_SOURCE == "merge_request_event"` |
| Include mechanism | Standalone workflow file | Fragment; needs `include:` in `.gitlab-ci.yml` |
| Commit guidance | PR instructions | MR instructions + include guidance |

The forge is resolved automatically from `koryph.project.json`; you never
need to specify it explicitly.

### GitLab: adding the include entry

On GitLab, koryph writes CI fragments to `.koryph/ci/`. After installing,
add each fragment to your root `.gitlab-ci.yml` with an `include:` entry:

```yaml
include:
  - local: '.koryph/ci/koryph-gate.yml'
  # (if you installed the scanner)
  - local: '.koryph/ci/koryph-scanner.yml'
```

`koryph ci setup` prints the exact `include:` snippet after installation
so you can copy-paste it directly.

---

## See also

- [`koryph ci setup` flags reference](../reference/cli#koryph-ci-setup)
- [`koryph ci check` flags reference](../reference/cli#koryph-ci-check)
- [Release pipeline setup](release.md) — routing through the same forge seam
- [Choosing a forge](forges.md) — GitHub vs GitLab capabilities
