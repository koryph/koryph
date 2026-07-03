<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Intake

`koryph intake` bridges external issue trackers into the Koryph planning
funnel. It polls configured sources, creates one planning bead per new issue,
and stamps each bead with mandatory labels so that **no issue reaches the
dispatch loop without human or planner review first**.

---

## Quick start

Add an `intake` list to your project's `koryph.project.json`:

```json
{
  "intake": [
    {
      "provider": "github",
      "source":   "acme/widgets",
      "trigger":  "triage"
    },
    {
      "provider":    "github",
      "source":      "acme/backend",
      "trigger":     "bug",
      "limit":       50,
      "comment_back": true
    },
    {
      "provider":    "jira",
      "source":      "acme.atlassian.net/ENG",
      "trigger":     "status = \"To Do\" AND labels = \"koryph\"",
      "comment_back": true
    }
  ]
}
```

Then run:

```sh
koryph intake --project <project-id>
```

Both sources are polled in one run. The output table groups results by source:

```
intake acme/widgets
ACTION        ISSUE  BEAD        TITLE                                             NOTE
ingested      #42    beads-0abc  Fix nil deref in wave scheduler                   -
skipped       #38    beads-0a12  Add prometheus metrics                             already ingested

ingested 1, skipped 1

intake acme/backend
ACTION        ISSUE  BEAD        TITLE                                             NOTE
ingested      #7     beads-0xyz  Crash on login                                    commented

ingested 1, skipped 0

total across 2 sources: ingested 2, skipped 1
```

---

## Configured sources vs. single-source mode

**Configured sources (recommended):** When the project's `koryph.project.json`
has a non-empty `intake` list, `koryph intake` iterates every entry in
order. Per-source settings (`trigger`, `limit`, `comment_back`) apply to that
source only.

**Single-source mode (legacy / quick use):** When no `intake` list is
configured, intake falls back to the project's registry remote with CLI flags:

```sh
koryph intake --project my-project --label triage --limit 20
```

---

## intake source schema

Each entry in the `intake` list is an **IntakeSource**:

| Field | Type | Default | Description |
|---|---|---|---|
| `provider` | string | `"github"` | Issue-tracker type: `"github"` or `"jira"`. |
| `source` | string | *(required)* | Target within the provider. GitHub: `"owner/repo"`. JIRA: `"<host>/<project-key>"`. |
| `trigger` | string | `"triage"` | Label (GitHub) or JQL predicate (JIRA) that filters open issues. |
| `limit` | int | `20` | Maximum number of open issues fetched per run. |
| `comment_back` | bool | `false` | Post the new bead ID back as a comment on each ingested issue. |
| `mapping` | object | `{}` | Reserved for future provider-specific field remapping. Ignored in v1. |

---

## Provider setup

### GitHub

**Prerequisites:**

- The `gh` CLI must be on `PATH` and authenticated (`gh auth login`).
- The `bd` CLI must be on `PATH`.

**Source format:** `"owner/repo"` — for example `"koryph/koryph"`.

**Trigger:** Any GitHub label name. The default is `"triage"`. Combine with
GitHub issue templates that auto-apply labels for zero-touch routing:

```yaml
# .github/ISSUE_TEMPLATE/bug.yml
labels: ["triage", "bug"]
```

**Comment-back:** When `comment_back: true`, intake posts a comment on each
newly ingested issue:

```
Tracked as bead beads-0abc for planning.
```

This links GitHub to Koryph without modifying labels or closing issues.
Comment-back is non-fatal: if the `gh issue comment` call fails the bead is
still created and the failure reason is recorded in the output table.

---

### JIRA Cloud

**Prerequisites:**

- A JIRA Cloud account with API token access.
- The `bd` CLI must be on `PATH`.
- Two environment variables set before running `koryph intake`:
  - `JIRA_EMAIL` — the Atlassian account email address.
  - `JIRA_TOKEN` — the API token ([generate one here](https://id.atlassian.com/manage-profile/security/api-tokens)).

Either value may be a 1Password vault reference (starts with `op://`). When
detected, intake resolves it via `op read <ref>` before use:

```sh
export JIRA_EMAIL="me@acme.com"
export JIRA_TOKEN="op://Personal/Atlassian API Token/credential"
```

**Source format:** `"<host>/<project-key>"` — for example
`"acme.atlassian.net/ENG"`. The host is your Atlassian Cloud subdomain; the
project key is the uppercase identifier shown in your JIRA project settings.

**Trigger (JQL):** A JQL predicate that is AND-combined with
`project = "<key>"`. For example:

```json
{
  "provider": "jira",
  "source":   "acme.atlassian.net/ENG",
  "trigger":  "status = \"To Do\" AND labels = \"koryph\"",
  "comment_back": true
}
```

When `trigger` is empty the full project is polled (up to `limit` issues).

**Provenance key:** `jira-<host>/<project-key>#<number>` — for example
`jira-acme.atlassian.net/ENG#42`. This key is unique across instances and
projects and is stored as both the bead's `--external-ref` and a label.

**Priority mapping:**

| JIRA priority | Bead priority |
|---|---|
| Highest / Blocker | 0 (critical) |
| High / Critical | 1 (high) |
| Medium *(default)* | 2 (medium) |
| Low / Minor / Lowest / Trivial | 3 (low) |

**Type mapping:** Issues with issuetype `Bug` (case-insensitive) set the bead
type to `bug`. All other types use the default bead type.

**Comment-back:** When `comment_back: true`, intake posts an ADF-wrapped
comment on each newly ingested JIRA issue via the REST v3 API. Non-fatal:
if the comment POST fails the bead is still created.

**Note:** The JQL trigger scope is the configured `<project-key>`. Multi-project
JQL queries are not recommended in v1 — issues from projects other than the
configured key will receive incorrect provenance keys and may be re-ingested.

---

## CLI flags

| Flag | Default | Description |
|---|---|---|
| `--project` | *(required)* | Project ID from the local registry. |
| `--label` | *(per-source)* | Override trigger label for **all** sources in this run. |
| `--limit` | *(per-source)* | Override issue fetch limit for **all** sources in this run. |
| `--dry-run` | `false` | Print what would be ingested; mutate nothing. |
| `--comment` | `false` | Override comment-back for **all** sources in this run. |

When `--label`, `--limit`, or `--comment` are passed on the command line they
override the per-source settings for that run only. They do **not** modify the
`koryph.project.json` configuration.

### Dry run

```sh
koryph intake --project my-project --dry-run
```

No beads are created and no external state is mutated. The output table shows
`would-ingest` instead of `ingested`. Use dry run to preview what a scheduled
intake would produce before enabling it.

---

## Planning funnel and mandatory labels

Every bead created by intake receives **two mandatory labels**:

| Label | Purpose |
|---|---|
| `intake` | Marks the bead as originating from an external issue. |
| `no-dispatch` | **Blocks the wave engine** from building this bead autonomously. |

The `no-dispatch` label is the critical gate: intake is a *planning input*, not
a dispatch trigger. A human or planner agent reviews the bead, decides whether
and how to act, and removes `no-dispatch` when the issue is ready to build.

In addition, a provenance label is applied to record the source issue:

| Provider | Example key |
|---|---|
| GitHub | `gh-acme/widgets#42` |
| JIRA | `jira-acme.atlassian.net/ENG#42` |

Including the owner/repo (or host/project-key for JIRA) in the key guarantees
that issues sharing a number across different configured sources are never
conflated. The same value is stored in the bead's `--external-ref` field for
reliable deduplication across all configured sources.

---

## Idempotency

Before creating a bead, intake looks up any existing bead whose `external-ref`
matches the issue's provenance key. If one is found the issue is **skipped**
regardless of the bead's current status. Re-running intake at any time is safe.

---

## Priority and type mapping

### GitHub

Intake reads `p0`–`p3` issue labels and maps them to bead priority:

| GitHub label | Bead priority |
|---|---|
| `p0` | 0 (critical) |
| `p1` | 1 (high) |
| `p2` | 2 (medium, default) |
| `p3` | 3 (low) |

Issues without a `p0`–`p3` label default to priority **2** (medium).

A `bug`-labeled issue sets the bead type to `bug`. All other issues use the
default bead type. Combined with GitHub issue templates that automatically apply
a `triage` label plus a `bug` or `feature` label, intake routes issues into the
right bead type without manual intervention.

### JIRA

JIRA priority names are mapped to bead priority (see [JIRA Cloud](#jira-cloud)
section above for the full table). The issuetype `Bug` maps to bead type `bug`.
Native JIRA labels are carried through verbatim as additional bead labels.

---

## Provenance footer

Each bead's description ends with a footer linking back to the source issue:

**GitHub:**
```
---
Source: github.com/acme/widgets/issues/42, author @alice, ingested by koryph intake
```

**JIRA:**
```
---
Source: https://acme.atlassian.net/browse/ENG-42, author alice@acme.com, ingested by koryph intake
```

---

## What intake never does

- Closes or relabels external issues (v1).
- Creates beads that are immediately dispatchable — `no-dispatch` is always
  applied and must be explicitly removed by a person or planner.
- Uses a raw GitHub API token — all GitHub access goes through `gh`.
- Stores credentials on disk — JIRA credentials are read from environment
  variables only, resolved at runtime (or via `op read` for vault refs).
