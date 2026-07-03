<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Releasing & versioning

Koryph follows [Semantic Versioning 2.0.0](https://semver.org).

## Semver policy

| Change | Version component |
|---|---|
| Breaking change to the project JSON contract, CLI flags, or engine API | **MAJOR** |
| New backward-compatible feature or engine capability | **MINOR** |
| Bug fix, documentation, internal refactor | **PATCH** |

Pre-1.0 rule: minor bumps may carry breaking changes while the project is
`0.x`. Document every breaking change in the commit body and in the release
notes.

## `internal/version.Engine`

The single source of truth for the current engine version is the constant
`Engine` in `internal/version/version.go`:

```go
const Engine = "0.2.0"   // bump here before tagging
```

No other file hard-codes the version. Every component that needs to advertise
or check the engine version imports this package.

## Tagging a release

1. **Bump** `Engine` in `internal/version/version.go` per the policy above.
2. **Update** `koryph.project.json` → `"engine_version"` if the project
   itself requires the new minimum (usually after a MINOR or MAJOR bump).
3. **Commit** (Conventional Commits, see below), e.g.:
   ```
   chore(release): bump engine to 0.3.0
   ```
4. **Run the green gate** — all checks must be green:
   ```bash
   test -z "$(gofmt -l .)"
   go build ./...
   go vet ./...
   go test ./...
   ```
5. **Tag** with the `v` prefix:
   ```bash
   git tag -s v0.3.0 -m "chore(release): koryph 0.3.0"
   git push origin v0.3.0
   ```
   The tag name must be `v<Engine>` exactly (e.g., `v0.2.0`).

## `engine_version` pinning in projects

Projects declare a minimum required engine in `koryph.project.json`:

```json
{ "engine_version": "0.2+" }
```

`version.Satisfied(have, want)` enforces this at dispatch time. Accepted forms:

| Requirement | Meaning |
|---|---|
| `"0.2+"` | engine ≥ 0.2.0 |
| `"0.2"` | same as `"0.2+"` (bare = minimum) |
| `">=0.2"` | same as `"0.2+"` |
| `"0.2.3+"` | engine ≥ 0.2.3 |
| `"1+"` | engine ≥ 1.0.0 |
| `"v0.2+"` | leading `v` accepted everywhere |
| `""` | no requirement (always satisfied) |

Major versions are never cross-satisfied: `"0.2+"` is not met by engine
`1.0.0`. Pin to `"1+"` explicitly after a MAJOR bump.

## Conventional Commits

Every commit must follow Conventional Commits:

```
type(scope): subject in imperative mood, lowercase, ≤72 chars
```

Accepted types: `feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `ci`,
`build`, `perf`, `style`. Breaking changes add `!` after the type or scope
(`feat!:`) and a `BREAKING CHANGE:` footer.

Commits must also carry `Signed-off-by` (DCO) and be cryptographically signed
(see `CONTRIBUTING.md` at the repo root).

## Release checklist

```
[ ] Decide version bump (major / minor / patch)
[ ] Bump Engine constant in internal/version/version.go
[ ] Update engine_version in koryph.project.json if needed
[ ] Commit: chore(release): bump engine to X.Y.Z
[ ] Run green gate (gofmt / build / vet / test)
[ ] Tag: git tag -s vX.Y.Z -m "chore(release): koryph X.Y.Z"
[ ] Push tag: git push origin vX.Y.Z
[ ] Draft GitHub release — paste conventional-commit log since last tag
[ ] Verify CI passes on the tagged commit
```

## Signed releases (sigstore keyless)

Every `v*` tag triggers `.github/workflows/release.yml`: the green gate runs,
multi-platform binaries are built (`darwin`/`linux` × `amd64`/`arm64`), and
the `checksums.txt` manifest is signed **keylessly** with cosign — the
workflow's GitHub OIDC identity is the certificate subject (issued by
Fulcio, recorded in the Rekor transparency log). No signing key exists
anywhere, so there is nothing to leak or rotate.

To verify a release artifact:

```sh
sha256sum -c --ignore-missing checksums.txt   # binary matches the manifest

cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/koryph/koryph' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

A successful verification proves the manifest was produced by this
repository's release workflow — and the checksum match extends that trust
to the binary itself.
