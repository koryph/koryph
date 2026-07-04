// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import "github.com/koryph/koryph/internal/runtime"

// init registers the default-binary claude adapter into runtime.Default —
// the registry's first real entry (koryph-v8u.2). Any real koryph binary
// already imports this package transitively (internal/dispatch delegates
// argv/env/parse to it), so this registration happens automatically; no
// separate wiring call is required. A panic here means two packages tried to
// register "claude" (a programming error caught at process start, not a
// runtime condition to recover from).
func init() {
	if err := runtime.Default.Register(New("")); err != nil {
		panic("runtime/claude: registering default adapter: " + err.Error())
	}
}
