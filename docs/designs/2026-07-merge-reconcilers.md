<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Merge reconcilers: self-healing rebase conflicts confined to known generated artifacts (2026-07-14)

Status: implemented 2026-07-14 (orchestrator-built inline; refactor-core merge
path authored on main). Beads at the end of this doc. All layers landed: L2–L4 +
L7 the reconciler, L6 the `merge_prepare` hook, L5 the guidance rollout.
Origin: three field incidents on a downstream project (2026-07) where the
serialize-then-merge half of the pipeline surfaced spurious rebase conflicts on
derived/generated files, each resolved by hand with an identical regenerate-and-continue
recipe. Field incident refs (opaque, project-neutral): a single
checksum-over-a-directory collision, a single secrets-baseline collision, and a
**triple cascading** renumber-into-renumber collision in one rebase.

## 1. Problem

koryph's footprint scheduler (`internal/sched/footprint.go`) prevents two beads
that share a **write** footprint token from being *dispatched* concurrently —
`Conflicts()` is RWMutex-shaped, so any writer excludes every other holder. That
protects the merge against two agents editing the same conflict domain at the
same time.

It has **no equivalent protection for the serialize-then-merge sequence** of a
narrow but recurring file class: **derived artifacts that are a pure function of
a directory, checked into git alongside their inputs.** The canonical shapes:

- a **checksum-over-a-directory-listing** (a migrations lockfile — `atlas.sum`
  and friends): a hash of the ordered set of migration files.
- a **secrets baseline** (`.secrets.baseline`): a file-keyed set of reviewed
  findings, one block per source file.

The failure mode, precisely:

1. Two beads each add a new timestamped/sequence-numbered input file (a
   migration). The input files **never conflict** — different filenames,
   purely additive.
2. Each bead **independently regenerates the derived artifact** from its *own*
   point-in-time view of the directory. Both regenerations are individually
   correct.
3. The derived artifacts now diverge in the exact byte region that encodes the
   directory. git's line-level three-way merge has no idea the file is a
   checksum-over-a-listing — it sees two edits to the same lines and calls it a
   **conflict**. The rebase stops and koryph aborts it (`merge.go:160`),
   surfacing `StatusConflict` and requeueing the bead for in-worktree
   resolution (`poll.go:1190`).

The content does not genuinely conflict: the correct merged artifact is the
regeneration over the **union** of both sides' inputs, which every git rebase
already produces in the working tree (the input `.sql` files merge cleanly). The
only casualty is the derived file, which is trivially recomputable.

Worse — the **cascade** (the triple-collision incident): the manual dodge for a
duplicate sequence number is to *renumber* the losing migration (`000013 →
000014`). But a renumber is itself a new commit that regenerates the checksum,
and in the interim another migration had landed at `000014`, so the renumbered
commit collided again, and its renumber (`000014 → 000015`) collided a third
time. Three conflict rounds in one rebase, each mechanically identical.

Every one of the three incidents was resolved by hand with the **same** recipe:
discard the conflicted derived file, regenerate it from the *post-merge*
directory, stage it, and `git rebase --continue`. For the checksum:
`atlas migrate hash --dir file://migrations`. For the secrets baseline: a
**structured union of the file-keyed findings** — not a blind rescan, which
would drop the human review state (which findings were audited as false
positives) that both sides legitimately hold.

This design closes the residual gap for exactly this file class, in three layers
of leverage (§3), the middle one being the concrete new feature.

## 2. Invariants (the correctness contract)

The merge pipeline's existing contract is unchanged; auto-heal is a strictly
*additive* branch taken only at the single point where a rebase already failed.

- **I1 — only known-generated paths, all-or-nothing.** A reconciler runs only
  for an unmerged path matching a configured entry. If **any** unmerged path in
  a rebase step is not covered by a reconciler, the entire rebase aborts exactly
  as today (`StatusConflict`, CONFLICT.md, requeue). There is **no partial
  resolution** — auto-heal never stages a subset of a conflicted step, and never
  touches a hand-authored file. Mantra: **footprints protect the merge;
  reconcilers heal the derivatives.**
- **I2 — fail safe to today's behavior.** Any reconciler failure — non-zero
  exit, timeout, the path still carrying conflict markers / still unmerged after
  the command, an unreadable command, or the round cap (I4) — aborts the rebase
  and returns `StatusConflict`, the same requeue-for-agent path as an
  un-healable conflict. Auto-heal only ever turns a fatal conflict into a clean
  merge **or leaves it fatal**; it never lands something it could not fully
  reconcile.
- **I3 — the green gate is the backstop.** A healed rebase still runs the
  project green gate before the fast-forward merge (auto-heal is strictly before
  the existing gate step, `merge.go:168`). A regeneration that produced a wrong
  artifact is caught there — e.g. `atlas migrate validate` or
  `detect-secrets audit --report` in the gate fails a bad heal. **Nothing merges
  ungated.** This is what makes a trusted-but-fallible regeneration command
  safe.
- **I4 — bounded cascade.** The `--continue` loop is capped
  (`maxReconcileRounds`). Exceeding the cap aborts to `StatusConflict`. This
  bounds the renumber-into-renumber cascade instead of looping, and caps a
  misbehaving command.
- **I5 — additive; no config = today's behavior.** A project with no
  `merge_reconcilers` behaves byte-for-byte as today: a rebase conflict aborts
  and requeues. The field is `omitempty`; old configs load unchanged.
- **I6 — determinism / idempotence contract.** A reconciler command must be a
  pure function of the merged tree and the two conflict sides; re-running it
  yields the same bytes. The engine does not trust this blindly — it verifies
  the path is no longer unmerged and is staged before `--continue` (I2 catches a
  command that lied).
- **I7 — sandbox parity with the gate.** Reconciler commands run with the same
  allowlisted environment and `direnv exec` wrapping as gate commands
  (`gate.go`). A reconciler is trusted exactly as much as a gate command — both
  execute project-authored, agent-adjacent code — and no more: the orchestrator's
  ambient secrets are not in scope.

## 3. Design

### L1 — Layer one (leverage: highest, cost: lowest): footprint the inputs

The concurrent case is preventable with a label, no code. Every bead whose diff
touches the migration directory should carry a shared **write** footprint
(`area:migrations` mapped in `area_map`, or a direct `fp:*` token) so the
scheduler serializes them at *dispatch*: bead B never leaves the ready queue
while bead A (same token) is in flight, so B's worktree is cut from a default
branch that already carries A's migration **and** its regenerated checksum. B
then rebases onto an unchanged base and there is no conflict to heal.

This is labeling discipline, not a missing mechanism — `Conflicts()` already
serializes any two writers of a shared token. The incidents happened because the
generating beads were **unlabeled** (fell through to `domain:unknown`, which
serializes against *every* unlabeled bead but not specifically, and can be
defeated by sparse labeling elsewhere) or mislabeled onto a per-package area
that did not encode the shared migration directory.

**Why layer one is necessary but not sufficient.** Footprints serialize
*dispatched* beads. They do **not** serialize against work that enters the
default branch out of band between B's worktree creation and B's merge: a human
push, a `merge-request`/`feature`/`epic` bead (not dispatch-shaped, so never
footprint-scheduled), or a cross-project/rebased landing. Any of those can land
a migration under B's feet, re-opening the exact conflict at merge time. Layer
two is the belt to layer one's suspenders.

Guidance rollout mirrors the resource-governor precedent (that design's L6):
a one-line rule on every planning surface that already carries the footprint
rules — koryph-plan / koryph-issue / koryph-replan / koryph-import step 3,
koryph-ops, `agents/koryph-architect.md`, the plan-scorer's
scheduler-correctness checks, and CLAUDE.md's Conventions block. The rule:

> **Co-footprint generated-artifact inputs.** A bead that adds a file to a
> directory with a checked-in derived artifact (a migrations lockfile, a
> secrets baseline, a generated index) must share a write footprint with every
> other such bead — the derived file serializes even when the inputs don't.
> Declare the merge reconciler in `koryph.project.json` so a residual collision
> self-heals (`docs/designs/2026-07-merge-reconcilers.md`).

An optional plan-audit heuristic (filed, not blocking) flags beads whose
description mentions a migration/generated artifact but carry no shared
footprint — the koryph-replan repair-pass shape.

### L2 — Layer two (the feature): merge-time auto-heal for known-generated files

A per-project allowlist of **path → regeneration command**, consulted at the one
point where a rebase already failed, before the abort is made final.

**The contract (L1 of the runtime seam).** On a rebase conflict, for each
unmerged path matching a reconciler, the engine runs the reconciler's command
in the worktree with:

- **cwd** = the worktree root.
- **environment** = the gate's allowlisted env (`execx.GateEnv()`) + `direnv
  exec` wrapping (I7), plus:
  - `KORYPH_MERGE_PATH` — absolute path to the conflicted file in the worktree.
    The command **must** leave this a valid, conflict-marker-free file, staged-ready.
  - `KORYPH_MERGE_OURS` / `KORYPH_MERGE_THEIRS` / `KORYPH_MERGE_BASE` — absolute
    paths to temp files holding the three conflict stages (git index stages 2,
    3, 1). Any stage absent (add/add has no base; add on one side only) → the
    variable is set to the empty string. The command reads these when it needs a
    structured merge; a regenerate-from-tree command ignores them.

Two regeneration idioms, both expressible as a command (the engine stays generic
— the *strategy* lives in the project's command, not in koryph):

- **Regenerate-from-tree** (the checksum case). After a rebase conflict confined
  to `migrations/atlas.sum`, the migration `.sql` files have **already** merged
  cleanly into the working tree — it is exactly the post-merge union. So
  `atlas migrate hash --dir file://migrations` rewrites `atlas.sum` correctly
  from the tree and ignores the conflict stages entirely.
  ```json
  { "path": "migrations/atlas.sum",
    "command": "atlas migrate hash --dir file://migrations" }
  ```
- **Structured union** (the secrets-baseline case). A blind rescan drops the
  audited review state both sides hold, so the command must merge the two sides.
  The engine hands them over via `KORYPH_MERGE_OURS`/`_THEIRS`; the project
  supplies a small keyed-union helper writing `KORYPH_MERGE_PATH`:
  ```json
  { "path": ".secrets.baseline",
    "command": "koryph-secrets-union \"$KORYPH_MERGE_OURS\" \"$KORYPH_MERGE_THEIRS\" \"$KORYPH_MERGE_PATH\"" }
  ```

**The heal loop (L2 of the seam), in `internal/merge`:**

```
rb := git rebase <def>            # in the worktree (existing merge.go:156)
if rb failed:
    healed := reconcileRebase(worktree, reconcilers):
        for round in 0 .. maxReconcileRounds:
            U := unmerged paths (git diff --name-only --diff-filter=U)
            if U is empty:                      return not-healed   # stopped for a reason we don't own
            if any p in U has no matching reconciler:  return not-healed   # I1: real conflict
            for p in U:
                stages := extract :1:p, :2:p, :3:p to temp files
                run reconciler(p) with KORYPH_MERGE_* env          # I7
                if command failed OR p still unmerged:  return not-healed   # I2/I6
                git add p
            cont := git rebase --continue   (GIT_EDITOR=true)      # reuse commit msg
            if cont succeeded (exit 0):      return healed          # rebase complete
            # else: a *new* conflict — loop re-reads U; a non-conflict
            #       failure surfaces next round as empty-U → not-healed (I2)
        return not-healed                       # I4: cap exceeded
    if not healed:
        git rebase --abort; write CONFLICT.md; return StatusConflict   # unchanged path
    # healed → fall through to the existing green gate (I3)
```

`git rebase --continue` returns exit 0 **only** on full completion and non-zero
when it stops on the next commit's conflict — so the loop naturally handles the
cascade: each round heals the current step's derived-file conflict and advances.
The renumber-into-renumber triple collision becomes three quiet rounds. A
now-empty commit (reconciliation left no diff) makes `--continue` fail with no
unmerged paths → the next round sees empty `U` → not-healed → abort (fail-safe,
I2); v1 does not `--skip` (that could silently drop a bead's commit).

**Rebase stage inversion (documented, not a bug).** In `git rebase <def>` run in
the worktree, HEAD is `<def>` while each replayed bead commit is applied, so git
stage 2 ("ours") is the rebase *base* (`<def>`/main) and stage 3 ("theirs") is
the *bead's* commit — the reverse of a normal merge. `KORYPH_MERGE_OURS`/`_THEIRS`
are labeled from git's stage numbers as-is; a **union** command is symmetric so
it does not care, and a regenerate-from-tree command ignores them. The design
deliberately does not paper over the inversion — it documents it in the user
guide so a project writing an *asymmetric* command knows which side is which.

**Path matching.** stdlib `path.Match` semantics (plus exact-equality
fast-path): the incident files are fixed paths (`migrations/atlas.sum`,
`.secrets.baseline`); patterns like `migrations/*.sum` are supported without a
new dependency. No `**`. An over-broad glob only costs a wasted (fail-safe)
command run; it can never *widen* what gets healed past I1's all-covered gate.

### L3 — Config surface (project vocabulary)

`project.Config` gains `MergeReconcilers`, a sibling of `Resources`/`AreaMap` —
a portable, checked-in vocabulary, additive and `omitempty`:

```go
type MergeReconciler struct {
    Path    string `json:"path"`    // path.Match glob against each unmerged path
    Command string `json:"command"` // sh -c in the worktree; see KORYPH_MERGE_* env
}
// Config:
MergeReconcilers []MergeReconciler `json:"merge_reconcilers,omitempty"`
```

`Config.Validate()` gains `validateMergeReconcilers`: every entry needs a
non-empty `path` and a non-empty `command`, and `path` must be a legal
`path.Match` pattern (reject `filepath.ErrBadPattern` early rather than at merge
time). Schema mechanics follow the `Resources` precedent exactly: additive
`omitempty` field, `go generate ./internal/project` regenerates the JSON schema,
and the `internal/project/testdata` schemaver fingerprint golden is regenerated
(a new struct field changes the fingerprint; the drift test in `make gate` is
the tripwire). No `schema_version` bump — the field is additive and old binaries
that don't know it simply ignore an absent map, matching how `Resources` landed.

### L4 — Engine wiring

`merge.Opts` gains `Reconcilers []Reconciler` (a merge-package type so `merge`
keeps not importing `project`, exactly like `Gate []string`). `merge.Merge`
consumes it in the rebase branch (L2). All **three** `merge.Opts` construction
sites thread `cfg.MergeReconcilers` through a one-line mapper:

- `internal/engine/poll.go:1046` — the auto-merge loop (the incident path).
- `internal/engine/poll.go:1254` — the PR-open path (a healed rebase then opens
  the PR green, same benefit).
- `internal/engine/land.go:76` — the `koryph merge` / `koryph land` CLI, so an
  operator landing by hand gets the same self-heal the loop does.

### L7 — Observability

A healed rebase is not a silent success: it emits a distinct progress line
(`bead X: rebase conflict auto-healed (N generated file(s), M round(s)): <paths>`)
and a `registry.Audit` event (`Kind: "merge-reconcile"`, detail: bead, branch,
paths, rounds) so an operator can see how often the derived-file collision fires
— the signal that layer-one labeling is missing somewhere (the fix is cheaper
than relying on the heal). `merge.Result` gains `Reconciled []string` +
`ReconcileRounds int` (`omitempty`) carrying the healed paths up to the engine
for the log/audit; an un-healed or reconciler-free merge leaves them zero.

### L5 / L6 — layers one and three

- **L5 (layer one)** is the guidance rollout in L1 — no engine code: the
  co-footprint rule on every planning surface that already carries the footprint
  rules (koryph-plan/issue/replan/import/ops command sources, the architect and
  plan-scorer personas, CLAUDE.md's Conventions block), plus a keyword-driven
  `plan audit` heuristic that flags a bead whose description names a generated
  artifact but carries no shared write footprint.
- **L6 (layer three): merge-time migration-number allocation — the
  `merge_prepare` hook.** The root cause of the *cascade* specifically is that a
  bead grabs its migration sequence/timestamp number at authoring time, so two
  in-flight beads can both pick `000013`. Allocating the number at *merge* time
  removes the duplicate-number collision at the source, so no renumber (and thus
  no cascade) is ever needed. koryph is migration-tool-agnostic, so — like the
  reconciler — the allocation is a **project-supplied command**:
  `merge_prepare`, an ordered command list run in the worktree AFTER the
  (possibly reconciler-healed) rebase and BEFORE the gate, with
  `KORYPH_DEFAULT_BRANCH` in the environment so a renumber-to-tip command can
  diff the rebased tree against the branch it is landing on. koryph commits any
  resulting change itself (a single `chore(merge): …` commit — conventional
  message, signed with the same worktree config the rebase just used) so the
  normalization rides the fast-forward and is gated. A duplicate migration
  number is **not** a git conflict (distinct filenames), so this runs on every
  merge, not only on a conflict; a clean tree is a no-op. A command regression is
  a gate-shaped failure (requeue), never a hard error. The renumber-to-tip
  command is the canonical use; the seam is general "normalize the rebased tree
  before the gate."

## 4. Compatibility

| Surface | Behavior |
|---|---|
| Project with no `merge_reconcilers` | Byte-for-byte today (I5): a rebase conflict aborts, writes CONFLICT.md, requeues. No new code path is entered. |
| `merge_reconcilers` set, conflict on a non-covered path | Aborts exactly as today (I1). Auto-heal is all-or-nothing over the step's unmerged set. |
| `merge_reconcilers` set, conflict confined to covered paths | Regenerate + `--continue`; on full success, proceed to the **unchanged** green gate + ff-merge (I3). |
| Reconciler command fails / leaves markers / cap exceeded | Aborts to `StatusConflict` — the same requeue-for-agent outcome as an un-healable conflict (I2/I4). |
| `koryph.project.json` schema | Additive `merge_reconcilers` array; `go generate ./internal/project` + committed schema + regenerated fingerprint golden. Protected path → orchestrator-applied. No `schema_version` bump. |
| Old koryph binary reading a config **with** `merge_reconcilers` | The unknown field is ignored (encoding/json), so the binary behaves as today. Reconcilers only bind on a binary that knows the field. |
| `koryph merge` / `koryph land` (CLI) | Same self-heal as the loop (L4, land.go site). A healed manual land reports `Reconciled` paths. |
| PR-open path (`merge_policy=pr`) | Healed rebase then opens the PR green (L4, poll.go:1254 site). Both reconcilers and `merge_prepare` apply. |
| Project with no `merge_prepare` | Byte-for-byte today: no post-rebase command runs, no extra commit. |
| `merge_prepare` set, clean tree after commands | No-op: nothing committed, `Result.Prepared` false — the common case. |
| `merge_prepare` set, tree dirty after commands | koryph stages + commits a single `chore(merge): …` (conventional, signed as the rebase); the branch stays a fast-forward, the change is gated, `Result.Prepared` true. |
| `merge_prepare` command fails | `StatusGateFailed` — the same requeue path as a gate regression; the partial change is discarded (`git checkout -- .`). |
| Squash merges | Unaffected — the heal happens during the pre-merge rebase, before the ff/squash branch at merge.go:190. |
| Protected paths | Orthogonal and unchanged: the protected-path preflight runs before any rebase; a reconciler cannot resurrect a protected-path change (it only regenerates already-conflicted, allowlisted derived files). |
| Engine full-run test fixtures | Reconcilers are inert without `merge_reconcilers`; fixtures stay hermetic. |

## 5. Testing

- **merge (`internal/merge/reconcile_test.go`, git-integration, models
  `merge_test.go`'s `initRepo`/`worktreeOn`/`commitIn`):**
  - single collision heals: main adds `migrations/0002_x.sql` + regenerates a
    listing-checksum stand-in; the branch adds `migrations/0002_y.sql` +
    regenerates its own; a reconciler that recomputes the listing heals → merged,
    and the merged artifact reflects **both** inputs. (Checksum stand-in:
    `(cd migrations && ls *.sql | sort) > migrations/atlas.sum` — a
    derived-from-directory file that line-conflicts but regenerates trivially,
    no external tool.)
  - **cascade**: a branch with two commits each touching the derived file, each
    conflicting after the previous `--continue` — exercises the loop and asserts
    round count = 2.
  - **structured union** via the env contract: a `.secrets.baseline` stand-in as
    a newline set; reconciler `cat "$KORYPH_MERGE_OURS" "$KORYPH_MERGE_THEIRS" |
    sort -u > "$KORYPH_MERGE_PATH"` heals and the result is the union — pins
    `KORYPH_MERGE_OURS/_THEIRS`.
  - **I1 refusal**: a conflict on a covered path **and** a hand-authored path →
    no heal, `StatusConflict`, worktree clean (rebase aborted), CONFLICT.md
    written.
  - **I2 fail-safe**: reconciler exits non-zero → `StatusConflict`, aborted; and
    a command that leaves the file still conflicted → `StatusConflict`.
  - **I4 cap**: rounds beyond `maxReconcileRounds` → `StatusConflict`.
  - **I3**: a reconciler that produces a gate-failing artifact → `StatusGateFailed`
    (heal succeeded, gate caught it), not merged.
  - inert path: `merge_reconcilers` empty → identical to the existing conflict
    test.
  - **merge_prepare (L6):** a duplicate migration number (distinct filenames, no
    git conflict) after a **clean** rebase → a prepare command renumbers it →
    merged, the renumbered file lands, and koryph committed one
    `chore(merge): …`; the command sees `KORYPH_DEFAULT_BRANCH`; a clean prepare
    is a no-op (no extra commit, `Prepared` false); a failing prepare command →
    `StatusGateFailed`, main unmoved.
- **project:** `validateMergeReconcilers` (empty path, empty command, bad glob
  pattern rejected) + `merge_prepare` blank-command rejection; schema round-trip;
  regenerated fingerprint golden.
- **engine:** the three `Opts` sites carry `Reconcilers` + `Prepare`; a fake-git
  or the merge integration proves the loop path merges a healable conflict
  end-to-end and logs the `merge-reconcile` / `merge-prepare` audit events.

## 6. Sequencing (bead map)

- **M1** `merge`: `Reconciler` type, `reconcile.go` heal loop, `merge.Merge`
  wiring, `Result.Reconciled`/`ReconcileRounds`, reconcile_test. *(refactor-core
  — merge/rebase path, orchestrator-authored on main.)*
- **M2** `project`: `MergeReconciler` type + `MergeReconcilers` field,
  `validateMergeReconcilers`, `go generate` schema, fingerprint golden.
  *(area:project; can parallel M1 — merge type is independent of config type.)*
- **M3** `engine`: thread `cfg.MergeReconcilers → merge.Opts.Reconcilers` at the
  three sites + the `merge-reconcile` progress/audit line. *(refactor-core;
  depends M1 + M2.)*
- **M4** docs: `docs/user-guide/merge-reconcilers.md` (the contract, the two
  idioms, the stage-inversion note) + a pointer from `running-waves.md`; the
  `docs/developer-guide/packages.md` `internal/merge` entry gains the reconciler.
  *(area:docs; depends M1–M3.)*
- **M5** guidance (layer one, L5): the co-footprint rule across the planning
  surfaces + the `plan audit` heuristic. *(area:cli internal/commands +
  orchestrator-applied `agents/`/CLAUDE.md; anytime.)*
- **M6** `merge`+`project`+`engine`: the `merge_prepare` hook (`prepare.go`,
  `Opts.Prepare`, `Result.Prepared`, config field + validation, the three `Opts`
  sites, `merge-prepare` audit line, prepare_test). *(refactor-core — merge path;
  independent of the reconciler, composes with it.)*

M1 ∥ M2 → M3 → M4. M1 and M3 are `refactor-core` (self-hosting safety rule:
the merge/rebase path is orchestrator-authored on main, never loop-dispatched).

## 7. Risks

- **A wrong regeneration masking a real problem.** The mitigation is structural,
  not hopeful: the green gate runs on the healed tree before merge (I3), so a
  bad artifact fails a gate that validates it (`atlas migrate validate`,
  `detect-secrets audit`). Projects are told, in the user guide, to put the
  validator in their gate — a reconciler without a validating gate is the one
  configuration that can land a bad derivative, and it is called out explicitly.
- **Reconciler command as an execution surface.** A reconciler runs
  project-authored code mid-merge. It is scoped identically to a gate command
  (I7: allowlisted env, direnv) — the same trust the project already extends to
  its gate — and no ambient orchestrator secret is exposed. It is not a new
  trust boundary, it is the existing gate boundary.
- **All-or-nothing is deliberately conservative.** A step with one covered and
  one genuine conflict does not partially heal (I1) — the operator/agent still
  resolves the whole step. This can feel like "it didn't help" on a mixed
  conflict; the alternative (partial staging) risks masking the real conflict
  and is rejected.
- **Empty-commit-after-heal aborts rather than `--skip`.** Fail-safe (I2): v1
  will not silently drop a commit whose only content was a now-redundant
  regeneration. Rare in practice (the union includes the bead's new input, so
  the derived file differs); documented as a known limitation with `--skip` as a
  considered v2.
- **Layer one still matters most.** Auto-heal is the belt; the L7 audit event
  exists precisely so a rising heal rate flags missing footprint labels — the
  cheaper fix. The design does not let the heal become an excuse to stop
  labeling.
- **Cross-project / out-of-band landings.** The residual gap layer one cannot
  close (§L1) is exactly what layer two covers; between them the window is a
  genuine conflict on the *inputs* (two edits to the same migration file), which
  is a real conflict and correctly refuses to heal (I1).

## Beads (filed 2026-07-14)

| Epic | ID | Children |
|---|---|---|
| Merge reconcilers | koryph-ma3 | .1 merge core (refactor-core) · .2 project config · .3 engine wiring (refactor-core) · .4 docs · .5 layer-one guidance · .6 migration-number allocation (`merge_prepare`) |

Deps: .3←{.1,.2}; .4←.3; .5 anytime; .6 independent of the reconciler (composes
with it). .1/.3/.6 touch the refactor-core merge path, orchestrator-authored on
main (self-hosting safety rule). All six built and closed inline this session.
