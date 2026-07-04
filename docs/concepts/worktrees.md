<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Safety: worktrees, protected paths, and the green gate

*This page expands the [Concepts overview](index.md). See
[Running waves](../user-guide/running-waves.md) and
[Projects & accounts](../user-guide/projects-and-accounts.md) for the commands
that operate it.*

## The idea

Autonomous agents make mistakes. The design question is not *how do we prevent
every mistake* but *how do we make a mistake cheap to discard and impossible to
merge*. koryph answers with three layers: isolation, an unconditional merge
gate, and a small set of lines no automated merge may cross.

**Isolation.** Each agent works in its **own git worktree** — a separate
checkout on its own branch, under a sibling directory such as
`<repo>-worktrees/agent-<bead>`. Your primary working copy is never touched. A
misbehaving agent can be killed and its worktree deleted with no cleanup and no
trace in your tree.

**The green gate.** A finished branch does not merge because it looks done. It
merges only after passing, in order: review (findings block until addressed),
a rebase onto current `main`, and then the project's **own** build/test/lint
commands. Only a gate-green, up-to-date branch fast-forwards onto `main`.

**Protected paths.** Some files govern the factory itself — CI workflows,
hooks, policy. An automated merge that touches them is *refused*, gate-green or
not, so a human lands those changes deliberately.

## In koryph

koryph's own green gate is the set of commands every branch must pass:

```bash
gofmt -l .        # must print nothing
go build ./...
go vet ./...
go test ./...
make lint
make reuse        # SPDX/REUSE compliance
```

The projects `make gate` target runs them together. They execute in the
agent's worktree, **after** the rebase and **before** the merge, so the gate
reflects the code as it will actually land on `main`.

The protected paths for this project — merges touching them are refused —
include `.claude/`, `.beads/`, `hooks/`, `agents/`, `.github/`,
`koryph.project.json`, `Makefile`, `.pre-commit-config.yaml`, `.envrc`, and
`LICENSE`. Every commit is additionally required to be signed and
DCO-signed-off; an unsigned-off commit is rejected by the merge gate and by CI.

## The failure mode it prevents

Without isolation, one runaway agent corrupts the shared checkout and every
other agent inherits the damage. Without the gate, "the diff looks fine" quietly
becomes a broken `main` that the *next* agent rebases onto, multiplying the
breakage across the fleet. Without protected paths, an agent could edit the very
hooks and workflows that enforce all the other rules — disabling the guardrails
as a side effect of a feature branch. The three layers together mean a bad
branch is discarded, not merged, and the machinery that keeps the fleet honest
is never modified by the fleet itself.

## Operate it

- [Running waves](../user-guide/running-waves.md) — merge policies and the gate.
- [Projects & accounts](../user-guide/projects-and-accounts.md) — configuring
  `gate` commands and `protected_paths` per project.
- [Signing](../user-guide/signing.md) — enabling the required commit signing.
- Feeds directly from [rolling dispatch](rolling-dispatch.md): each freed slot
  is one worktree taken down and another brought up.
