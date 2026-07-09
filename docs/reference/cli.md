<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

<!--
  DO NOT EDIT MANUALLY — this file is auto-generated from the command registry.

  Re-generate:  koryph __docgen > docs/reference/cli.md
  Drift check:  go test ./cmd/koryph/ -run TestCLIRefDrift
-->

# CLI Reference

koryph — central multi-project orchestrator for autonomous Claude Code agents.

## Quick index

| Command | Summary |
|---------|----------|
| [`koryph init`](#koryph-init) | create ~/.koryph, verify tools on PATH, print next steps |
| [`koryph project`](#koryph-project) | onboard and manage registered projects |
| ↳ [`koryph project add`](#koryph-project-add) | register a project (inspect + register + scaffold adapter + install assets) |
| ↳ [`koryph project install-assets`](#koryph-project-install-assets) | (re)install koryph assets — agents, commands, and rules |
| ↳ [`koryph project list`](#koryph-project-list) | list managed projects (id, account, status, root) |
| ↳ [`koryph project show`](#koryph-project-show) | print one project record as JSON |
| ↳ [`koryph project set-account`](#koryph-project-set-account) | change a project's account (audited; resets validation) |
| [`koryph validate`](#koryph-validate) | run the pre-dispatch gate |
| [`koryph run`](#koryph-run) | execute one engine run over a project |
| [`koryph intake`](#koryph-intake) | poll labeled GitHub issues into planning beads |
| [`koryph nudge`](#koryph-nudge) | append an operator note to a phase INBOX |
| [`koryph stop`](#koryph-stop) | stop an agent (or every agent with --all) |
| [`koryph drain`](#koryph-drain) | gracefully wind down a run: finish active slots, dispatch nothing new |
| [`koryph resize`](#koryph-resize) | live width override for a running loop |
| [`koryph merge`](#koryph-merge) | land a finished agent branch |
| [`koryph land`](#koryph-land) | land an engine-opened PR fast-forward-only |
| [`koryph review-pr`](#koryph-review-pr) | analyze another author's PR |
| [`koryph pr-sync`](#koryph-pr-sync) | reconcile pr-opened beads against live PR state |
| [`koryph bot`](#koryph-bot) | provision and manage koryph GitHub App bots |
| ↳ [`koryph bot create`](#koryph-bot-create) | create a GitHub App via the manifest flow (one browser click) |
| ↳ [`koryph bot install`](#koryph-bot-install) | print/open the installation page for a provisioned bot |
| ↳ [`koryph bot attach`](#koryph-bot-attach) | wire a repo to a bot: set secrets and enable Actions PR-approval toggle |
| ↳ [`koryph bot list`](#koryph-bot-list) | list provisioned bots in ~/.koryph/bots/ |
| ↳ [`koryph bot check`](#koryph-bot-check) | run the bot validator chain (JWT, installation, secrets, Actions toggle) |
| ↳ [`koryph bot vault-migrate`](#koryph-bot-vault-migrate) | move a plaintext bot private key into a vault or encrypted file |
| [`koryph signing`](#koryph-signing) | configure and operate vault-backed commit signing |
| ↳ [`koryph signing setup`](#koryph-signing-setup) | write the signing policy into the adapter |
| ↳ [`koryph signing enable`](#koryph-signing-enable) | load the key + apply repo git config |
| ↳ [`koryph signing keygen`](#koryph-signing-keygen) | generate a passphrase-protected SSH signing key (no-vault path) |
| ↳ [`koryph signing status`](#koryph-signing-status) | mode/provider/agent-ready summary |
| ↳ [`koryph signing verify`](#koryph-signing-verify) | verify branch commit signatures |
| [`koryph sign`](#koryph-sign) | cosign sign-blob an artifact |
| ↳ [`koryph sign blob`](#koryph-sign-blob) | sign a file via the vault key |
| [`koryph release`](#koryph-release) | configure and operate the project release pipeline |
| ↳ [`koryph release setup`](#koryph-release-setup) | render and install release workflow + release-please config |
| ↳ [`koryph release kick`](#koryph-release-kick) | close+reopen the Release PR so checks fire under your gh auth |
| [`koryph board`](#koryph-board) | one-line-per-project run overview |
| [`koryph roster`](#koryph-roster) | per-bead titled roster grouped by lifecycle |
| [`koryph status`](#koryph-status) | latest-run per-slot detail |
| [`koryph tail`](#koryph-tail) | tail a phase's session.log + stderr.log |
| [`koryph doctor`](#koryph-doctor) | health check: layout, binaries, registry, governor |
| [`koryph plan`](#koryph-plan) | plan and analyze the project bead corpus |
| ↳ [`koryph plan audit`](#koryph-plan-audit) | read-only corpus conflict analysis: footprint gaps, non-dispatchable beads, parallel width |
| [`koryph governor`](#koryph-governor) | inspect and set the machine-wide concurrency cap |
| ↳ [`koryph governor show`](#koryph-governor-show) | show the cap, active leases, and demand |
| ↳ [`koryph governor set`](#koryph-governor-set) | set the machine-wide cap |
| ↳ [`koryph governor set-resource`](#koryph-governor-set-resource) | configure or remove a machine resource kind (kind-cluster, docker, ...) |
| [`koryph quota`](#koryph-quota) | per-account governor snapshot |
| ↳ [`koryph quota calibrate`](#koryph-quota-calibrate) | calibrate a governor ceiling from an observed /usage reading |
| ↳ [`koryph quota guard`](#koryph-quota-guard) | live billing-guard toggle — on\|advisory\|off \[--until <duration>]; re-read each wave without a restart |
| [`koryph metrics`](#koryph-metrics) | burn + reliability rollup across projects |
| ↳ [`koryph metrics estimator`](#koryph-metrics-estimator) | per-(model,size) estimator accuracy stats |
| ↳ [`koryph metrics tokens`](#koryph-metrics-tokens) | per-bead and per-tier token composition, cache-hit ratio, and tokens-per-bead trend |
| [`koryph repo`](#koryph-repo) | check or apply .github IaC (rulesets, repo settings) |
| ↳ [`koryph repo describe`](#koryph-repo-describe) | explain every setting in .github IaC and why |
| ↳ [`koryph repo check`](#koryph-repo-check) | diff live GitHub settings/rulesets against .github IaC (exit 1 on drift) |
| ↳ [`koryph repo apply`](#koryph-repo-apply) | apply .github IaC (rulesets, repo settings) to the live repo |
| ↳ [`koryph repo rollback`](#koryph-repo-rollback) | roll back to a pre-apply snapshot |
| [`koryph posture`](#koryph-posture) | apply a named desired-state profile to a GitHub repo |
| ↳ [`koryph posture list`](#koryph-posture-list) | list built-in and user-defined profiles |
| ↳ [`koryph posture describe`](#koryph-posture-describe) | explain every setting a profile enforces and why |
| ↳ [`koryph posture check`](#koryph-posture-check) | diff live GitHub state against a profile (exit 1 on drift) |
| ↳ [`koryph posture diff`](#koryph-posture-diff) | show drift between live state and a profile (always exit 0) |
| ↳ [`koryph posture apply`](#koryph-posture-apply) | show diff then apply a profile to the live GitHub repo |
| [`koryph agents`](#koryph-agents) | install fallback personas |
| ↳ [`koryph agents install`](#koryph-agents-install) | install personas into <root>/.claude/agents |
| [`koryph commands`](#koryph-commands) | install koryph-* Claude slash commands |
| ↳ [`koryph commands install`](#koryph-commands-install) | install commands into <root>/.claude/commands |
| [`koryph rules`](#koryph-rules) | install hook scripts + merge wiring |
| ↳ [`koryph rules install`](#koryph-rules-install) | install hooks into <root>/.claude/settings.json |
| [`koryph onboard`](#koryph-onboard) | read-only inventory of a project |
| [`koryph batch`](#koryph-batch) | submit a Message Batch (explicit per-token spend) |
| ↳ [`koryph batch run`](#koryph-batch-run) | submit a batch from a JSONL file |
| [`koryph version`](#koryph-version) | print the engine version |
| [`koryph completion`](#koryph-completion) | print or install a shell completion script |
| ↳ [`koryph completion bash`](#koryph-completion-bash) | print the bash completion script |
| ↳ [`koryph completion zsh`](#koryph-completion-zsh) | print the zsh completion script |
| ↳ [`koryph completion install`](#koryph-completion-install) | install the completion script to the standard location |
| [`koryph ci`](#koryph-ci) | render and install forge-native CI pipeline assets |
| ↳ [`koryph ci setup`](#koryph-ci-setup) | render and install CI assets into the project |
| ↳ [`koryph ci check`](#koryph-ci-check) | report drift between installed CI assets and current Render output |
| [`koryph cockpit`](#koryph-cockpit) | emit a cockpit snapshot for the VS Code extension |
| [`koryph epic`](#koryph-epic) | epic lifecycle management (validate, …) |
| ↳ [`koryph epic validate`](#koryph-epic-validate) | on-demand epic validation: completeness + structural health review |
| [`koryph gc`](#koryph-gc) | apply data lifecycle policy: compress old run dirs, rotate audit logs |
| [`koryph obs`](#koryph-obs) | manage observability: status, level, enable, disable, tail, export, prune |
| ↳ [`koryph obs status`](#koryph-obs-status) | print current observability configuration |
| ↳ [`koryph obs level`](#koryph-obs-level) | set the log level for a component (or default) |
| ↳ [`koryph obs enable`](#koryph-obs-enable) | enable observability (set default level to info) |
| ↳ [`koryph obs disable`](#koryph-obs-disable) | silence all output (set all levels to error) |
| ↳ [`koryph obs tail`](#koryph-obs-tail) | tail the telemetry JSONL stream in human-readable form |
| ↳ [`koryph obs export`](#koryph-obs-export) | bundle one run's telemetry as redaction-verified JSONL |
| ↳ [`koryph obs prune`](#koryph-obs-prune) | remove telemetry files older than the retention window |
| [`koryph tui`](#koryph-tui) | interactive terminal cockpit (threads, queue, events) |

---

## `koryph init` { #koryph-init }

create ~/.koryph, verify tools on PATH, print next steps

**See also:** [Installation](../user-guide/installation) · [Quickstart](../user-guide/quickstart)

No flags.


---

## `koryph project` { #koryph-project }

onboard and manage registered projects

**See also:** [Projects and accounts](../user-guide/projects-and-accounts) · [Accounts](../concepts/accounts)

Run `koryph project <subcommand> -h` for subcommand flags.

## `koryph project add` { #koryph-project-add }

register a project (inspect + register + scaffold adapter + install assets)

**See also:** [Projects and accounts](../user-guide/projects-and-accounts) · [Zero to shipped](../user-guide/zero-to-shipped)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--account` | string |  | account profile: personal\|work (required) |
| `--branch` | string |  | default branch (default: detected) |
| `--config-dir` | string |  | CLAUDE_CONFIG_DIR for non-personal accounts |
| `--force` | bool |  | override an .envrc account-disagreement refusal |
| `--id` | string |  | project slug (default: repo dir name slugified) |
| `--identity` | string |  | login email that must match at dispatch (required) |
| `--name` | string |  | display name (default: project id) |
| `--no-posture` | bool |  | skip the posture profile offer entirely |
| `--posture` | string |  | posture profile to apply non-interactively (e.g. oss-solo-maintainer); skips the interactive prompt |

## `koryph project install-assets` { #koryph-project-install-assets }

(re)install koryph assets — agents, commands, and rules

**See also:** [Projects and accounts](../user-guide/projects-and-accounts)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all-projects` | bool |  | install into every registered project (registry-wide refresh) |
| `--force` | bool |  | overwrite existing assets whose content differs |

## `koryph project list` { #koryph-project-list }

list managed projects (id, account, status, root)

**See also:** [Projects and accounts](../user-guide/projects-and-accounts)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit JSON array of project records |

## `koryph project show` { #koryph-project-show }

print one project record as JSON

**See also:** [Projects and accounts](../user-guide/projects-and-accounts)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--project` | string |  | project id (alternative to positional <id>; default: the project containing the current directory) |

## `koryph project set-account` { #koryph-project-set-account }

change a project's account (audited; resets validation)

**See also:** [Projects and accounts](../user-guide/projects-and-accounts) · [Accounts](../concepts/accounts)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--config-dir` | string |  | CLAUDE_CONFIG_DIR for the new account |
| `--identity` | string |  | new expected login email (required) |
| `--profile` | string |  | new account profile: personal\|work (required) |
| `--project` | string |  | project id (alternative to positional <id>; default: the project containing the current directory) |
| `--reason` | string |  | why the account is changing (required, audited) |


---

## `koryph validate` { #koryph-validate }

run the pre-dispatch gate

**See also:** [Projects and accounts](../user-guide/projects-and-accounts) · [Zero to shipped](../user-guide/zero-to-shipped)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--project` | string |  | project id (alternative to positional <project-id>; default: the project containing the current directory) |


---

## `koryph run` { #koryph-run }

execute one engine run over a project

**See also:** [Running waves](../user-guide/running-waves) · [Rolling dispatch](../concepts/rolling-dispatch) · [Beads](../concepts/beads)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--allow-api-spend` | bool |  | permit api-key billing at governor stop |
| `--allow-unvalidated` | bool |  | permit runs on non-validated projects |
| `--auto-merge` | bool |  | allow auto-merge for merge:auto items |
| `--budget` | float64 |  | per-run cost ceiling in USD (0 = unlimited) |
| `--default-model` | string |  | model for label-less beads |
| `--direct` | bool |  | owner override: skip PRs and merge straight to the default branch (needs branch-protection bypass) |
| `--dispatch-mode` | string |  | dispatch mode: wave\|rolling (default: project config, else wave) |
| `--dry-run` | bool |  | plan and print without dispatching |
| `--manual` | bool |  | single manual dispatch semantics (quota-exempt) |
| `--max` | int |  | wave width cap (0 = project/engine default) |
| `--no-billing-guard` | bool |  | disable quota throttling for this run (usage still measured; billing stays subscription) |
| `--once` | bool |  | run exactly one wave |
| `--only` | string |  | dispatch only this specific ready bead id |
| `--parent` | string |  | epic scope for the bd frontier |
| `--project` | string |  | project id (default: the project containing the current directory) |
| `--require-calibration` | bool |  | refuse to dispatch while the quota governor is uncalibrated (koryph-grz); run `koryph quota calibrate` first |
| `--resume` | bool |  | classify and re-dispatch the latest run first |
| `--review` | bool |  | post-implementation review pass before merge |


---

## `koryph intake` { #koryph-intake }

poll labeled GitHub issues into planning beads

**See also:** [Intake](../user-guide/intake) · [Beads](../concepts/beads)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--comment` | bool |  | comment the bead id back on each ingested issue |
| `--dry-run` | bool |  | print what would be ingested; mutate nothing |
| `--label` | string |  | trigger label to poll (overrides per-source config; default "triage") |
| `--limit` | int |  | max open issues to poll (overrides per-source config; default 20) |
| `--project` | string |  | project id (default: the project containing the current directory) |


---

## `koryph nudge` { #koryph-nudge }

append an operator note to a phase INBOX

**See also:** [Running waves](../user-guide/running-waves)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--project` | string |  | project id (default: the project containing the current directory) |


---

## `koryph stop` { #koryph-stop }

stop an agent (or every agent with --all)

**See also:** [Running waves](../user-guide/running-waves)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all` | bool |  | stop every active agent — across ALL managed projects, or one project with --project |
| `--force` | bool |  | SIGKILL instead of SIGTERM — uncommitted worktree work is LOST |
| `--project` | string |  | project id (default: the project containing the current directory; unless --all) |


---

## `koryph drain` { #koryph-drain }

gracefully wind down a run: finish active slots, dispatch nothing new

**See also:** [Running waves](../user-guide/running-waves) · [Rolling dispatch](../concepts/rolling-dispatch)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all` | bool |  | request a drain for every registered project |
| `--project` | string |  | project id (default: the project containing the current directory; unless --all) |


---

## `koryph resize` { #koryph-resize }

live width override for a running loop

**See also:** [Running waves](../user-guide/running-waves) · [Governors](../concepts/governors)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all` | bool |  | apply to every registered project |
| `--clear` | bool |  | remove the width override (revert to project config) |
| `--force` | bool |  | allow --max to exceed the project's max_concurrent_slots |
| `--max` | int |  | new width cap (must be > 0; use --clear to remove an override) |
| `--project` | string |  | project id (default: the project containing the current directory; unless --all) |


---

## `koryph merge` { #koryph-merge }

land a finished agent branch

**See also:** [Running waves](../user-guide/running-waves) · [Worktrees](../concepts/worktrees)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--close-bead` | string |  | bead to close on a successful merge |
| `--keep-worktree` | bool |  | keep the worktree + branch after merge |
| `--project` | string |  | project id (default: the project containing the current directory) |
| `--push` | bool |  | push the default branch after merge |
| `--reason` | string |  | close reason for --close-bead |
| `--squash` | bool |  | squash-merge instead of ff-only |


---

## `koryph land` { #koryph-land }

land an engine-opened PR fast-forward-only

**See also:** [Running waves](../user-guide/running-waves) · [Worktrees](../concepts/worktrees)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--method` | string |  | landing method override: ff\|squash (default: project merge_method, else ff) |
| `--project` | string |  | project id (default: the project containing the current directory) |
| `--reason` | string |  | bead close reason |


---

## `koryph review-pr` { #koryph-review-pr }

analyze another author's PR

**See also:** [Running waves](../user-guide/running-waves) · [Collaboration](../user-guide/collaboration)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all` | bool |  | analyze every open PR in the queue (skips drafts and PRs you authored) |
| `--approve` | bool |  | register an approving review (your explicit instruction — koryph never approves autonomously) |
| `--body` | string |  | review/approval body, or the --close comment |
| `--close` | bool |  | close the PR (optionally with --body as the comment) |
| `--comment` | bool |  | post koryph's line-anchored findings as inline PR comments |
| `--comment-on` | multi |  | post an inline comment: 'path:line:message' (repeatable) |
| `--project` | string |  | project id (default: the project containing the current directory) |
| `--resume` | bool |  | re-display the saved analysis for a PR (after an IDE handoff) |


---

## `koryph pr-sync` { #koryph-pr-sync }

reconcile pr-opened beads against live PR state

**See also:** [Running waves](../user-guide/running-waves)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--project` | string |  | project id (default: the project containing the current directory) |


---

## `koryph bot` { #koryph-bot }

provision and manage koryph GitHub App bots

**See also:** [Release train](../concepts/release-train) · [Release bot](../user-guide/release-bot)

Run `koryph bot <subcommand> -h` for subcommand flags.

## `koryph bot create` { #koryph-bot-create }

create a GitHub App via the manifest flow (one browser click)

**See also:** [Release bot](../user-guide/release-bot)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--headless` | bool |  | print the URL instead of opening the browser (set automatically when TERM is unset) |
| `--key-ref` | string |  | provider-specific reference for the key (e.g. pass:// URI, op:// ref, or file path); auto-derived when omitted |
| `--name` | string |  | GitHub App name (e.g. mylogin-release-bot); defaults to <gh-login>-release-bot when omitted (requires gh CLI) |
| `--org` | string |  | create the app under this GitHub organization (omit for personal account) |
| `--plaintext` | bool |  | store the private key inline as plaintext PEM (legacy; prefer a vault or encrypted-file provider) |
| `--public` | bool |  | make the app publicly installable (required for guest-org repo-admin installs) |
| `--vault-provider` | string |  | vault provider for the private key (protonpass\|onepassword\|encrypted-file\|keychain\|file\|…); auto-selects when omitted |

## `koryph bot install` { #koryph-bot-install }

print/open the installation page for a provisioned bot

**See also:** [Release bot](../user-guide/release-bot)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--name` | string |  | bot name (required) |

## `koryph bot attach` { #koryph-bot-attach }

wire a repo to a bot: set secrets and enable Actions PR-approval toggle

**See also:** [Release bot](../user-guide/release-bot)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--name` | string |  | bot name (required) |
| `--org-secrets` | bool |  | set secrets at org level with selected-repos visibility instead of per-repo |
| `--repo` | string |  | GitHub repository as OWNER/REPO (required) |

## `koryph bot list` { #koryph-bot-list }

list provisioned bots in ~/.koryph/bots/

**See also:** [Release bot](../user-guide/release-bot)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--check` | bool |  | perform a live GET /app identity check for each bot |

## `koryph bot check` { #koryph-bot-check }

run the bot validator chain (JWT, installation, secrets, Actions toggle)

**See also:** [Release bot](../user-guide/release-bot)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--name` | string |  | bot name (required) |
| `--repo` | string |  | GitHub repository as OWNER/REPO (optional; adds repo-scoped validators) |

## `koryph bot vault-migrate` { #koryph-bot-vault-migrate }

move a plaintext bot private key into a vault or encrypted file

**See also:** [Signing](../user-guide/signing) · [Release bot](../user-guide/release-bot)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--key-ref` | string |  | provider-specific key reference (auto-derived when omitted) |
| `--name` | string |  | bot name (required) |
| `--vault-provider` | string |  | destination vault provider (auto-selected when omitted) |


---

## `koryph signing` { #koryph-signing }

configure and operate vault-backed commit signing

**See also:** [Signing](../user-guide/signing) · [Postures](../concepts/postures)

Run `koryph signing <subcommand> -h` for subcommand flags.

## `koryph signing setup` { #koryph-signing-setup }

write the signing policy into the adapter

**See also:** [Signing](../user-guide/signing)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--artifacts` | bool |  | enable cosign blob signing (`koryph sign blob`) |
| `--identity` | string |  | signer email (required) |
| `--item-title` | string |  | item title for public-key resolution via view_by_title template (requires --vault-name) |
| `--key-ref` | string |  | vault item URI / file path for the signing key (also used for public-key resolution when no --public-key or --vault-name/--item-title is given) |
| `--mode` | string | `ssh` | signing mode: ssh\|gitsign |
| `--project` | string |  | project id (required) |
| `--provider` | string |  | vault provider: protonpass\|onepassword\|file\|command |
| `--public-key` | string |  | SSH public key: literal ("ssh-ed25519 AAAA...") or "@<path>" to read from file |
| `--vault-name` | string |  | vault name for public-key resolution via view_by_title template |

## `koryph signing enable` { #koryph-signing-enable }

load the key + apply repo git config

**See also:** [Signing](../user-guide/signing)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--project` | string |  | project id (default: the project containing the current directory) |

## `koryph signing keygen` { #koryph-signing-keygen }

generate a passphrase-protected SSH signing key (no-vault path)

**See also:** [Signing](../user-guide/signing)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--identity` | string |  | key comment / signer identity (default: <username>@<host>) |
| `--key-ref` | string |  | path to store the key (default: ~/.koryph/signing/<project>.key) |
| `--project` | string |  | project id (used to resolve existing config; optional) |
| `--provider` | string |  | vault provider: keychain\|encrypted-file\|file (default: platform best) |

## `koryph signing status` { #koryph-signing-status }

mode/provider/agent-ready summary

**See also:** [Signing](../user-guide/signing)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit JSON |
| `--project` | string |  | project id (default: the project containing the current directory) |

## `koryph signing verify` { #koryph-signing-verify }

verify branch commit signatures

**See also:** [Signing](../user-guide/signing)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--branch` | string |  | branch to verify against the default branch (required) |
| `--project` | string |  | project id (default: the project containing the current directory) |


---

## `koryph sign` { #koryph-sign }

cosign sign-blob an artifact

**See also:** [Signing](../user-guide/signing) · [Supply chain](../user-guide/supply-chain)

Run `koryph sign <subcommand> -h` for subcommand flags.

## `koryph sign blob` { #koryph-sign-blob }

sign a file via the vault key

**See also:** [Signing](../user-guide/signing) · [Supply chain](../user-guide/supply-chain)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--project` | string |  | project id (default: the project containing the current directory) |


---

## `koryph release` { #koryph-release }

configure and operate the project release pipeline

**See also:** [Release train](../concepts/release-train) · [Releasing projects](../user-guide/releasing-projects)

Run `koryph release <subcommand> -h` for subcommand flags.

## `koryph release setup` { #koryph-release-setup }

render and install release workflow + release-please config

**See also:** [Release train](../concepts/release-train) · [Releasing projects](../user-guide/releasing-projects)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--bot` | bool |  | run scripts/provision-release-bot.sh --attach after setup |
| `--mode` | string |  | build mode: goreleaser (mode A) or commands (mode B); required when the project has no release block yet |
| `--project` | string |  | project id (default: the project containing the current directory) |
| `--version` | string | `0.0.0` | initial version for the release-please manifest (only used when the manifest does not yet exist) |

## `koryph release kick` { #koryph-release-kick }

close+reopen the Release PR so checks fire under your gh auth

**See also:** [Release train](../concepts/release-train) · [Releasing projects](../user-guide/releasing-projects)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--pr` | int |  | explicit PR number (skips auto-detect) |
| `--repo` | string |  | owner/repo GitHub slug (required) |
| `--wait` | bool |  | poll check conclusions after reopening |
| `--wait-timeout` | string | `10m` | max wait duration (e.g. 10m, 30m) |


---

## `koryph board` { #koryph-board }

one-line-per-project run overview

**See also:** [Running waves](../user-guide/running-waves)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit the board as JSON |


---

## `koryph roster` { #koryph-roster }

per-bead titled roster grouped by lifecycle

**See also:** [Running waves](../user-guide/running-waves) · [Beads](../concepts/beads)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit roster as JSON |
| `--project` | string |  | project id (default: the project containing the current directory) |
| `--run` | string |  | run id (default: latest) |


---

## `koryph status` { #koryph-status }

latest-run per-slot detail

**See also:** [Running waves](../user-guide/running-waves)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit the run as JSON |
| `--project` | string |  | project id (default: the project containing the current directory) |


---

## `koryph tail` { #koryph-tail }

tail a phase's session.log + stderr.log

**See also:** [Running waves](../user-guide/running-waves)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--follow` | bool |  | stream new lines as they appear (Ctrl-C to stop) |
| `--n` | int | `40` | number of trailing lines |
| `--project` | string |  | project id (default: the project containing the current directory) |


---

## `koryph doctor` { #koryph-doctor }

health check: layout, binaries, registry, governor

**See also:** [Doctor](../user-guide/doctor)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--fix` | bool |  | auto-remediate: remove zombie slots/stale demand (global); install missing assets (project) |
| `--force` | bool |  | with --fix and --project: also overwrite stale asset files (default: only install missing) |
| `--json` | bool |  | emit the report as JSON instead of a table |
| `--matrix` | bool |  | render the integration matrix for the project at --root (or current dir) |
| `--project` | string |  | run project-scoped checks for the named project |
| `--root` | string | `.` | project repository root for --matrix mode |


---

## `koryph plan` { #koryph-plan }

plan and analyze the project bead corpus

**See also:** [Beads](../concepts/beads) · [Footprints](../concepts/footprints)

Run `koryph plan <subcommand> -h` for subcommand flags.

## `koryph plan audit` { #koryph-plan-audit }

read-only corpus conflict analysis: footprint gaps, non-dispatchable beads, parallel width

**See also:** [Beads](../concepts/beads) · [Footprints](../concepts/footprints)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit the audit report as JSON (for agent consumption) |
| `--project` | string |  | project id (default: the project containing the current directory) |


---

## `koryph governor` { #koryph-governor }

inspect and set the machine-wide concurrency cap

**See also:** [Governors](../concepts/governors) · [Billing and quota](../user-guide/billing-and-quota) · [Global governor](../developer-guide/global-governor)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit JSON array of pool snapshots |

## `koryph governor show` { #koryph-governor-show }

show the cap, active leases, and demand

**See also:** [Governors](../concepts/governors)

## `koryph governor set` { #koryph-governor-set }

set the machine-wide cap

**See also:** [Governors](../concepts/governors) · [Global governor](../developer-guide/global-governor)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--adaptive` | bool |  | enable the AIMD overlay: probe the cap up on quiet, halve it on rate-limit |
| `--break-sec` | int |  | circuit breaker base open duration, under --adaptive (default 300, doubles per re-open, cap 3600) |
| `--hard-max` | int |  | absolute ceiling for upward probing under --adaptive (default 2x --max-global) |
| `--max-global` | int |  | cap on concurrently running agents in this pool (required, > 0) |
| `--min-dispatch-interval` | int |  | minimum inter-dispatch spacing in seconds, under --adaptive (default 3, jittered ±50%) |
| `--min-free-memory-mb` | int |  | memory admission floor (koryph-930): defer new agents while host available memory is below N MB. 0 = auto-size to physical memory (the default; the gate is ON); a negative value disables the gate. May be set alone or alongside --max-global |
| `--provider` | string |  | governor pool to configure (default: anthropic) — koryph-v8u.11 independent per-provider pools |
| `--settle-sec` | int |  | settle window after any cap change, under --adaptive (default 120) |

## `koryph governor set-resource` { #koryph-governor-set-resource }

configure or remove a machine resource kind (kind-cluster, docker, ...)

**See also:** [Governors](../concepts/governors) · [Global governor](../developer-guide/global-governor)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--capacity` | int |  | max concurrent holders of this kind across all pools (<=0 resolves to the fail-safe default of 1) |
| `--mem-mb` | int |  | per-holder memory reservation in MB during the ramp window (0 = uncalibrated, no reservation) |
| `--probe` | string |  | leak-detection shell command listing live instance names (patrol/doctor only — never consulted on the admission path) |
| `--ramp-seconds` | int |  | ramp window in seconds before a holder's reservation is assumed materialized (<=0 = machine/global default) |
| `--unset` | bool |  | remove this kind from the resources ledger (must be the only flag) |


---

## `koryph quota` { #koryph-quota }

per-account governor snapshot

**See also:** [Governors](../concepts/governors) · [Billing and quota](../user-guide/billing-and-quota)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--account` | string |  | limit to one account (default: all across records) |
| `--json` | bool |  | emit JSON |

## `koryph quota calibrate` { #koryph-quota-calibrate }

calibrate a governor ceiling from an observed /usage reading

**See also:** [Governors](../concepts/governors) · [Billing and quota](../user-guide/billing-and-quota)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--account` | string |  | account to calibrate (required) |
| `--observed-pct` | float64 |  | observed /usage percentage |
| `--observed-usd` | float64 |  | observed ccusage spend (USD) |
| `--plan-tier` | string |  | plan tier label (e.g. max20x) |
| `--window` | string |  | window to calibrate: 5h\|weekly (required) |

## `koryph quota guard` { #koryph-quota-guard }

live billing-guard toggle — on|advisory|off \[--until <duration>]; re-read each wave without a restart

**See also:** [Billing and quota](../user-guide/billing-and-quota) · [Governors](../concepts/governors)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--account` | string |  | account to configure (required) |
| `--until` | string |  | auto-revert duration from now (e.g. 2h, 24h); omit for permanent |


---

## `koryph metrics` { #koryph-metrics }

burn + reliability rollup across projects

**See also:** [Billing and quota](../user-guide/billing-and-quota) · [Governors](../concepts/governors)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit JSON |
| `--project` | string |  | limit to one project |

## `koryph metrics estimator` { #koryph-metrics-estimator }

per-(model,size) estimator accuracy stats

**See also:** [Billing and quota](../user-guide/billing-and-quota)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--account` | string |  | limit to one account |
| `--json` | bool |  | emit JSON |

## `koryph metrics tokens` { #koryph-metrics-tokens }

per-bead and per-tier token composition, cache-hit ratio, and tokens-per-bead trend

**See also:** [Billing and quota](../user-guide/billing-and-quota) · [2026 07 token economy](../docs/designs/2026-07-token-economy)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--experiment` | bool |  | render the L6 two-arm (proxied vs holdout) standing-canary comparison instead |
| `--json` | bool |  | emit JSON |
| `--project` | string |  | limit to one project ID |


---

## `koryph repo` { #koryph-repo }

check or apply .github IaC (rulesets, repo settings)

**See also:** [Postures](../user-guide/postures) · [Ejectability](../concepts/ejectability)

Run `koryph repo <subcommand> -h` for subcommand flags.

## `koryph repo describe` { #koryph-repo-describe }

explain every setting in .github IaC and why

**See also:** [Postures](../user-guide/postures)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--repo` | string |  | repository in owner/name form — when given, shows live value per setting |

## `koryph repo check` { #koryph-repo-check }

diff live GitHub settings/rulesets against .github IaC (exit 1 on drift)

**See also:** [Postures](../user-guide/postures) · [Ejectability](../concepts/ejectability)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--repo` | string |  | repository in owner/name form (default: detected from git remote via gh) |

## `koryph repo apply` { #koryph-repo-apply }

apply .github IaC (rulesets, repo settings) to the live repo

**See also:** [Postures](../user-guide/postures) · [Ejectability](../concepts/ejectability)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--repo` | string |  | repository in owner/name form (default: detected from git remote via gh) |

## `koryph repo rollback` { #koryph-repo-rollback }

roll back to a pre-apply snapshot

**See also:** [Postures](../user-guide/postures)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--repo` | string |  | repository in owner/name form (default: detected from git remote via gh) |
| `--to` | string | `latest` | snapshot selector: "latest" or a RFC3339 timestamp (or prefix, e.g. "2026-07-04T16") |


---

## `koryph posture` { #koryph-posture }

apply a named desired-state profile to a GitHub repo

**See also:** [Postures](../concepts/postures) · [Postures](../user-guide/postures)

Run `koryph posture <subcommand> -h` for subcommand flags.

## `koryph posture list` { #koryph-posture-list }

list built-in and user-defined profiles

**See also:** [Postures](../user-guide/postures)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--fragments` | bool |  | list built-in security-scanner fragments instead of profiles |

## `koryph posture describe` { #koryph-posture-describe }

explain every setting a profile enforces and why

**See also:** [Postures](../concepts/postures) · [Postures](../user-guide/postures)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--param` | multi |  | profile parameter as key=value (repeatable, e.g. --param required_checks="pre-commit,make gate") |
| `--repo` | string |  | repository in owner/name form — when given, shows live value per setting |

## `koryph posture check` { #koryph-posture-check }

diff live GitHub state against a profile (exit 1 on drift)

**See also:** [Postures](../concepts/postures) · [Postures](../user-guide/postures)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--force` | bool |  | with apply: overwrite stale fragment files (default: only install missing fragments) |
| `--org` | string |  | GitHub organisation for org-level ruleset check/apply (requires org owner/admin) |
| `--param` | multi |  | profile parameter as key=value (repeatable, e.g. --param required_checks="pre-commit,make gate") |
| `--repo` | string |  | repository in owner/name form (default: detected from git remote via gh) |

## `koryph posture diff` { #koryph-posture-diff }

show drift between live state and a profile (always exit 0)

**See also:** [Postures](../concepts/postures) · [Postures](../user-guide/postures)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--force` | bool |  | with apply: overwrite stale fragment files (default: only install missing fragments) |
| `--org` | string |  | GitHub organisation for org-level ruleset check/apply (requires org owner/admin) |
| `--param` | multi |  | profile parameter as key=value (repeatable, e.g. --param required_checks="pre-commit,make gate") |
| `--repo` | string |  | repository in owner/name form (default: detected from git remote via gh) |

## `koryph posture apply` { #koryph-posture-apply }

show diff then apply a profile to the live GitHub repo

**See also:** [Postures](../concepts/postures) · [Postures](../user-guide/postures)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--force` | bool |  | with apply: overwrite stale fragment files (default: only install missing fragments) |
| `--org` | string |  | GitHub organisation for org-level ruleset check/apply (requires org owner/admin) |
| `--param` | multi |  | profile parameter as key=value (repeatable, e.g. --param required_checks="pre-commit,make gate") |
| `--repo` | string |  | repository in owner/name form (default: detected from git remote via gh) |


---

## `koryph agents` { #koryph-agents }

install fallback personas

**See also:** [Projects and accounts](../user-guide/projects-and-accounts)

Run `koryph agents <subcommand> -h` for subcommand flags.

## `koryph agents install` { #koryph-agents-install }

install personas into <root>/.claude/agents

**See also:** [Projects and accounts](../user-guide/projects-and-accounts)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all-projects` | bool |  | install into every registered project (registry-wide refresh) |
| `--force` | bool |  | overwrite existing personas whose content differs |
| `--runtime` | string |  | target runtime name (default: <root>'s default_runtime, else "claude") |


---

## `koryph commands` { #koryph-commands }

install koryph-* Claude slash commands

**See also:** [Projects and accounts](../user-guide/projects-and-accounts)

Run `koryph commands <subcommand> -h` for subcommand flags.

## `koryph commands install` { #koryph-commands-install }

install commands into <root>/.claude/commands

**See also:** [Projects and accounts](../user-guide/projects-and-accounts)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all-projects` | bool |  | install into every registered project (registry-wide refresh) |
| `--force` | bool |  | overwrite existing commands whose content differs |


---

## `koryph rules` { #koryph-rules }

install hook scripts + merge wiring

**See also:** [Projects and accounts](../user-guide/projects-and-accounts) · [Worktrees](../concepts/worktrees)

Run `koryph rules <subcommand> -h` for subcommand flags.

## `koryph rules install` { #koryph-rules-install }

install hooks into <root>/.claude/settings.json

**See also:** [Projects and accounts](../user-guide/projects-and-accounts)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--force` | bool |  | overwrite differing hook scripts; rebuild an unparseable settings.json |


---

## `koryph onboard` { #koryph-onboard }

read-only inventory of a project

**See also:** [Projects and accounts](../user-guide/projects-and-accounts) · [Architecture](../architecture)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit the inventory as JSON |


---

## `koryph batch` { #koryph-batch }

submit a Message Batch (explicit per-token spend)

**See also:** [Billing and quota](../user-guide/billing-and-quota)

Run `koryph batch <subcommand> -h` for subcommand flags.

## `koryph batch run` { #koryph-batch-run }

submit a batch from a JSONL file

**See also:** [Billing and quota](../user-guide/billing-and-quota)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--cache-prefix` | bool |  | apply a 1h cache breakpoint to the shared system prefix |
| `--input` | string |  | JSONL input file with {id,system,user} lines (required) |
| `--key-env` | string |  | env var NAME holding the API key (required; never ANTHROPIC_API_KEY) |
| `--max-tokens` | int |  | max output tokens per request (default 4096) |
| `--model` | string |  | model tier: haiku\|sonnet\|opus\|fable (required) |
| `--out` | string |  | results JSONL destination (default stdout) |
| `--yes` | bool |  | confirm the estimated spend and submit |


---

## `koryph version` { #koryph-version }

print the engine version

<!-- TODO: add DocLinks to the `version` command registration in cmd/koryph/ -->

No flags.


---

## `koryph completion` { #koryph-completion }

print or install a shell completion script

**See also:** [Installation](../user-guide/installation)

Run `koryph completion <subcommand> -h` for subcommand flags.

## `koryph completion bash` { #koryph-completion-bash }

print the bash completion script

**See also:** [Installation](../user-guide/installation)

## `koryph completion zsh` { #koryph-completion-zsh }

print the zsh completion script

**See also:** [Installation](../user-guide/installation)

## `koryph completion install` { #koryph-completion-install }

install the completion script to the standard location

**See also:** [Installation](../user-guide/installation)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--shell` | string |  | target shell: bash\|zsh (default: detect from $SHELL) |


---

## `koryph ci` { #koryph-ci }

render and install forge-native CI pipeline assets

**See also:** [Ci setup](../user-guide/ci-setup)

Run `koryph ci <subcommand> -h` for subcommand flags.

## `koryph ci setup` { #koryph-ci-setup }

render and install CI assets into the project

**See also:** [Ci setup](../user-guide/ci-setup)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--gate-cmd` | string |  | override the gate command (default: make gate) |
| `--kind` | string | `gate` | CI asset kind(s) to install: gate, scanner, or all |
| `--project` | string |  | project id |

## `koryph ci check` { #koryph-ci-check }

report drift between installed CI assets and current Render output

**See also:** [Ci setup](../user-guide/ci-setup)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--gate-cmd` | string |  | override the gate command (default: make gate) |
| `--kind` | string | `gate` | CI asset kind(s) to check: gate, scanner, or all |
| `--project` | string |  | project id |


---

## `koryph cockpit` { #koryph-cockpit }

emit a cockpit snapshot for the VS Code extension

**See also:** [Ide setup](../developer-guide/ide-setup)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit snapshot as JSON (used by the VS Code extension) |
| `--project` | string |  | project id (required) |


---

## `koryph epic` { #koryph-epic }

epic lifecycle management (validate, …)

**See also:** [Epic validation](../user-guide/epic-validation) · [Beads](../concepts/beads)

Run `koryph epic <subcommand> -h` for subcommand flags.

## `koryph epic validate` { #koryph-epic-validate }

on-demand epic validation: completeness + structural health review

**See also:** [Epic validation](../user-guide/epic-validation)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit the raw verdict JSON; actions still apply |
| `--project` | string |  | project id (default: the project containing the current directory) |
| `--round` | int |  | validation round override (0 = auto-detect from prior verdict files) |


---

## `koryph gc` { #koryph-gc }

apply data lifecycle policy: compress old run dirs, rotate audit logs

**See also:** [Gc](../user-guide/gc)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--dry-run` | bool |  | report without making any changes |
| `--json` | bool |  | emit the result as JSON |
| `--project` | string |  | apply run-dirs gc for this project |


---

## `koryph obs` { #koryph-obs }

manage observability: status, level, enable, disable, tail, export, prune

**See also:** [Observability](../user-guide/observability)

Run `koryph obs <subcommand> -h` for subcommand flags.

## `koryph obs status` { #koryph-obs-status }

print current observability configuration

**See also:** [Observability](../user-guide/observability)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | emit as JSON |

## `koryph obs level` { #koryph-obs-level }

set the log level for a component (or default)

**See also:** [Observability](../user-guide/observability)

No flags.

## `koryph obs enable` { #koryph-obs-enable }

enable observability (set default level to info)

**See also:** [Observability](../user-guide/observability)

No flags.

## `koryph obs disable` { #koryph-obs-disable }

silence all output (set all levels to error)

**See also:** [Observability](../user-guide/observability)

No flags.

## `koryph obs tail` { #koryph-obs-tail }

tail the telemetry JSONL stream in human-readable form

**See also:** [Observability](../user-guide/observability)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--component` | string |  | filter to this component (engine\|govern\|sched\|…) |
| `--follow` | bool |  | stream new records as they arrive (Ctrl-C to stop) |
| `--level` | string |  | minimum level to display (trace\|debug\|info\|warn\|error) |
| `--n` | int | `40` | number of trailing records to show (0 = all) |

## `koryph obs export` { #koryph-obs-export }

bundle one run's telemetry as redaction-verified JSONL

**See also:** [Observability](../user-guide/observability)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--output` | string |  | write to this file instead of stdout (default: stdout) |
| `--run` | string |  | run ID to export (required) |

## `koryph obs prune` { #koryph-obs-prune }

remove telemetry files older than the retention window

**See also:** [Observability](../user-guide/observability)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--dry-run` | bool |  | list files that would be removed without removing them |


---

## `koryph tui` { #koryph-tui }

interactive terminal cockpit (threads, queue, events)

<!-- TODO: add DocLinks to the `tui` command registration in cmd/koryph/ -->

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--a` | bool |  | shorthand for --all-projects |
| `--all-projects` | bool |  | show every registered project (aggregate cockpit) |
| `--project` | string |  | project id (default: the project containing the current directory) |
| `--read-only` | bool |  | disable write actions (nudge, drain) — safe for shared/observer sessions |


---

## Environment { #env }

| Variable | Description |
|----------|-------------|
| `KORYPH_HOME` | central registry + governor root (default: ~/.koryph) |
| `KORYPH_BD_BIN` | path to the bd (beads) binary (default: bd on PATH) |
| `KORYPH_GH_BIN` | path to the gh (GitHub CLI) binary (default: gh on PATH) |
| `KORYPH_NO_NPX` | set to any value to disable npx-based tool fallbacks (e.g. ccusage) |
