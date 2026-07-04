// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/forge"
)

// snapshotsDirName is the directory under <repo-root>/.koryph/ that holds
// pre-change snapshots.
const snapshotsDirName = ".koryph/snapshots"

// gitignoreEntry is the line appended to the project's .gitignore to ensure
// snapshot files are never accidentally committed.
const gitignoreEntry = ".koryph/snapshots/"

// Snapshot holds the live state of every section managed by
// `koryph repo apply` / `koryph posture apply`, captured before any change is
// made. A snapshot can be fed back through the same apply machinery to restore
// the previous state (`koryph repo rollback`).
//
// NOTE: snapshots contain observed repo config — no secrets. They are
// gitignored by default; do not present committing them as an option.
type Snapshot struct {
	// CapturedAt is the UTC timestamp at which the snapshot was taken (RFC 3339).
	CapturedAt string `json:"captured_at"`

	// Repo is the "owner/name" slug that was inspected.
	Repo string `json:"repo"`

	// AppliedProfile is the posture profile name, when the snapshot was taken
	// for `koryph posture apply`. Mutually exclusive with IAC.
	AppliedProfile string `json:"applied_profile,omitempty"`

	// IAC is the source directory label (e.g. ".github"), when the snapshot was
	// taken for `koryph repo apply`. Mutually exclusive with AppliedProfile.
	IAC string `json:"iac,omitempty"`

	// Sections holds the live state of each managed section at capture time.
	Sections SnapshotSections `json:"sections"`
}

// SnapshotSections holds the per-section live state.
type SnapshotSections struct {
	// RepoFlags is the raw JSON of the repo flags section (description,
	// homepage, merge options, etc.).
	RepoFlags json.RawMessage `json:"repo_flags,omitempty"`

	// SecurityAndAnalysis is the flat status map of secret_scanning,
	// secret_scanning_push_protection, and dependabot_security_updates.
	SecurityAndAnalysis json.RawMessage `json:"security_and_analysis,omitempty"`

	// VulnerabilityAlerts is whether Dependabot vulnerability alerts are enabled.
	VulnerabilityAlerts *bool `json:"vulnerability_alerts,omitempty"`

	// ActionsWorkflowPermissions is the raw JSON of the actions workflow
	// permissions section.
	ActionsWorkflowPermissions json.RawMessage `json:"actions_workflow_permissions,omitempty"`

	// Rulesets maps each managed ruleset name to its full normalized JSON, as
	// returned by the GitHub API (volatile fields stripped).
	Rulesets map[string]json.RawMessage `json:"rulesets,omitempty"`
}

// SnapshotEntry is a lightweight descriptor used when listing snapshots.
type SnapshotEntry struct {
	// Path is the absolute filesystem path to the snapshot JSON file.
	Path string
	// CapturedAt is the parsed timestamp (UTC).
	CapturedAt time.Time
	// Repo is the "owner/name" slug.
	Repo string
}

// SnapshotsDir returns the path to the snapshots directory inside the project
// root. The directory may or may not exist yet.
func SnapshotsDir(root string) string {
	return filepath.Join(root, snapshotsDirName)
}

// EnsureGitignored appends the ".koryph/snapshots/" entry to root/.gitignore
// if it is not already present. Idempotent. A missing .gitignore is created.
func EnsureGitignored(root string) error {
	gitignorePath := filepath.Join(root, ".gitignore")

	// Check if the entry already exists.
	if data, err := os.ReadFile(gitignorePath); err == nil {
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == gitignoreEntry {
				return nil // already present
			}
		}
	}

	// Append the entry. Open for append-or-create; ensure a trailing newline.
	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("posture snapshot: open .gitignore: %w", err)
	}
	defer f.Close() //nolint:errcheck

	// Check if the file ends with a newline before appending.
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("posture snapshot: stat .gitignore: %w", err)
	}
	prefix := "\n"
	if fi.Size() == 0 {
		prefix = ""
	} else {
		// Read the last byte to decide whether we need a leading newline.
		buf := make([]byte, 1)
		if _, err := f.ReadAt(buf, fi.Size()-1); err == nil && buf[0] == '\n' {
			prefix = ""
		}
	}

	if _, err := fmt.Fprintf(f, "%s%s\n", prefix, gitignoreEntry); err != nil {
		return fmt.Errorf("posture snapshot: write .gitignore: %w", err)
	}
	return nil
}

// CaptureSnapshot fetches the current live state of all sections managed by
// koryph repo apply / koryph posture apply and writes it to a timestamped JSON
// file under <root>/.koryph/snapshots/.
//
// kind is either "profile:<name>" (posture apply) or "iac" (repo apply).
//
// It also ensures ".koryph/snapshots/" is in root/.gitignore.
//
// Returns the path of the written snapshot file.
func CaptureSnapshot(ctx context.Context, repoSvc forge.RepoService, prot forge.ProtectionService, repo, root, kind string) (string, error) {
	owner, repoName := splitOwnerRepo(repo)
	now := time.Now().UTC()
	snap := Snapshot{
		CapturedAt: now.Format(time.RFC3339),
		Repo:       repo,
	}

	if strings.HasPrefix(kind, "profile:") {
		snap.AppliedProfile = strings.TrimPrefix(kind, "profile:")
	} else {
		snap.IAC = kind
	}

	// Fetch live repo metadata once via the forge service.
	liveRepoRaw, err := repoSvc.GetRaw(ctx, owner, repoName)
	if err != nil {
		return "", fmt.Errorf("posture snapshot: fetch repo: %w", err)
	}
	var liveRepoFull map[string]json.RawMessage
	if err := json.Unmarshal(liveRepoRaw, &liveRepoFull); err != nil {
		return "", fmt.Errorf("posture snapshot: parse live repo: %w", err)
	}

	// Section 1: repo flags.
	repoFlags, err := extractRepoFlags(liveRepoFull)
	if err != nil {
		return "", fmt.Errorf("posture snapshot: extract repo flags: %w", err)
	}
	snap.Sections.RepoFlags = repoFlags

	// Section 2: security & analysis.
	secAndAnalysis, err := extractSecurityAndAnalysis(liveRepoFull)
	if err != nil {
		return "", fmt.Errorf("posture snapshot: extract security & analysis: %w", err)
	}
	snap.Sections.SecurityAndAnalysis = secAndAnalysis

	// Section 3: vulnerability alerts.
	liveVuln, err := repoSvc.VulnAlerts(ctx, owner, repoName)
	if err != nil {
		return "", fmt.Errorf("posture snapshot: check vuln alerts: %w", err)
	}
	snap.Sections.VulnerabilityAlerts = &liveVuln

	// Section 4: actions workflow permissions.
	liveActRaw, err := repoSvc.ActionsWorkflow(ctx, owner, repoName)
	if err != nil {
		return "", fmt.Errorf("posture snapshot: fetch actions perms: %w", err)
	}
	snap.Sections.ActionsWorkflowPermissions = liveActRaw

	// Section 5: rulesets — fetch each live ruleset by name and normalize.
	liveList, err := prot.List(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("posture snapshot: list rulesets: %w", err)
	}
	if len(liveList) > 0 {
		snap.Sections.Rulesets = make(map[string]json.RawMessage, len(liveList))
		for _, ref := range liveList {
			norm, err := normalizeRuleset(ref.Raw)
			if err != nil {
				return "", fmt.Errorf("posture snapshot: normalize ruleset %q: %w", ref.Name, err)
			}
			snap.Sections.Rulesets[ref.Name] = norm
		}
	}

	// Write the snapshot file.
	dir := SnapshotsDir(root)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("posture snapshot: create snapshots dir: %w", err)
	}

	// Ensure .gitignore entry exists (idempotent).
	if err := EnsureGitignored(root); err != nil {
		// Non-fatal: warn but don't abort the snapshot.
		fmt.Fprintf(os.Stderr, "warning: posture snapshot: could not update .gitignore: %v\n", err)
	}

	// File name: settings-<RFC3339>.json, colons replaced with hyphens for
	// cross-platform filename safety.
	ts := now.Format(time.RFC3339)
	safeTS := strings.ReplaceAll(ts, ":", "-")
	fname := fmt.Sprintf("settings-%s.json", safeTS)
	path := filepath.Join(dir, fname)

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", fmt.Errorf("posture snapshot: marshal: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o640); err != nil {
		return "", fmt.Errorf("posture snapshot: write: %w", err)
	}
	return path, nil
}

// ListSnapshots returns snapshot entries from <root>/.koryph/snapshots/,
// sorted newest-first. Returns nil (not an error) when the directory does not
// exist yet.
func ListSnapshots(root string) ([]SnapshotEntry, error) {
	dir := SnapshotsDir(root)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("posture snapshot: list snapshots: %w", err)
	}

	var out []SnapshotEntry
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue // skip unreadable files
		}
		var header struct {
			CapturedAt string `json:"captured_at"`
			Repo       string `json:"repo"`
		}
		if err := json.Unmarshal(data, &header); err != nil {
			continue // skip malformed files
		}
		t, err := time.Parse(time.RFC3339, header.CapturedAt)
		if err != nil {
			continue
		}
		out = append(out, SnapshotEntry{
			Path:       path,
			CapturedAt: t,
			Repo:       header.Repo,
		})
	}

	// Sort newest-first.
	sort.Slice(out, func(i, j int) bool {
		return out[i].CapturedAt.After(out[j].CapturedAt)
	})
	return out, nil
}

// LoadSnapshot reads and parses a snapshot file at path.
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("posture snapshot: read %s: %w", path, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("posture snapshot: parse %s: %w", path, err)
	}
	return &snap, nil
}

// snapshotSource implements Source using the contents of a Snapshot.  It
// materializes the snapshot sections into a temporary directory so the
// existing check/apply machinery can consume them unchanged.
type snapshotSource struct {
	root string // temp dir
}

// newSnapshotSource creates a temporary directory populated with the snapshot
// sections in the layout that LocalSource expects. Callers must call the
// returned cleanup func when done.
func newSnapshotSource(snap *Snapshot) (*snapshotSource, func(), error) {
	tmp, err := os.MkdirTemp("", "koryph-rollback-*")
	if err != nil {
		return nil, nil, fmt.Errorf("posture rollback: create temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmp) } //nolint:errcheck

	// Build the repo-settings.json from the snapshot sections.
	// Each section is optional; omit missing sections from the file so the
	// apply machinery skips them (nil JSON stays nil in the file).
	settings := struct {
		Repo                json.RawMessage `json:"repo,omitempty"`
		SecurityAndAnalysis json.RawMessage `json:"security_and_analysis,omitempty"`
		VulnerabilityAlerts *bool           `json:"vulnerability_alerts,omitempty"`
		ActionsWorkflow     json.RawMessage `json:"actions_workflow,omitempty"`
	}{
		Repo:                snap.Sections.RepoFlags,
		SecurityAndAnalysis: snap.Sections.SecurityAndAnalysis,
		VulnerabilityAlerts: snap.Sections.VulnerabilityAlerts,
		ActionsWorkflow:     snap.Sections.ActionsWorkflowPermissions,
	}

	settingsData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("posture rollback: marshal settings: %w", err)
	}

	ghDir := filepath.Join(tmp, ".github")
	if err := os.MkdirAll(ghDir, 0o750); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("posture rollback: create .github dir: %w", err)
	}
	settingsPath := filepath.Join(ghDir, "repo-settings.json")
	if err := os.WriteFile(settingsPath, append(settingsData, '\n'), 0o640); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("posture rollback: write settings: %w", err)
	}

	// Write each ruleset as a named JSON file under .github/rulesets/.
	if len(snap.Sections.Rulesets) > 0 {
		rulesetsDir := filepath.Join(ghDir, "rulesets")
		if err := os.MkdirAll(rulesetsDir, 0o750); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("posture rollback: create rulesets dir: %w", err)
		}
		for name, raw := range snap.Sections.Rulesets {
			fname := sanitizeFilename(name) + ".json"
			if err := os.WriteFile(filepath.Join(rulesetsDir, fname), raw, 0o640); err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("posture rollback: write ruleset %q: %w", name, err)
			}
		}
	}

	return &snapshotSource{root: tmp}, cleanup, nil
}

// RulesetsDir implements Source.
func (s *snapshotSource) RulesetsDir() (string, error) {
	dir := filepath.Join(s.root, ".github", "rulesets")
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("posture rollback: no rulesets in snapshot")
	}
	return dir, nil
}

// RepoSettingsFile implements Source.
func (s *snapshotSource) RepoSettingsFile() (string, error) {
	f := filepath.Join(s.root, ".github", "repo-settings.json")
	if _, err := os.Stat(f); err != nil {
		return "", fmt.Errorf("posture rollback: no settings in snapshot")
	}
	return f, nil
}

// OrgRulesetsDir implements Source. Snapshots do not capture org-level rulesets
// (those are org-scoped, not repo-scoped).
func (s *snapshotSource) OrgRulesetsDir() (string, error) {
	return "", fmt.Errorf("posture rollback: org rulesets not captured in snapshots")
}

// sanitizeFilename replaces characters that are unsafe in filenames with
// hyphens. Used to derive a JSON filename from a ruleset name.
func sanitizeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
