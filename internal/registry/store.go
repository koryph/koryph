// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
)

// Store is the git-backed central registry rooted at Home (usually
// ~/.koryph). Every mutation writes a JSON record atomically, appends an
// audit event, and lands a conventional-commit so the history is the log.
//
// Home is the single source of truth for every subpath; Store never consults
// KORYPH_HOME directly, so a Store created with NewStoreAt is fully
// hermetic (this is what lets tests point Home at a t.TempDir()).
type Store struct {
	Home string
}

// NewStore returns a Store rooted at the resolved KoryphHome.
func NewStore() *Store { return &Store{Home: paths.KoryphHome()} }

// NewStoreAt returns a Store rooted at an explicit home directory.
func NewStoreAt(home string) *Store { return &Store{Home: home} }

var (
	slugRe  = regexp.MustCompile(`^[a-z0-9-]+$`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

const readmeStub = `# koryph home

Machine-local koryph state. This directory is a git repository: every
registry and audit mutation is a commit, so the history is the log.

Do not edit files here by hand — use the koryph CLI.
`

// --- path helpers (all derived from Home, never from the environment) ---

func (s *Store) registryDir() string { return filepath.Join(s.Home, "registry.d") }
func (s *Store) quotaDir() string    { return filepath.Join(s.Home, "quota") }
func (s *Store) auditLog() string    { return filepath.Join(s.Home, "audit.jsonl") }
func (s *Store) recordPath(id string) string {
	return filepath.Join(s.registryDir(), id+".json")
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// Init creates Home and its layout, makes it a git repository (with a stable
// koryph identity), seeds a README, and lands the initial commit. It is
// idempotent: re-running introduces no churn and no extra commits.
func (s *Store) Init(ctx context.Context) error {
	for _, d := range []string{s.Home, s.registryDir(), s.quotaDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("registry: mkdir %s: %w", d, err)
		}
	}

	if !fsx.Exists(filepath.Join(s.Home, ".git")) {
		if _, err := execx.MustSucceed(ctx, s.git("init", "-b", "main")); err != nil {
			return err
		}
	}

	// Repo-local identity + hardening so commits never depend on machine git
	// config, global hooks, or GPG signing.
	for _, kv := range [][2]string{
		{"user.name", "koryph"},
		{"user.email", "koryph@local"},
		{"commit.gpgsign", "false"},
	} {
		if _, err := execx.MustSucceed(ctx, s.git("config", kv[0], kv[1])); err != nil {
			return err
		}
	}

	readme := filepath.Join(s.Home, "README.md")
	if !fsx.Exists(readme) {
		if err := fsx.WriteAtomic(readme, []byte(readmeStub), 0o644); err != nil {
			return err
		}
	}

	if !s.hasCommits(ctx) {
		if err := s.commit(ctx, "chore(registry): initialize koryph home"); err != nil {
			return err
		}
	}
	return nil
}

// Add registers a new project. It validates the record, refuses duplicates,
// stamps schema/timestamps, writes atomically, audits, and commits.
func (s *Store) Add(ctx context.Context, rec *Record) error {
	if err := validate(rec); err != nil {
		return err
	}
	if fsx.Exists(s.recordPath(rec.ProjectID)) {
		return fmt.Errorf("registry: %s already registered", rec.ProjectID)
	}

	now := nowRFC3339()
	rec.SchemaVersion = 1
	rec.CreatedAt = now
	rec.UpdatedAt = now
	if rec.MigrationStatus == "" {
		rec.MigrationStatus = StatusRegistered
	}

	if err := s.put(rec); err != nil {
		return err
	}
	if err := s.Audit(Event{
		Kind:      "register",
		ProjectID: rec.ProjectID,
		Detail:    map[string]string{"root": rec.Root, "account_profile": rec.AccountProfile},
	}); err != nil {
		return err
	}
	return s.commit(ctx, "feat(registry): register "+rec.ProjectID)
}

// Get loads one record by project id.
func (s *Store) Get(id string) (*Record, error) {
	var rec Record
	if err := fsx.ReadJSON(s.recordPath(id), &rec); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("registry: %s not found", id)
		}
		return nil, err
	}
	return &rec, nil
}

// List returns every record sorted by ProjectID.
func (s *Store) List() ([]*Record, error) {
	entries, err := os.ReadDir(s.registryDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []*Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var rec Record
		if err := fsx.ReadJSON(filepath.Join(s.registryDir(), e.Name()), &rec); err != nil {
			return nil, err
		}
		recs = append(recs, &rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ProjectID < recs[j].ProjectID })
	return recs, nil
}

// Save updates an existing record. It refuses to change the account triple
// (AccountProfile / ClaudeConfigDir / ExpectedIdentity) relative to the
// on-disk record — those move only through SetAccount.
func (s *Store) Save(ctx context.Context, rec *Record) error {
	old, err := s.Get(rec.ProjectID)
	if err != nil {
		return err
	}
	if rec.AccountProfile != old.AccountProfile ||
		rec.ClaudeConfigDir != old.ClaudeConfigDir ||
		rec.ExpectedIdentity != old.ExpectedIdentity {
		return fmt.Errorf("registry: account fields are immutable via Save; use SetAccount")
	}

	rec.UpdatedAt = nowRFC3339()
	if err := s.put(rec); err != nil {
		return err
	}
	if err := s.Audit(Event{Kind: "update", ProjectID: rec.ProjectID}); err != nil {
		return err
	}
	return s.commit(ctx, "chore(registry): update "+rec.ProjectID)
}

// SetAccount is the ONLY path that mutates the account triple. It requires a
// non-empty reason, records the prior values in the audit detail, resets the
// migration status to StatusRegistered (forcing re-validation before the next
// dispatch), and commits.
func (s *Store) SetAccount(ctx context.Context, id, profile, configDir, expectedIdentity, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("registry: SetAccount requires a non-empty reason")
	}
	if strings.TrimSpace(profile) == "" {
		return fmt.Errorf("registry: SetAccount requires a non-empty account_profile")
	}
	if !emailRe.MatchString(expectedIdentity) {
		return fmt.Errorf("registry: expected_identity %q must be an email", expectedIdentity)
	}

	rec, err := s.Get(id)
	if err != nil {
		return err
	}

	old := map[string]string{
		"account_profile":   rec.AccountProfile,
		"claude_config_dir": rec.ClaudeConfigDir,
		"expected_identity": rec.ExpectedIdentity,
	}

	rec.AccountProfile = profile
	rec.ClaudeConfigDir = configDir
	rec.ExpectedIdentity = expectedIdentity
	rec.MigrationStatus = StatusRegistered
	rec.UpdatedAt = nowRFC3339()

	if err := s.put(rec); err != nil {
		return err
	}
	if err := s.Audit(Event{
		Kind:      "set-account",
		ProjectID: id,
		Detail: map[string]any{
			"reason": reason,
			"old":    old,
			"new": map[string]string{
				"account_profile":   profile,
				"claude_config_dir": configDir,
				"expected_identity": expectedIdentity,
			},
		},
	}); err != nil {
		return err
	}
	return s.commit(ctx, fmt.Sprintf("feat(registry): set-account %s %s->%s", id, old["account_profile"], profile))
}

// Audit appends one event to the audit log (append-only; never rewritten).
func (s *Store) Audit(ev Event) error {
	if ev.At == "" {
		ev.At = nowRFC3339()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return fsx.AppendLine(s.auditLog(), data)
}

// --- internals ---

func (s *Store) put(rec *Record) error {
	return fsx.WriteJSONAtomic(s.recordPath(rec.ProjectID), rec)
}

func (s *Store) git(args ...string) execx.Cmd {
	return execx.Cmd{Dir: s.Home, Name: "git", Args: args}
}

func (s *Store) hasCommits(ctx context.Context) bool {
	res, err := execx.Run(ctx, s.git("rev-parse", "--verify", "-q", "HEAD"))
	return err == nil && res.ExitCode == 0
}

// commit stages everything and commits with msg, tolerating a clean tree.
// Hooks and signing are skipped so an unattended registry commit can never be
// blocked by a machine's global git configuration.
func (s *Store) commit(ctx context.Context, msg string) error {
	res, err := execx.MustSucceed(ctx, s.git("status", "--porcelain"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return nil // nothing to commit
	}
	if _, err := execx.MustSucceed(ctx, s.git("add", "-A")); err != nil {
		return err
	}
	_, err = execx.MustSucceed(ctx, s.git("commit", "--no-verify", "-m", msg))
	return err
}

func validate(rec *Record) error {
	if rec == nil {
		return fmt.Errorf("registry: nil record")
	}
	if !slugRe.MatchString(rec.ProjectID) {
		return fmt.Errorf("registry: project_id %q must be a slug matching [a-z0-9-]+", rec.ProjectID)
	}
	if rec.Root == "" || !fsx.Exists(rec.Root) {
		return fmt.Errorf("registry: root %q does not exist", rec.Root)
	}
	if !fsx.Exists(filepath.Join(rec.Root, ".git")) {
		return fmt.Errorf("registry: root %q is not a git repository (.git missing)", rec.Root)
	}
	if strings.TrimSpace(rec.AccountProfile) == "" {
		return fmt.Errorf("registry: account_profile required")
	}
	if !emailRe.MatchString(rec.ExpectedIdentity) {
		return fmt.Errorf("registry: expected_identity %q must be an email", rec.ExpectedIdentity)
	}
	return nil
}
