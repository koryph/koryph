// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"embed"
)

//go:embed builtin
var builtinFS embed.FS

// Manifest is the parsed contents of a profile's manifest.json.
type Manifest struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Parameters  map[string]ParamDescriptor `json:"parameters,omitempty"`
	// RecommendedFragments lists the fragment names (from builtin/fragments/)
	// that this profile suggests as opt-in scanner fragments. Projects declare
	// their chosen fragments in koryph.project.json posture.fragments; listing
	// them here is informational — they are never auto-installed.
	RecommendedFragments []string `json:"recommended_fragments,omitempty"`
	// Descriptions maps setting field names and rule type keys (prefixed with
	// "rule.") to human-readable security rationale, overriding the built-in
	// fallbacks in builtinSettingRationale / builtinRuleRationale.  Community
	// profiles use this to make their profiles self-documenting without
	// modifying the Go source.
	Descriptions map[string]string `json:"descriptions,omitempty"`
}

// ParamDescriptor describes one profile parameter.
type ParamDescriptor struct {
	Description string `json:"description"`
	Default     string `json:"default"`
}

// ProfileEntry is one entry returned by ListProfiles.
type ProfileEntry struct {
	Name     string
	Source   string // "builtin" or "user"
	Manifest Manifest
}

// profileTemplateData is the data passed to profile template files.
type profileTemplateData struct {
	// RequiredChecks holds the names to inject into the required_status_checks
	// rule.  When nil the rule block is omitted from the pr-checks ruleset.
	RequiredChecks []statusCheck
}

// statusCheck is one entry in a required_status_checks array.
type statusCheck struct {
	Context string `json:"context"`
}

// ListBuiltins returns the names of all embedded built-in profiles.
func ListBuiltins() ([]ProfileEntry, error) {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, fmt.Errorf("posture: list builtins: %w", err)
	}
	var out []ProfileEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := readBuiltinManifest(e.Name())
		if err != nil {
			// Skip directories without a valid manifest.
			continue
		}
		out = append(out, ProfileEntry{Name: e.Name(), Source: "builtin", Manifest: m})
	}
	return out, nil
}

// ListUserProfiles returns the profiles found under <home>/postures/.
// home is typically ~/.koryph.  A missing directory is not an error — it
// returns an empty slice.
func ListUserProfiles(home string) ([]ProfileEntry, error) {
	dir := filepath.Join(home, "postures")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("posture: list user profiles: %w", err)
	}
	var out []ProfileEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := readUserManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, ProfileEntry{Name: e.Name(), Source: "user", Manifest: m})
	}
	return out, nil
}

// RenderProfile loads a named profile (user paths take precedence over
// built-ins), renders any template files with params, and materialises the
// result into a fresh temp directory.  The returned LocalSource points at that
// directory; the caller must invoke the cleanup func to remove it.
//
// params maps parameter names to string values (e.g. "required_checks" →
// "pre-commit,make gate").
func RenderProfile(name string, params map[string]string, home string) (LocalSource, func(), error) {
	tmpDir, err := os.MkdirTemp("", "koryph-posture-"+name+"-*")
	if err != nil {
		return LocalSource{}, nil, fmt.Errorf("posture: create temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmpDir) } //nolint:errcheck

	data, err := buildTemplateData(params)
	if err != nil {
		cleanup()
		return LocalSource{}, nil, err
	}

	// Profile files are rendered into <tmpDir>/.github/ so LocalSource works
	// (it expects .github/rulesets/ and .github/repo-settings.json).
	ghDir := filepath.Join(tmpDir, ".github")
	if err := os.MkdirAll(ghDir, 0o755); err != nil {
		cleanup()
		return LocalSource{}, nil, fmt.Errorf("posture: mkdir .github: %w", err)
	}

	// Try user profile first, then built-in.
	userDir := filepath.Join(home, "postures", name)
	if fi, err2 := os.Stat(userDir); err2 == nil && fi.IsDir() {
		if err := renderDir(os.DirFS(userDir), ".", ghDir, data); err != nil {
			cleanup()
			return LocalSource{}, nil, fmt.Errorf("posture: render user profile %s: %w", name, err)
		}
		return LocalSource{Root: tmpDir}, cleanup, nil
	}

	// Built-in.
	builtinDir := "builtin/" + name
	if _, err := builtinFS.Open(builtinDir); err != nil {
		cleanup()
		return LocalSource{}, nil, fmt.Errorf("posture: profile %q not found (checked user and built-in sources)", name)
	}
	sub, err := fs.Sub(builtinFS, builtinDir)
	if err != nil {
		cleanup()
		return LocalSource{}, nil, fmt.Errorf("posture: sub fs for %s: %w", name, err)
	}
	if err := renderDir(sub, ".", ghDir, data); err != nil {
		cleanup()
		return LocalSource{}, nil, fmt.Errorf("posture: render builtin profile %s: %w", name, err)
	}
	return LocalSource{Root: tmpDir}, cleanup, nil
}

// EjectCheck reports whether the current working directory has repo-local
// .github files that should override the profile for each section.
// It returns (hasRulesets, hasSettings).
func EjectCheck(root string) (hasRulesets, hasSettings bool) {
	if fi, err := os.Stat(filepath.Join(root, ".github", "rulesets")); err == nil && fi.IsDir() {
		hasRulesets = true
	}
	if _, err := os.Stat(filepath.Join(root, ".github", "repo-settings.json")); err == nil {
		hasSettings = true
	}
	return
}

// buildTemplateData constructs the template data struct from the raw params map.
func buildTemplateData(params map[string]string) (profileTemplateData, error) {
	var data profileTemplateData
	if v := params["required_checks"]; v != "" {
		names := strings.Split(v, ",")
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n != "" {
				data.RequiredChecks = append(data.RequiredChecks, statusCheck{Context: n})
			}
		}
	}
	return data, nil
}

// renderDir walks srcFS rooted at srcRoot, renders each file into dstRoot.
// Files ending in ".tmpl" are rendered as text/templates (output is written
// without the ".tmpl" suffix); all other files are copied verbatim.
// The manifest.json is skipped — it is metadata, not desired state.
func renderDir(srcFS fs.FS, srcRoot, dstRoot string, data profileTemplateData) error {
	return fs.WalkDir(srcFS, srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err2 := filepath.Rel(srcRoot, path)
		if err2 != nil {
			return err2
		}
		if d.IsDir() {
			if path == srcRoot {
				return nil // root itself — skip, already exists
			}
			return os.MkdirAll(filepath.Join(dstRoot, rel), 0o755)
		}

		base := filepath.Base(path)
		// Skip the profile manifest and any dot-files.
		if base == "manifest.json" || strings.HasPrefix(base, ".") {
			return nil
		}

		raw, err := fs.ReadFile(srcFS, path)
		if err != nil {
			return fmt.Errorf("posture: read profile file %s: %w", path, err)
		}

		var content []byte
		dstRel := rel
		if strings.HasSuffix(base, ".tmpl") {
			rendered, err := renderTemplate(path, raw, data)
			if err != nil {
				return err
			}
			content = rendered
			// Drop the .tmpl suffix on the output file.
			dstRel = strings.TrimSuffix(rel, ".tmpl")
		} else {
			content = raw
		}

		dst := filepath.Join(dstRoot, dstRel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("posture: mkdir for %s: %w", dst, err)
		}
		return os.WriteFile(dst, content, 0o644)
	})
}

// renderTemplate executes name/raw as a text/template with data.
func renderTemplate(name string, raw []byte, data profileTemplateData) ([]byte, error) {
	funcMap := template.FuncMap{
		"toJSON": func(v interface{}) (string, error) {
			b, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
	}
	tmpl, err := template.New(name).Funcs(funcMap).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("posture: parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("posture: execute template %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// readBuiltinManifest reads the manifest.json for a built-in profile.
func readBuiltinManifest(name string) (Manifest, error) {
	raw, err := builtinFS.ReadFile("builtin/" + name + "/manifest.json")
	if err != nil {
		return Manifest{}, fmt.Errorf("posture: read manifest for %s: %w", name, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("posture: parse manifest for %s: %w", name, err)
	}
	return m, nil
}

// readUserManifest reads the manifest.json from a user profile directory.
func readUserManifest(dir string) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("posture: read manifest in %s: %w", dir, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("posture: parse manifest in %s: %w", dir, err)
	}
	return m, nil
}
