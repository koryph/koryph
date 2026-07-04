// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

// fragments.go — security-scanner fragment support (design §3.3).
//
// A "fragment" is a named set of files (CI workflows, config snippets) that a
// posture profile declares as recommended and that a project explicitly opts
// into.  Fragments are installed into the project's working tree by
// `koryph posture apply` and drift-checked by `koryph doctor --project`.
//
// Fragment lifecycle:
//
//  1. Profile's manifest.json lists `recommended_fragments` (informational).
//  2. Project opts in via koryph.project.json posture.fragments: ["gitleaks"].
//  3. `koryph posture apply` (or `koryph doctor --project --fix`) installs
//     the fragment's files into the project root.
//  4. `koryph doctor --project` reports missing/stale fragment files as WARN.
//
// Scope discipline (enforced here): this package installs and drift-checks
// fragment files. It does NOT run the scanners, parse their output, or
// aggregate results. The scanners' own CI exit codes are the gate.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const fragmentsDir = "builtin/fragments"

// FragmentManifest is the parsed contents of a fragment's manifest.json.
type FragmentManifest struct {
	// Name is the fragment identifier, e.g. "gitleaks".
	Name string `json:"name"`
	// Description is a one-line human summary.
	Description string `json:"description"`
	// InstalledFiles lists paths relative to the project root that this
	// fragment owns. Used by CheckFragments / ApplyFragments.
	InstalledFiles []string `json:"installed_files"`
}

// FragmentEntry is one entry returned by ListFragments.
type FragmentEntry struct {
	Name     string
	Manifest FragmentManifest
}

// FragmentFileStatus is the installation state of one file owned by a fragment.
type FragmentFileStatus struct {
	// Path is the project-relative path (e.g. ".github/workflows/gitleaks.yml").
	Path string
	// Status is "ok", "missing", or "stale".
	Status string
}

// FragmentDrift describes the drift state of one fragment across its files.
type FragmentDrift struct {
	Fragment string
	Files    []FragmentFileStatus
	// HasDrift is true when at least one file is missing or stale.
	HasDrift bool
}

// ListFragments returns the names and manifests of all embedded built-in
// fragments. An empty slice (no error) means no fragments exist.
func ListFragments() ([]FragmentEntry, error) {
	entries, err := builtinFS.ReadDir(fragmentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("posture: list fragments: %w", err)
	}
	var out []FragmentEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := readFragmentManifest(e.Name())
		if err != nil {
			continue // skip invalid entries
		}
		out = append(out, FragmentEntry{Name: e.Name(), Manifest: m})
	}
	return out, nil
}

// CheckFragments compares the installed fragment files in projectRoot against
// the embedded versions.  For each named fragment it reports missing and stale
// files.  Fragments not found in the built-in set are skipped with a note.
//
// Output lines are written to w in the same style as CheckRulesets/
// CheckSettings: "OK fragment:file", "MISSING fragment:file",
// "DRIFT fragment:file".
//
// Returns (true, nil) when any drift is detected.
func CheckFragments(projectRoot string, fragments []string, w io.Writer) (bool, error) {
	drifts, err := fragmentDriftAll(projectRoot, fragments)
	if err != nil {
		return false, err
	}
	hasDrift := false
	for _, d := range drifts {
		for _, f := range d.Files {
			switch f.Status {
			case "ok":
				fmt.Fprintf(w, "OK       fragment:%s %s\n", d.Fragment, f.Path)
			case "missing":
				fmt.Fprintf(w, "MISSING  fragment:%s %s\n", d.Fragment, f.Path)
				hasDrift = true
			case "stale":
				fmt.Fprintf(w, "DRIFT    fragment:%s %s (installed file differs from embedded version)\n", d.Fragment, f.Path)
				hasDrift = true
			}
		}
		if d.HasDrift {
			hasDrift = true
		}
	}
	return hasDrift, nil
}

// ApplyFragments installs missing and stale fragment files into projectRoot.
// When force is false, stale files (content differs) are left untouched and
// reported as DRIFT but not overwritten; only missing files are created.
// When force is true, stale files are also overwritten.
//
// Output follows the same OK/CREATED/UPDATED/DRIFT convention as ApplyRulesets.
func ApplyFragments(projectRoot string, fragments []string, force bool, w io.Writer) (bool, error) {
	drifts, err := fragmentDriftAll(projectRoot, fragments)
	if err != nil {
		return false, err
	}
	anyDrift := false
	for _, d := range drifts {
		for _, f := range d.Files {
			dstPath := filepath.Join(projectRoot, filepath.FromSlash(f.Path))
			switch f.Status {
			case "ok":
				fmt.Fprintf(w, "OK       fragment:%s %s\n", d.Fragment, f.Path)
			case "missing":
				anyDrift = true
				content, rerr := readFragmentFile(d.Fragment, f.Path)
				if rerr != nil {
					return false, fmt.Errorf("posture: read fragment %s/%s: %w", d.Fragment, f.Path, rerr)
				}
				if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
					return false, fmt.Errorf("posture: mkdir for %s: %w", dstPath, err)
				}
				if err := os.WriteFile(dstPath, content, 0o644); err != nil {
					return false, fmt.Errorf("posture: write %s: %w", dstPath, err)
				}
				fmt.Fprintf(w, "CREATED  fragment:%s %s\n", d.Fragment, f.Path)
			case "stale":
				anyDrift = true
				if !force {
					fmt.Fprintf(w, "DRIFT    fragment:%s %s (add --force to overwrite)\n", d.Fragment, f.Path)
					continue
				}
				content, rerr := readFragmentFile(d.Fragment, f.Path)
				if rerr != nil {
					return false, fmt.Errorf("posture: read fragment %s/%s: %w", d.Fragment, f.Path, rerr)
				}
				if err := os.WriteFile(dstPath, content, 0o644); err != nil {
					return false, fmt.Errorf("posture: write %s: %w", dstPath, err)
				}
				fmt.Fprintf(w, "UPDATED  fragment:%s %s\n", d.Fragment, f.Path)
			}
		}
	}
	return anyDrift, nil
}

// RecommendedFragments returns the fragment names recommended by the named
// profile (from its manifest's recommended_fragments field).
// Unknown profiles return (nil, nil) rather than an error — callers can
// present an empty list gracefully.
func RecommendedFragments(profileName string) ([]string, error) {
	m, err := readBuiltinManifest(profileName)
	if err != nil {
		return nil, nil //nolint:nilerr // missing profile → empty list
	}
	return m.RecommendedFragments, nil
}

// fragmentDriftAll computes FragmentDrift for each named fragment.
func fragmentDriftAll(projectRoot string, fragments []string) ([]FragmentDrift, error) {
	var out []FragmentDrift
	for _, name := range fragments {
		m, err := readFragmentManifest(name)
		if err != nil {
			// Unknown fragment: emit a single MISSING entry so it appears in output.
			out = append(out, FragmentDrift{
				Fragment: name,
				Files: []FragmentFileStatus{{
					Path:   "(fragment not found in built-in set)",
					Status: "missing",
				}},
				HasDrift: true,
			})
			continue
		}
		d := FragmentDrift{Fragment: name}
		for _, relPath := range m.InstalledFiles {
			status, err := fragmentFileStatus(projectRoot, name, relPath)
			if err != nil {
				return nil, err
			}
			d.Files = append(d.Files, FragmentFileStatus{Path: relPath, Status: status})
			if status != "ok" {
				d.HasDrift = true
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// fragmentFileStatus returns "ok", "missing", or "stale" for one fragment file.
func fragmentFileStatus(projectRoot, fragmentName, relPath string) (string, error) {
	embedded, err := readFragmentFile(fragmentName, relPath)
	if err != nil {
		return "", fmt.Errorf("posture: read embedded fragment %s/%s: %w", fragmentName, relPath, err)
	}
	dstPath := filepath.Join(projectRoot, filepath.FromSlash(relPath))
	onDisk, err := os.ReadFile(dstPath)
	if os.IsNotExist(err) {
		return "missing", nil
	}
	if err != nil {
		return "", fmt.Errorf("posture: read installed fragment file %s: %w", dstPath, err)
	}
	if sha256.Sum256(onDisk) == sha256.Sum256(embedded) {
		return "ok", nil
	}
	return "stale", nil
}

// readFragmentFile reads an embedded file within a fragment's directory.
// relPath is relative to the project root (e.g. ".github/workflows/gitleaks.yml").
//
// Go's embed package excludes files in directories starting with '.' by default.
// Fragment files destined for .github/ are therefore stored under github/ (no
// leading dot) in the embedded FS and remapped at read time:
//
//	".github/workflows/gitleaks.yml" → embedded at "github/workflows/gitleaks.yml"
//	"scripts/allowed-licenses.txt"  → embedded at "scripts/allowed-licenses.txt" (no change)
func readFragmentFile(fragmentName, relPath string) ([]byte, error) {
	// Normalise to forward slashes for the embed path.
	fwdSlash := strings.ReplaceAll(relPath, string(filepath.Separator), "/")

	// Remap .github/ → github/ to avoid embed's dot-file exclusion.
	embRel := fwdSlash
	if strings.HasPrefix(embRel, ".github/") {
		embRel = "github/" + embRel[len(".github/"):]
	}

	embPath := fragmentsDir + "/" + fragmentName + "/" + embRel
	return fs.ReadFile(builtinFS, embPath)
}

// readFragmentManifest reads and parses the manifest.json for a built-in
// fragment.
func readFragmentManifest(name string) (FragmentManifest, error) {
	raw, err := builtinFS.ReadFile(fragmentsDir + "/" + name + "/manifest.json")
	if err != nil {
		return FragmentManifest{}, fmt.Errorf("posture: read fragment manifest %s: %w", name, err)
	}
	var m FragmentManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return FragmentManifest{}, fmt.Errorf("posture: parse fragment manifest %s: %w", name, err)
	}
	return m, nil
}

// PrintFragmentDiff writes a human-readable diff of fragment drift to w.
// It is a thin wrapper around CheckFragments used by posture diff/check.
func PrintFragmentDiff(projectRoot string, fragments []string, w io.Writer) (bool, error) {
	return CheckFragments(projectRoot, fragments, w)
}

// FragmentDriftResult is returned by CheckFragmentDrift for use in the
// doctor check — it avoids re-computing drift twice.
type FragmentDriftResult struct {
	Drifts []FragmentDrift
	Output bytes.Buffer
}

// CheckFragmentDrift checks all declared fragments and returns a structured
// result for the doctor check.
func CheckFragmentDrift(projectRoot string, fragments []string) (*FragmentDriftResult, error) {
	r := &FragmentDriftResult{}
	drifts, err := fragmentDriftAll(projectRoot, fragments)
	if err != nil {
		return nil, err
	}
	r.Drifts = drifts
	_, _ = CheckFragments(projectRoot, fragments, &r.Output)
	return r, nil
}
