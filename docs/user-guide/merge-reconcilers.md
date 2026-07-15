<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Merge reconcilers

Some files in a repo are **derived**: a pure function of a directory, checked in
alongside their inputs. A migrations lockfile (`atlas.sum`) is a checksum over
the migration directory; a secrets baseline (`.secrets.baseline`) is a set of
reviewed findings keyed by source file. When two beads each add an input and
each regenerate the derived file, the inputs merge cleanly (different filenames)
but the derived file collides — git's line-level merge cannot tell that the file
is a checksum-over-a-listing, so it calls the divergent block a conflict and the
rebase aborts.

The **correct** merged artifact is just the regeneration over the union of both
sides' inputs — which the rebase has already produced in the working tree.
Merge reconcilers close this gap: a per-project allowlist of `path → command`
that koryph consults **only** at a rebase conflict, regenerating the derived
file and continuing instead of surfacing a spurious conflict.

> **This is the belt, not the suspenders.** The first-line fix is a footprint
> label — see [Parallelism: footprints](../concepts/footprints.md). A bead that
> adds a file to a directory with a checked-in derived artifact should share a
> **write** footprint with every other such bead, so the scheduler serializes
> them at dispatch and the collision never happens. Reconcilers heal the
> residual case a label cannot cover (an out-of-band push, a non-dispatch-shaped
> merge landing between a worktree's creation and its merge). A rising heal rate
> in the logs is the signal that a label is missing upstream.

## Configuring

Add `merge_reconcilers` to `koryph.project.json` — a list of `{path, command}`
entries:

```json
"merge_reconcilers": [
  {
    "path": "migrations/atlas.sum",
    "command": "atlas migrate hash --dir file://migrations"
  },
  {
    "path": ".secrets.baseline",
    "command": "koryph-secrets-union \"$KORYPH_MERGE_OURS\" \"$KORYPH_MERGE_THEIRS\" \"$KORYPH_MERGE_PATH\""
  }
]
```

- **`path`** — matched against each conflicted path with `path.Match` glob
  semantics. An exact path (`migrations/atlas.sum`) is the common case; globs
  like `migrations/*.sum` work; there is no `**`.
- **`command`** — runs via `sh -c` in the worktree (under `direnv exec` when
  available), with the **same allowlisted environment as the green gate** — the
  orchestrator's ambient secrets are not exposed. It must leave
  `$KORYPH_MERGE_PATH` a valid, conflict-marker-free file.

Empty or absent `merge_reconcilers` is exactly today's behavior: any rebase
conflict aborts and requeues the bead for in-worktree resolution.

## The command contract

Each reconciler command receives these environment variables:

| Variable | Meaning |
|---|---|
| `KORYPH_MERGE_PATH` | Absolute path to the conflicted file to write. |
| `KORYPH_MERGE_OURS` | Temp file with the **base branch's** version (git stage 2). Empty if absent. |
| `KORYPH_MERGE_THEIRS` | Temp file with the **bead's** version (git stage 3). Empty if absent. |
| `KORYPH_MERGE_BASE` | Temp file with the merge-base version (git stage 1). Empty if absent. |

Two idioms cover the real cases:

- **Regenerate from the tree** (a checksum). After the conflict, the input files
  have already merged cleanly into the working tree — it is the post-merge
  union. So `atlas migrate hash --dir file://migrations` rewrites the lockfile
  correctly from the tree and ignores the stage variables entirely.
- **Structured union** (a secrets baseline). A blind rescan would drop the
  audited review state both sides hold, so the command must merge the two sides.
  Read `$KORYPH_MERGE_OURS` and `$KORYPH_MERGE_THEIRS`, union the file-keyed
  findings, and write `$KORYPH_MERGE_PATH`.

> **Stage inversion.** During a rebase, git's "ours" is the branch being rebased
> **onto** (your default branch) and "theirs" is the bead's commit — the reverse
> of a normal merge. `KORYPH_MERGE_OURS`/`_THEIRS` are labeled from git's stage
> numbers as-is. A **union** command is symmetric and does not care; only write
> an asymmetric command if you have accounted for this.

## Guarantees

Reconcilers are conservative by construction:

- **All-or-nothing.** A reconciler runs only when **every** conflicted path in a
  rebase step matches a configured entry. If any conflicted path is not covered
  — including a genuine conflict on an input file itself — the whole rebase
  aborts exactly as before. Auto-heal never partially resolves a step and never
  touches a hand-authored file.
- **Fail safe.** A command that fails, times out, or leaves conflict markers
  aborts the rebase to the normal requeue-for-agent path. Auto-heal only ever
  turns a fatal conflict into a clean merge, or leaves it fatal.
- **The gate is the backstop.** A healed tree still runs your green gate before
  the merge. **Put the artifact's validator in your gate** — `atlas migrate
  validate`, `detect-secrets audit --report`. That is what catches a bad
  regeneration; a reconciler without a validating gate is the one setup that can
  land a wrong derivative.
- **Bounded.** The cascade (a renumber that re-collides with the next migration)
  heals round by round, capped so a misbehaving command cannot loop forever.

## Observing

A healed merge is logged distinctly and audited:

```
bead koryph-abc: merged (a1b2c3d)
bead koryph-abc: rebase conflict auto-healed (1 generated file(s), 1 round(s)): migrations/atlas.sum
```

and a `merge-reconcile` audit event records the bead, branch, healed paths, and
round count. Watch that rate: frequent heals mean a footprint label is missing
on the beads that touch that directory — fix the label, and most collisions stop
happening in the first place.

## Migration numbers: `merge_prepare`

A reconciler heals a *conflict* on a derived file. But two beads that each add a
migration can pick the **same sequence number** (`0002_a.sql` and `0002_b.sql`)
— distinct filenames, so it is not even a git conflict, yet a migration tool
rejects the duplicate. The renumber cascade (renumber 13→14, but 14 landed too,
so 14→15…) is this problem compounding.

The root fix is to allocate the number at **merge time**, against the branch the
work is actually landing on. `merge_prepare` is that seam — an ordered command
list run in the worktree **after** the (possibly reconciler-healed) rebase and
**before** the gate. Point it at a small project script that renames the newly
added migration to the next free sequence and regenerates the checksum with a
community command (`atlas migrate hash`):

```json
"merge_prepare": [
  "scripts/renumber-migration-to-tip.sh"
]
```

- The command sees `KORYPH_DEFAULT_BRANCH` in its environment, so a
  renumber-to-tip command can diff the rebased tree against its target.
- If the command leaves the tree changed, **koryph commits it** as a single
  conventional `chore(merge): …` commit, signed with the same configuration the
  rebase just used, so the normalization rides the fast-forward merge and is
  gated. A clean tree is a no-op — most merges need nothing.
- A command that exits non-zero is a gate-shaped failure: the merge is not made
  and the bead requeues, exactly like a gate regression.

Because a duplicate number is not a git conflict, `merge_prepare` runs on **every**
merge, not only on a conflict. As with reconcilers, the first-line fix is still a
footprint label — with the migration-touching beads serialized, they pick
sequential numbers to begin with and `merge_prepare` is only the backstop.

## Tooling: keep it OSS/community

koryph invokes whatever you put in these fields as an opaque shell command and
depends on **no** migration tool itself — nothing here is a koryph dependency.
Keep the commands you configure on OSS/community tooling.

If you use Atlas, install the Apache-2.0 **Community Edition** — the default
`atlas` install is a source-available (Atlas EULA) binary with proprietary
extras, not the OSS build:

```
curl -sSf https://atlasgo.sh | sh -s -- --community
```

`atlas migrate hash` (regenerate the lockfile) and `atlas migrate validate` (a
gate check) are in the Community build. `atlas migrate rebase`, `migrate lint`,
and `migrate checkpoint` are **not** — they require the standard binary — so
renumber-to-tip with a small script rather than `atlas migrate rebase`.

## Applies to

Both the reconciler self-heal and the `merge_prepare` normalization run on every
merge path: the wave/rolling auto-merge loop, the PR-open path
(`merge_policy=pr`), and the `koryph merge` / `koryph land` CLI.
