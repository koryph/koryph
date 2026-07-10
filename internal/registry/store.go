// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/netx"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/schemaver"
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
	// KORYPH_HOME holds account identities, the audit trail, and quota state:
	// private to the operator. 0700 on the tree keeps other local users out;
	// this also protects state files created 0644 by shared helpers, since the
	// parent dir blocks traversal. Chmod migrates a pre-existing 0755 home.
	for _, d := range []string{s.Home, s.registryDir(), s.quotaDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("registry: mkdir %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return fmt.Errorf("registry: chmod %s: %w", d, err)
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
	rec.SchemaVersion = schemaver.Current(schemaver.Registry)
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

// Get loads one record by project id. The agent_proxy block (if present) is
// validated at load (koryph-3l1.1, I4: loopback-only is machine-checked, not
// merely documented) — a record hand-edited (or written by a future version)
// with a non-loopback base_url refuses to load rather than dispatching
// through it silently.
func (s *Store) Get(id string) (*Record, error) {
	var rec Record
	if err := fsx.ReadJSON(s.recordPath(id), &rec); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("registry: %s not found (run 'koryph project list' to see registered projects)", id)
		}
		return nil, err
	}
	if err := schemaver.CheckRead(schemaver.Registry, rec.SchemaVersion); err != nil {
		return nil, err
	}
	if err := validateAgentProxy(&rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// List returns every record sorted by ProjectID. Each record's agent_proxy
// block is validated at load exactly as Get does (see Get's doc).
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
		if err := schemaver.CheckRead(schemaver.Registry, rec.SchemaVersion); err != nil {
			return nil, err
		}
		if err := validateAgentProxy(&rec); err != nil {
			return nil, err
		}
		recs = append(recs, &rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ProjectID < recs[j].ProjectID })
	return recs, nil
}

// FindByPath returns the registered project whose Root is path itself or an
// ancestor of path. When project roots are nested, the deepest (most specific)
// root wins. It returns (nil, nil) when path lies inside no registered
// project's root — that is a normal "not in a project here" outcome, not an
// error, so the caller decides how to surface it (koryph tui turns it into a
// "specify --project" usage message rather than opening an unrelated cockpit).
//
// Symlinks in both path and each record's Root are resolved before comparison
// so a symlinked checkout still matches the registered root; a path that does
// not resolve (e.g. does not yet exist) falls back to a lexical absolute clean.
func (s *Store) FindByPath(path string) (*Record, error) {
	recs, err := s.List()
	if err != nil {
		return nil, err
	}
	target := canonPath(path)
	var best *Record
	bestLen := -1
	for _, rec := range recs {
		root := canonPath(rec.Root)
		if !withinOrEqual(root, target) {
			continue
		}
		if len(root) > bestLen {
			best, bestLen = rec, len(root)
		}
	}
	return best, nil
}

// canonPath resolves p to an absolute, symlink-free path for comparison,
// falling back to a lexical absolute clean when the path cannot be resolved
// (e.g. it does not exist). Both the query path and each record's Root pass
// through here so the comparison is apples-to-apples (macOS, for instance,
// resolves /var → /private/var on only one side otherwise).
func canonPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// withinOrEqual reports whether child is parent itself or a descendant of it.
// Both paths must already be canonical (see canonPath).
func withinOrEqual(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Save updates an existing record. It refuses to change the account triple
// (AccountProfile / ClaudeConfigDir / ExpectedIdentity) relative to the
// on-disk record — those move only through SetAccount.
//
// When agent_proxy changes (by AgentProxy.ID()), the account's quota
// calibration is marked stale (koryph-3l1.2): the ccusage-USD vs /usage-%
// slope is not proven invariant under a compression change, so a re-run of
// `koryph quota calibrate` is prompted via the doctor check.
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

	// Get above already refused a newer-than-supported on-disk record
	// (schemaver.CheckRead), so this read-modify-write cannot silently strip a
	// newer binary's fields. Stamp the write at this build's version.
	rec.SchemaVersion = schemaver.Current(schemaver.Registry)

	// Detect proxy flip before stamping UpdatedAt so the diff is clear in the audit.
	oldProxyID := old.AgentProxy.ID()
	newProxyID := rec.AgentProxy.ID()

	rec.UpdatedAt = nowRFC3339()
	if err := s.put(rec); err != nil {
		return err
	}
	if err := s.Audit(Event{Kind: "update", ProjectID: rec.ProjectID}); err != nil {
		return err
	}

	// Best-effort: mark calibration stale when proxy identity changed. We use
	// SetCalibrationStaleAt with s.quotaDir() so tests that point the Store at a
	// temp home also write the stale flag there (not to the global KORYPH_HOME).
	if oldProxyID != newProxyID {
		qAccount := rec.QuotaProfile
		if qAccount == "" {
			qAccount = rec.AccountProfile
		}
		reason := fmt.Sprintf("agent_proxy changed for project %s (%q → %q); re-run `koryph quota calibrate --account %s`",
			rec.ProjectID, oldProxyID, newProxyID, qAccount)
		_ = quota.SetCalibrationStaleAt(qAccount, reason, s.quotaDir())
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
	return fsx.AppendLinePerm(s.auditLog(), data, 0o600)
}

// --- internals ---

func (s *Store) put(rec *Record) error {
	return fsx.WriteJSONAtomicPerm(s.recordPath(rec.ProjectID), rec, 0o600)
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
	return validateAgentProxy(rec)
}

// validateAgentProxy enforces the agent_proxy loopback-only invariant
// (koryph-3l1.1, design I4): absent AgentProxy (direct dispatch) is always
// valid; a present one must have a base_url that parses as an "http" URL
// (no https/other scheme — a loopback proxy has no need for TLS, and
// permitting one invites configuring a non-local endpoint under the guise of
// a scheme check) whose host is loopback (127.0.0.0/8, "localhost", or
// "::1"). Called from validate (Store.Add) and directly from Get/List so
// every load path machine-checks it, not just the docs. Also validates
// Holdout (koryph-3l1.3, design §3 L6) when explicitly set: it is a
// fraction, so anything outside [0, 1] is refused at load rather than
// silently clamped or ignored.
func validateAgentProxy(rec *Record) error {
	if rec.AgentProxy == nil {
		return nil
	}
	raw := rec.AgentProxy.BaseURL
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("registry: agent_proxy.base_url is required when agent_proxy is set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("registry: agent_proxy.base_url %q: %w", raw, err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("registry: agent_proxy.base_url %q must be an http URL (loopback-only; got scheme %q)", raw, u.Scheme)
	}
	host := u.Hostname()
	if !netx.IsLoopbackHost(host) {
		return fmt.Errorf("registry: agent_proxy.base_url %q host %q is not loopback (must be 127.0.0.1/127.0.0.0-8, localhost, or [::1])", raw, host)
	}
	if h := rec.AgentProxy.Holdout; h != nil && (*h < 0 || *h > 1) {
		return fmt.Errorf("registry: agent_proxy.holdout %v must be in [0, 1]", *h)
	}
	return nil
}
