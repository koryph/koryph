// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package schemaver_test

import (
	_ "embed"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/schemaver"
	"github.com/koryph/koryph/internal/signing"
)

//go:embed fingerprints.golden
var fingerprintsGolden []byte

func TestPersistedSurfaceFingerprints(t *testing.T) {
	types := map[schemaver.Surface]any{
		schemaver.Registry:       registry.Record{},
		schemaver.Quota:          quota.Config{},
		schemaver.SigningVault:   signing.VaultConfig{},
		schemaver.Project:        project.Config{},
		schemaver.LedgerRun:      ledger.Run{},
		schemaver.LedgerManifest: ledger.Manifest{},
	}
	if err := schemaver.VerifyFingerprint(fingerprintsGolden, types); err != nil {
		t.Fatal(err)
	}
}
