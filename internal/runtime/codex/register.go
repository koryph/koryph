// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import "github.com/koryph/koryph/internal/runtime"

func init() {
	if err := runtime.Default.Register(New("")); err != nil {
		panic("runtime/codex: registering default adapter: " + err.Error())
	}
}
