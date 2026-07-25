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
