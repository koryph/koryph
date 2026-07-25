// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package schemaver_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/schemaver"
	"github.com/koryph/koryph/internal/signing"
)

func TestPersistedSurfaceFingerprints(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Clean(filepath.Join(root, "..", ".."))
	for _, test := range []struct {
		surface schemaver.Surface
		path    string
		value   any
	}{
		{schemaver.Registry, "internal/registry/registry.fingerprints", registry.Record{}},
		{schemaver.Quota, "internal/quota/quota.fingerprints", quota.Config{}},
		{schemaver.Governor, "internal/govern/governor.fingerprints", govern.File{}},
		{schemaver.SigningVault, "internal/signing/vault.fingerprints", signing.VaultConfig{}},
		{schemaver.GlobalConfig, "internal/signing/global_config.fingerprints", signing.GlobalConfig{}},
		{schemaver.Project, "internal/project/project.fingerprints", project.Config{}},
		{schemaver.LedgerRun, "internal/ledger/run.fingerprints", ledger.Run{}},
		{schemaver.LedgerManifest, "internal/ledger/manifest.fingerprints", ledger.Manifest{}},
		{schemaver.AuditLog, "internal/registry/audit_log.fingerprints", registry.Event{}},
		{schemaver.Telemetry, "internal/obs/telemetry.fingerprints", obs.TelemetryRecord{}},
	} {
		t.Run(string(test.surface), func(t *testing.T) {
			history, err := os.ReadFile(filepath.Join(root, test.path))
			if err != nil {
				t.Fatal(err)
			}
			if err := schemaver.VerifyFingerprint(test.surface, history, test.value); err != nil {
				t.Fatal(err)
			}
		})
	}
}
