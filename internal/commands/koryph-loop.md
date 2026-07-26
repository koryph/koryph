---
name: koryph-loop
description: Start the binary-native autonomous loop supervisor
---
<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

Run the native supervisor directly:

```bash
koryph loop --auto-merge --review $ARGUMENTS
```

It observes ready and recoverable work before creating an engine run. Idle
observation launches no model process and creates no empty run ledger.
Structured alerts and durable supervisor state live under `.koryph/loop/`.

Control it with the binary:

```bash
koryph loop status
koryph loop inject <bead-id>
koryph loop drain
koryph loop stop
```

Autonomous mode has no `--allow-unvalidated` switch. A bounded canary uses
`--canary-cohort=<comma-separated-ids> --max=<target-width>`; admission is
restricted to that immutable cohort, starts at width 2, and widens only after
five consecutive fully authenticated merged outcomes under normal host
pressure. Canary mode requires both `--review=true` and `--auto-merge=true`;
its required hard stops drain immediately and open the durable supervisor
circuit.

When a terminal failed generation was produced by an older binary and the
repair also changed the authenticated contract or execution policy, start the
same cohort once with `--supersede-failed-canary-policy`. Koryph still requires
a clean strict-descendant installed commit, the exact unchanged cohort
envelope, and authenticated failed state/report evidence. It creates an
immutable archive recording both policy identities before releasing the fixed
state paths.

If completed members make the old cohort impossible to replay, replace only
its membership once with `--supersede-failed-canary-cohort`. Koryph requires
the same authenticated failed evidence and clean strict-descendant binary,
keeps target width, inactivity limit, and report path fixed, and records both
cohort memberships and digests in the immutable archive. Policy drift still
requires its separate switch. Neither switch can supersede a live, passing,
same-commit, fixed-envelope-drifted, or corrupt generation.
