<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# State versioning: schema gates, a write ceiling, and explicit migrations (2026-07-07)

**Status:** partially implemented (2026-07-10). `internal/schemaver` exists and
provides the surface registry plus the forward-compatibility read/write guards
(`Current`/`CheckRead`/`CheckWrite`): an older binary now REFUSES to load or
overwrite state written by a newer koryph rather than silently misreading or
field-stripping it. The guards are wired into the registry, quota, signing
vault, per-project config, and ledger (run + manifest) load paths, and each
surface's write path stamps its version from schemaver (single source of truth).
Still open under epic **koryph-r0l**: governor.json versioning (koryph-r0l.4),
the explicit forward-migration runner, the `koryph doctor` state-schema check
(koryph-r0l.8), and the `koryph version --state` / `state migrate` CLI
(koryph-r0l.9). Sections below marked "intended" describe those unbuilt parts.

Multiple koryph binaries of different versions run against the same shared
state: KORYPH_HOME is one per machine, several flake environments each pin
their own koryph, and the cross-project governor is *designed* for concurrent
writers (`koryph-loop` "joins the shared cross-project governor"). Today
nothing reconciles binary version against state version in either direction.
This design adds the two missing capabilities: (a) a binary refuses to write
state stamped by a newer schema than it was compiled with, and (b) a binary
loads any older state, migrating it explicitly and auditably.

## 1. Problem, with evidence

Audited 2026-07-07 against v0.8.0 (`3784ad0`). Every persisted surface,
what it stamps, and what is enforced:

| Surface | Location | `schema_version` written | Checked at load | Migration |
|---|---|---|---|---|
| Registry records | `~/.koryph/registry.d/*.json` | `1` (`Store.Add`) | **never** | none |
| Quota config | `~/.koryph/quota/<account>.json` | `1` | only `== 0` backfill (`governor.go:98`) | none |
| Governor config | `~/.koryph/governor.json` | **none** | — | none |
| Slot leases / demand | `~/.koryph/slots/` | **none** | — | none |
| Signing vault | `~/.koryph/vault.json` | `1` | **never** | none |
| Audit log | `~/.koryph/audit.jsonl` | none (per-event `kind` only) | — | none |
| Telemetry | `~/.koryph/telemetry/*.jsonl` | **none** | — | none |
| Project config | `<root>/koryph.project.json` | `1` | **never** | none |
| Ledger runs/manifests | `<root>/.koryph/ledger/` | `2` | **never** (`LoadRun` has no gate) | none — v1→v2 already happened with no migration path |
| Epic reviews / operator state | `<root>/.koryph/` | **none** | — | none |

Three concrete failure modes follow:

1. **Silent field stripping (the sharp one).** Every save path is
   `fsx.ReadJSON` into a struct → `WriteJSONAtomic` of the struct. Go's
   `encoding/json` drops unknown fields, so an older binary that
   read-modify-writes any state file **silently deletes every field a newer
   binary added**. `registry.Store.Save`, `quota.SaveConfig`,
   `quota.SetGuardMode` (RMW under flock), `govern.SetCap` (documented as
   "PRESERVING every other field" — true only for fields its struct knows),
   vault saves: all destructive. The git-backed home makes registry damage
   *recoverable*, not *prevented*; quota/governor/slots state gets no commit
   discipline at all.
2. **Silent misreads.** A binary loading state with a newer schema than it
   understands gets zero values for renamed/moved fields and proceeds — no
   error, wrong behavior. The ledger is already at schema 2 and tolerates
   absent-token-field v1 rows only by accident of additive layout.
3. **No floor for state, only for projects.** The one enforced version
   relationship in the codebase is `engine_version` in `koryph.project.json`
   → `version.Satisfied` at `engine run` (run.go:174): a *project pins
   minimum binary* floor, on the run path only. `tui`, `quota`, `project`,
   `signing`, `governor` never version-check anything.

The additive-JSON discipline that comments rely on ("absent on every record
written before this bead") makes *reads* compatible in both directions but is
exactly what makes *writes* destructive, and nothing enforces the discipline.

## 2. Invariants (the correctness contract)

- **I1 — Write ceiling.** A binary never writes a state file whose stamped
  schema is newer than the schema it compiles for that surface. The write
  fails closed with an error naming the surface, both schema numbers, both
  engine versions (stamped vs running), and the remediation ("re-run from the
  environment that owns the newer koryph, or upgrade this one"). This is what
  makes failure mode 1 unreachable.
- **I2 — No silent misreads.** A load of state with `schema_version` greater
  than the binary's current for that surface refuses with "upgrade koryph",
  never returns zero-valued structs.
- **I3 — Older state always loads.** Every schema bump ships a migration; the
  chain v*N*→current is total for every N ever released. A binary can be
  pointed at a home or project last touched by any older release and work.
- **I4 — Migrations are explicit and auditable.** State mutates schema only
  through the migration framework: eagerly, at the first *write-access* by a
  newer binary, committed to the home's git history as its own
  conventional-commit (surfaces outside the home repo log an audit event
  instead). Reads of not-yet-migrated state migrate **in memory only** — a
  read-only command (`tui --read-only`, `doctor` without `--fix`) never
  ratchets the fleet.
- **I5 — Schema gates, not engine gates.** Interop is decided by per-surface
  schema numbers. Engine semver is recorded in the stamp for diagnostics but
  never compared: two binaries with identical schemas — 0.8.0 vs 0.8.1 —
  coexist freely. (This is why the stamp is a schema map, not one version.)
- **I6 — The discipline is machine-enforced.** Any change to any persisted
  struct's field set requires bumping that surface's schema (a no-op
  migration is fine for additive fields). CI enforces it: a golden
  fingerprint test hashes the persisted types' field sets per surface and
  fails when fields change without a schema bump + golden update.

The operational model I1+I4 produce is a deliberate **ratchet**: the first
newer binary to write migrates the state forward; older binaries in other
flake environments drop to read-only against that surface until their pin is
bumped. That is the requested semantics ("state at the same version or less
than the binary"), made visible instead of corrupting.

## 3. Design

### L1 — `internal/schemaver`: the surface registry and the stamp

A small package with no koryph dependencies beyond `fsx`/`paths`:

- `Surface` — enumerated names: `registry`, `quota`, `governor`, `slots`,
  `vault`, `project`, `ledger`, `audit`, `telemetry`.
- `Current(surface) int` — a typed central const-map. The foundation reserves
  every surface required by this epic once; sibling wiring beads do not edit
  the map.
- The **stamp is the `schema_version` in each persisted file**. There is no
  `~/.koryph/version.json`: a second aggregate truth can drift from the files
  it describes, while per-file stamps travel atomically with their data.
- `CheckWrite(surface, stamped int) error` (I1) and
  `CheckRead(surface, got int) error` (I2) — the two guards, trivially pure
  so every owning package can call them without import cycles.
- Absent/zero stamps mean "legacy" (see §5).

### Decision ledger

| Decision | Rejected alternative | Invariant / consumers |
|---|---|---|
| Per-file `schema_version` is the only stamp | Aggregate `version.json` | No dual truth; every surface bead reads and writes its own stamp |
| Typed central surface/current map, fully reserved by L1 | Runtime string registration for current versions | Unknown surfaces fail closed without sibling edits to the shared map |
| Owner-registered migration steps | Central package importing state owners | Avoids import cycles and lets surface siblings add migrations independently |
| Per-surface append-only fingerprint history in the owning package | One shared editable golden | Siblings are write-disjoint; an existing `(surface, version)` fingerprint is immutable |
| Governor reads remain fail-open for admission, writes fail closed | Wedging admission on unreadable/newer governor state | Resource governance remains available while an older binary cannot corrupt newer state |
| Slot leases, operator sentinels, and ledger overrides remain unversioned ephemeral coordination state | Treat every sidecar as durable schema | Durable state is protected without turning disposable control files into migration surfaces |
| Global signing config is a distinct durable surface | Treat it as part of `signing_vault` | Older binaries cannot strip newly added global defaults |

### L2 — Write-ceiling wiring (I1)

Each mutating path gains one guard call before its write:

- `registry.Store` — `Add`/`Save`/`SetAccount`/`put` (one choke point: `put`).
- `quota` — `SaveConfig`, `SetGuardMode`, calibration writes.
- `govern` — `SetCap`, AIMD updates, slot-lease writes. Governor state also
  gains its missing `schema_version` field (bump to 1, no-op migration from
  0/absent).
- `signing` vault — save path.
- `ledger` — `NewRun`/`SaveManifest`/`completeSlot` overwrite paths compare
  against the on-disk run's stamp.
- `project` — `Config.Save`.
- Append-only surfaces (`audit.jsonl`, telemetry) get a per-line
  `schema_version` on new events only; appends never rewrite old lines, so
  the ceiling does not apply — readers tolerate mixed-version lines (I2
  applies per line).

Each successful write stamps that file with the owning surface's current
version. No aggregate stamp or extra home commit is produced.

### L3 — Load-time gates (I2)

Symmetric guard calls on every load: `registry.Get`/`List`,
`quota.LoadConfig`, `govern` config/slot reads, vault load, `project.Load`,
`ledger.LoadRun`/`LoadManifest`/`LoadLatest`. Refusal errors carry surface,
got-vs-current, and upgrade remediation. Engine semver remains diagnostic and
is not part of the schema gate.

### L4 — Migration framework and the backfill migrations

- A migration is `func(raw map[string]any) (map[string]any, error)` —
  raw-JSON in, raw-JSON out — registered per surface as an ordered chain
  `v0→v1, v1→v2, …`. Raw maps mean a migration can move/rename fields the
  current structs no longer describe, and unknown fields survive the
  transform (no struct round-trip during migration).
- `Migrate(surface, raw) (raw, changed, error)` runs the chain from the
  file's stamped version to `Current`. Loads call it in memory (I4); the
  first ceiling-guarded write persists the result and bumps that file's stamp.
- Ships with the backfill set: `governor` 0→1, `slots` 0→1, `quota` "absent
  →1" formalized, `ledger` "absent→2" formalized (v1 rows get explicit
  zero-token semantics instead of accidental ones), `registry`/`vault`/
  `project` 1→1 no-ops registering the surfaces.

### L5 — Operator surface

- `koryph doctor` gains a `state-schema` check: scan actual state files and
  report, per surface, stamped vs
  binary schema — OK (equal), INFO (older; will migrate on next write), FAIL
  (newer; this binary is read-only for that surface, upgrade). Governor is
  the explicit exception: admission reads warn and fail open, but every write
  still fails closed. `--fix`
  eagerly migrates older surfaces (the explicit alternative to
  first-write migration).
- `koryph version --state` prints the per-file stamps beside the binary's
  schemas — the one-glance answer to "which env owns this state?".
- `koryph state migrate [--dry-run]` — explicit migration entry point for
  operators who want the commit to happen at a chosen moment (e.g. right
  after bumping every flake pin).

### L6 — CI fingerprint enforcement (I6)

Each owning package reflects over its persisted types, hashes `(field name,
JSON tag, type)` tuples, and calls the shared `VerifyFingerprint` harness
against an append-only per-surface history under that package's `testdata/`.
An existing `(surface, schema_version)` entry is immutable: regeneration
refuses to overwrite it. A shape change therefore requires incrementing
`Current(surface)` and appending the new version's fingerprint. The harness
implementation lives in `internal/schemaver`, while golden files remain
write-disjoint across surface siblings.

## 4. What we deliberately do NOT build now

- **Unknown-field preservation on round-trip** (raw-JSON shadow copies inside
  every struct save). The write ceiling makes old-binary stripping
  unreachable; preservation would add a custom-marshal layer everywhere for
  a case that can no longer occur.
- **Downgrade migrations.** The home's git history is the rollback for home
  surfaces; per-project state rolls back with the project repo. Ratchets go
  forward.
- **Cross-binary write locking beyond what exists.** flocks already guard
  the RMW hot spots (quota); the ceiling is a version gate, not a mutex.
- **Beads/dolt versioning.** `bd` owns its own schema and sync; out of scope.
- **Engine-version ceilings.** Schema numbers gate; engine semver is
  diagnostic only (I5). We explicitly do not want 0.8.0 vs 0.8.1 flake skew
  to block anything.

## 5. Compatibility

- **Bootstrap.** A file without `schema_version` is legacy for its surface.
  Loads migrate it in memory; the first versioned write stamps the file.
  Nothing breaks for read-only use of legacy state.
- **Binaries older than this design** predate the guards and will still
  strip fields if pointed at newer state — nothing can retro-protect them.
  The ratchet protects from the first release carrying L1/L2 onward; the
  operational note in docs/user-guide says so and recommends bumping all
  flake pins past that release together.
- **Additive changes after this design** bump the surface schema with a
  no-op migration (I6 forces the bump). Old-but-versioned binaries then
  refuse writes instead of stripping — the failure mode converts from silent
  corruption to a clear error.

## 6. Testing

- Per-surface unit trio: older state loads+migrates in memory; newer state
  refuses read (I2); newer stamp refuses write (I1) — table-driven across
  all registered surfaces so a new surface can't skip coverage.
- Migration chain: totality (every version 0..current reaches current),
  idempotency (`Migrate` twice ≡ once), raw-field survival (unknown keys
  present after migration).
- Ratchet integration test: fixture state stamped current+1 → every mutating
  command exits with the I1 error and identical state bytes after; fixture
  state stamped current−1 → first write migrates and stamps the file, while a
  second write performs no migration.
- Read-only never ratchets: `tui --read-only`/`doctor` (no `--fix`) against
  an older home leaves bytes identical.
- Fingerprint golden drift test (L6) wired into `make gate`.

## 7. Sequencing

1. **L1** `internal/schemaver` migration runner + all surface reservations +
   fingerprint harness scaffolding (L6 lands with L1 so the discipline exists
   before the wiring spreads). The existing guards remain.
2. **L2+L3 per surface, parallel:** registry, quota, govern, vault, ledger,
   project — each surface's guard wiring + gate + backfill migration is an
   independent bead with a clean per-package footprint (`area:registry`,
   `area:quota`, `area:govern`, `area:ledger`, …).
3. **L4** framework core ships inside L1; the per-surface backfills ride the
   L2/L3 beads.
4. **L5** doctor check + `version --state` + `state migrate` after the
   surfaces are wired.
5. Docs: user-guide chapter (operational ratchet model, flake-pin guidance)
   + developer-guide section (how to bump a schema, write a migration,
   regenerate the fingerprint golden).

The L1 seam reserves shared identifiers and APIs once. Per-surface wiring
beads add only owner-package migrations, tests, and per-surface golden files,
so they are normal write-disjoint dispatchable work.

## 8. Risks

- **Guard-call coverage drift** — a future write path skips the guard. The
  fingerprint test can't catch call-site omissions; mitigate with the
  table-driven per-surface tests (a new surface must register, and
  registration without wiring fails the trio) and a doctor WARN for state
  files whose stamp trails their surface's current despite recent writes.
- **Migration bugs corrupt state.** Migrations run on raw maps with
  idempotency tests, and home surfaces migrate inside a git commit — a bad
  migration is one `git revert` away. Per-project ledger migrations are the
  riskier path: mitigate by migrating copies (write-new-then-rename via
  existing `fsx.WriteAtomic`) and never deleting the pre-migration bytes on
  version-bump writes (keep `<file>.v<N>.bak` for one generation).
- **Ratchet surprise** — an operator's cron env silently goes read-only after
  another env upgrades. That is the designed behavior, but the error message
  and the doctor check must make the remediation obvious; the user-guide
  chapter owns the operational story.
- **Surface enumeration drift** — doctor/version must locate every durable
  file for a registered surface. The typed surface list and fixture matrix
  fail when a new durable surface lacks an enumerator.
