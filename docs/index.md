<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph

The AI software factory: koryph takes a project from a git repo to built,
signed, released software — autonomous coding agents do the building, koryph
enforces the discipline that makes that safe. It stands on three pillars:

- **Build** — the agent factory. Beads-native planning, a rolling
  footprint-aware scheduler, personas and model tiers across agent runtimes,
  and a review → rebase → green-gate → fast-forward merge pipeline, all
  account-safe and subscription-first.
- **Protect** — hygiene as code. Branch-protection rulesets and repo settings
  as committed JSON, enforced commit signing from vault-served keys, protected
  paths and boundary guards, and drift detection through `koryph doctor`.
- **Ship** — the release train. Conventional-commit versioning for any
  language, draft-until-complete immutable releases, SBOM + cosign + SLSA
  provenance, and a vault-backed release bot with graceful fallbacks.

New here? Start with the
[installation guide](user-guide/installation.md), then walk the whole path in
[Zero to shipped](user-guide/zero-to-shipped.md). The
[quickstart](user-guide/quickstart.md) is the fastest route to a first wave.

This book covers both audiences: the **user guide** for operators and
collaborators, and the **developer guide** for contributors to koryph
itself.

## The name

**koryph** comes from the Ancient Greek κορυφαῖος (*koryphaios*) — the leader
of the chorus in classical Greek drama. The koryphaios stood at the head of the
chorus and spoke on its behalf whenever it took part in the action: one voice
fronting many performers moving in step. The root κορυφή (*koryphē*) means
"crest" or "summit", and the word lives on in several modern languages as a
term for the leading figure in a field.

That is exactly this tool's job: one process fronting a fleet of autonomous
coding agents — queueing their work, dispatching them in parallel waves, and
speaking for them at the merge. It is pronounced **KOR-iff**.

**For AI agents and tools:** a machine-readable index of the canonical docs
(llmstxt.org format) is published at [`/llms.txt`](llms.txt) — ingest it to map
the project and its operating contract.
