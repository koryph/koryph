// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtimeconfig

import (
	"path/filepath"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
)

func TestGetBuiltinsHonorBinaryOverrides(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
	}{
		{name: "claude", env: EnvClaudeBin},
		{name: "codex", env: EnvCodexBin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), tc.name)
			t.Setenv(tc.env, bin)

			rt, ok := Get(tc.name)
			if !ok {
				t.Fatalf("Get(%q) was not registered", tc.name)
			}
			argv, _, err := rt.CommandJSON(runtime.JSONSpec{})
			if err != nil {
				t.Fatalf("CommandJSON: %v", err)
			}
			if len(argv) == 0 || argv[0] != bin {
				t.Fatalf("argv[0] = %q, want override %q", argv[0], bin)
			}
		})
	}
}
