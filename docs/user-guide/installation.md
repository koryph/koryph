<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Installation

## Prerequisites

Before installing koryph, make sure the following are in place:

| Requirement | Notes |
|---|---|
| **Go 1.26+** | `go version` must report `go1.26` or later |
| **git** | koryph uses git worktrees; `git` must be on your `PATH` |
| **Claude CLI** (`claude`) | Install from [claude.ai/download](https://claude.ai/download) and log in with `claude auth login` |
| **bd (beads)** CLI | Install from [github.com/gastownhall/beads](https://github.com/gastownhall/beads); run `bd doctor` to confirm |

### Verify prerequisites

```sh
go version          # go1.26.x or later
git --version       # any recent version
claude --version    # must be authenticated
bd doctor           # no errors
```

---

## Install `koryph`

```sh
go install github.com/koryph/koryph/cmd/koryph@latest
```

This fetches the latest release from the module proxy, compiles it, and
places the `koryph` binary in `$(go env GOPATH)/bin` (typically
`~/go/bin`).

### Verify the installation

```sh
koryph version
```

Expected output:

```
koryph 0.2.0
```

The exact version string reflects the engine version baked in at build time.

---

## Machine state

koryph keeps all central state in a single directory:

```
~/.koryph/
├── registry.d/      # one JSON record per managed project
├── quota/           # per-account governor snapshots
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

---

## Troubleshooting

### `koryph: command not found`

`koryph` is installed to `$(go env GOPATH)/bin`. Add it to your `PATH`:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
```

Add that line to your shell's profile file (`.zshrc`, `.bashrc`, etc.) so
it persists across sessions.

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
