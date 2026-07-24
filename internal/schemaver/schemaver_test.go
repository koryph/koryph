// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package schemaver

import (
	"encoding/json"
	"errors"
	"strings"
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

func TestMigrateRunsOrderedStepsAndPreservesUnknownFields(t *testing.T) {
	original, hadOriginal := migrations[Registry]
	migrations[Registry] = []Migration{func(state map[string]json.RawMessage) (map[string]json.RawMessage, error) {
		state["renamed"] = state["old_name"]
		delete(state, "old_name")
		return state, nil
	}}
	t.Cleanup(func() {
		if hadOriginal {
			migrations[Registry] = original
		} else {
			delete(migrations, Registry)
		}
	})

	raw, err := Migrate(Registry, []byte(`{"old_name":"value","unknown":{"keep":true}}`))
	if err != nil {
		t.Fatalf("Migrate = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode migrated state: %v", err)
	}
	if got["schema_version"] != float64(1) || got["renamed"] != "value" {
		t.Errorf("migrated state = %#v, want schema_version=1 and renamed=value", got)
	}
	if unknown, ok := got["unknown"].(map[string]any); !ok || unknown["keep"] != true {
		t.Errorf("unknown field was not preserved: %#v", got["unknown"])
	}

	again, err := Migrate(Registry, raw)
	if err != nil {
		t.Fatalf("second Migrate = %v", err)
	}
	if string(again) != string(raw) {
		t.Errorf("Migrate must be idempotent: first %s, second %s", raw, again)
	}
}

func TestMigrateRejectsUnknownSurfaceAndMissingStep(t *testing.T) {
	if _, err := Migrate(Surface("unknown"), []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "unknown surface") {
		t.Fatalf("Migrate(unknown) error = %v, want unknown-surface error", err)
	}
	if _, err := Migrate(Quota, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "no migration") {
		t.Fatalf("Migrate(missing step) error = %v, want missing-step error", err)
	}
}

func TestMigrateLiftsLegacyLedgerState(t *testing.T) {
	for _, surface := range []Surface{LedgerRun, LedgerManifest} {
		for _, raw := range [][]byte{
			[]byte(`{"legacy":"v0","unknown":{"keep":true}}`),
			[]byte(`{"schema_version":1,"legacy":"v1","unknown":{"keep":true}}`),
		} {
			t.Run(string(surface)+"/"+string(raw), func(t *testing.T) {
				migrated, err := Migrate(surface, raw)
				if err != nil {
					t.Fatalf("Migrate(%s) = %v", surface, err)
				}
				var got map[string]any
				if err := json.Unmarshal(migrated, &got); err != nil {
					t.Fatalf("decode migrated state: %v", err)
				}
				if got["schema_version"] != float64(Current(surface)) {
					t.Errorf("schema_version = %#v, want %d", got["schema_version"], Current(surface))
				}
				if unknown, ok := got["unknown"].(map[string]any); !ok || unknown["keep"] != true {
					t.Errorf("unknown field was not preserved: %#v", got["unknown"])
				}
			})
		}
	}
}

func TestMigrateRefusesNewerState(t *testing.T) {
	_, err := Migrate(SigningVault, []byte(`{"schema_version":2}`))
	var tooNew *TooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("Migrate(newer) error = %v, want *TooNewError", err)
	}
}
