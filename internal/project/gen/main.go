// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Command gen emits the JSON Schema for koryph.project.json from the Go
// project.Config struct and writes it to docs/schema/koryph.project.schema.json.
//
// It is invoked by `go generate ./internal/project` (see the //go:generate
// directive in schema.go). The committed output is drift-checked by a plain
// `go test` in the project package, so regenerating after a Config change is
// mechanically enforced — no Makefile target needed.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/koryph/koryph/internal/project"
)

func main() {
	// go generate runs with the working directory set to the package that
	// carries the directive (internal/project), so the repo root is two levels
	// up. The drift test uses the same relative anchor.
	repoRoot := filepath.Join("..", "..")

	schema, err := project.GenerateSchema(repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}

	out := filepath.Join(repoRoot, project.SchemaRelPath)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, schema, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", project.SchemaRelPath, len(schema))
}
