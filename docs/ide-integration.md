<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Using koryph from VS Code (and other IDEs)

Three questions this answers: how to access koryph from an IDE, how the
existing `CLAUDE_CONFIG_DIR` / `code .` account isolation relates to it, and
how commands issued from the Claude Code plugin interoperate with koryph
without crossing accounts.

## 0. Editor configuration

The repo ships two config files so that editor formatting and the pre-commit
hooks always agree — open the repo and you should never see spurious diffs.

### `.editorconfig`

Sets per-file-type defaults that any [EditorConfig](https://editorconfig.org)-aware editor picks up automatically:

| File type | Indent | Line ending | Final newline | Trim trailing whitespace |
|---|---|---|---|---|
| `*.go` | tab (4-wide) | LF | yes | yes |
| `*.md`, `*.yaml`, `*.yml`, `*.json`, `*.toml` | 2-space | LF | yes | yes |
| `Makefile` | tab (4-wide) | LF | yes | yes |
| everything else | (inherited) | LF | yes | yes |

These rules mirror the `trailing-whitespace`, `end-of-file-fixer`,
`mixed-line-ending`, and `gofmt` pre-commit hooks exactly.

### `.vscode/settings.json`

Applies the same rules workspace-wide for VS Code:

- `"files.eol": "\n"` — LF for all new files
- `"editor.formatOnSave": true` — auto-format on every save
- `"[go].editor.defaultFormatter": "golang.go"` — use the Go extension's `gofmt` wrapper
- `"[go].editor.codeActionsOnSave"` → `source.organizeImports: "explicit"` — organise imports on save (matches `goimports` behaviour)
- `"files.trimTrailingWhitespace"` / `"files.insertFinalNewline"` — editor-side enforcement (EditorConfig also handles this, but belt-and-suspenders)

### `.vscode/extensions.json`

Recommends four extensions (VS Code prompts to install on first open):

| Extension ID | Purpose |
|---|---|
| `golang.go` | Go language support + `gopls`, `gofmt`, test runner |
| `EditorConfig.EditorConfig` | reads `.editorconfig` so the settings above are applied |
| `bierner.markdown-mermaid` | renders the Mermaid diagrams in `docs/architecture.md` in the preview pane |
| `redhat.vscode-yaml` | schema validation for `koryph.project.json` and workflow YAML |

## 1. Accessing koryph from an IDE

`koryph` is a plain CLI installed on PATH (`go install ./cmd/koryph` →
`~/bin/koryph`). There is nothing IDE-specific to install:

- **Any integrated terminal, any window, any account**: `koryph board`,
  `koryph status --project <id>`, `koryph tail`, `koryph nudge`,
  `koryph metrics`, `koryph onboard <root>` all work from anywhere —
  the registry and audit log live centrally in `~/.koryph`, and run
  ledgers are plain files in each project's `.plan-logs/`. None of these
  invoke Claude, so the window's account is irrelevant to them.
- **Working on koryph itself**: open
  `~/src/github.com/koryph/koryph` as its own workspace root (the Go
  module is at the root, so gopls just works; opening it as a stray folder
  inside another workspace produces "not included in your workspace"
  warnings). The repo carries the personal-account `.envrc` managed block —
  `direnv allow` once, then `cd` + `code .` opens it in the personal VS Code
  instance like any other personal repo.
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
