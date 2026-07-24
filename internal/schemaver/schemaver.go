// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package schemaver is the single source of truth for the on-disk schema
// version of every persisted koryph state surface, and the forward-compatibility
// guard that keeps an older binary from silently corrupting state a newer binary
// wrote.
//
// The problem it solves (design docs/designs/2026-07-state-versioning.md): a
// koryph release adds a field or changes semantics for one of the shared
// ~/.koryph state files (registry, quota, signing vault, ledger, per-project
// config). koryph is explicitly built for concurrent multi-environment writers
// sharing one KORYPH_HOME, so an OLDER binary can be pointed at state a NEWER
// binary just wrote. Without a guard, the older binary loads the newer-shaped
// JSON as zero-valued unknown fields and proceeds (misreading), and its next
// read-modify-write Save round-trips the struct through the older type, silently
// DROPPING every field the newer binary added.
//
// The fix is a write ceiling, not a shadow copy: each surface carries a
// schema_version, and every load/save is gated. If the on-disk version is NEWER
// than this build understands, the operation is REFUSED with a clear "upgrade
// koryph" error rather than proceeding and corrupting. Same-or-older versions
// proceed (a newer binary always reads older state; migrations, when needed,
// live behind CheckRead at the call site).
//
// Single source of truth: Current(surface) is the version THIS binary writes and
// understands. Write paths stamp Current(surface); read paths compare the stamp
// to Current(surface) via CheckRead. Because both the stamp and the guard read
// the same map, the on-disk "current" and the compiled "current" cannot drift.
package schemaver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Surface names one persisted state surface. The string value is stable — it
// appears in error messages and (later) migration keys — so never rename one.
type Surface string

const (
	Registry       Surface = "registry"        // ~/.koryph/projects/<id>.json (registry.Record)
	Quota          Surface = "quota"           // ~/.koryph/quota/<account>.json (quota.Config)
	SigningVault   Surface = "signing_vault"   // ~/.koryph/vault.json (signing.VaultConfig)
	Project        Surface = "project"         // <repo>/koryph.project.json (project.Config)
	LedgerRun      Surface = "ledger_run"      // <run>/ledger.json (ledger.Run)
	LedgerManifest Surface = "ledger_manifest" // <run>/<phase>/manifest.json (ledger.Manifest)
)

// current is the schema version this binary writes and fully understands for
// each surface. Bump a surface here (and add a migration behind CheckRead at its
// load sites) whenever you make a breaking on-disk change to it. Every write
// path stamps Current(surface); every read path guards with CheckRead — so this
// map is the ONE place the number lives.
//
// SemVer contract: a schema bump within a koryph MAJOR version must be additive
// with a read migration (older files still load); a breaking change requires a
// new koryph major and a documented upgrade path.
var current = map[Surface]int{
	Registry:       2,
	Quota:          1,
	SigningVault:   1,
	Project:        1,
	LedgerRun:      2,
	LedgerManifest: 2,
}

// Migration transforms raw JSON from one schema version to the next. The map
// form deliberately preserves fields unknown to the current Go struct: a
// migration can rename or remove fields without a struct round-trip dropping
// unrelated data.
type Migration func(map[string]json.RawMessage) (map[string]json.RawMessage, error)

// migrations holds ordered vN-to-vN+1 steps for each surface. A missing step
// is an error, never an implicit no-op. Surfaces at v1 currently have no
// backfill migration because their legacy formats are not yet declared.
//
// When bumping a surface, append its vN-to-vN+1 step here in the same change as
// the version bump in current. A no-op step is valid for an additive change.
var migrations = map[Surface][]Migration{}

// Current returns the schema version this binary writes for surface s. Write
// paths stamp this value into the state's schema_version field. Panics on an
// unknown surface — surfaces are compile-time constants, so an unknown one is a
// programming error, not runtime input.
func Current(s Surface) int {
	v, ok := current[s]
	if !ok {
		panic("schemaver: unknown surface " + string(s))
	}
	return v
}

// Surfaces returns every registered surface (for doctor/state audits).
func Surfaces() []Surface {
	out := make([]Surface, 0, len(current))
	for s := range current {
		out = append(out, s)
	}
	return out
}

// Migrate lifts raw, versioned JSON state to the version understood by this
// binary. It changes only the returned bytes; callers decide whether and when
// to persist them. Unknown surfaces, newer state, malformed input, and a
// missing version step all return errors.
func Migrate(s Surface, raw []byte) ([]byte, error) {
	if _, ok := current[s]; !ok {
		return nil, fmt.Errorf("schemaver: cannot migrate unknown surface %q", s)
	}

	var state map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("schemaver: decode %s state: %w", s, err)
	}
	if state == nil {
		return nil, fmt.Errorf("schemaver: %s state must be a JSON object", s)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("schemaver: decode %s state: multiple JSON values", s)
		}
		return nil, fmt.Errorf("schemaver: decode %s state: %w", s, err)
	}

	version, err := rawVersion(state["schema_version"])
	if err != nil {
		return nil, fmt.Errorf("schemaver: decode %s schema_version: %w", s, err)
	}
	if err := CheckRead(s, version); err != nil {
		return nil, err
	}

	steps := migrations[s]
	for version < Current(s) {
		if version < 0 || version >= len(steps) || steps[version] == nil {
			return nil, fmt.Errorf("schemaver: no migration for %s v%d to v%d", s, version, version+1)
		}
		state, err = steps[version](state)
		if err != nil {
			return nil, fmt.Errorf("schemaver: migrate %s v%d to v%d: %w", s, version, version+1, err)
		}
		if state == nil {
			return nil, fmt.Errorf("schemaver: migrate %s v%d to v%d returned nil state", s, version, version+1)
		}
		version++
		state["schema_version"] = json.RawMessage(strconv.Itoa(version))
	}

	result, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("schemaver: encode %s state: %w", s, err)
	}
	return result, nil
}

func rawVersion(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, nil
	}
	var version int
	if err := json.Unmarshal(raw, &version); err != nil {
		return 0, err
	}
	if version < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return version, nil
}

// VerifyFingerprint compares each persisted surface type's schema fingerprint
// with one shared golden file. The golden format is one line per surface:
//
//	<surface> <schema-version> <sha256>
//
// Call this from an external schemaver test package so persisted packages can
// be reflected without creating an import cycle back into schemaver.
func VerifyFingerprint(golden []byte, types map[Surface]any) error {
	want, err := parseFingerprints(golden)
	if err != nil {
		return err
	}
	if len(types) != len(current) {
		return fmt.Errorf("schemaver: fingerprint types cover %d surfaces, want %d", len(types), len(current))
	}
	surfaces := Surfaces()
	sort.Slice(surfaces, func(i, j int) bool { return surfaces[i] < surfaces[j] })
	var problems []string
	for _, surface := range surfaces {
		value, ok := types[surface]
		if !ok {
			problems = append(problems, fmt.Sprintf("type missing for %s", surface))
			continue
		}
		got := fingerprint(reflect.TypeOf(value))
		entry, ok := want[surface]
		if !ok {
			problems = append(problems, fmt.Sprintf("golden missing for %s; got %s %d %s", surface, surface, Current(surface), got))
			continue
		}
		if entry.version != Current(surface) || entry.hash != got {
			problems = append(problems, fmt.Sprintf("mismatch for %s: got %s %d %s, want %s %d %s", surface, surface, Current(surface), got, surface, entry.version, entry.hash))
		}
	}
	for _, surface := range sortedFingerprintSurfaces(want) {
		if _, ok := current[surface]; !ok {
			problems = append(problems, fmt.Sprintf("golden has unknown surface %s", surface))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("schemaver: fingerprint verification failed:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

type fingerprintEntry struct {
	version int
	hash    string
}

func parseFingerprints(golden []byte) (map[Surface]fingerprintEntry, error) {
	entries := make(map[Surface]fingerprintEntry)
	for lineNo, line := range strings.Split(strings.TrimSpace(string(golden)), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("schemaver: fingerprint golden line %d: want surface version sha256", lineNo+1)
		}
		version, err := strconv.Atoi(fields[1])
		if err != nil || version < 0 {
			return nil, fmt.Errorf("schemaver: fingerprint golden line %d: invalid version %q", lineNo+1, fields[1])
		}
		if len(fields[2]) != sha256.Size*2 {
			return nil, fmt.Errorf("schemaver: fingerprint golden line %d: invalid sha256", lineNo+1)
		}
		surface := Surface(fields[0])
		if _, duplicate := entries[surface]; duplicate {
			return nil, fmt.Errorf("schemaver: fingerprint golden duplicates %s", surface)
		}
		entries[surface] = fingerprintEntry{version: version, hash: fields[2]}
	}
	return entries, nil
}

func sortedFingerprintSurfaces(entries map[Surface]fingerprintEntry) []Surface {
	surfaces := make([]Surface, 0, len(entries))
	for surface := range entries {
		surfaces = append(surfaces, surface)
	}
	sort.Slice(surfaces, func(i, j int) bool { return surfaces[i] < surfaces[j] })
	return surfaces
}

func fingerprint(t reflect.Type) string {
	if t == nil {
		return ""
	}
	seen := make(map[reflect.Type]bool)
	var fields []string
	var visit func(reflect.Type)
	visit = func(t reflect.Type) {
		t = dereference(t)
		if t.Kind() != reflect.Struct || seen[t] || !strings.HasPrefix(t.PkgPath(), "github.com/koryph/koryph/") {
			return
		}
		seen[t] = true
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() || strings.Split(field.Tag.Get("json"), ",")[0] == "-" {
				continue
			}
			fields = append(fields, t.PkgPath()+"."+t.Name()+"|"+field.Name+"|"+field.Tag.Get("json")+"|"+field.Type.String())
			visit(field.Type)
		}
	}
	visit(t)
	sort.Strings(fields)
	sum := sha256.Sum256([]byte(strings.Join(fields, "\n")))
	return hex.EncodeToString(sum[:])
}

func dereference(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		if t.Kind() == reflect.Map {
			t = t.Elem()
		} else {
			t = t.Elem()
		}
	}
	return t
}

// CheckRead guards a load. It returns a *TooNewError when the on-disk version is
// NEWER than this build understands — the file was written by a newer koryph and
// may carry fields/semantics this build would drop or misread, so the safe act
// is to refuse and tell the operator to upgrade. onDisk == 0 is unstamped legacy
// (predates stamping) and always passes; a same-or-older version passes so a
// newer binary always reads older state.
func CheckRead(s Surface, onDisk int) error {
	if cur := Current(s); onDisk > cur {
		return &TooNewError{Surface: s, OnDisk: onDisk, Supported: cur, Op: "read"}
	}
	return nil
}

// CheckWrite guards a save. It returns a *TooNewError when the on-disk version
// is newer than this build — overwriting it would strip the fields the newer
// writer added (the read-modify-write field-loss failure). Same rule as
// CheckRead. Call it on the FRESH on-disk version read under the same lock the
// save holds, before the struct round-trip.
func CheckWrite(s Surface, onDisk int) error {
	if cur := Current(s); onDisk > cur {
		return &TooNewError{Surface: s, OnDisk: onDisk, Supported: cur, Op: "write"}
	}
	return nil
}

// TooNewError is returned by CheckRead/CheckWrite when on-disk state is newer
// than this build. Callers can errors.As it to distinguish a version-skew
// refusal (upgrade koryph) from a genuine corruption/IO error.
type TooNewError struct {
	Surface   Surface
	OnDisk    int
	Supported int
	Op        string // "read" or "write"
}

func (e *TooNewError) Error() string {
	return fmt.Sprintf(
		"koryph: refusing to %s %s state at schema v%d — this build understands only up to v%d; upgrade koryph (state was written by a newer version)",
		e.Op, e.Surface, e.OnDisk, e.Supported)
}
