<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Security Policy

## Reporting a Vulnerability

**Please do NOT open a public GitHub issue for security vulnerabilities.**

Report vulnerabilities privately via **GitHub Security Advisories**:

1. Go to the **Security** tab of this repository.
2. Click **Report a vulnerability**.
3. Fill in the details and submit.

We will acknowledge receipt and coordinate disclosure privately before any
public announcement.

## Supported Versions

| Version | Supported |
|---------|-----------|
| 0.2.x   | ✅ Yes    |
| < 0.2   | ❌ No     |

## Response

Responses are **best-effort**. There is **no guaranteed SLA** for fixes or
disclosures. We will make a reasonable effort to respond promptly to critical
issues, but timelines depend on severity, complexity, and maintainer
availability.

## Scope and Operator Responsibility

> ⚠️ **Important**: koryph orchestrates **autonomous AI agents with shell
> access**. This fundamentally expands the attack surface beyond a typical
> software tool.

**Operators are solely responsible for:**

- **Sandboxing** — isolating agents from sensitive systems, networks, and
  credentials they should not access.
- **Credentials and accounts** — what API keys, tokens, and cloud accounts
  are made available to agents.
- **Agent actions** — all commands, file writes, API calls, and network
  requests that agents execute.
- **Costs incurred** — any charges from LLM providers, cloud services, or
  other APIs invoked by agents.

The shipped `hooks/`, protected-path enforcement, and deny mechanisms are
**defense-in-depth controls, not security boundaries**. They reduce risk but
cannot substitute for proper sandboxing and least-privilege configuration at
the infrastructure level.

Do not assume that koryph's internal controls are sufficient to prevent a
compromised or misbehaving agent from causing harm in your environment.
