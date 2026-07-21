<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph adopt

`koryph adopt` is the front door for bringing an **existing** git repository
under koryph: one command that takes it to a green `koryph validate`, or
tells you precisely what's missing and offers to fix it. It exists because
the manual path is a funnel with a lot of exits — install `claude` and `bd`
by hand, run `bd init` by hand, run `koryph project add` with two flags you
have to already understand, hand-edit `koryph.project.json`'s
`gate`/`area_map`/`forge`, then `validate` — an eight-step, four-document
journey where every step is a place to give up. `adopt` replaces no existing
verb — `project add`, `install-assets`, `validate`, and `doctor` all keep
their contracts — it sequences them and fills the gaps between them.

```
koryph adopt [<root>] [--yes] [--dry-run] [--json] [flags]
```

`<root>` defaults to the current directory.

---

## The five phases

`adopt` runs five strictly ordered phases: **detect** (read-only) → **plan**
(printed, nothing written yet) → **confirm** (three consent scopes) →
**execute** (dependency-ordered, idempotent) → **verify** (`koryph validate`
must go green).

### 1 — Detect (read-only)

Everything in this phase only reads: platform and package manager; the
repo's own `flake.nix`/`.envrc`/direnv posture; the four binaries `git`,
`claude`, `bd`, `gh`; Claude account candidates (`~/.claude.json` and each
`CLAUDE_CONFIG_DIR`'s own copy); git facts (remote → forge, default branch,
dirty tree); beads state (`.beads/` presence, `bd doctor`, hardening, git
hooks); gate candidates (Makefile, `package.json`, `go.mod`, `Cargo.toml`,
`pyproject.toml`); the top-level source layout (for `area_map`); and
koryph's own state (registry record, `koryph.project.json`, asset drift).
Nothing is written here — plan and confirm come first.

### 2 — Plan

The plan is a printed, ordered list of steps. Each carries a **state**
(`done` — skip; `needed` — required; `offer` — optional, default-off;
`blocked` — a consent was declined or an install failed) and, for a
`needed`/`blocked` step, one plain-language **why**. A realistic run on a
repo missing only `bd` looks like:

```
ADOPTION PLAN — myrepo (/Users/me/src/myrepo)

  done    tools      git 2.49, claude 2.1 (authed), gh 2.62
  needed  tools      bd not found → install via brew (brew install gastownhall/beads/bd)
                     why: koryph dispatches work from the beads ready-graph; without it the loop has nothing to build
  done    home       /Users/me/.koryph initialized
  needed  beads      initialize issue DB (bd init --prefix myrepo --remote git+https://github.com/me/myrepo.git)
                     why: koryph dispatches work from the beads ready-graph; without it the loop has nothing to build
  needed  register   account personal <me@example.com> (derived from ~/.claude.json); confirm
                     why: koryph must know which account/identity is authorized to dispatch on this project's behalf
  needed  config     gate: make gate (confirm); forge: github (host-matched from remote URL "git@github.com:me/myrepo.git"); area_map: 4 area(s) proposed from api, cmd, internal, web
                     why: gate/forge/area_map drive dispatch safety — a wrong gate green-lights garbage
  needed  assets     install AGENTS.md, agent personas, commands, hooks + settings.json (capability-gated by runtime)
                     why: AGENTS.md + personas + commands + hooks make koryph semantics apply whether invoked explicitly or implied by a prompt
  offer   signing    `koryph signing keygen` (no-vault path) or `koryph signing setup` (vault-backed)
  offer   posture    `koryph posture apply oss-solo-maintainer` (default profile) or a named profile
  needed  commit     commit whatever this wizard wrote (AGENTS.md, .claude/, koryph.project.json, .beads/ tracked files, ...)
                     why: leaves the repo fully committed instead of half-onboarded
  needed  verify     run `koryph validate` and require green
                     why: koryph validate is the pre-dispatch gate; adopt isn't done until it's green
```

`offer` steps (signing, posture) never print a why-line in the plan itself —
they're explained interactively if you accept them, or printed as a
one-liner "enable later" pointer otherwise. `--dry-run` stops here and
writes nothing.

### 3 — Confirm — three consent scopes

Consent is never one blanket yes:

1. **Repo scope** — one consolidated y/N for the plan as printed: everything
   written inside the repo and `~/.koryph`.
2. **System scope** — each package-manager install (and each `flake.nix`
   edit, see below) is confirmed individually, showing the exact command —
   or diff — before it runs. koryph never elevates with `sudo` on your
   behalf, even under `--yes`; a sudo-requiring install is called out
   explicitly, and declining downgrades that step to `blocked` with manual
   instructions — the wizard keeps going where it can.
3. **Value confirmations** — the account/identity, gate commands, and forge
   are derived values you have to own. Each is shown with its provenance
   (e.g. "derived from `~/.claude.json`") and a one-keystroke accept. The
   gate is always confirmed explicitly: a wrong gate green-lights garbage.

### 4 — Execute

Dependency-ordered, each step idempotent, streamed as `ok`/`skip`/`warn`/
`block` lines:

1. **deps** — install any missing `claude`/`bd`/`gh` via the consented
   route (Homebrew, apt/dnf/pacman/zypper, `nix profile install`, or the
   repo's own flake — see below). A decline or failure blocks that tool but
   never aborts the run.
2. **home** — `koryph init`'s layout, including `slots/`, `slots/demand/`,
   and `telemetry/`, so `koryph doctor` is clean immediately after.
3. **beads** — initialize (`bd init --non-interactive --init-if-missing
   --prefix <id> [--remote <derived>]`) or, on an existing DB, snapshot +
   `bd doctor` + harden (`.beads/.gitignore`, `sync.remote`, git hooks).
   This runs **before** assets so the settings merge in step 5 dedupes bd's
   own session-priming hook instead of double-registering it.
4. **register + config** — registers the project under the confirmed
   account, then writes the confirmed `gate`/`forge`/`area_map` into
   `koryph.project.json` — an **existing** config is never overwritten.
5. **assets** — the same asset sequence `koryph project add` runs:
   `AGENTS.md`, personas, `koryph-*` commands, hooks + `.claude/settings.json`
   merged additively. See [Quickstart](quickstart.md) for the full asset
   table.
6. **offers** — signing and posture: explained, default-off, one keystroke
   to accept; declining prints the command to run later.
7. **commit** — offers one signed conventional commit
   (`chore: adopt koryph`) of everything the wizard wrote, so the repo never
   lands half-committed. Declining leaves the tree dirty with a summary of
   what's unstaged.
8. **verify** — runs the `koryph validate` check sequence in-process and
   prints next steps (describe what to build, `/koryph-design`,
   `/koryph-plan`, `koryph run --once --dry-run`).

### 5 — Verify

`adopt` exits 0 only when `koryph validate` is green (or on `--dry-run`). On
the first green pass it promotes the registry record `registered →
migrated`, exactly as running `koryph validate` by hand would. A `FAILED`
exit names the failing check and leaves the repo exactly where the execute
phase got it — fix the cause and re-run `adopt`.

---

## Non-interactive and `--json`: the agent-drivable contract

Any of `--yes`, `--json`, or a non-TTY stdin puts `adopt` in non-interactive
mode: derived values are accepted only where the provenance is unambiguous;
anything ambiguous **fails closed**, printing the exact flag that resolves
it and writing nothing for that value.

| Flag | Purpose |
|---|---|
| `[<root>]` | Repo to adopt (default: current directory) |
| `--yes` | Non-interactive: accept unambiguous derivations, fail closed on ambiguity |
| `--dry-run` | Detect + print the plan, write nothing |
| `--json` | Emit `{root, project_id, steps[]}` on stdout; progress moves to stderr; implies non-interactive |
| `--account <profile>` | Account profile (with `--identity`; overrides discovery) |
| `--identity <email>` | Login email that must match at dispatch (with `--account`) |
| `--config-dir <dir>` | `CLAUDE_CONFIG_DIR` for a non-personal account |
| `--id <slug>` | Project slug (default: repo dir name slugified) |
| `--branch <name>` | Default branch (default: detected) |
| `--gate "cmd1;;cmd2"` | Gate command (repeatable, or one `;;`-separated list); overrides inference |
| `--forge github\|gitlab` | Forge provider; overrides inference |
| `--remote <url>` | Beads sync remote URL; overrides the derived origin |
| `--no-remote` | Force a local-only beads init (no sync remote) |
| `--no-posture` | Skip the posture profile offer |
| `--no-commit` | Skip the adoption commit offer |
| `--force` | Override an `.envrc` account-disagreement refusal |

What fails closed, and how to resolve it without a prompt:

- **Zero or 2+ verified Claude account candidates** → pass `--account` and
  `--identity` (required together).
- **No gate could be inferred** (no Makefile/`package.json`/`go.mod`/
  `Cargo.toml`/`pyproject.toml` match) → pass `--gate "cmd1;;cmd2"`
  (repeatable).
- **A git remote that matches no known forge host** → pass `--forge
  github|gitlab`. A repo with **no** remote at all is not ambiguous — forge
  simply resolves empty, nothing to be wrong about.

`koryph adopt <root> --yes --json` is the whole [llms.txt](../llms.txt)
agent runbook collapsed into one command; that file keeps the manual
step-by-step as the fallback path for whatever a `blocked` step can't
resolve non-interactively.

---

## The nix-flake route

When the target repo has its own `flake.nix`, a missing `bd` is offered the
repo's own flake as the install route **before** falling back to a system
package manager: `adopt` proposes the minimal, structural edit that adds the
beads flake input and wires `bd` into the default devShell's package list,
shows it as a diff for consent, applies it, runs `nix flake lock`, then
verifies with `direnv exec <root> bd version` (or `nix develop -c bd
version` when direnv isn't set up). Declining the edit, or any step of it
failing, falls back to the system route with its own consent — `adopt`
never leaves you without an install path.

---

## What adopt deliberately does not do

- **No vault account provisioning.** Setting up Proton Pass/1Password/etc.
  stays yours to do; `koryph signing keygen` is the offered no-vault path.
- **No GitHub App/bot provisioning.** `adopt` signposts `koryph bot create`
  as a later step; it never creates one for you.
- **No app-layout opinions.** `area_map` inference only names top-level
  directories that already exist — it never proposes how a repo *should* be
  laid out.
- **No `koryph new` decomposition.** `adopt` is the koryphization tail a
  future `koryph new` will call after scaffolding a brand-new repo — it
  isn't a substitute for that greenfield verb, which doesn't exist yet.
- **No background/daemon anything.** `adopt` is one foreground run, start to
  finish.

---

## The `/koryph-adopt` skill

Adoption installs a `/koryph-adopt` slash command into `.claude/commands`
alongside `/koryph-plan` and friends, so an agent session in an adopted
workspace can drive this whole wizard conversationally — adopt *another*
repo (`/koryph-adopt ~/src/other-repo`), or re-run it here as a repair
pass. The skill previews with `--dry-run --json`, asks you for exactly the
fail-closed values the wizard couldn't derive, and never runs a `sudo`
install itself. A session in a repo that hasn't been adopted yet won't have
the skill — that's what the agent runbook in
[`llms.txt`](https://koryph.build/llms.txt) is for: point any AI session at
it and ask it to adopt koryph.

## From adoption to work: describe what you want built

An adopted repo needs no koryph-aware `CLAUDE.md` — most repos predate
koryph or have no instruction file at all. The bridge is installed, not
assumed: the `koryph-intent.sh` hook (wired as a `UserPromptSubmit` hook in
`.claude/settings.json`) watches interactive prompts, and when one reads
like a description of something to **build, change, or fix**, it injects a
small routing note telling the session to map the ask onto the planning
commands — `/koryph-design` for feature-sized asks, `/koryph-plan` for an
existing design doc, `/koryph-import` for TODO/roadmap markdown,
`/koryph-issue` for a single small fix — rather than implementing ad hoc.
Those commands compute the footprint (`area:*`/`fp:*`) and resource
(`res:*`) labels the parallel scheduler needs. The same routing ships in
the installed `AGENTS.md` for runtimes without hook support. See
[Zero to shipped](zero-to-shipped.md) Stage 2.

## Re-run any time

Re-running `adopt` on an already-adopted repo costs nothing: `tools` through
`assets`, and `commit`, all read `done`; `signing`/`posture` stay `offer`;
and `verify` re-runs `koryph validate` every time, reporting `done` itself
once a canary wave has promoted the record all the way to the `validated`
rung (see [Migration lifecycle](projects-and-accounts.md#migration-lifecycle)).
Either way the run exits 0, or names the failing check — so `adopt` doubles
as an onboarding-scoped `koryph doctor`, safe to run again after any change
or just to confirm a project is still healthy. A fully steady-state repo
prints:

```
ADOPTION PLAN — myrepo (/Users/me/src/myrepo)

  done    tools      git 2.49, claude 2.1 (authed), bd 1.2.0, gh 2.62
  done    home       /Users/me/.koryph initialized
  done    beads      hardened (+hooks)
  done    register   already registered as myrepo (account personal <me@example.com>)
  done    config     existing config kept (koryph.project.json already present)
  done    assets     3 persona(s), commands, hooks + settings.json present
  offer   signing    `koryph signing keygen` (no-vault path) or `koryph signing setup` (vault-backed)
  offer   posture    `koryph posture apply oss-solo-maintainer` (default profile) or a named profile
  done    commit     nothing to commit
  done    verify     previously validated
```
