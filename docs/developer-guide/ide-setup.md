<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Developing koryph: editor setup

This chapter is for **contributors working on koryph itself**. If you want
to *use* koryph from your IDE while it builds your projects, see
[IDE integration](../ide-integration.md) in the user guide instead.

## Workspace

Open the repo as its own workspace root (the Go module is at the root, so
`gopls` just works; opening it as a stray folder inside another workspace
produces "not included in your workspace" warnings). The repo carries a
managed `.envrc` block — `direnv allow` once, then `cd` + `code .` opens it
under the right account like any other repo.

## Editor configuration

The repo ships config files so that editor formatting and the pre-commit
hooks always agree — open the repo and you should never see spurious diffs.

### `.editorconfig`

Sets per-file-type defaults that any
[EditorConfig](https://editorconfig.org)-aware editor picks up automatically:

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

## The VS Code extension source

koryph's own VS Code extension lives in [`ide/vscode/`](https://github.com/koryph/koryph/tree/main/ide/vscode)
(see the user guide's [IDE integration](../ide-integration.md) chapter for
what it does). Build and test it with:

```sh
make ext-build    # npm ci + bundle (skipped with a notice if npm is absent)
make ext-test     # unit test suite
```

CI enforces both on every PR.
