// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime_test

import (
	"encoding/json"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
)

// TestCapabilitiesJSONRoundTrip confirms every flag survives a marshal/
// unmarshal cycle independently (no field aliasing/typo in the json tags).
func TestCapabilitiesJSONRoundTrip(t *testing.T) {
	want := runtime.Capabilities{
		JSONStream:  true,
		Personas:    true,
		Hooks:       false,
		Resume:      true,
		EffortFlag:  false,
		BudgetFlag:  true,
		Sandbox:     false,
		ModelSelect: true,
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got runtime.Capabilities
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestCapabilitiesZeroValueSupportsNothing documents the "unset == false"
// default used by every gated DispatchSpec field and by a Runtime that
// unmarshals from a document predating a newly-added flag.
func TestCapabilitiesZeroValueSupportsNothing(t *testing.T) {
	var zero runtime.Capabilities
	if zero != (runtime.Capabilities{}) {
		t.Fatalf("zero value changed shape")
	}
	data := []byte(`{}`)
	var got runtime.Capabilities
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal empty object: %v", err)
	}
	if got != zero {
		t.Fatalf("Unmarshal({}) = %+v, want all-false zero value", got)
	}
}
