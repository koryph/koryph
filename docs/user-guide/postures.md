<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Posture Profiles

A **posture profile** is a named bundle of desired-state files — branch-protection
rulesets and repository settings — that you can apply to any GitHub repository
with a single command. Profiles generalise the `koryph repo check|apply` workflow:
instead of writing `.github/` IaC files yourself, you pick a profile and pass
parameters.

Koryph ships one built-in profile: **`oss-solo-maintainer`**. You can also
create custom profiles in `~/.koryph/postures/<name>/`.

---

## Built-in profile: `oss-solo-maintainer`

Designed for an open-source project with a single (or small team of) maintainer(s).
Applies:

| Section | What it sets |
|---|---|
| `pr-checks` ruleset | 1 required review, optional required CI check names (via `--param`) |
| `signed-commits` ruleset | `required_signatures`, `non_fast_forward`, `deletion` on the default branch |
| Repo settings | `allow_squash_merge=true`, `allow_merge_commit=false`, `delete_branch_on_merge=true`, `web_commit_signoff_required=true` |
| Security & analysis | `secret_scanning`, `secret_scanning_push_protection`, `dependabot_security_updates` all **enabled** |
| Vulnerability alerts | **enabled** |
| Actions workflow | `default_workflow_permissions=read`, `can_approve_pull_request_reviews=true` |

### Parameters

| Name | Description | Default |
|---|---|---|
| `required_checks` | Comma-separated required CI check names added to the `pr-checks` ruleset. Omit to skip the `required_status_checks` rule entirely. | _(empty — rule omitted)_ |

---

## Commands

All posture commands accept `--repo owner/name`. When `--repo` is omitted,
koryph detects the repository from the current directory's git remote via `gh`.

### `posture list`

```
koryph posture list
```

Lists all available profiles — built-ins (embedded in the binary) and user
profiles in `~/.koryph/postures/`.

```
NAME                  SOURCE    DESCRIPTION
oss-solo-maintainer   builtin   Baseline posture for an OSS project with a solo maintainer: ...
my-custom-profile     user      My company standard posture
```

### `posture describe`

```
koryph posture describe <profile> [--repo owner/name] [--param k=v]...
```

Renders a human-readable explanation of every managed setting and ruleset rule
in the profile:

- **Target value** — what the profile enforces for each setting key.
- **Security rationale** — plain-language explanation of what attack or mistake
  the setting prevents (e.g. signed commits = commit provenance; push protection =
  credential leak prevention; 1 required review = no unreviewed changes on main).
- **Live value and change status** (with `--repo`) — the current GitHub value for
  each setting, and whether applying the profile would change it.

Rationale text is sourced from three places, in order of precedence:
1. The `--param`-derived manifest `descriptions` map (community profiles may ship this in `manifest.json`).
2. The `descriptions` map in the profile's `repo-settings.json` file.
3. Built-in fallback rationale in koryph for all well-known setting keys and rule types.

Ruleset files may also carry `_rationale` (a per-ruleset summary) and
`_rule_descriptions` (a per-rule map) at the top level of their JSON — these
fields are stripped during check/apply normalization and are purely informational.

**Profile-only description (no live check):**

```
koryph posture describe oss-solo-maintainer
```

Sample output (truncated):

```
Profile: oss-solo-maintainer
Baseline posture for an OSS project with a solo maintainer: ...

── Repo Settings ───────────────────────────────────────────────────────────────

  [repo flags]
  allow_merge_commit                         false
    Prevents merge commits on the default branch, enforcing a clean bisectable
    history where every change is squash-merged or rebase-merged.

  ...

── Rulesets ────────────────────────────────────────────────────────────────────

  [signed-commits] target: branch  conditions: ~DEFAULT_BRANCH
  Enforces cryptographic commit signing and protects default-branch integrity ...

    required_signatures
      All commits must be GPG or SSH signed, proving commit provenance and
      making unauthorized history modifications detectable.

    non_fast_forward
      Prevents force-pushes that rewrite history on the protected branch.

    deletion
      Prevents accidental or malicious deletion of the protected branch.
```

**With live comparison (`--repo`):**

```
koryph posture describe oss-solo-maintainer --repo myorg/myrepo \
  --param required_checks="pre-commit,make gate"
```

Each setting line gains a `live:` sub-line:

```
  secret_scanning                            "enabled"
    live: "disabled"                              [→ WOULD CHANGE]
```

### `posture check`

```
koryph posture check <profile> [--repo owner/name] [--param k=v]...
```

Compares the live GitHub repository state against the profile. Prints `OK`,
`MISSING`, or `DRIFT` per section — identical to `koryph repo check`. **Exits 1**
if drift is detected (useful for CI gating).

```
koryph posture check oss-solo-maintainer --repo myorg/myrepo \
  --param required_checks="pre-commit,make gate"
```

### `posture diff`

```
koryph posture diff <profile> [--repo owner/name] [--param k=v]...
```

Same as `check` but **always exits 0** — drift is informational, not a failure.
Useful for exploration and auditing without breaking scripts.

### `posture apply`

```
koryph posture apply <profile> [--repo owner/name] [--param k=v]...
```

Prints the diff between the live state and the profile, then applies any
changes. Never deletes rulesets it does not know about.

Before making any live change, koryph captures the **current** GitHub state into
a timestamped snapshot under `<repo-root>/.koryph/snapshots/settings-<ts>.json`.
If the diff is empty (nothing to change), no snapshot is written. Roll back with
`koryph repo rollback` (see below).

```
koryph posture apply oss-solo-maintainer --repo myorg/myrepo \
  --param required_checks="pre-commit,make gate"
```

Output:

```
--- rulesets diff ---
MISSING  pr-checks (no live ruleset)
MISSING  signed-commits (no live ruleset)
captured pre-change state → .koryph/snapshots/settings-2026-07-04T16-40-18Z.json; rollback with koryph repo rollback
--- applying rulesets ---
CREATED  pr-checks
CREATED  signed-commits
--- settings diff ---
DRIFT    security & analysis:
         - {"dependabot_security_updates":"disabled","secret_scanning":"disabled",...}
         + {"dependabot_security_updates":"enabled","secret_scanning":"enabled",...}
--- applying settings ---
UPDATED  security & analysis
```

---

## `repo describe` — describe repo-local IaC

`koryph repo describe` produces the same output format as `posture describe`
but reads from the repository's own `.github/` IaC files instead of a named
profile:

```
koryph repo describe [--repo owner/name]
```

Use it to understand what a repository's own `.github/rulesets/*.json` and
`.github/repo-settings.json` enforce and why, with the same per-setting
rationale as `posture describe`. The `--repo` flag adds live values and
change markers identical to those in `posture describe --repo`.

---

## Ejectability — repo-local `.github/` overrides the profile

A repository that has **ejected** from a profile by writing its own `.github/`
IaC files stays sovereign. Koryph detects this automatically, per section:

- If `.github/rulesets/` exists in the current directory → local rulesets win;
  profile rulesets are ignored.
- If `.github/repo-settings.json` exists → local settings win; profile settings
  are ignored.

Koryph prints an `INFO` line for each overridden section:

```
INFO     rulesets: repo has .github/rulesets/ — using local IaC (profile rulesets ignored)
```

This means you can safely run `koryph posture check oss-solo-maintainer` in any
repo — repos that have their own IaC are unaffected. Ejected repos continue to be
managed by `koryph repo check|apply`.

---

## Solo-maintainer walkthrough (zero to compliant)

This walkthrough shows how to apply the `oss-solo-maintainer` profile to a new
GitHub repository. You need `gh` authenticated and `koryph` on your PATH.

**1. Check current drift:**

```
koryph posture check oss-solo-maintainer --repo myorg/myrepo \
  --param required_checks="pre-commit,ci"
```

Expect `MISSING` lines for the rulesets and `DRIFT` lines for security settings
on a freshly created repo.

**2. Apply the profile:**

```
koryph posture apply oss-solo-maintainer --repo myorg/myrepo \
  --param required_checks="pre-commit,ci"
```

Koryph prints the diff, then creates the two rulesets and patches the settings.

**3. Verify no remaining drift:**

```
koryph posture check oss-solo-maintainer --repo myorg/myrepo \
  --param required_checks="pre-commit,ci"
# exits 0 — OK for every section
```

**4. Ongoing enforcement (optional):**

Add the check to CI (e.g. a scheduled GitHub Actions workflow):

```yaml
- name: posture check
  run: |
    koryph posture check oss-solo-maintainer \
      --param required_checks="pre-commit,make gate"
```

---

## Creating a custom profile

A user profile lives at `~/.koryph/postures/<name>/`. Its structure mirrors the
built-in profiles:

```
~/.koryph/postures/my-company/
  manifest.json
  rulesets/
    main-protection.json
    signed-commits.json
  repo-settings.json
```

`manifest.json` describes the profile:

```json
{
  "name": "my-company",
  "description": "Company-standard GitHub posture",
  "parameters": {
    "required_checks": {
      "description": "Comma-separated required CI check names",
      "default": "build,test"
    }
  }
}
```

Template files (suffix `.json.tmpl`) are rendered with Go `text/template`.
Available variables:

| Variable | Description |
|---|---|
| `.RequiredChecks` | Slice of `{Context string}` objects for required CI checks |

Use `{{toJSON .RequiredChecks}}` to emit a JSON array of `{"context":"…"}` objects.

Static files (`.json`, no `.tmpl` suffix) are copied verbatim.

User profiles take precedence over built-ins of the same name — you can override
`oss-solo-maintainer` by creating `~/.koryph/postures/oss-solo-maintainer/`.

### Making a custom profile self-documenting

Add a `descriptions` map to `manifest.json` to override or add rationale for
any setting key. Built-in rationale already exists for all keys used by
`oss-solo-maintainer`; override when your profile sets keys with different
intent or adds novel keys:

```json
{
  "name": "my-company",
  "description": "Company-standard GitHub posture",
  "descriptions": {
    "allow_auto_merge": "Auto-merge is enabled for our release bot (override: intentional).",
    "my_custom_key":   "Explanation of what this company-specific setting prevents."
  }
}
```

Alternatively, add a `descriptions` map directly in `repo-settings.json`:

```json
{
  "repo": { ... },
  "descriptions": {
    "allow_merge_commit": "Custom rationale next to the setting it describes."
  }
}
```

For rulesets, add `_rationale` (per-ruleset summary) and `_rule_descriptions`
(per-rule map) to the ruleset JSON file — these fields are **stripped during
check/apply normalization** and are purely informational:

```json
{
  "name": "my-protection",
  "_rationale": "Enforces our branch protection policy.",
  "_rule_descriptions": {
    "deletion": "Prevents accidental branch deletion by our CI bots."
  },
  "enforcement": "active",
  "target": "branch",
  "rules": [
    { "type": "deletion" }
  ]
}
```

Rationale lookup order: `_rule_descriptions` in the file > `manifest.json`
`descriptions` (keyed as `"rule.<type>"`) > built-in fallback.

---

## Pre-apply snapshots and rollback

Every `koryph repo apply` and `koryph posture apply` that would change live
settings first captures the **current** live state into a timestamped snapshot:

```
<repo-root>/.koryph/snapshots/settings-2026-07-04T16-40-18Z.json
```

The snapshot schema:

```json
{
  "captured_at": "2026-07-04T16:40:18Z",
  "repo": "owner/name",
  "applied_profile": "oss-solo-maintainer",
  "sections": {
    "repo_flags": { "description": "...", "allow_squash_merge": true, "..." : "..." },
    "security_and_analysis": { "secret_scanning": "enabled", "..." : "..." },
    "vulnerability_alerts": true,
    "actions_workflow_permissions": { "default_workflow_permissions": "read", "..." : "..." },
    "rulesets": {
      "protect-main": { "name": "protect-main", "..." : "..." }
    }
  }
}
```

For `koryph repo apply` the top-level key is `"iac"` instead of `"applied_profile"`.
Snapshots contain observed repo config — no secrets.

**Snapshots are gitignored by default.** Koryph writes `.koryph/snapshots/` into
the project's `.gitignore` automatically (idempotent, appended if missing) the
first time a snapshot is created, and again during `koryph project add`. Do not
commit snapshot files — they are machine-local state.

When the diff is empty (nothing would change) no snapshot is written.

### `repo rollback`

```
koryph repo rollback [--repo owner/name] [--to <timestamp>|latest]
```

Lists the available snapshots when no `--to` is given and multiple exist for the
repo. Shows a **diff of snapshot vs. live state** before applying (same
diff-first discipline as `apply`). If the live state already matches the
snapshot, prints "no drift" and exits without changing anything.

```sh
# Roll back to the most recent snapshot:
koryph repo rollback

# Roll back to a specific snapshot by exact or prefix timestamp:
koryph repo rollback --to 2026-07-04T16:40:18Z
koryph repo rollback --to 2026-07-04T16          # must resolve to exactly one snapshot

# Specify a repo explicitly:
koryph repo rollback --repo myorg/myrepo --to latest
```

Rollback applies the snapshot through the **same apply machinery** — it is
idempotent and safe to run repeatedly. Snapshots are never deleted automatically;
clean them up manually when you no longer need them.
