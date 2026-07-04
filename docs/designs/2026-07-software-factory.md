<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph, the AI software factory (2026-07-04)

Status: vision + gap analysis; epics filed per §4.
Origin: operator direction — koryph has outgrown "AI runtime orchestrator."
It now also enforces GitHub security hygiene and runs a full supply-chain
release train. The emerging identity: **the tool that takes a person from an
empty directory to built, signed, released software, with AI agents doing
the building and koryph enforcing the discipline that makes that safe.**

## 1. The three pillars (what koryph is now)

1. **Build** — the agent factory: beads planning (koryph-plan/import/replan,
   plan audit), the rolling footprint scheduler, personas/tiers across agent
   runtimes, review→rebase→gate→ff merge pipeline, cost + per-provider
   concurrency governors (AIMD, breakers).
2. **Protect** — hygiene as code: branch-protection rulesets and repo
   settings as committed JSON (`make repo-check/apply`), enforced signing
   (vault-served keys), protected paths + boundary guards, doctor drift
   detection, secret scanning posture.
3. **Ship** — the release train: conventional-commit versioning for any
   language (the artifacts_dir contract), draft-until-complete immutable
   releases, SBOM + cosign + SLSA provenance, the release bot (vault-backed
   key, three replication models), graceful bot-less fallbacks.

The **story gap**: pillars 2 and 3 grew out of pillar 1's needs, and the
docs, CLI front door, and repo bootstrap still assume you arrive with an
existing repo and know the internals. The journey from *nothing* is not yet
a product.

## 1b. Standing principle: capabilities live in the binary

Every user-facing capability ships inside the `koryph` binary — a brew
install is the complete product. Repo scripts (`scripts/ensure-*.sh`) and
make targets are development conveniences or thin wrappers at most, and any
capability that starts life as a script retires at binary parity. This rule
has now recurred three times (release-bot provisioning, release kick,
hygiene ensure scripts) — treat it as a review gate for all new features.

## 2. The journey, stage by stage

| Stage | Today | Gap |
|---|---|---|
| **Zero → repo** | `koryph project add` (existing repos only) | `koryph new` (§3.1) |
| **Idea → plan** | koryph-plan / koryph-import / plan audit | guided first-run flow; templates for PRD-shaped input |
| **Build** | complete (2026-07 scheduler/governor work) | — |
| **Hygiene** | this-repo IaC files + ensure scripts | named reusable **posture profiles** (§3.2); org-level rulesets; scanner gate presets (§3.3) |
| **Release** | full train, any language | container/registry preset (§3.4); brew tap (koryph-1hr); Pages (koryph-c6j) |
| **Operate** | board/roster/metrics/quota; IDE cockpit (koryph-ew2) | dependency lifecycle as beads (§3.5) |
| **Community/OSS** | REUSE, badges epic (koryph-8uk), scorecard workflow | folded into `koryph new` + posture profiles |

## 3. Missing features (the brainstorm, concretized)

### 3.1 `koryph new` — the front door
`koryph new <name> --lang go|node|python|rust [--org X] [--private]
[--posture <profile>] [--release] [--bot <name>]` : create the GitHub repo,
seed license + REUSE + .gitignore + README skeleton + SECURITY/CONTRIBUTING,
language starter (gate commands preset, CI workflow, version stamp file),
`bd init` + koryph.project.json (area_map starter, gate, release block),
install agents/commands/rules, apply the posture profile, wire signing
(vault flow), optionally `release setup` + bot attach — ending with:
"ready: run `koryph run --project <name>` or `/koryph-plan` your idea."
One command, one identity story. Everything it does is replayable IaC.

### 3.2 Posture profiles — hygiene beyond one repo
Generalize `.github/rulesets/*.json` + `repo-settings.json` + ensure scripts
from repo-local files into **named profiles** (`~/.koryph/postures/<name>/`
or a git repo of profiles): `koryph posture apply <profile> --repo O/R`,
`koryph posture check --all-projects`, org-ruleset support (the same API
family at org level), diff-first always. koryph's own posture becomes the
shipped `oss-solo-maintainer` example profile (1-approval + signed + checks
+ secret scanning + the release-train toggles).

### 3.3 Security scanning presets
Gate/CI presets a project can opt into per language: gitleaks (pre-commit +
CI), osv-scanner / govulncheck-equivalents per ecosystem, dependency license
allowlist. Shipped as posture-profile fragments + `koryph release setup`-style
installers, surfaced by doctor.

### 3.4 Container/registry release mode
Mode-A preset for image-shaped projects: build + push to ghcr by digest,
cosign sign the image, SBOM the image (syft), provenance subjects include
the digest. Extends the release block: `"container": {registry, image}`.

### 3.5 Dependency lifecycle as beads
dependabot config as IaC (posture fragment); an intake provider that turns
dependabot/security PRs into dispatch-shaped beads (footprint: the lockfile
area) so the loop rebases/gates/merges routine bumps under the same
discipline as any other work; auto-merge policy knob per severity.

### 3.6 Positioning + journey docs (do first — cheap, high leverage)
README, docs/index.md, architecture.md intro rewritten around the three
pillars; a new `docs/user-guide/zero-to-shipped.md` walking the whole
journey with today's real commands (project add → plan → loop → posture →
release), honestly marking the `koryph new` gap until §3.1 lands. Taglines
stay runtime-neutral (koryph-oji.4's rule).

## 4. Epics filed from this doc

- `koryph new` front door (§3.1) — depends on posture profiles for its
  --posture step; starter presets per language.
- Posture profiles (§3.2 + §3.3 fragments).
- Container release mode (§3.4).
- Dependency lifecycle (§3.5) — extends the koryph-0wv intake epic.
- Positioning + journey docs (§3.6) — immediate, loop-safe.

Existing epics already serving this story (unchanged): koryph-0wv (intake),
koryph-c6j (Pages), koryph-1hr (brew), koryph-8uk (badges/posture
advertising), koryph-fr3 (vaults), koryph-v8u (runtimes), koryph-ew2 (IDE
cockpit).
