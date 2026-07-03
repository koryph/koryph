// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package agents exposes the embedded fallback persona definitions shipped
// with the koryph binary. Each *.md file is a Claude sub-agent persona
// intended for the target project's .claude/agents directory.
package agents

import "embed"

// FS holds every *.md persona bundled at compile time.
//
//go:embed *.md
var FS embed.FS
