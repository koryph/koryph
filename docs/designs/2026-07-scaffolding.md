<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Scaffolding without the tar pit: koryph new and the ecosystem seam (2026-07-04)

Status: DESIGN ITERATION — deliberately unscheduled. No beads until this
stabilizes; expect several revisions.
Companion: docs/designs/2026-07-software-factory.md (the three pillars,
§3.1 `koryph new`).

## 1. Positioning (the sentence that constrains everything)

koryph is **the quickstart for vibecoding without lock-in**: lightweight,
free of SaaS dependencies, with *just enough opinionation to keep you secure
while you learn the ropes*, and a paved road to sharing and releasing what
you build.

Two design guarantees fall out of that sentence:

- **The opinionation boundary.** koryph's opinions are exclusively about
  *process hygiene*: signed commits, a green gate, protected branches,
  conventional commits, SBOM/provenance on releases, footprinted planning.
  koryph holds **no opinions about your application**: no framework picks,
  no folder layouts, no dependency choices, no architecture. This boundary
  is the anti-scope-creep fence — every scaffolding decision below is
  derived from it.
- **Ejectability.** Everything koryph produces is standard, inspectable
  artifacts: a plain git repo, GitHub-native settings, ordinary workflows,
  standard release assets. Delete koryph and nothing breaks — you lose the
  factory, not the product. Any feature that would violate ejectability
  (runtime dependencies on koryph, bespoke formats a repo can't live
  without) is out, categorically.

## 2. The tar pit, named precisely

"Extend koryph to scaffold a hello app for a couple frameworks" expands
along three orthogonal axes:

- **language** — go, rust, java, kotlin, js/ts, python, c#, zig, …
- **runtime** — native, cgo, jvm, node/deno/bun, cpython, .NET, wasm, …
- **packaging** — archive, homebrew, container/ghcr, apt/rpm (nfpm),
  RedHat OLM, MSI, npm/PyPI/crates as registries, …

An owned-template approach is an N×M×K matrix that is stale the week it
ships (ecosystem scaffolders churn constantly; CRA→vite is the canonical
grave marker). **koryph must never own application templates.** The matrix
is the trap; the fence from §1 is what keeps us out of it.

## 3. The reframe: koryph doesn't scaffold — it *koryphizes*

Every serious ecosystem already ships a blessed hello-world scaffolder:
`go mod init` + a main.go, `cargo init`, `npm create vite@latest` /
`npm init -y`, `uv init`, `gradle init`, `dotnet new console`. They are
maintained by the people who break them.

What none of them produce is what koryph actually adds: the gate, the
hygiene, the beads, the release contract. So the unit of koryph work is not
"generate an app" — it is **koryphization**: take *any* directory that
builds and runs, and wire the factory around it. That operation is
language-agnostic by construction and mostly exists today (`project add`,
posture apply, `release setup`, assets install).

The **scaffold contract** — what koryphization needs from whatever produced
the code (native scaffolder, agent, or human):

| Requirement | Feeds |
|---|---|
| builds + runs a hello equivalent | the canary `--once` run, the gate |
| gate commands (fmt/build/test equivalents) | `koryph.project.json` `gate` |
| a version-stamp location | release-please `extra_files` |
| an artifacts recipe (commands or tool) | the release build contract (mode A/B) |
| declared source layout roots | starter `area_map` for footprints |

Five facts. Not templates — a contract any scaffold output can satisfy, in
any language, on any runtime.

## 4. Three scaffold sources, one pipeline

`koryph new <name> --lang X [--scaffold …]` resolves a **scaffold source**,
runs it, then applies identical koryphization regardless of source:

**S1 — native scaffolder presets (deterministic, default).** A preset is
*data, not templates*: the ecosystem command to run plus the five contract
facts. The full preset for Go is roughly:

```jsonc
{ "lang": "go",
  "scaffold": ["go mod init {{module}}", "koryph:write-hello-main"],
  "gate": ["gofmt -l .", "go build ./...", "go vet ./...", "go test ./..."],
  "version_stamp": {"strategy": "annotated-const", "file": "internal/version/version.go"},
  "release": {"type": "go", "build": {"goreleaser": true}},
  "layout_roots": ["cmd/", "internal/"] }
```

Ship a **small blessed set** (go, rust, node/ts, python — the learning-
the-ropes audience) and hold the line there. `koryph:write-hello-main` is
the one permitted koryph-owned artifact per preset: a sub-20-line hello
entrypoint, because `go mod init` alone doesn't produce one. That is the
entire template surface area we ever own.

**S2 — pluggable presets (community-extensible, no Go code).** The same
preset schema, loaded from `~/.koryph/scaffolds/*.json` or a git URL —
exactly the vault-provider/argv-template pattern already proven in
`internal/signing`. Java-via-gradle, zig, a company's internal starter:
someone writes a JSON file, not a koryph PR. Presets are validated against
the contract (run scaffold → does it build? do the gate commands exist?)
by `koryph scaffold check <preset>` before first use.

**S3 — the agent scaffold (koryph's unfair advantage, explicit fallback).**
For combinations no preset covers, koryph's own thesis applies: scaffolding
is a bead. `koryph new --lang crystal --scaffold agent` files a scaffold
bead whose acceptance criteria ARE the contract table, runs a single-bead
loop (`--once`), and gate-verifies the result like any other work. Slower
and non-deterministic — which is why it is the *fallback*, never the
default for blessed languages — but it makes the language axis effectively
unbounded without koryph owning anything.

All three converge on the same koryphization tail: `bd init`, project
config from the contract facts, assets install, posture apply, signing,
optional `release setup` — the existing machinery, unchanged.

## 5. The packaging axis maps to the release train, not to scaffolding

Packaging is a *release* concern and stays in the release block, as
composable **packaging fragments** on top of mode A/B:

- archives/checksums/SBOM/provenance — exists (the train)
- container/ghcr + image signing — koryph-2ge (filed)
- homebrew tap — koryph-1hr (filed)
- apt/rpm — nfpm fragment (goreleaser-native for Go; standalone nfpm
  invocation as a mode-A fragment for everyone else)
- language registries (npm/PyPI/crates) — publish fragments, per-ecosystem,
  same draft-until-complete lifecycle
- **RedHat OLM — explicitly deferred.** Operator-lifecycle packaging drags
  in catalogs, bundles, and cluster-side machinery; it is a product in
  itself. Revisit only against a real user.

Scaffold presets may *declare* compatible fragments ("go → brew, nfpm,
container") so `koryph new --release` offers sane choices — declaration,
not implementation.

## 6. Non-goals (the fence, restated as a checklist)

- No framework templates beyond the sub-20-line hello entrypoint.
- No app architecture, folder conventions, or dependency selections.
- No UI/web starter kits; `npm create vite` exists and is better.
- No template versioning/upgrade machinery (nothing to upgrade — presets
  are five facts + one command line).
- No OLM/operator packaging in v1.
- Nothing that survives `rm -rf .koryph .claude .beads` as a breakage.

## 7. Open questions for iteration (deliberately unresolved)

1. **Preset trust**: an S2 preset executes arbitrary commands. Same trust
   model as `gate` commands (you run what you configure), or a
   first-use confirmation showing the exact argv?
2. **Version-stamp strategies**: annotated-const (go) vs manifest-native
   (package.json, Cargo.toml, pyproject.toml — release-please handles
   these natively). Is the strategy enum just release-please's type list?
3. **S3 determinism**: should the agent scaffold commit a `SCAFFOLD.md`
   recording what it chose, so re-runs and audits have an anchor?
4. **`koryph new` vs `koryph init` naming** and whether koryphizing an
   existing non-git directory is the same verb (`koryph new --here`?).
5. **Windows** (runtime axis includes it eventually): presets are argv
   templates — do we require sh-free commands from day one?
6. Where does the blessed-preset line sit in two years — is java in or
   out of tree? Criterion proposal: in-tree only if CI can afford to
   scaffold+gate it on every koryph release.

## 8. Relationship to filed work

`koryph-om7` (koryph new epic) stays open but **must not be decomposed**
until this document stabilizes — its children will be S1 presets +
koryphization tail + `scaffold check`, with S2/S3 as separate follow-ons.
koryph-9jn (posture), koryph-2ge (container), koryph-1hr (brew) proceed
independently; they are consumers of this design, not dependencies of it.
