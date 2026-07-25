<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Container release mode: digest-first GHCR publication

## Problem

Image-shaped projects need the same reproducible, signed release posture as
archive releases. A short roadmap entry established the tools but did not make
publication ordering, permissions, drift checks, or operator verification
explicit.

## Goals

- Configure an optional container stanza in Koryph's release contract.
- Build and publish GHCR images by immutable digest.
- Sign the published digest with Cosign.
- Generate an image SBOM with Syft and include the digest in provenance.
- Keep publication downstream of the successful release gate.
- Diagnose missing/stale configuration and workflow assets.

## Non-goals

- A general container build service.
- Mutable-tag-only publication.
- Registry credentials stored by Koryph.
- Support for registries other than GHCR in the first implementation.

## Current state

- `project.ReleaseConfig.Container` carries registry and image.
- `internal/forge/github/container-workflow.yml.tmpl` is the renderer asset.
- `internal/release/release.go` installs/removes the optional workflow.
- `internal/doctor/release_infra.go` owns release infrastructure checks.

## Decision ledger

| Decision | Rejected alternative | Invariant / consumers |
|---|---|---|
| Push by digest, then attach tags | Publish mutable tags as the identity | Signing, SBOM, and provenance identify immutable content |
| Publication depends on the successful release gate | Independent tag-triggered publish | A failed release gate cannot publish an image |
| `packages: write` and OIDC permissions are job-scoped | Workflow-global write permissions | Build/validation jobs remain read-only |
| Cosign signs the registry digest | Sign a local tarball only | Consumers can verify the exact pulled image |
| Syft scans the published image/digest | Repository-only source SBOM | SBOM describes shipped filesystem contents |
| Renderer output is desired state | Hand-maintained workflow | Setup and doctor can detect/reconcile drift |

## Design

The release configuration validates a GHCR registry/image pair. The GitHub
renderer builds once, pushes by digest, records that digest as an output, and
uses it as the subject for signing, SBOM, and provenance. Publication runs
only after the release gate succeeds. Registry/OIDC permissions exist only on
the publishing job.

`release setup` installs the rendered asset. Doctor compares configuration,
expected rendered bytes, required permission placement, and artifact steps.
Documentation explains configuration, produced artifacts, `cosign verify`,
SBOM inspection, and how stale assets are repaired.

## Implementation units

| Unit | Owned paths | Dependencies | Acceptance |
|---|---|---|---|
| Contract and renderer | project release schema, GitHub renderer/template, release installer | none | Digest-first gated publication with scoped permissions |
| Validation and docs | doctor release check, user/developer docs | renderer | Drift findings and verification instructions |

## Acceptance criteria

- Invalid/missing container configuration fails before rendering.
- Publication cannot run when the release gate fails.
- Signing, SBOM, and provenance all reference the published digest.
- Write/OIDC permissions are scoped to the publishing job.
- Doctor identifies missing and byte-stale workflow assets.
- Documentation covers configuration, artifacts, and verification.
- Renderer/doctor tests and `make gate-agent` pass.

## Open questions / assumptions

- GitHub-hosted runners provide the OIDC and GHCR environment assumed by the
  template; fake-render tests remain the normal local gate.
