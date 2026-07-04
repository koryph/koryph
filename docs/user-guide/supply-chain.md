<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Verifying a release

Every koryph release ships more than binaries: a checksum manifest, a
keyless cosign signature over that manifest, a SLSA provenance attestation,
and a per-archive SPDX software bill of materials (SBOM). Together they let
you prove — without trusting anything but the forge's OIDC issuer and the
public [Rekor](https://docs.sigstore.dev/logging/overview/) transparency
log — that a download came from this repository's release workflow and
matches exactly what it built.

This chapter walks through consuming those assets after downloading a
release. For how they're produced, see
[Releasing & versioning](../developer-guide/releasing.md).

!!! note "Forge coverage"
    Koryph releases are built on **GitHub** (the reference forge), so the OIDC
    issuer is `token.actions.githubusercontent.com` and full **SLSA Build L3**
    provenance is available via the SLSA GitHub Generator. On **GitLab**, cosign
    keyless signing works against the GitLab CI OIDC issuer, but the SLSA
    generator is GitHub-specific — provenance is reduced to cosign authenticity
    plus (on GitLab 16.1+) artifact attestations. See
    [Choosing a forge](forges.md#capability-differences) for the honest gap.

## What's on the release page

For each tagged release (e.g. `v0.5.0`) the
[Releases page](https://github.com/koryph/koryph/releases) carries:

| Asset | What it is |
|---|---|
| `koryph_<version>_<os>_<arch>.tar.gz` | A platform binary archive (darwin/linux × amd64/arm64). |
| `<archive>.sbom.spdx.json` | A per-archive SPDX 2.3 software bill of materials, one per platform tarball. |
| `checksums.txt` | SHA-256 of every archive and SBOM in the release. |
| `checksums.txt.sigstore.json` | A cosign keyless (Sigstore) signature bundle over `checksums.txt` — certificate + signature combined. |
| `checksums.txt.intoto.jsonl` | A SLSA v0.2 provenance attestation (DSSE envelope) for `checksums.txt`, produced by the SLSA generic generator. |

Only `checksums.txt` is directly signed and attested. Because it lists the
SHA-256 of every other asset, verifying it and then checking a downloaded
file's hash against its line in the manifest **transitively** authenticates
that file too — you don't need a separate signature per archive.

## 1. Integrity: does the download match the manifest?

Download `checksums.txt` alongside whichever archive(s) you need, then
check the archive's hash against its line in the manifest:

```sh
# macOS / BSD
shasum -a 256 -c --ignore-missing checksums.txt

# Linux (GNU coreutils)
sha256sum -c --ignore-missing checksums.txt
```

Expected output (one `OK` line per file you actually downloaded — anything
else you didn't fetch is skipped by `--ignore-missing`):

```
koryph_0.5.0_darwin_arm64.tar.gz: OK
```

If this fails, stop — do not run the archive. Either the download was
corrupted in transit, or the file does not match what the release actually
published.

## 2. Authenticity: was this manifest produced by koryph's release workflow?

`checksums.txt` is signed **keylessly** with [cosign](https://docs.sigstore.dev/):
the signing identity is a short-lived certificate binding the exact GitHub
Actions workflow file and ref that ran, issued by Sigstore's Fulcio CA and
recorded in the public Rekor log. There is no long-lived private key to
leak, steal, or rotate.

Verify with `cosign verify-blob`, pinning both the OIDC issuer and the
workflow identity that must have produced the signature:

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/koryph/koryph/\.github/workflows/release-please\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

Expected output:

```
Verified OK
```

The `--certificate-identity-regexp` above is anchored to the *exact*
workflow file (`.github/workflows/release-please.yml`) and ref
(`refs/heads/main`) that GitHub's OIDC token attests to for this repo's
release pipeline — not just the repository name — so a signature minted by
some other workflow in this repo (or a fork) will not pass. Passing
verification proves `checksums.txt` was produced by that workflow run, and
the integrity check in step 1 extends that trust to every archive whose
hash it lists.

## 3. Provenance: was it built the way it claims to be?

For stronger, non-forgeable assurance than a signature alone, verify the
SLSA provenance attestation with
[`slsa-verifier`](https://github.com/slsa-framework/slsa-verifier):

```sh
go install github.com/slsa-framework/slsa-verifier/v2/cli/slsa-verifier@v2.7.1
# or download a prebuilt binary from its releases page

slsa-verifier verify-artifact checksums.txt \
  --provenance-path checksums.txt.intoto.jsonl \
  --source-uri github.com/koryph/koryph \
  --source-branch main
```

Expected output:

```
Verified build using builder "https://github.com/slsa-framework/slsa-github-generator/.github/workflows/generator_generic_slsa3.yml@refs/tags/v2.1.0" at commit <sha>
Verifying artifact checksums.txt: PASSED

PASSED: SLSA verification passed
```

**Use `--source-branch main`, not `--source-tag`.** koryph's release
pipeline runs as a single workflow triggered by a **push to `main`** (see
[Releasing & versioning](../developer-guide/releasing.md#why-one-workflow-not-two)
for why), and the provenance's build invocation records that trigger —
`refs/heads/main` — not the release tag it eventually pushes. Passing
`--source-tag v0.5.0` fails with `invalid ref` because the attestation
never claims a tag ref; `--source-branch main`, or omitting the ref check
entirely (`--source-uri` alone), matches what was actually attested.

A `PASSED` result proves `checksums.txt` (and, via the checksum chain in
step 1, every archive it lists) was built by this repository's GitHub
Actions workflow at the recorded commit — Build Level 3 provenance, not
just a signature.

## 4. Consuming the SBOMs

Each platform archive carries a companion SPDX 2.3 SBOM,
`<archive>.sbom.spdx.json`, generated by [syft](https://github.com/anchore/syft)
during the same build that produced the archive. It enumerates every Go
module compiled into that binary, with checksums, package URLs (purl), and
CPE identifiers for vulnerability matching.

List every package and version:

```sh
jq -r '.packages[] | "\(.name) \(.versionInfo)"' koryph_0.5.0_linux_amd64.tar.gz.sbom.spdx.json
```

Check declared licenses (koryph's own Go-module SBOMs mostly read
`NOASSERTION` — syft did not find embedded license metadata for those
modules — so treat this as a starting point for your own license review,
not a final answer):

```sh
jq -r '.packages[] | "\(.name): \(.licenseDeclared)"' koryph_0.5.0_linux_amd64.tar.gz.sbom.spdx.json
```

Feed the SBOM to a vulnerability scanner instead of re-deriving the
dependency list yourself. For example, with
[grype](https://github.com/anchore/grype):

```sh
grype sbom:koryph_0.5.0_linux_amd64.tar.gz.sbom.spdx.json
```

or [osv-scanner](https://github.com/google/osv-scanner):

```sh
osv-scanner --sbom koryph_0.5.0_linux_amd64.tar.gz.sbom.spdx.json
```

Neither tool is required to use koryph — they're independent, optional
scanners that happen to consume the SPDX format koryph already publishes.

## Putting it together

A minimal end-to-end check for one platform archive:

```sh
tag=v0.5.0
archive=koryph_0.5.0_darwin_arm64.tar.gz

gh release download "$tag" --repo koryph/koryph \
  -p "$archive" -p "$archive.sbom.spdx.json" \
  -p checksums.txt -p checksums.txt.sigstore.json -p checksums.txt.intoto.jsonl

shasum -a 256 -c --ignore-missing checksums.txt

cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/koryph/koryph/\.github/workflows/release-please\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

A downloaded archive that passes all three (checksum, signature, and
optionally SLSA provenance) is verifiably the binary koryph's own release
workflow built and published for that tag.
