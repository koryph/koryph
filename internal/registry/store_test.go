// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/quota"
)

// gitProject creates a fresh directory that is a valid git repository, usable
// as a Record.Root.
func gitProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func commitCount(t *testing.T, home string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(gitOut(t, home, "rev-list", "--count", "HEAD")))
	if err != nil {
		t.Fatalf("commit count: %v", err)
	}
	return n
}

// newInitStore returns an initialized Store rooted at a fresh temp home.
func newInitStore(t *testing.T) *Store {
	t.Helper()
	s := NewStoreAt(t.TempDir())
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	return s
}

func sampleRecord(id, root string) *Record {
	return &Record{
		ProjectID:        id,
		Name:             id,
		Root:             root,
		DefaultBranch:    "main",
		AccountProfile:   ProfilePersonal,
		ExpectedIdentity: "personal@example.com",
	}
}

func TestInitIdempotent(t *testing.T) {
	home := t.TempDir()
	s := NewStoreAt(home)
	ctx := context.Background()

	if err := s.Init(ctx); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("second init: %v", err)
	}

	for _, d := range []string{".git", "registry.d", "quota"} {
		if _, err := os.Stat(filepath.Join(home, d)); err != nil {
			t.Fatalf("expected %s to exist: %v", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "README.md")); err != nil {
		t.Fatalf("expected README.md: %v", err)
	}

	// Idempotent: the second Init must not add a commit.
	if got := commitCount(t, home); got != 1 {
		t.Fatalf("expected exactly 1 commit after idempotent init, got %d", got)
	}
}

// TestInitTightensPerms asserts KORYPH_HOME and its state dirs are 0700 (so
// other local users can't read account identities / audit / quota), including
// migration of a pre-existing 0755 home.
func TestInitTightensPerms(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o755); err != nil { // simulate a legacy loose home
		t.Fatal(err)
	}
	s := NewStoreAt(home)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, d := range []string{"", "registry.d", "quota"} {
		fi, err := os.Stat(filepath.Join(home, d))
		if err != nil {
			t.Fatalf("stat %q: %v", d, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%q perm = %o, want 700", d, perm)
		}
	}
	// A written record is private (0600).
	if err := s.put(sampleRecord("proj", t.TempDir())); err != nil {
		t.Fatalf("put: %v", err)
	}
	fi, err := os.Stat(filepath.Join(home, "registry.d", "proj.json"))
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("record perm = %o, want 600", perm)
	}
}

func TestAddGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	rec := sampleRecord("my-proj", root)
	if err := s.Add(ctx, rec); err != nil {
		t.Fatalf("add: %v", err)
	}

	got, err := s.Get("my-proj")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProjectID != "my-proj" || got.Root != root {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("schema_version=%d, want 1", got.SchemaVersion)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("timestamps not set: created=%q updated=%q", got.CreatedAt, got.UpdatedAt)
	}
	if got.MigrationStatus != StatusRegistered {
		t.Fatalf("migration_status=%q, want %q", got.MigrationStatus, StatusRegistered)
	}

	if log := gitOut(t, s.Home, "log", "--oneline"); !strings.Contains(log, "register my-proj") {
		t.Fatalf("expected register commit in log:\n%s", log)
	}
}

func TestAddDuplicateRefused(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	rec := sampleRecord("dup", root)
	if err := s.Add(ctx, rec); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := s.Add(ctx, sampleRecord("dup", root)); err == nil {
		t.Fatal("expected duplicate add to be refused")
	}
}

func TestAddValidation(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	// Non-slug id.
	if err := s.Add(ctx, sampleRecord("Bad_ID", root)); err == nil {
		t.Fatal("expected invalid slug to be refused")
	}
	// Root that is not a git repo.
	notGit := sampleRecord("plain", t.TempDir())
	if err := s.Add(ctx, notGit); err == nil {
		t.Fatal("expected non-git root to be refused")
	}
	// Bad identity.
	badEmail := sampleRecord("bad-email", root)
	badEmail.ExpectedIdentity = "not-an-email"
	if err := s.Add(ctx, badEmail); err == nil {
		t.Fatal("expected non-email identity to be refused")
	}
}

func TestSaveRefusesAccountDrift(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	if err := s.Add(ctx, sampleRecord("drift", root)); err != nil {
		t.Fatalf("add: %v", err)
	}
	rec, err := s.Get("drift")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// A non-account edit is allowed.
	rec.Name = "renamed"
	if err := s.Save(ctx, rec); err != nil {
		t.Fatalf("save non-account edit: %v", err)
	}

	// Mutating an account field via Save must be refused.
	rec.AccountProfile = ProfileWork
	err = s.Save(ctx, rec)
	if err == nil {
		t.Fatal("expected account drift via Save to be refused")
	}
	if !strings.Contains(err.Error(), "SetAccount") {
		t.Fatalf("error should point at SetAccount, got: %v", err)
	}
}

func TestSetAccountHappyPath(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	if err := s.Add(ctx, sampleRecord("acct", root)); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Advance the migration status so we can prove SetAccount resets it.
	rec, err := s.Get("acct")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	rec.MigrationStatus = StatusValidated
	if err := s.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Empty reason is refused.
	if err := s.SetAccount(ctx, "acct", ProfileWork, "/cfg/work", "work@example.com", ""); err == nil {
		t.Fatal("expected empty reason to be refused")
	}

	if err := s.SetAccount(ctx, "acct", ProfileWork, "/cfg/work", "work@example.com", "moving to work account"); err != nil {
		t.Fatalf("set-account: %v", err)
	}

	got, err := s.Get("acct")
	if err != nil {
		t.Fatalf("get after set-account: %v", err)
	}
	if got.AccountProfile != ProfileWork {
		t.Fatalf("account_profile=%q, want %q", got.AccountProfile, ProfileWork)
	}
	if got.ClaudeConfigDir != "/cfg/work" {
		t.Fatalf("claude_config_dir=%q, want /cfg/work", got.ClaudeConfigDir)
	}
	if got.ExpectedIdentity != "work@example.com" {
		t.Fatalf("expected_identity=%q", got.ExpectedIdentity)
	}
	if got.MigrationStatus != StatusRegistered {
		t.Fatalf("migration_status=%q, want reset to %q", got.MigrationStatus, StatusRegistered)
	}

	// Audit line appended.
	audit, err := os.ReadFile(filepath.Join(s.Home, "audit.jsonl"))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(audit), `"kind":"set-account"`) {
		t.Fatalf("audit missing set-account line:\n%s", audit)
	}
	if !strings.Contains(string(audit), "moving to work account") {
		t.Fatalf("audit missing reason:\n%s", audit)
	}

	// Commit landed.
	if log := gitOut(t, s.Home, "log", "--oneline"); !strings.Contains(log, "set-account acct personal->work") {
		t.Fatalf("expected set-account commit in log:\n%s", log)
	}
}

// TestAgentProxyLoopbackValidation is the koryph-3l1.1 acceptance test for
// I4: agent_proxy.base_url is machine-checked at load, not merely documented.
// A record with an absent or loopback-hosted agent_proxy is accepted; any
// other host, scheme, or shape is refused at Add (which itself loads through
// validate).
func TestAgentProxyLoopbackValidation(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)

	cases := []struct {
		name    string
		proxy   *AgentProxy
		wantErr bool
	}{
		{"absent (direct)", nil, false},
		{"loopback IPv4", &AgentProxy{BaseURL: "http://127.0.0.1:8091"}, false},
		{"loopback IPv4 non-.1", &AgentProxy{BaseURL: "http://127.0.0.42:8091"}, false},
		{"localhost", &AgentProxy{BaseURL: "http://localhost:8091"}, false},
		{"loopback IPv6", &AgentProxy{BaseURL: "http://[::1]:8091"}, false},
		{"loopback with pin and health", &AgentProxy{BaseURL: "http://127.0.0.1:8091", Health: "http://127.0.0.1:8091/health", Pin: "v3"}, false},
		{"non-loopback host refused", &AgentProxy{BaseURL: "http://example.com:8091"}, true},
		{"public IP refused", &AgentProxy{BaseURL: "http://93.184.216.34:8091"}, true},
		{"https scheme refused (loopback needs no TLS)", &AgentProxy{BaseURL: "https://127.0.0.1:8091"}, true},
		{"empty base_url refused", &AgentProxy{Health: "http://127.0.0.1:8092/health"}, true},
		{"unparseable URL refused", &AgentProxy{BaseURL: "http://[::not-valid"}, true},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newInitStore(t)
			id := fmt.Sprintf("proj-%d", i)
			rec := sampleRecord(id, root)
			rec.AgentProxy = tc.proxy

			err := s.Add(ctx, rec)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Add succeeded with an invalid agent_proxy; want refusal at load")
				}
				return
			}
			if err != nil {
				t.Fatalf("Add: %v", err)
			}

			got, err := s.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if tc.proxy == nil {
				if got.AgentProxy != nil {
					t.Fatalf("AgentProxy = %+v, want nil", got.AgentProxy)
				}
				return
			}
			if got.AgentProxy == nil || got.AgentProxy.BaseURL != tc.proxy.BaseURL || got.AgentProxy.Pin != tc.proxy.Pin {
				t.Fatalf("AgentProxy roundtrip mismatch: %+v, want %+v", got.AgentProxy, tc.proxy)
			}
		})
	}
}

// TestAgentProxyHoldoutValidation is the koryph-3l1.3 acceptance test for
// AgentProxy.Holdout's load-time range check: unset (nil) and any value in
// [0, 1] are accepted; anything outside that range is refused at Add, the
// same "machine-checked, not just documented" contract as the loopback check
// above.
func TestAgentProxyHoldoutValidation(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)

	f := func(v float64) *float64 { return &v }

	cases := []struct {
		name    string
		holdout *float64
		wantErr bool
	}{
		{"unset (default)", nil, false},
		{"zero", f(0), false},
		{"one", f(1), false},
		{"typical 0.1", f(0.1), false},
		{"negative refused", f(-0.01), true},
		{"above one refused", f(1.01), true},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newInitStore(t)
			id := fmt.Sprintf("proj-holdout-%d", i)
			rec := sampleRecord(id, root)
			rec.AgentProxy = &AgentProxy{BaseURL: "http://127.0.0.1:8091", Holdout: tc.holdout}

			err := s.Add(ctx, rec)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Add succeeded with an out-of-range holdout; want refusal at load")
				}
				return
			}
			if err != nil {
				t.Fatalf("Add: %v", err)
			}

			got, err := s.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.AgentProxy == nil {
				t.Fatal("AgentProxy = nil after roundtrip")
			}
			gotH, wantH := got.AgentProxy.Holdout, tc.holdout
			switch {
			case gotH == nil && wantH == nil:
				// ok
			case gotH == nil || wantH == nil:
				t.Fatalf("Holdout roundtrip mismatch: got %v, want %v", gotH, wantH)
			case *gotH != *wantH:
				t.Fatalf("Holdout roundtrip = %v, want %v", *gotH, *wantH)
			}
		})
	}
}

// TestAgentProxyValidatedIndependentlyAtLoad proves the loopback check runs
// on every load path (Get and List), not merely inside Add — a record
// hand-edited (or written by a future codepath that bypasses Add) with a
// non-loopback agent_proxy must refuse to load rather than silently
// dispatching agents through a non-local endpoint.
func TestAgentProxyValidatedIndependentlyAtLoad(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	rec := sampleRecord("hand-edited", root)
	if err := s.Add(ctx, rec); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Bypass Add's validation entirely: write a bad agent_proxy straight to
	// the record file via the unexported put(), simulating a hand-edit.
	rec.AgentProxy = &AgentProxy{BaseURL: "http://evil.example.com"}
	if err := s.put(rec); err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, err := s.Get("hand-edited"); err == nil {
		t.Fatal("Get succeeded loading a non-loopback agent_proxy; want refusal at load")
	}
	if _, err := s.List(); err == nil {
		t.Fatal("List succeeded loading a non-loopback agent_proxy; want refusal at load")
	}
}

// TestAgentProxyIDAndProxyBaseURL covers AgentProxy.ID() (the ledger.Slot.
// ProxyID / future quota proxyID value) and Record.ProxyBaseURL() (the
// single accessor every spawn site threads into its ChildEnvSpec).
func TestAgentProxyIDAndProxyBaseURL(t *testing.T) {
	var nilProxy *AgentProxy
	if got := nilProxy.ID(); got != "" {
		t.Errorf("nil AgentProxy.ID() = %q, want \"\"", got)
	}

	p := &AgentProxy{BaseURL: "http://127.0.0.1:8091"}
	if got, want := p.ID(), "http://127.0.0.1:8091"; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
	p.Pin = "v3"
	if got, want := p.ID(), "http://127.0.0.1:8091#v3"; got != want {
		t.Errorf("ID() with pin = %q, want %q", got, want)
	}

	rec := &Record{}
	if got := rec.ProxyBaseURL(); got != "" {
		t.Errorf("ProxyBaseURL() with nil agent_proxy = %q, want \"\"", got)
	}
	rec.AgentProxy = p
	if got, want := rec.ProxyBaseURL(), "http://127.0.0.1:8091"; got != want {
		t.Errorf("ProxyBaseURL() = %q, want %q", got, want)
	}
}

func TestListSorted(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	for _, id := range []string{"zeta", "alpha", "mango"} {
		if err := s.Add(ctx, sampleRecord(id, root)); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}

	recs, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, len(recs))
	for i, r := range recs {
		got[i] = r.ProjectID
	}
	want := []string{"alpha", "mango", "zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("list order = %v, want %v", got, want)
	}
}

func TestFindByPathExactRoot(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)
	if err := s.Add(ctx, sampleRecord("proj", root)); err != nil {
		t.Fatalf("add: %v", err)
	}

	rec, err := s.FindByPath(root)
	if err != nil {
		t.Fatalf("FindByPath: %v", err)
	}
	if rec == nil || rec.ProjectID != "proj" {
		t.Fatalf("FindByPath(root) = %v, want project proj", rec)
	}
}

func TestFindByPathDescendant(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)
	if err := s.Add(ctx, sampleRecord("proj", root)); err != nil {
		t.Fatalf("add: %v", err)
	}

	sub := filepath.Join(root, "internal", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	rec, err := s.FindByPath(sub)
	if err != nil {
		t.Fatalf("FindByPath: %v", err)
	}
	if rec == nil || rec.ProjectID != "proj" {
		t.Fatalf("FindByPath(subdir) = %v, want project proj", rec)
	}
}

func TestFindByPathNoMatchReturnsNil(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)
	if err := s.Add(ctx, sampleRecord("proj", root)); err != nil {
		t.Fatalf("add: %v", err)
	}

	// A directory that is not inside any registered root.
	outside := t.TempDir()
	rec, err := s.FindByPath(outside)
	if err != nil {
		t.Fatalf("FindByPath: %v", err)
	}
	if rec != nil {
		t.Fatalf("FindByPath(outside) = %v, want nil (no match)", rec)
	}
}

// TestFindByPathNestedDeepestWins covers overlapping roots: an inner project
// registered inside an outer project's tree. A path in the inner tree must
// resolve to the inner (most specific) project, not the outer one.
func TestFindByPathNestedDeepestWins(t *testing.T) {
	ctx := context.Background()
	outer := gitProject(t)
	inner := filepath.Join(outer, "vendor", "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir inner: %v", err)
	}
	runGit(t, inner, "init")

	s := newInitStore(t)
	if err := s.Add(ctx, sampleRecord("outer", outer)); err != nil {
		t.Fatalf("add outer: %v", err)
	}
	if err := s.Add(ctx, sampleRecord("inner", inner)); err != nil {
		t.Fatalf("add inner: %v", err)
	}

	// A path inside the inner tree resolves to inner.
	deep := filepath.Join(inner, "sub")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}
	rec, err := s.FindByPath(deep)
	if err != nil {
		t.Fatalf("FindByPath: %v", err)
	}
	if rec == nil || rec.ProjectID != "inner" {
		t.Fatalf("FindByPath(inner subdir) = %v, want inner (deepest root wins)", rec)
	}

	// A path inside outer but outside inner resolves to outer.
	rec, err = s.FindByPath(outer)
	if err != nil {
		t.Fatalf("FindByPath: %v", err)
	}
	if rec == nil || rec.ProjectID != "outer" {
		t.Fatalf("FindByPath(outer root) = %v, want outer", rec)
	}
}

// TestFindByPathResolvesSymlinks verifies a symlinked path still matches the
// registered root: canonPath resolves symlinks on both sides.
func TestFindByPathResolvesSymlinks(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)
	if err := s.Add(ctx, sampleRecord("proj", root)); err != nil {
		t.Fatalf("add: %v", err)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	rec, err := s.FindByPath(link)
	if err != nil {
		t.Fatalf("FindByPath: %v", err)
	}
	if rec == nil || rec.ProjectID != "proj" {
		t.Fatalf("FindByPath(symlink) = %v, want project proj", rec)
	}
}

// TestSaveMarksCalibrationStaleOnProxyFlip verifies that Store.Save writes the
// CalibrationStale flag to the quota config when agent_proxy.ID() changes
// (koryph-3l1.2). The flag is written to s.quotaDir() so the test is hermetic
// (no KORYPH_HOME dependency).
func TestSaveMarksCalibrationStaleOnProxyFlip(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	// Register a project with an initial proxy.
	rec := sampleRecord("proxy-proj", root)
	rec.AgentProxy = &AgentProxy{
		BaseURL: "http://127.0.0.1:8787",
		Health:  "/health",
		Pin:     "headroom-ai==0.30.0",
	}
	if err := s.Add(ctx, rec); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Load the record back and flip to a different proxy pin.
	saved, err := s.Get("proxy-proj")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	saved.AgentProxy = &AgentProxy{
		BaseURL: "http://127.0.0.1:8787",
		Health:  "/health",
		Pin:     "headroom-ai==0.31.0", // pin changed → new proxy ID
	}
	if err := s.Save(ctx, saved); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Read the quota config from the store's own quota dir (not global KORYPH_HOME).
	account := rec.AccountProfile // "personal"
	qPath := filepath.Join(s.quotaDir(), account+".json")
	data, err := os.ReadFile(qPath)
	if err != nil {
		t.Fatalf("read quota config %s: %v", qPath, err)
	}
	var cfg quota.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse quota config: %v", err)
	}
	if !cfg.CalibrationStale {
		t.Errorf("CalibrationStale = false after proxy flip; want true")
	}
	if cfg.CalibrationStaleReason == "" {
		t.Errorf("CalibrationStaleReason is empty; want a non-empty reason string")
	}
}

// TestSaveNoStaleOnSameProxy verifies that Save does NOT mark calibration stale
// when the proxy identity is unchanged (same base_url + same pin).
func TestSaveNoStaleOnSameProxy(t *testing.T) {
	ctx := context.Background()
	root := gitProject(t)
	s := newInitStore(t)

	rec := sampleRecord("stable-proj", root)
	rec.AgentProxy = &AgentProxy{BaseURL: "http://127.0.0.1:8787", Pin: "v1"}
	if err := s.Add(ctx, rec); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Save with identical proxy — same base_url and same pin.
	saved, _ := s.Get("stable-proj")
	saved.AgentProxy = &AgentProxy{BaseURL: "http://127.0.0.1:8787", Pin: "v1"}
	if err := s.Save(ctx, saved); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Quota file should NOT be written (or should have CalibrationStale=false).
	qPath := filepath.Join(s.quotaDir(), rec.AccountProfile+".json")
	if data, err := os.ReadFile(qPath); err == nil {
		var cfg quota.Config
		if json.Unmarshal(data, &cfg) == nil && cfg.CalibrationStale {
			t.Errorf("CalibrationStale = true after no-change save; want false")
		}
	}
	// If file is absent (quota file never created), that's also fine — no stale.
}
