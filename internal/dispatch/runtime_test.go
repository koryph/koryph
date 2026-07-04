// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package dispatch

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// TestDispatchInjectedRuntimeVerifyIdentityFailureBlocksDispatch proves
// CLIBackend.Runtime (koryph-v8u.5) is a real seam, not decorative: a
// non-claude runtime.Runtime whose VerifyIdentity fails closed refuses
// Dispatch exactly like the default claude adapter's identity mismatch does
// (TestDispatchIdentityMismatchWritesNothing) — no filesystem effect, error
// surfaced verbatim.
func TestDispatchInjectedRuntimeVerifyIdentityFailureBlocksDispatch(t *testing.T) {
	spec := baseSpec(t)
	stub := runtimetest.Stub{VerifyErr: errors.New("stub: not logged in")}
	b := CLIBackend{ClaudeBin: fakeClaude(t), Runtime: stub}

	_, err := b.Dispatch(context.Background(), spec)
	if err == nil {
		t.Fatal("Dispatch succeeded despite the injected runtime's failing VerifyIdentity; must fail closed")
	}
	if !strings.Contains(err.Error(), "stub: not logged in") {
		t.Errorf("err = %v, want the stub's VerifyErr surfaced verbatim", err)
	}
	if _, statErr := os.Stat(spec.PhaseDir); !os.IsNotExist(statErr) {
		t.Errorf("PhaseDir created despite identity refusal (stat err %v)", statErr)
	}
}

// TestDispatchInjectedRuntimeUsedForCommand proves Dispatch routes argv/env
// construction through the injected runtime.Runtime too (not just identity),
// so a future non-claude adapter is a drop-in Backend without CLIBackend
// changes.
func TestDispatchInjectedRuntimeUsedForCommand(t *testing.T) {
	spec := baseSpec(t)
	// Stub.Command hard-errors on any spec field not backed by a matching
	// Capabilities flag (koryph-v8u.1's capability-gating contract) — clear
	// the ones baseSpec sets that this minimal stub doesn't declare.
	spec.Persona = ""
	spec.Effort = ""
	spec.MaxBudgetUSD = 0
	stub := runtimetest.Stub{
		Caps: runtime.Capabilities{ModelSelect: true},
	}
	b := CLIBackend{Runtime: stub}

	h, err := b.Dispatch(context.Background(), spec)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if h.VerifiedIdentity != spec.ExpectedIdentity {
		t.Errorf("VerifiedIdentity = %q, want the stub's echoed expected identity %q", h.VerifiedIdentity, spec.ExpectedIdentity)
	}
	launch, err := os.ReadFile(h.LaunchPath)
	if err != nil {
		t.Fatalf("reading launch.sh: %v", err)
	}
	if !strings.Contains(string(launch), "'stub' 'run' '--session-id' '"+spec.SessionID+"'") {
		t.Errorf("launch.sh does not embed the stub runtime's own argv shape:\n%s", launch)
	}
}
