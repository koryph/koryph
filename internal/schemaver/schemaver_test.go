// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package schemaver

import (
	"encoding/json"
	"errors"
	"fmt"
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
		Registry: true, Quota: true, Governor: true, SigningVault: true,
		GlobalConfig: true, Project: true, LedgerRun: true, LedgerManifest: true,
		AuditLog: true, Telemetry: true,
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
	delete(migrations, Registry)
	t.Cleanup(func() {
		if hadOriginal {
			migrations[Registry] = original
		} else {
			delete(migrations, Registry)
		}
	})
	if err := RegisterMigration(Registry, 0, func(state map[string]json.RawMessage) (map[string]json.RawMessage, error) {
		state["renamed"] = state["old_name"]
		delete(state, "old_name")
		return state, nil
	}); err != nil {
		t.Fatalf("RegisterMigration() = %v", err)
	}
	if err := RegisterMigration(Registry, 1, func(state map[string]json.RawMessage) (map[string]json.RawMessage, error) {
		if _, ok := state["renamed"]; !ok {
			return nil, errors.New("v0-to-v1 step did not run first")
		}
		state["second_step"] = json.RawMessage("true")
		return state, nil
	}); err != nil {
		t.Fatalf("RegisterMigration() second step = %v", err)
	}

	raw, changed, err := Migrate(Registry, []byte(`{"old_name":"value","unknown":{"keep":true}}`))
	if err != nil {
		t.Fatalf("Migrate = %v", err)
	}
	if !changed {
		t.Fatal("Migrate() changed = false, want true after v0-to-v1 step")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode migrated state: %v", err)
	}
	if got["schema_version"] != float64(Current(Registry)) || got["renamed"] != "value" || got["second_step"] != true {
		t.Errorf("migrated state = %#v, want schema_version=%d, renamed=value, and second_step=true", got, Current(Registry))
	}
	if unknown, ok := got["unknown"].(map[string]any); !ok || unknown["keep"] != true {
		t.Errorf("unknown field was not preserved: %#v", got["unknown"])
	}

	again, changed, err := Migrate(Registry, raw)
	if err != nil {
		t.Fatalf("second Migrate = %v", err)
	}
	if changed {
		t.Fatal("second Migrate() changed = true, want false for current state")
	}
	if string(again) != string(raw) {
		t.Errorf("Migrate must be idempotent: first %s, second %s", raw, again)
	}
}

func TestRegisterMigrationRejectsDuplicateAndUnknownSurface(t *testing.T) {
	if err := RegisterMigration(Surface("unknown"), 0, noOpMigration); err == nil || !strings.Contains(err.Error(), "unknown surface") {
		t.Fatalf("RegisterMigration(unknown) error = %v, want unknown-surface error", err)
	}
	original, hadOriginal := migrations[Quota]
	delete(migrations, Quota)
	t.Cleanup(func() {
		if hadOriginal {
			migrations[Quota] = original
		} else {
			delete(migrations, Quota)
		}
	})
	if err := RegisterMigration(Quota, 0, noOpMigration); err != nil {
		t.Fatalf("RegisterMigration() = %v", err)
	}
	if err := RegisterMigration(Quota, 0, noOpMigration); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("RegisterMigration(duplicate) error = %v, want duplicate refusal", err)
	}
}

func TestMigrateRejectsUnknownSurfaceAndMissingStep(t *testing.T) {
	if _, _, err := Migrate(Surface("unknown"), []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "unknown surface") {
		t.Fatalf("Migrate(unknown) error = %v, want unknown-surface error", err)
	}
	if _, _, err := Migrate(Quota, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "no migration") {
		t.Fatalf("Migrate(missing step) error = %v, want missing-step error", err)
	}
}

func TestMigrateRefusesNewerState(t *testing.T) {
	_, _, err := Migrate(SigningVault, []byte(`{"schema_version":2}`))
	var tooNew *TooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("Migrate(newer) error = %v, want *TooNewError", err)
	}
}

func TestVerifyFingerprintRejectsMalformedHash(t *testing.T) {
	err := VerifyFingerprint(Registry, []byte("1 "+strings.Repeat("z", 64)), struct{}{})
	if err == nil || !strings.Contains(err.Error(), "invalid sha256") {
		t.Fatalf("VerifyFingerprint() error = %v, want invalid-sha256 error", err)
	}
}

func TestVerifyFingerprintRejectsShapeMismatchAtExistingVersion(t *testing.T) {
	history := fmt.Sprintf("%d %s", Current(Registry), strings.Repeat("a", 64))
	err := VerifyFingerprint(Registry, []byte(history), struct {
		Name string `json:"name"`
	}{})
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("VerifyFingerprint() error = %v, want shape mismatch", err)
	}
}

func TestAppendFingerprintAddsVersionBumpAndRefusesOverwrite(t *testing.T) {
	original := current[Registry]
	current[Registry] = original + 1
	t.Cleanup(func() { current[Registry] = original })

	history := []byte(fmt.Sprintf("# schema-version persisted-shape-sha256\n%d %s\n", original, strings.Repeat("a", 64)))
	type v2Record struct {
		Name string `json:"name"`
	}
	appended, err := AppendFingerprint(Registry, history, v2Record{})
	if err != nil {
		t.Fatalf("AppendFingerprint() = %v", err)
	}
	if !strings.HasPrefix(string(appended), string(history)) {
		t.Fatalf("AppendFingerprint() rewrote history:\n%s", appended)
	}
	if err := VerifyFingerprint(Registry, appended, v2Record{}); err != nil {
		t.Fatalf("VerifyFingerprint(appended) = %v", err)
	}
	if _, err := AppendFingerprint(Registry, appended, v2Record{}); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("AppendFingerprint(existing version) error = %v, want overwrite refusal", err)
	}
}
