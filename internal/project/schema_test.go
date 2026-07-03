// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// repoRoot is the module root relative to this package's directory, matching
// the anchor the go:generate emitter (internal/project/gen) uses.
const repoRoot = "../.."

// TestSchemaNoDrift regenerates the JSON Schema from the live Config struct and
// asserts it byte-matches the committed docs/schema/koryph.project.schema.json.
// This is the drift guard: any change to Config, its field docs, or its
// jsonschema tags that is not accompanied by `go generate ./internal/project`
// fails `go test`, so the committed schema cannot silently diverge from the
// structs. No Makefile change is needed — the gate runs `go test ./...`.
func TestSchemaNoDrift(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(repoRoot, SchemaRelPath))
	if err != nil {
		t.Fatalf("read committed schema (run `go generate ./internal/project`): %v", err)
	}

	got, err := GenerateSchema(repoRoot)
	if err != nil {
		t.Fatalf("GenerateSchema: %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("committed schema %s is stale; run `go generate ./internal/project` and commit the result", SchemaRelPath)
	}
}

// TestSchemaWellFormed sanity-checks the generated document: it parses as JSON,
// declares the 2020-12 meta-schema, and carries the load-bearing enums/ranges
// that editors rely on for validation.
func TestSchemaWellFormed(t *testing.T) {
	raw, err := GenerateSchema(repoRoot)
	if err != nil {
		t.Fatalf("GenerateSchema: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if s, _ := doc["$schema"].(string); s != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("unexpected $schema meta-reference: %q", s)
	}

	props, ok := doc["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties object")
	}
	// Spot-check a required top-level field and a constrained field so a
	// regression in the emitter (dropped fields, lost tags) is caught here.
	for _, key := range []string{"merge_policy", "merge_method", "risk_tier_default", "signing", "area_map"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema missing property %q", key)
		}
	}
	rt, _ := props["risk_tier_default"].(map[string]any)
	if rt["maximum"] != float64(3) || rt["minimum"] != float64(0) {
		t.Errorf("risk_tier_default range not [0,3]: %v", rt)
	}
}
