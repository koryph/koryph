<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# People: accounts, identity, and personas

*This page expands the [Concepts overview](index.md). See
[Projects & accounts](../user-guide/projects-and-accounts.md) for the commands
that operate it.*

## The idea

When agents run on your machine, two questions must have unambiguous answers
*before* any work starts: **who** are they running as, and **what kind of
worker** is each one. Getting the first wrong bills the wrong account or leaks
across a personal/work boundary. Getting the second wrong sends a frontier
reasoning task to a light model, or wastes a frontier model on a mechanical
edit.

koryph pins the **account** each project's agents run under, and verifies that
identity **fail-closed** before dispatch — agents never simply inherit whatever
happens to be logged in. Separately, **personas** describe the role a task needs
(implementer, reviewer, architect, docs author, …) and carry a **model tier**
rather than a hard-coded model name, so the same persona maps to the right model
on whatever runtime is executing it.

## In koryph

Each project binds an account triple — profile, identity email, and reason:

```bash
koryph project set-account --project koryph \
  --account personal --identity cody@mccain.family \
  --reason "solo maintainer"
```

Before it dispatches, koryph reads the resolved Claude profile, extracts
`oauthAccount.emailAddress`, and compares it case-insensitively to the project's
expected identity. **Any** discrepancy — file missing, unparseable, empty, or a
mismatch — refuses dispatch immediately with no fallback. The fix is to log in
as the right account (`claude auth login`) or to change the binding
deliberately.

Personas live as markdown files under `.claude/agents/` (e.g.
`koryph-implementer`, `koryph-feature-docs-author`). Each stage resolves to a
persona, and each persona carries a **tier** — frontier / standard / light —
which the runtime maps to its own concrete models (`opus`, `sonnet`, `haiku`
for Claude Code today). Because the bead names a tier and not a model, the same
plan runs unchanged when the model lineup shifts, and koryph stays
runtime-neutral: Claude Code now, other agent CLIs through the same adapter seam
later.

## The failure mode it prevents

The two silent cross-wirings. Without fail-closed identity, an agent dispatched
while the wrong profile is active bills the wrong subscription — or worse, does
work under an identity that has no business touching the repo — and nothing
warns you until the ledger or the commit log shows it. Without tier-based
personas, model names get hard-coded into plans, so every model rename or
runtime swap becomes a migration, and tasks routinely run one tier too high
(wasting spend) or too low (producing work that fails review). Pinning identity
and abstracting the model to a tier removes both classes of error before the
first agent starts.

## Operate it

- [Projects & accounts](../user-guide/projects-and-accounts.md) — the account
  model, the registry record, and identity binding.
- Identity gates [rolling dispatch](rolling-dispatch.md); tiers feed the
  [governors](governors.md), since model choice drives spend.
