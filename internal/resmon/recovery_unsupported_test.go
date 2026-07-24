// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build !linux

package resmon

import (
	"errors"
	"testing"
)

func TestStopChildlessFailsClosedWithoutStableHandle(t *testing.T) {
	checkpointed := false
	signalled, err := StopChildless(t.Context(), 123, "birth", func() error {
		checkpointed = true
		return nil
	})
	if signalled || checkpointed {
		t.Fatalf("StopChildless = signalled %v checkpointed %v, want false/false", signalled, checkpointed)
	}
	if !errors.Is(err, ErrStableProcessHandleUnavailable) {
		t.Fatalf("StopChildless error = %v, want ErrStableProcessHandleUnavailable", err)
	}
}
