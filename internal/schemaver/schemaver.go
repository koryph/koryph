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

import "fmt"

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
	Registry:       1,
	Quota:          1,
	SigningVault:   1,
	Project:        1,
	LedgerRun:      2,
	LedgerManifest: 2,
}

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
