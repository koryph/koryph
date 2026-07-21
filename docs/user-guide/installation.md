<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Installation

koryph is a single static binary (built with `CGO_ENABLED=0`). **Running it
requires no Go toolchain, no runtime, and no libraries** — download it, put
it on your `PATH`, done. Go is only involved if you choose to build from
source.

## Prerequisites

Before installing koryph, make sure the following are in place:

| Requirement | Notes |
|---|---|
| **git** | koryph uses git worktrees; `git` must be on your `PATH` |
| **Claude CLI** (`claude`) | Install from [claude.ai/download](https://claude.ai/download) and log in with `claude auth login` |
| **bd (beads)** CLI | Install from [github.com/gastownhall/beads](https://github.com/gastownhall/beads); run `bd doctor` to confirm |
| **gh (GitHub CLI)** | Optional — needed by the GitHub-facing commands (`koryph bot`, `koryph repo`, `koryph posture`, `koryph release`) |

### Verify prerequisites

```sh
git --version       # any recent version
claude --version    # must be authenticated
bd doctor           # no errors
```

!!! tip "Let the wizard install the rest for you"
    Installing `koryph` itself (below) and having `git` on your `PATH` are
    the only prerequisites that have to happen by hand. Once `koryph` is
    installed, `koryph adopt <root>` detects `claude`, `bd`, and `gh` and
    proposes an install for anything missing — via Homebrew, apt/dnf/pacman/
    zypper, `nix profile install`, or the repo's own `flake.nix` — asking
    for your consent before running each one, showing the exact command and
    calling out any `sudo` explicitly. Manually working through the rest of
    the table above is optional if you're about to run `adopt`; see
    [koryph adopt](adopt.md).

---

## Install `koryph`

### Option A — Homebrew (recommended; macOS only)

```sh
brew install koryph/tap/koryph
```

This installs the latest release binary from the
[koryph/homebrew-tap](https://github.com/koryph/homebrew-tap) cask. No Go
toolchain is required. The tap ships a **cask**, and Homebrew casks are
macOS-only — on Linux (including Linuxbrew), use Option B or C below.
Upgrades use the standard brew workflow:

```sh
brew upgrade koryph/tap/koryph
```

> **macOS quarantine note:** koryph's binaries are built with
> `CGO_ENABLED=0` and signed with keyless cosign (Sigstore), but are not
> Apple-notarised. The cask's post-install hook automatically removes the
> Gatekeeper quarantine flag so `koryph` runs without a security prompt:
>
> ```sh
> # run automatically by the cask — no action needed
> xattr -dr com.apple.quarantine /opt/homebrew/bin/koryph
> ```
>
> If you install via the tarball instead (Option B), run that command
> manually once after placing the binary on your `PATH`.

### Option B — prebuilt binary (no Homebrew, no Go required)

Every release ships signed binaries for macOS and Linux (amd64/arm64) with
checksums, SBOMs, and SLSA provenance:

```sh
# pick your platform: darwin|linux x amd64|arm64
curl -LO https://github.com/koryph/koryph/releases/latest/download/koryph_<version>_darwin_arm64.tar.gz
tar -xzf koryph_<version>_darwin_arm64.tar.gz
install -m 0755 koryph ~/.local/bin/   # or any directory on your PATH
# macOS only — remove Gatekeeper quarantine flag
xattr -dr com.apple.quarantine ~/.local/bin/koryph
```

To verify the download against the release's checksums, signature, and
provenance, see [Supply-chain verification](supply-chain.md).

### Option C — build from source (requires a Go toolchain)

```sh
go install github.com/koryph/koryph/cmd/koryph@latest
```


This fetches the module, compiles it, and places the `koryph` binary in
`$(go env GOPATH)/bin` (typically `~/go/bin`). Any Go **1.21 or later**
works: `go.mod` pins the exact toolchain (currently 1.26.x) and modern Go
downloads it automatically on first build.

### Verify the installation

```sh
koryph version
```

Expected output:

```
koryph <version>
```

The exact version string reflects the engine version baked in at build time.

---

## Machine state

koryph keeps all central state in a single directory:

```
~/.koryph/
├── registry.d/      # one JSON record per managed project
├── quota/           # per-account governor snapshots
├── slots/           # machine-global concurrency governor leases
│   └── demand/      # per-project demand heartbeats
├── governor.json    # machine-wide concurrency cap config
├── audit.jsonl      # append-only account/dispatch audit trail
└── runs.jsonl       # cross-project run index
```

The directory is initialised automatically on first use — there is nothing to
create manually.

### Override the state directory

Set `KORYPH_HOME` to redirect all state to a different path. This is
useful for test fixtures or multiple isolated environments:

```sh
export KORYPH_HOME=/tmp/my-koryph-home
koryph project list     # uses /tmp/my-koryph-home
```

Per-project run logs are stored inside each project's own repository, under
`.plan-logs/koryph/`, and are not affected by `KORYPH_HOME`.

### Environment variables

koryph reads a small set of environment variables. They are also listed in the
`ENVIRONMENT` section of `koryph help`, and `koryph doctor` reports on them.

| Variable | Purpose |
|---|---|
| `KORYPH_HOME` | Central registry + governor root (default `~/.koryph`) |
| `KORYPH_BD_BIN` | Path to the `bd` (beads) binary (default: `bd` on `PATH`) |
| `KORYPH_GH_BIN` | Path to the `gh` (GitHub CLI) binary (default: `gh` on `PATH`) |
| `KORYPH_NO_NPX` | Set to any value to disable `npx`-based tool fallbacks (e.g. `ccusage`) |

---

## Getting help

Every command is self-documenting. There is no wrong door:

```sh
koryph                       # global command listing
koryph help                  # same global listing
koryph <command> -h          # one command's purpose, synopsis, and flags
koryph help <command>        # identical to `koryph <command> -h`
koryph <parent>              # a parent (project, signing, ...) lists its subcommands
koryph <parent> -h           # same subcommand listing
```

For example, `koryph project -h`, `koryph signing help`, and
`koryph help project add` all resolve to the right help without an error.

---

## Shell completions

koryph ships first-class tab completion for **bash** and **zsh**. The completion
scripts are thin wrappers: every keypress delegates to the hidden
`koryph __complete` resolver, so the binary is the single source of truth and the
scripts never go stale as commands and flags evolve. Completion covers top-level
commands, each command's subcommands and flags, and dynamic values where they are
cheap and read-only — `--project` completes your registered project ids,
`--model`/`--default-model` complete `haiku|sonnet|opus|fable`, and `--shell`
completes `bash|zsh`.

Run `koryph completion -h` to discover the subcommands.

### Try it in the current shell

Source the script into your running shell to try it immediately:

```sh
# bash
source <(koryph completion bash)

# zsh
source <(koryph completion zsh)
```

Then type `koryph <TAB>` to complete subcommands, or `koryph run --<TAB>` for flags.

### Install it permanently

`koryph completion install` writes the script to the standard user-level location
for your shell. With no `--shell` it detects the shell from `$SHELL`; pass
`--shell bash` or `--shell zsh` to be explicit. It is idempotent (re-running
overwrites the same path) and never edits your shell rc files — it prints the path
it wrote and any activation step you still need.

```sh
koryph completion install                 # detect from $SHELL
koryph completion install --shell bash
koryph completion install --shell zsh
```

- **bash** installs to
  `${XDG_DATA_HOME:-~/.local/share}/bash-completion/completions/koryph`.
  With [bash-completion](https://github.com/scop/bash-completion) enabled, new
  shells pick it up automatically.
- **zsh** installs to `~/.koryph/completions/_koryph`. If that directory is not
  already on your `fpath`, add the snippet the command prints to your `~/.zshrc`:

  ```sh
  fpath=(~/.koryph/completions $fpath)
  autoload -U compinit && compinit
  ```

Start a new shell after installing (or source the script as shown above).

---

## Troubleshooting

### `koryph: command not found`

The directory holding the binary is not on your `PATH`. For a prebuilt
binary, that is wherever you installed it (e.g. `~/.local/bin`); for a
source build it is `$(go env GOPATH)/bin`:

```sh
export PATH="$HOME/.local/bin:$PATH"          # prebuilt binary
export PATH="$(go env GOPATH)/bin:$PATH"      # go install
```

Add the matching line to your shell's profile file (`.zshrc`, `.bashrc`,
etc.) so it persists across sessions.

### Wrong `koryph` binary is picked up

If `which koryph` points to an unexpected location, you may have an older
build shadowing the freshly installed one. Check all copies:

```sh
which -a koryph
```

Remove or rename the stale binary, then confirm with `koryph version`.

### Multiple `bd` binaries on `PATH`

`bd` may resolve to a different binary (e.g. `bd` from another tool) if your
`PATH` orders that directory first. Check:

```sh
which -a bd
bd --version        # should print a beads version string
```

If the wrong `bd` is picked up, reorder your `PATH` so the beads install
directory comes first, or use an absolute path.

### Claude CLI not authenticated

If `claude --version` works but `koryph run` fails with an auth error, the
CLI session has expired. Re-authenticate:

```sh
claude auth login
```

---

## Next steps

Once `koryph version` runs cleanly, continue to the
[Quickstart](quickstart.md) to register your first project and launch a wave.
