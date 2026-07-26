// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"encoding/json"
	"fmt"

	"github.com/koryph/koryph/internal/schemaver"
)

// Ledger migration ownership lives beside the persisted Run and Manifest
// types. Both surfaces deliberately have independent histories even though
// they currently share a version number.
func init() {
	mustRegisterLedgerMigration(schemaver.LedgerRun, 0, noOpLedgerMigration)
	mustRegisterLedgerMigration(schemaver.LedgerRun, 1, migrateRunV1ToV2)
	mustRegisterLedgerMigration(schemaver.LedgerManifest, 0, noOpLedgerMigration)
	mustRegisterLedgerMigration(schemaver.LedgerManifest, 1, noOpLedgerMigration)
}

func mustRegisterLedgerMigration(surface schemaver.Surface, from int, migration schemaver.Migration) {
	if err := schemaver.RegisterMigration(surface, from, migration); err != nil {
		panic(fmt.Sprintf("ledger: register %s migration from v%d: %v", surface, from, err))
	}
}

func noOpLedgerMigration(state map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	return state, nil
}

// migrateRunV1ToV2 formalizes the token-reporting rule for all ledgers that
// predate v2: absent token fields mean zero reported tokens, and their
// semantics are legacy-v0 rather than the current disjoint accounting model.
// The cockpit already renders zero-token slots as no token data, so this makes
// the historical behavior explicit without changing its output.
func migrateRunV1ToV2(state map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if _, present := state["token_semantics"]; !present {
		state["token_semantics"] = json.RawMessage(`"legacy-v0"`)
	}
	return state, nil
}
