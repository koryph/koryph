<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Using koryph from VS Code (and other IDEs)

This chapter is about **using koryph from your IDE** while it manages your
projects: how to access it, the upcoming VS Code extension, how the existing
`CLAUDE_CONFIG_DIR` / `code .` account isolation relates to it, and how
commands issued from the Claude Code plugin interoperate with koryph without
crossing accounts.

(Contributing to koryph itself? Editor formatting, workspace setup, and the
extension's build/test live in the developer guide:
[Developing koryph: editor setup](developer-guide/ide-setup.md).)

## 0. The VS Code extension (in development)

A first-class **koryph agent cockpit for VS Code** is actively being built
(epic `koryph-ew2`, source in `ide/vscode/`). Already working today:

- a **tree view of agent threads** — live per-bead lifecycle for the
  current run (dispatched / reviewing / merging / blocked) without leaving
  the editor;
- a **quota status bar** — the account's subscription-window burn at a
  glance, the same numbers `koryph board` reports.

Planned: dispatch/drain controls, review-finding surfacing, and per-slot
log tailing. It is not yet published to the marketplace — early adopters
can build it from source (`make ext-build` in the koryph repo). Until it
ships, everything below works with any editor and no extension at all.

## 1. Accessing koryph from an IDE

`koryph` is a plain CLI installed on PATH (`go install ./cmd/koryph` →
`~/bin/koryph`). There is nothing IDE-specific to install:

- **Any integrated terminal, any window, any account**: `koryph board`,
  `koryph status --project <id>`, `koryph tail`, `koryph nudge`,
  `koryph metrics`, `koryph onboard <root>` all work from anywhere —
  the registry and audit log live centrally in `~/.koryph`, and run
  ledgers are plain files in each project's `.plan-logs/`. None of these
  invoke Claude, so the window's account is irrelevant to them.
- **From the Claude Code plugin**: ask the session's Claude to run `koryph
  …` like any other CLI (it is just a Bash invocation). A project-local
  slash command wrapping common invocations (`/koryph` → wave preview,
  status, merge) is part of onboarding (bead koryph-um6 / koryph-9aw).

## 2. The existing `code .` account isolation — unchanged and still required

The two-layer scheme from CLAUDE-ACCOUNTS.md stays exactly as it is:

1. **direnv** sets/unsets `CLAUDE_CONFIG_DIR` per repo (`personal` = unset,
   `work` = a dedicated config dir, e.g. `~/.claude-work`).
2. The **`code()` zsh wrapper** gives each account its own VS Code instance
   (`--user-data-dir`) so the extension host captures the right env.

That scheme governs **interactive** Claude sessions — the plugin chat in a
window bills to that window's account, exactly as before. Koryph neither
replaces nor depends on it; koryph adds a third, independent layer for
**dispatched** work (below). Keep using the managed `.envrc` blocks; the
onboarding inventory (`koryph onboard <root>`) verifies them and flags the
deprecated explicit-`~/.claude` style.

## 3. How plugin-issued commands interoperate with koryph accounts

The rule of thumb:

| Work | Account used | Mechanism |
|---|---|---|
| Interactive plugin chat editing a project | the window's account | direnv + per-account VS Code instance (unchanged) |
| Agents dispatched by `koryph run` / `merge` / `review` | the **registry's** account for that project — regardless of which window ran the command | dispatch scrubs ambient env and rebuilds it |
| `koryph board/status/tail/nudge/metrics/onboard` | none (no Claude invoked) | plain file/git/bd reads |
| `koryph batch run` | Anthropic API key (per-token) | explicit purpose-named key env var + `--yes`; never ambient |

The load-bearing detail: **the ambient environment of whatever
terminal/window you run `koryph` from never reaches a dispatched agent.**
The dispatcher builds each agent's environment from a **credential-free
allowlist** (not a denylist) — only known-safe operational variables pass
through (`PATH`, `HOME`, locale, Go/toolchain caches, `KORYPH_*`); everything
else, including `GH_TOKEN`, `VAULT_TOKEN`, `AWS_*`/`AZURE_*`/`GOOGLE_*`, and the
operator's ambient `SSH_AUTH_SOCK`, is dropped by omission so a secret you never
named cannot leak. It then re-injects only what the agent legitimately needs:
the project's registry-declared `CLAUDE_CONFIG_DIR` (or unset for personal), the
API key when in api-key billing, and the **scoped signing socket** (holds only
the commit-signing key — see [Signing](user-guide/signing.md#two-agents-operator-vs-dispatched)).
A project that genuinely needs an extra variable opts in via the registry
record's `env_passthrough` list. Before anything launches, the dispatcher
**verifies the logged-in identity**
(`<config-dir>/.claude.json → oauthAccount.emailAddress`) against the
registry's `expected_identity`. Mismatch or unreadable identity = the dispatch
is refused, loudly.

Concretely: running `koryph run --project project-a --once` from a
**work-account** VS Code window still dispatches every agent on the
**personal** subscription (project-a's registry record says personal), and
running a work project's wave from a personal window uses its registered work config dir.
If the required account isn't logged in, koryph fails closed instead of
silently using the wrong one. Every dispatch records
`account_profile / claude_config_dir / verified_identity / billing_mode` in
the run ledger and appends to `~/.koryph/audit.jsonl`, so "which account
did that run use?" is always answerable after the fact.

Two edges to know:

- **Quota and review one-shots** also construct their env from the registry
  (usage is measured against the project's account, not the window's).
- **Sessions spawned by koryph are separate from the plugin's session
  list per account**: a dispatched agent's transcript lives under the
  *registry account's* config dir (`<config-dir>/projects/...`), named
  `koryph/<project>/<bead>/a<attempt>` — visible in the session picker of
  a window running on that same account (and via `claude agents`), not in
  windows on the other account.

## 4. Terminal cockpit vs VS Code cockpit

Koryph ships two live-data cockpits that share the same `internal/cockpit`
data layer. They are complementary, not competing — pick the one that fits
the context:

| | Terminal cockpit (`koryph tui`) | VS Code cockpit (extension koryph-ew2) |
|---|---|---|
| **Access** | Any terminal, SSH, headless | VS Code window |
| **Zero-install** | Yes — ships with `koryph` binary | Must build from source until marketplace publication |
| **Tabs** | Threads, Burndown, Events, Efficiency, Queue, Detail | Thread tree view (more panels planned) |
| **Write actions** | Nudge (`n`), Drain (`D`) | Dispatch/drain controls (planned) |
| **Read-only mode** | `--read-only` flag | N/A (not yet implemented) |
| **SSH / headless** | Full support | Requires VS Code remote extension |
| **Best for** | Monitoring over SSH, CI dashboards, full-lifecycle ops | Staying in the editor while agents run |

### Shared data layer

Both cockpits are backed by `internal/cockpit` (`cockpit.Provider` /
`cockpit.Snapshot`). The provider reads from:

- The project's **run ledger** (`.plan-logs/<project>/<run>/`) for slot
  state, events, and cost data.
- The **beads DB** (`refs/dolt/data`) for queue topology, epic membership,
  and velocity history.
- The **governor state file** for pool caps, AIMD state, and breaker status.
- The **quota ledger** for window spend and ceiling.

Because both cockpits call the same `Refresh()` and `BeadDetail()` methods,
a number visible in the terminal tab is the same number the VS Code panel
would show for the same project — there is no separate sync path.

### Choosing between them

- **Ops from a remote machine** (SSH, paired session, CI observer): use
  `koryph tui`. It has no GUI dependency and the full six-tab surface.
- **Staying in the editor**: use the VS Code extension. It keeps bead status
  visible in the editor sidebar without a separate terminal window.
- **Both at once**: they coexist without conflict. Both are read-mostly;
  write actions (nudge/drain) are serialised by the ledger layer so a drain
  from the TUI and a drain from the extension produce one sentinel, not two.

See [tui.md](user-guide/tui.md) for the full terminal cockpit reference.
