// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

import "io"

// ActivityKind classifies one runtime-neutral entry in an agent's live trace.
type ActivityKind int

const (
	ActThinking ActivityKind = iota
	ActToolUse
	ActMessage
	ActSession
	ActResult
	ActError
)

// ActivityEntry is the portable activity projection consumed by operator UIs.
// Parent is optional nested-agent attribution; Tool is set for ActToolUse.
type ActivityEntry struct {
	Kind   ActivityKind
	Parent string
	Tool   string
	Text   string
}

// ActivityScanner incrementally projects an append-only runtime JSONL stream.
type ActivityScanner interface {
	Write([]byte)
	Entries() []ActivityEntry
}

// ActivityProjector is an optional Runtime capability. It is deliberately
// separate from Runtime so a future adapter can dispatch safely before its
// native activity stream has been normalized.
type ActivityProjector interface {
	ExtractActivity(io.Reader) []ActivityEntry
	NewActivityScanner() ActivityScanner
}
