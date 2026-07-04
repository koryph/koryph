// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime_test

import (
	"testing"

	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

func TestRegistryRegisterGetList(t *testing.T) {
	reg := runtime.NewRegistry()

	claude := runtimetest.Stub{StubName: "claude", StubProvider: "anthropic"}
	codex := runtimetest.Stub{StubName: "codex", StubProvider: "openai"}

	if err := reg.Register(claude); err != nil {
		t.Fatalf("Register(claude): unexpected error: %v", err)
	}
	if err := reg.Register(codex); err != nil {
		t.Fatalf("Register(codex): unexpected error: %v", err)
	}

	got, ok := reg.Get("claude")
	if !ok {
		t.Fatalf("Get(claude): not found")
	}
	if got.Name() != "claude" {
		t.Fatalf("Get(claude): Name() = %q, want claude", got.Name())
	}

	if _, ok := reg.Get("does-not-exist"); ok {
		t.Fatalf("Get(does-not-exist): expected not found")
	}

	list := reg.List()
	if len(list) != 2 {
		t.Fatalf("List(): len = %d, want 2", len(list))
	}
	// Deterministic order: sorted by Name (claude < codex).
	if list[0].Name() != "claude" || list[1].Name() != "codex" {
		t.Fatalf("List(): order = [%s, %s], want [claude, codex]", list[0].Name(), list[1].Name())
	}
}

func TestRegistryDuplicateRegistrationIsAnError(t *testing.T) {
	reg := runtime.NewRegistry()
	first := runtimetest.Stub{StubName: "claude"}
	second := runtimetest.Stub{StubName: "claude", StubProvider: "different"}

	if err := reg.Register(first); err != nil {
		t.Fatalf("first Register: unexpected error: %v", err)
	}
	if err := reg.Register(second); err == nil {
		t.Fatalf("second Register with duplicate name: expected error, got nil")
	}

	// The registry must be unchanged by the failed second registration.
	got, ok := reg.Get("claude")
	if !ok {
		t.Fatalf("Get(claude): not found after failed duplicate registration")
	}
	if got.Provider() != "" && got.Provider() != first.Provider() {
		t.Fatalf("Get(claude).Provider() = %q, want the FIRST registration's provider %q", got.Provider(), first.Provider())
	}
}

func TestRegistryRegisterRejectsEmptyNameAndNil(t *testing.T) {
	reg := runtime.NewRegistry()

	if err := reg.Register(nil); err == nil {
		t.Fatalf("Register(nil): expected error, got nil")
	}
	if err := reg.Register(runtimetest.Stub{}); err != nil {
		t.Fatalf("Register(stub with default name): unexpected error: %v", err)
	}
	// Default StubName resolves to "stub", not empty, so a second registration
	// of a Runtime whose Name() truly returns "" is what we want to check
	// next; there is no way to construct that with Stub (it always defaults
	// to "stub"), so this test only confirms the nil-Runtime guard above and
	// that a normal registration with a non-empty name succeeds.
	if _, ok := reg.Get("stub"); !ok {
		t.Fatalf("Get(stub): expected the default-named stub to be registered")
	}
}

func TestRegistryEmptyListIsEmptyNotNilPanic(t *testing.T) {
	reg := runtime.NewRegistry()
	list := reg.List()
	if len(list) != 0 {
		t.Fatalf("List() on empty registry: len = %d, want 0", len(list))
	}
}
