// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package onboard

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/fsx"
)

// Infer* build PROPOSALS for the three "cannot be guessed" adopt fields
// (forge, gate, area_map — design docs/designs/2026-07-adopt.md §6). Every
// function here is pure and read-only: they inspect the working tree and
// derive a value, but NEVER write koryph.project.json or anything else. The
// adopt wizard presents the proposal with its Provenance, the user confirms
// or overrides it, and only THEN does the wizard write config (§3.3 "value
// confirmations": a wrong gate green-lights garbage, so it is always
// confirmed explicitly).

// Proposal carries a value derived by InferForge/InferGate/InferAreaMap and a
// human-readable Provenance string naming what was read to derive it (a
// remote URL, a Makefile target, a package.json script, …).
//
// Concrete, not generic: every proposal value produced by this file is a
// string (a forge name, a gate command), and the codebase does not use Go
// generics elsewhere, so Proposal[T any] would add a type parameter with
// nothing left to parametrize in practice — a plain struct matches the
// existing idiom (see internal/forge, internal/project: value types are
// concrete throughout).
type Proposal struct {
	Value      string
	Provenance string
}

// --- forge -------------------------------------------------------------

// InferForge derives the forge provider from a git remote URL. The host
// match itself is delegated to forge.SniffRemote — the same function
// internal/doctor already uses for release-infra checks — so the adopt
// wizard and doctor can never disagree about what a remote URL means; this
// function only adds the provenance the wizard shows at confirmation time.
//
// An empty or unrecognized remote proposes "" (unknown): the wizard falls
// back to asking or omitting the field rather than guessing wrong (design
// §6 — "forge: remote host match … else ask/omit").
func InferForge(remoteURL string) Proposal {
	trimmed := strings.TrimSpace(remoteURL)
	if trimmed == "" {
		return Proposal{Provenance: "no git remote URL to infer a forge from"}
	}
	name := forge.SniffRemote(trimmed)
	if name == "" {
		return Proposal{Provenance: fmt.Sprintf("remote URL %q does not match a known forge host", trimmed)}
	}
	return Proposal{Value: name, Provenance: fmt.Sprintf("host-matched from remote URL %q", trimmed)}
}

// --- gate ----------------------------------------------------------------

// makeTargetRe matches a Makefile rule or special-target line: a bare
// identifier (letters, digits, '_', '.', '-') immediately followed by ':'.
// It is anchored to the start of the (untrimmed) line, so indented recipe
// lines (which start with a tab) and comments/variable assignments with a
// space before their delimiter never match. This is deliberately NOT a full
// Makefile parser — it is textual evidence-gathering only; make is never
// executed.
var makeTargetRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+):`)

// gateOrderedTargets is the fixed presentation order for Makefile
// test/lint/build evidence (rule 2) and package.json scripts (rule 3).
var gateOrderedTargets = []string{"test", "lint", "build"}

// InferGate ranks gate-command candidates detected from the project's build
// tooling, per design §6's rule list. The FIRST matching rule group wins and
// is returned in full; rule groups are (in order):
//
//  1. A Makefile with a "gate" target → ["make gate"].
//  2. A Makefile with test/lint/build targets (no "gate") → "make <target>"
//     for whichever of test, lint, build actually exist, in that order.
//  3. package.json with scripts.test/lint/build → "npm test" / "npm run
//     lint" / "npm run build" for whichever scripts actually exist.
//  4. go.mod → ["go vet ./...", "go test ./..."].
//  5. Cargo.toml → ["cargo test"].
//  6. pyproject.toml (or setup.py as equally-valid evidence) → ["pytest"].
//
// A Makefile present but carrying none of the recognized targets (rules 1-2
// find no evidence) does not count as "the Makefile ecosystem" and falls
// through: when no Makefile evidence exists at all, every remaining
// ecosystem's group is combined (e.g. go.mod + package.json → both sets, Go
// first) rather than picking just one — a repo can be honestly polyglot.
func InferGate(root string) []Proposal {
	if props, ok := inferGateFromMakefile(root); ok {
		return props
	}

	var combined []Proposal
	combined = append(combined, inferGateFromGoMod(root)...)
	combined = append(combined, inferGateFromPackageJSON(root)...)
	combined = append(combined, inferGateFromCargo(root)...)
	combined = append(combined, inferGateFromPyProject(root)...)
	return combined
}

// inferGateFromMakefile applies rules 1-2. ok is false when no Makefile
// variant exists, or one exists but carries none of the recognized targets
// (letting the caller fall through to the combined ecosystems).
func inferGateFromMakefile(root string) (props []Proposal, ok bool) {
	name, content, found := readMakefile(root)
	if !found {
		return nil, false
	}
	targets := parseMakeTargets(content)

	if targets["gate"] {
		return []Proposal{
			{Value: "make gate", Provenance: fmt.Sprintf("detected from %s target 'gate'", name)},
		}, true
	}

	for _, t := range gateOrderedTargets {
		if targets[t] {
			props = append(props, Proposal{
				Value:      "make " + t,
				Provenance: fmt.Sprintf("detected from %s target '%s'", name, t),
			})
		}
	}
	return props, len(props) > 0
}

// readMakefile returns the basename and content of the first Makefile
// variant found at root, trying the same name precedence GNU make itself
// uses (GNUmakefile, makefile, Makefile). Candidates are matched against a
// directory listing rather than opened directly by name: several common
// dev filesystems (macOS default APFS/HFS+, Windows) are case-insensitive,
// so a direct os.ReadFile(root, "makefile") would silently open an
// on-disk "Makefile" and misreport its own name in the provenance string.
func readMakefile(root string) (name, content string, ok bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", "", false
	}
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			present[e.Name()] = true
		}
	}
	for _, candidate := range []string{"GNUmakefile", "makefile", "Makefile"} {
		if !present[candidate] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, candidate))
		if err != nil {
			continue
		}
		return candidate, string(data), true
	}
	return "", "", false
}

// parseMakeTargets returns the set of target names with textual evidence in
// content: every line matching makeTargetRe contributes its own name, and a
// ".PHONY" line additionally contributes every name it lists (a
// .PHONY-declared target is still evidence even when this pass hasn't also
// seen its own rule line, e.g. because the rule is generated by a pattern
// or include this parser does not follow).
func parseMakeTargets(content string) map[string]bool {
	targets := make(map[string]bool)
	for _, line := range strings.Split(content, "\n") {
		m := makeTargetRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if name == ".PHONY" {
			for _, f := range strings.Fields(line[len(m[0]):]) {
				targets[f] = true
			}
			continue
		}
		targets[name] = true
	}
	return targets
}

// packageJSON is the subset of package.json this file reads.
type packageJSON struct {
	Scripts map[string]string `json:"scripts"`
}

// inferGateFromPackageJSON applies rule 3. A missing or unparsable
// package.json (rather than an error) simply yields no evidence — adopt
// detection tolerates a malformed file the same way onboard.Inspect
// tolerates any other missing sub-probe.
func inferGateFromPackageJSON(root string) []Proposal {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}

	var props []Proposal
	if _, ok := pkg.Scripts["test"]; ok {
		props = append(props, Proposal{Value: "npm test", Provenance: "detected from package.json script 'test'"})
	}
	if _, ok := pkg.Scripts["lint"]; ok {
		props = append(props, Proposal{Value: "npm run lint", Provenance: "detected from package.json script 'lint'"})
	}
	if _, ok := pkg.Scripts["build"]; ok {
		props = append(props, Proposal{Value: "npm run build", Provenance: "detected from package.json script 'build'"})
	}
	return props
}

// inferGateFromGoMod applies rule 4. Presence alone is the evidence — unlike
// Makefile/package.json there are no named sub-targets to check for go.mod.
func inferGateFromGoMod(root string) []Proposal {
	if !fsx.Exists(filepath.Join(root, "go.mod")) {
		return nil
	}
	const provenance = "detected from go.mod"
	return []Proposal{
		{Value: "go vet ./...", Provenance: provenance},
		{Value: "go test ./...", Provenance: provenance},
	}
}

// inferGateFromCargo applies rule 5.
func inferGateFromCargo(root string) []Proposal {
	if !fsx.Exists(filepath.Join(root, "Cargo.toml")) {
		return nil
	}
	return []Proposal{{Value: "cargo test", Provenance: "detected from Cargo.toml"}}
}

// inferGateFromPyProject applies rule 6. pyproject.toml is preferred
// evidence; setup.py is accepted as an equally-valid signal for an
// otherwise-pyproject-less legacy layout, per the design's explicit
// allowance.
func inferGateFromPyProject(root string) []Proposal {
	switch {
	case fsx.Exists(filepath.Join(root, "pyproject.toml")):
		return []Proposal{{Value: "pytest", Provenance: "detected from pyproject.toml"}}
	case fsx.Exists(filepath.Join(root, "setup.py")):
		return []Proposal{{Value: "pytest", Provenance: "detected from setup.py"}}
	default:
		return nil
	}
}

// --- area_map --------------------------------------------------------------

// areaMapCap bounds the starter map size — an over-long proposal is noise
// the user has to read past rather than a useful default (design §6).
const areaMapCap = 12

// areaSourceLang maps a recognized source-code file extension to its short
// language token. Extensions absent from this map (and .md, deliberately —
// documentation is not "source" for footprint purposes) never make a
// directory eligible on their own.
var areaSourceLang = map[string]string{
	".go":   "go",
	".ts":   "ts",
	".js":   "js",
	".py":   "py",
	".rs":   "rs",
	".java": "java",
	".rb":   "rb",
	".c":    "c",
	".h":    "c",
	".cpp":  "c",
}

// areaExtPriority breaks a dominant-extension tie deterministically, in the
// order the language tokens are introduced in areaSourceLang above.
var areaExtPriority = []string{".go", ".ts", ".js", ".py", ".rs", ".java", ".rb", ".c", ".h", ".cpp"}

// areaSkipDirs are top-level directories that never become an area, whether
// at the root or nested inside an otherwise-eligible dir: dot-dirs are
// handled separately by name-prefix. These are generated/vendored trees or
// pure documentation with no code to protect a footprint over.
var areaSkipDirs = map[string]bool{
	"docs": true, "doc": true, "vendor": true, "node_modules": true,
	"dist": true, "build": true, "out": true, "target": true,
	"third_party": true, "testdata": true,
}

// areaCandidate is one top-level directory under consideration, before
// capping.
type areaCandidate struct {
	dir   string
	lang  string
	files int // recursive count of recognized-source files (the ranking key)
}

// InferAreaMap proposes a starter area_map from root's top-level
// directories, plus a Provenance string describing how it was derived (the
// wizard shows this alongside the map — design §6 "footprints are how
// parallel agents avoid merge conflicts").
//
// A top-level directory is skipped when: it is a dot-dir; it is one of
// areaSkipDirs (docs/doc, vendor, node_modules, dist, build, out, target,
// third_party, testdata); or it (recursively) contains no recognized source
// file. Each surviving directory becomes dir -> ["<lang>:<dir>"], matching
// the convention already used by this repo's own koryph.project.json
// area_map (e.g. "sched": ["go:sched"]). <lang> is the dominant source
// extension found under dir, mapped to its short token; an extension with
// no known token (unreached today, since every extension counted as
// "source" also has a token — kept as a defensive default) falls back to
// "src". Results are capped at areaMapCap, largest directories (by
// recursive source-file count) winning; ties break by directory name so the
// choice is deterministic.
//
// This is the "app-layout opinion" fence from design §9: it names what
// already exists and proposes nothing about how the repo *should* be laid
// out.
func InferAreaMap(root string) (map[string][]string, string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return map[string][]string{}, fmt.Sprintf("could not read %s: %v", root, err)
	}

	var candidates []areaCandidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || areaSkipDirs[strings.ToLower(name)] {
			continue
		}
		extCount, total := scanAreaSource(filepath.Join(root, name))
		if total == 0 {
			continue
		}
		candidates = append(candidates, areaCandidate{dir: name, lang: dominantAreaLang(extCount), files: total})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].files != candidates[j].files {
			return candidates[i].files > candidates[j].files
		}
		return candidates[i].dir < candidates[j].dir
	})
	total := len(candidates)
	if len(candidates) > areaMapCap {
		candidates = candidates[:areaMapCap]
	}

	result := make(map[string][]string, len(candidates))
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		result[c.dir] = []string{c.lang + ":" + c.dir}
		names = append(names, c.dir)
	}
	sort.Strings(names)

	dirWord := "directories"
	if len(candidates) == 1 {
		dirWord = "directory"
	}
	provenance := fmt.Sprintf(
		"starter area map inferred from %d top-level source %s (dot-dirs, docs/doc, vendor, node_modules, dist, build, out, target, third_party, testdata, and dirs with no source files skipped)",
		len(candidates), dirWord,
	)
	if total > areaMapCap {
		provenance += fmt.Sprintf("; capped at %d by source file count, largest first", areaMapCap)
	}
	if len(names) > 0 {
		provenance += ": " + strings.Join(names, ", ")
	}
	return result, provenance
}

// scanAreaSource walks dir recursively and returns the per-extension count
// of recognized source files plus their total. Nested dot-dirs and
// areaSkipDirs are pruned wherever they occur, not just at the top level —
// a vendor/ or node_modules/ tree nested inside an otherwise-eligible
// directory must not inflate that directory's file count. Unreadable
// subtrees are tolerated (skipped), matching onboard.Inspect's rule that a
// sub-probe failure never fails the whole inspection.
func scanAreaSource(dir string) (extCount map[string]int, total int) {
	extCount = make(map[string]int)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if path != dir && (strings.HasPrefix(base, ".") || areaSkipDirs[strings.ToLower(base)]) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if _, known := areaSourceLang[ext]; known {
			extCount[ext]++
			total++
		}
		return nil
	})
	return extCount, total
}

// dominantAreaLang returns the language token for the highest-count
// extension in extCount, breaking ties by areaExtPriority order. "src" is a
// defensive fallback for an extension outside areaSourceLang, which cannot
// occur given how extCount is populated today (scanAreaSource only counts
// known extensions) but keeps this function correct if that invariant ever
// changes.
func dominantAreaLang(extCount map[string]int) string {
	best := ""
	bestCount := 0
	for _, ext := range areaExtPriority {
		if c := extCount[ext]; c > bestCount {
			best, bestCount = ext, c
		}
	}
	lang, ok := areaSourceLang[best]
	if !ok {
		return "src"
	}
	return lang
}
