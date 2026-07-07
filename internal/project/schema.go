// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"reflect"
	"strings"

	"github.com/invopop/jsonschema"
)

//go:generate go run ./gen

// SchemaModulePath is the Go module base used to key doc-comment lookups when
// reflecting the config structs into a JSON Schema.
const SchemaModulePath = "github.com/koryph/koryph"

// SchemaRelPath is the committed schema's location relative to the repo root.
// It is usable by any editor via a `$schema` reference.
const SchemaRelPath = "docs/schema/koryph.project.schema.json"

// VSCodeSchemaRelPath is the VS Code extension's bundled copy of the schema,
// relative to the repo root. It is referenced by ide/vscode/package.json
// (jsonValidation url ./media/koryph.project.schema.json) so that the
// extension validates koryph.project.json without a network fetch.
// The generator (go generate ./internal/project) writes both this file and
// SchemaRelPath so they stay in sync; a sibling drift test enforces that.
const VSCodeSchemaRelPath = "ide/vscode/media/koryph.project.schema.json"

// GenerateSchema reflects the project Config struct (and the referenced
// signing.Config) into a JSON Schema document, using the Go doc comments in the
// source tree at repoRoot as field descriptions. The output is deterministic
// (stable field order, 2-space indent, trailing newline) so a committed copy
// can be drift-checked by a plain `go test`.
//
// The structs are the single source of truth: this is the emitter behind
// `go generate ./internal/project` and the drift test, so the committed schema
// at SchemaRelPath cannot silently diverge from the Go types.
func GenerateSchema(repoRoot string) ([]byte, error) {
	r := &jsonschema.Reflector{
		// Root the Config definition at the top level with referenced structs
		// (FootprintRule, PipelineStage, IntakeSource, signing.Config) under
		// $defs — the shape editors expect for a document schema.
		ExpandedStruct: true,
		// A stable, human-meaningful $id independent of the Go import path.
		BaseSchemaID: "https://koryph.dev/schemas",
		// Disambiguate types that share a short name across packages. Without
		// this, signing.Config collides with the root project.Config (both
		// "Config") and its $defs entry is dropped, leaving a dangling $ref.
		Namer: qualifiedTypeName,
	}
	// invopop keys doc comments by import path via gopath.Join(base, dir), so
	// the walked dir must be expressed relative to the module root (no "../..",
	// which would corrupt the join). Chdir to repoRoot for the walk, then
	// restore — this is single-threaded (gen main and the drift test).
	restore, err := pushDir(repoRoot)
	if err != nil {
		return nil, err
	}
	defer restore()
	// Pull field docs from the two packages whose types compose the config.
	for _, pkg := range []string{"internal/project", "internal/signing"} {
		if err := r.AddGoComments(SchemaModulePath, pkg); err != nil {
			return nil, fmt.Errorf("load go comments for %s: %w", pkg, err)
		}
	}

	schema := r.Reflect(&Config{})
	schema.Title = "koryph.project.json"
	schema.Description = "Per-project koryph adapter configuration (koryph.project.json). " +
		"Generated from the Go project.Config struct — do not edit by hand; run `go generate ./internal/project`."

	buf, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	// A trailing newline keeps the committed file POSIX-clean and diff-stable.
	return append(bytes.TrimRight(buf, "\n"), '\n'), nil
}

// qualifiedTypeName names a reflected type for its $defs key. Types defined in
// this package (project) keep their bare name; types imported from elsewhere
// (e.g. signing.Config) are prefixed with their capitalized package segment so
// they cannot collide with a same-named type in the root package.
func qualifiedTypeName(t reflect.Type) string {
	name := t.Name()
	pkg := t.PkgPath()
	if pkg == "" || strings.HasSuffix(pkg, "/internal/project") {
		return name
	}
	seg := path.Base(pkg)
	if seg == "" {
		return name
	}
	return strings.ToUpper(seg[:1]) + seg[1:] + name
}

// pushDir changes the working directory to dir and returns a func that restores
// the previous directory. It exists so GenerateSchema can satisfy invopop's
// import-path-relative comment walk without leaking a directory change.
func pushDir(dir string) (func(), error) {
	prev, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}
	if err := os.Chdir(dir); err != nil {
		return nil, fmt.Errorf("chdir %s: %w", dir, err)
	}
	return func() { _ = os.Chdir(prev) }, nil
}
