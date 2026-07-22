<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# agenc feature analysis — what to replicate, what to reject (2026-07-20)

Analysis of [mieubrisse/agenc](https://github.com/mieubrisse/agenc) against
koryph, to decide which of its features are worth adopting. Status: **analysis
only — no plan yet.** Captured for later follow-up.

## The two products have opposite centers of gravity

**agenc** is a *human-in-the-loop command center*: you interactively fire off
disposable "missions" (full git clones) from a `Ctrl+Y` palette, `tmux`-switch
between live Claudes, let a parent Claude spawn/read child missions, and roll
lessons back into config. Its thesis is **ergonomics + aggressive automation**
(push to main, commit every turn, "just one more mission"). It optimizes for
*velocity and feel*.

**koryph** is an *autonomous governed factory*: you plan a footprinted beads
graph, `koryph run` dispatches a fleet into worktrees, and each result goes
review → rebase → your green gate → fast-forward merge, under cost governors,
account-safety, posture, and a signed release train. Its thesis is **discipline
and the process around the code**. It optimizes for *correctness, safety, and
shippability*.

Because of that, several of agenc's headline features are things koryph
**deliberately rejects** and should not adopt.

## Rejected on purpose — koryph already does these better

| agenc feature | koryph's deliberate alternative |
|---|---|
| Full git clones per mission | Worktrees off one local repo — no clone overhead; the "repo library" problem is already solved (`internal/worktree`) |
| Shared long-lived OAuth token | Per-project pinned account + fail-closed identity verification (`internal/account`) |
| Push to main, commit every turn, no gate | Footprint scheduler + review + your gate + FF merge (`internal/sched`, `internal/merge`) |
| Disposable missions coordinate via git | Central conflict-free scheduler with rolling dispatch (`internal/engine/rolling.go`) |

## Real gaps worth considering (ranked)

### 1. Per-agent secrets injection for MCP — strongest candidate

Confirmed: koryph dispatches agents with a deliberately **credential-free**
allowlisted env (`internal/account/account.go`, `baseAllow` /
`baseAllowPrefixes` — drops `GH_TOKEN`, `AWS_*`, `VAULT_TOKEN`, ambient
`SSH_AUTH_SOCK`, etc.; identity/billing/signing socket injected explicitly by
`ChildEnv`). Great for safety, but it means **koryph agents cannot use MCP
servers that need credentials**. agenc solves exactly this by injecting
`.claude/secrets.env` from 1Password.

koryph *already has* a vault layer (Proton Pass / 1Password / macOS Keychain /
encrypted file) but only serves **signing keys** with it
(`internal/signing/*`, `docs/concepts/postures.md`). Extending that vault layer
to inject scoped, per-project MCP secrets into `ChildEnv` is a real capability
unlock that reuses infra we already trust, and stays true to the thesis
(scoped, explicit, auditable — never sourced from the ambient shell).

### 2. A conversational control plane ("Adjutant")

koryph's surface is large — governors, footprints, postures, release train,
accounts, quota calibration — a steep config learning curve, exposed today only
via CLI + observe-only TUI + VS Code. agenc's "Adjutant" knows the config
surface and can launch/configure work by chat. A koryph assistant that
understands `koryph.project.json`, postures, and footprints — and can
scaffold/validate them — would lower the on-ramp without compromising any
invariant. Complements rather than conflicts.

### 3. Ad-hoc dispatch from the cockpit

koryph is plan-first: work enters via `/koryph-plan` or GitHub `intake`
(`cmd/koryph/intake.go`). There is no "give this agent this prompt *right now*"
path — the TUI (`docs/user-guide/tui.md`) is observe + nudge + drain only.
agenc's palette makes launching a one-shot instant (and addictive).

**Tension:** an ad-hoc task has no declared footprint, which is the core
scheduling invariant. A disciplined version — a "quick task" that auto-files a
footprinted bead (or runs isolated/serialized until footprint is inferred) and
still passes the gate — could capture agenc's ergonomics without breaking the
model. Worth doing, but design-sensitive.

### 4. Token-refresh thrashing under concurrency — robustness check

agenc's entire auth story exists because concurrent Claude Code sessions collide
on OAuth token refresh. koryph pins one profile per project and can run many
worktree agents off that one profile's keychain token. Worth **verifying** koryph
doesn't hit the same refresh-thrash under a wide wave — likely a latent
robustness item, not a feature to copy.

## Secondary / lower-value

- **Side-shell into a live agent's worktree** from the TUI (quick human
  intervention) — minor ergonomic add.
- **Push notifications on "needs a human decision" / stall** — koryph already
  has stall detection (`⚠ stalled`, 15-min heartbeat silence) and an Events tab;
  surfacing it as a real notification is a small step.
- **Cross-agent delegation** (agent spawns/reads child agents) —
  architecturally interesting but fights koryph's central scheduler; leave alone
  unless we want to rethink orchestration.
- **Generalized "intelligence loop"** — koryph's `modellearn` (mine ledgers →
  learn model tier, `internal/modellearn/learn.go`) is a *more disciplined*
  version of agenc's "roll lessons into config." Could be generalized, but it is
  already the better design.

## Recommendation

The top three — **MCP secrets injection**, **conversational control plane**, and
**disciplined ad-hoc dispatch** — add capability koryph genuinely lacks while
staying true to its thesis. Everything else agenc does is already covered,
covered better, or philosophically opposed.

## Open questions before planning

1. Is koryph's audience *hands-off operators* (argues against ad-hoc / side-shell)
   or *active drivers* (argues for them)?
2. Is "agents that use credentialed MCP" a use case we actually want, or a door
   we'd rather keep closed for safety?
