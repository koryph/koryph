<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# The fence: ejectability and the opinionation boundary

*This page expands the [Concepts overview](index.md). Unlike the other
concepts, this one is a design constraint rather than a command — the
[Installation](../user-guide/installation.md) guide is where you meet its
binary-first consequence.*

## The idea

Every automation tool faces the same temptation: to make itself
load-bearing. To store state in a private format, generate config only it can
read, wrap your repo in a framework — so that leaving means a migration. koryph
refuses that on principle, and two rules draw the fence.

**The opinionation boundary.** koryph holds strong opinions about *process* —
signing, gates, provenance, footprinted planning — and *none* about your
application. No frameworks, no imposed directory layout, no dependencies added
to your project. It shapes how work is dispatched and verified, never what the
work is.

**Ejectability.** Everything koryph produces is standard, inspectable material:
an ordinary git repo, GitHub-native settings, plain workflow YAML, standard
release assets. Delete koryph and nothing breaks. You lose the factory, not the
product.

A third, operational rule follows the same spirit — **binary-first**:
capabilities live in the installed `koryph` binary. One binary is the complete
product; there is no repo to clone, no script kit to keep in sync, no hidden
runtime the binary shells out to.

## In koryph

The boundary is visible in what koryph *writes* to your project. A posture
becomes GitHub branch-protection rulesets you can read in the repo settings UI.
A release pipeline becomes ordinary workflow YAML under `.github/workflows/`
that runs with or without koryph installed. Signing becomes standard git config
and an SSH-agent key. Beads live in a database that travels with your git
remote, exported alongside as plain `.beads/issues.jsonl`. None of it is a
koryph-proprietary artifact.

The test is concrete: uninstall the binary and delete every koryph config, and
your project still builds, still has its branch protections, still releases from
its workflows, still carries its full git history. What you lose is the
*orchestration* — the thing that dispatched agents and merged their branches —
not any of the material they produced.

## The failure mode it prevents

Lock-in, and the fear of it. A tool that makes itself unremovable forces a bet:
adopt it and hope it stays maintained, because leaving means rebuilding. That
fear is a rational reason *not* to adopt automation at all. By producing only
standard, inspectable artifacts and keeping every opinion on the process side of
the fence, koryph makes adoption reversible — which is precisely what makes it
safe to adopt. The binary-first rule closes the last gap: nothing depends on a
sprawl of scripts you have to vendor and version, so there is no half-installed
state to get stuck in either.

## Operate it

- [Installation](../user-guide/installation.md) — the single-binary install
  that binary-first implies.
- Every other concept respects the fence: [postures](postures.md) emit native
  GitHub settings, the [release train](release-train.md) emits standard assets,
  and [beads](beads.md) travel as an ordinary git-backed database.
