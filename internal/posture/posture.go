// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package posture implements desired-state checking and applying for GitHub
// repository settings: branch-protection rulesets (.github/rulesets/*.json)
// and administrative settings (.github/repo-settings.json).
//
// It is the Go implementation of scripts/ensure-rulesets.sh and
// scripts/ensure-repo-settings.sh. Authentication is via gh-CLI passthrough:
// every API call is delegated to "gh api", which honours the authenticated
// credential already managed by the operator's gh installation. No token
// management or HTTP client setup is required in this package.
//
// Named-profile sources (~/.koryph/postures) are the next bead.  The
// [Source] interface is the planned seam: a profile-based source will
// implement it and slot in without touching the check/apply logic.
package posture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Source is the abstraction over desired-state providers.
//
// Current implementation: [LocalSource] reads from the project's .github/
// directory.  Next bead: named-profile sources (~/.koryph/postures) will
// implement this interface.
type Source interface {
	// RulesetsDir returns the path to the directory containing *.json ruleset
	// files, or a non-nil error when no such directory is available.
	RulesetsDir() (string, error)
	// RepoSettingsFile returns the path to the repo-settings.json desired-state
	// file, or a non-nil error when no such file is available.
	RepoSettingsFile() (string, error)
	// OrgRulesetsDir returns the path to the directory containing *.json
	// org-level ruleset files, or a non-nil error when no such directory is
	// available.  Callers treat the error as "no org rulesets" and skip the
	// org section gracefully.
	OrgRulesetsDir() (string, error)
}

// LocalSource reads desired state from the project's own .github/ directory.
// Root must be an absolute path to the repository root (the directory that
// contains .github/).
type LocalSource struct {
	Root string
}

// RulesetsDir implements [Source].
func (s LocalSource) RulesetsDir() (string, error) {
	dir := s.Root + "/.github/rulesets"
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("posture: no desired-state dir: %s", dir)
	}
	return dir, nil
}

// RepoSettingsFile implements [Source].
func (s LocalSource) RepoSettingsFile() (string, error) {
	f := s.Root + "/.github/repo-settings.json"
	if _, err := os.Stat(f); err != nil {
		return "", fmt.Errorf("posture: no desired-state file: %s", f)
	}
	return f, nil
}

// OrgRulesetsDir implements [Source].
func (s LocalSource) OrgRulesetsDir() (string, error) {
	dir := s.Root + "/.github/org-rulesets"
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("posture: no org rulesets dir: %s", dir)
	}
	return dir, nil
}

// PermissionError is returned when a GitHub API call fails with HTTP 403 due
// to insufficient permissions. It names the exact permission the caller needs.
type PermissionError struct {
	// Action describes the attempted operation, e.g.
	// "list org rulesets for koryph-hq".
	Action string
	// Needed is the required permission, e.g. "org owner / admin access".
	Needed string
	// Detail is the raw message from GitHub (may be empty).
	Detail string
}

func (e *PermissionError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("posture: %s requires %s: %s", e.Action, e.Needed, e.Detail)
	}
	return fmt.Sprintf("posture: %s requires %s", e.Action, e.Needed)
}

// GHBin returns the gh CLI binary path, honouring the KORYPH_GH_BIN
// environment variable (same convention as the rest of the koryph CLI).
func GHBin() string {
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		return v
	}
	return "gh"
}

// DetectRepo resolves the current repository's "owner/name" slug by calling
// "gh repo view".  When the caller provides an explicit --repo flag value,
// pass it directly and skip this function.
func DetectRepo(ctx context.Context, ghBin string) (string, error) {
	out, code, err := ghRun(ctx, ghBin, []string{
		"repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner",
	}, nil)
	if err != nil {
		return "", fmt.Errorf("posture: detect repo: %w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("posture: detect repo: gh exited %d", code)
	}
	return strings.TrimSpace(string(out)), nil
}

// ghRun executes the gh binary with the given arguments. input, if non-nil,
// is written to a temporary file and passed via --input.  It returns the
// stdout bytes, the process exit code, and any spawn-level error (non-zero
// exit is not a spawn error).
func ghRun(ctx context.Context, ghBin string, args []string, input []byte) ([]byte, int, error) {
	finalArgs := args

	var tmpFile string
	if input != nil {
		f, err := os.CreateTemp("", "koryph-posture-*.json")
		if err != nil {
			return nil, -1, fmt.Errorf("posture: create temp file: %w", err)
		}
		tmpFile = f.Name()
		defer os.Remove(tmpFile) //nolint:errcheck
		if _, err := f.Write(input); err != nil {
			f.Close()
			return nil, -1, fmt.Errorf("posture: write temp file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, -1, fmt.Errorf("posture: close temp file: %w", err)
		}
		finalArgs = append(append([]string{}, args...), "--input", tmpFile)
	}

	cmd := exec.CommandContext(ctx, ghBin, finalArgs...) //nolint:gosec
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	runErr := cmd.Run()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return out.Bytes(), ee.ExitCode(), nil
		}
		return nil, -1, fmt.Errorf("posture: exec gh: %w: %s", runErr, errb.String())
	}
	return out.Bytes(), 0, nil
}

// jsonSortKeys unmarshals raw JSON into a generic map, then re-marshals with
// sorted keys (Go's encoding/json sorts map keys lexicographically).
// It returns the canonical, indented representation.
func jsonSortKeys(raw []byte) ([]byte, error) {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.MarshalIndent(v, "", "  ")
}

// normalizeRuleset produces the canonical form of a ruleset JSON object for
// structural comparison.  It replicates the shell script's jq normalization:
//
//   - strips server-assigned / volatile fields: id, source, source_type,
//     created_at, updated_at, node_id, _links, current_user_can_bypass
//   - ensures bypass_actors defaults to [] when absent/null
//   - sorts all keys
func normalizeRuleset(raw []byte) ([]byte, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	// Strip volatile server-assigned fields.
	for _, k := range []string{
		"id", "source", "source_type", "created_at", "updated_at",
		"node_id", "_links", "current_user_can_bypass",
	} {
		delete(m, k)
	}
	// bypass_actors defaults to [] (mirrors jq's //= operator).
	if v, ok := m["bypass_actors"]; !ok || v == nil {
		m["bypass_actors"] = []interface{}{}
	}
	return json.MarshalIndent(m, "", "  ")
}

// jsonLines returns the lines of a JSON blob (for diff output).
func jsonLines(b []byte) []string {
	return strings.Split(string(b), "\n")
}

// printDiff emits a unified-ish diff between live and want to w, prefixed with
// spaces so it is visually indented under the DRIFT label.  Lines are capped
// at 40, matching the shell script's head -40.
func printDiff(w io.Writer, live, want []byte) {
	ls := jsonLines(live)
	ws := jsonLines(want)

	// Build a simple +/- diff: lines only in live appear with "-", lines only
	// in want appear with "+".
	// Use the longest sequence as the frame and annotate divergences.
	printed := 0
	lIdx, wIdx := 0, 0
	for (lIdx < len(ls) || wIdx < len(ws)) && printed < 40 {
		lLine := ""
		wLine := ""
		if lIdx < len(ls) {
			lLine = ls[lIdx]
		}
		if wIdx < len(ws) {
			wLine = ws[wIdx]
		}

		if lLine == wLine {
			fmt.Fprintf(w, "         %s\n", lLine)
			lIdx++
			wIdx++
			printed++
			continue
		}
		// Lines differ — emit both sides tagged.
		if lIdx < len(ls) {
			fmt.Fprintf(w, "         - %s\n", lLine)
			lIdx++
			printed++
		}
		if wIdx < len(ws) {
			fmt.Fprintf(w, "         + %s\n", wLine)
			wIdx++
			printed++
		}
	}
	if printed >= 40 {
		fmt.Fprintln(w, "         ... (truncated)")
	}
}
