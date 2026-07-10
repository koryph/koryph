// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package schemaver

import (
	"errors"
	"testing"
)

func TestCheckReadAllowsSameOrOlderAndLegacy(t *testing.T) {
	cur := Current(Registry)
	for _, onDisk := range []int{0, 1, cur} {
		if onDisk > cur {
			continue
		}
		if err := CheckRead(Registry, onDisk); err != nil {
			t.Errorf("CheckRead(Registry, %d) = %v, want nil (same-or-older/legacy must load)", onDisk, err)
		}
	}
}

func TestCheckReadRefusesNewer(t *testing.T) {
	cur := Current(Registry)
	err := CheckRead(Registry, cur+1)
	if err == nil {
		t.Fatalf("CheckRead(Registry, %d) = nil, want a refusal (newer-than-supported must not load)", cur+1)
	}
	var tn *TooNewError
	if !errors.As(err, &tn) {
		t.Fatalf("error type = %T, want *TooNewError", err)
	}
	if tn.OnDisk != cur+1 || tn.Supported != cur || tn.Op != "read" {
		t.Errorf("TooNewError = %+v, want OnDisk=%d Supported=%d Op=read", tn, cur+1, cur)
	}
}

func TestCheckWriteRefusesNewer(t *testing.T) {
	cur := Current(Quota)
	if err := CheckWrite(Quota, cur); err != nil {
		t.Errorf("CheckWrite(Quota, %d) = %v, want nil", cur, err)
	}
	err := CheckWrite(Quota, cur+5)
	var tn *TooNewError
	if !errors.As(err, &tn) || tn.Op != "write" {
		t.Fatalf("CheckWrite newer = %v, want *TooNewError Op=write", err)
	}
}

func TestCurrentPanicsOnUnknownSurface(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Current(unknown) did not panic")
		}
	}()
	_ = Current(Surface("does-not-exist"))
}

func TestSurfacesCoversAll(t *testing.T) {
	want := map[Surface]bool{
		Registry: true, Quota: true, SigningVault: true,
		Project: true, LedgerRun: true, LedgerManifest: true,
	}
	got := Surfaces()
	if len(got) != len(want) {
		t.Fatalf("Surfaces() returned %d, want %d", len(got), len(want))
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected surface %q", s)
		}
	}
}
