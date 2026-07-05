<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Rebase conflict

Rebasing `agent/koryph-9af.5` onto `main` hit a conflict and was aborted; the worktree is unchanged.
Resolve by rebasing manually, then retry.

```
Auto-merging docs/reference/cli.md
Auto-merging internal/cockpit/ledger_provider.go
Auto-merging internal/cockpit/snapshot.go
CONFLICT (content): Merge conflict in internal/cockpit/snapshot.go
Rebasing (1/1)error: could not apply 74997b5... feat(tui): events tab with nudge/drain actions (koryph-9af.5)
hint: Resolve all conflicts manually, mark them as resolved with
hint: "git add/rm <conflicted_files>", then run "git rebase --continue".
hint: You can instead skip this commit: run "git rebase --skip".
hint: To abort and get back to the state before "git rebase", run "git rebase --abort".
hint: Disable this message with "git config set advice.mergeConflict false"
Could not apply 74997b5... # feat(tui): events tab with nudge/drain actions (koryph-9af.5)
```
