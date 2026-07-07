<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Fixture: designs/-relative link (reproduces ew2.8 regression shape)

This file simulates a user-guide page that links into docs/designs/.
It passes a local zensical build (docs/designs/ present) but breaks CI
(docs.yml removes docs/designs/ before zensical build --strict).

See [the TUI design](../designs/2026-07-scaffolding.md) for background.

Also see [another design](../designs/2026-07-software-factory.md#section).
