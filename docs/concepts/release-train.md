<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Shipping: the release train and the supply chain

*This page expands the [Concepts overview](index.md). See
[Verifying a release](../user-guide/supply-chain.md) for the commands that
operate it.*

## The idea

A release should be an outcome of ordinary work, not a project you schedule.
And once published, a release should let anyone — not just you — verify exactly
what they downloaded and that it came from your pipeline unaltered. koryph
treats shipping as a **train**: commits accumulate, and when you decide to
depart, a complete and verifiable set of artifacts leaves together.

Two properties matter. **Completeness before publication:** binaries, checksums,
SBOMs, signatures, and provenance all attach to a *draft* release, and only a
complete set is published — an immutable release never locks a half-built one.
**Independent verifiability:** every artifact carries the material a third party
needs to check integrity and authorship without trusting you.

## In koryph

The train runs on [Conventional Commits](../user-guide/running-waves.md). They
accumulate into a **Release PR** (release-please); merging that PR triggers
gate-before-tag, an artifact build (GoReleaser, or your own ordered commands for
any language), and a **draft-until-complete** release. Setup and the optional
keep-it-moving bot are one command each:

```bash
koryph release setup --project koryph --mode goreleaser --bot
koryph release kick --repo koryph/koryph --wait   # bot-less nudge for the PR
```

A published release carries the full supply-chain set:

```
koryph_0.5.0_darwin_arm64.tar.gz        # platform binary archive
<archive>.sbom.spdx.json                # per-archive SPDX 2.3 SBOM
checksums.txt                           # SHA-256 of every archive and SBOM
checksums.txt.sigstore.json             # cosign keyless (Sigstore) signature
checksums.txt.intoto.jsonl              # SLSA provenance attestation
```

Anyone can verify integrity and authorship:

```bash
sha256sum -c --ignore-missing checksums.txt
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '.../release-please.yml@refs/heads/main' \
  checksums.txt
```

## The failure mode it prevents

The partial release and the unverifiable one. Publishing artifacts as they build
means a consumer can download a binary whose checksum, signature, or SBOM hasn't
landed yet — and since releases are immutable, that incomplete set is what they
keep. Shipping without provenance means a tampered or look-alike artifact is
indistinguishable from the real one; "trust me, I built it" is not a security
property. Draft-until-complete guarantees consumers only ever see a whole set,
and keyless signing plus SLSA provenance let them prove origin against your
actual workflow identity — no shared secret, no trust in your word.

## Operate it

- [Verifying a release](../user-guide/supply-chain.md) — the full verification
  walkthrough.
- The gate-before-tag step reuses the same [green gate](worktrees.md) that
  guards every merge.
