// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package beads

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/execx"
)

// fakeBD is a stand-in `bd` binary. It appends its full argv (one line) to the
// file named by $BD_ARGS_LOG, dispatches canned JSON on $1, and honors
// $BD_FAIL (exit 1 for `update`). It intentionally emits a different envelope
// shape per verb to exercise tolerant parsing.
const fakeBD = `#!/bin/sh
if [ -n "$BD_ARGS_LOG" ]; then
  echo "$@" >> "$BD_ARGS_LOG"
fi
case "$1" in
  version)
    printf 'bd 0.42.0\nbuild deadbeef\n'
    ;;
  ready)
    # JSON array, integer priority, one issue with null labels.
    printf '[{"id":"a-1","title":"Alpha","status":"open","priority":1,"issue_type":"task","labels":["fp:go:api"],"parent":"epic-1"},{"id":"a-2","title":"Beta","status":"open","priority":0,"issue_type":"task","labels":null}]'
    ;;
  show)
    # {"issue":{...}} envelope, string priority "P2", non-empty notes (a
    # pre-dispatch operator addendum, koryph-o72).
    printf '{"issue":{"id":"a-9","title":"Gamma","status":"in_progress","priority":"P2","issue_type":"task","labels":["area:web"],"notes":"pre-dispatch addendum"}}'
    ;;
  list)
    # {"issues":[...]} envelope.
    printf '{"issues":[{"id":"c-1","title":"Child","status":"open","priority":2,"issue_type":"task"}]}'
    ;;
  create)
    # --silent (single-bead create) emits only a canned id. Otherwise echo
    # stdin so graph tests can confirm the JSON was piped through.
    if printf '%s' "$*" | grep -q -- '--silent'; then
      printf 'bd-created-1\n'
    else
      if [ "$2" = "--graph" ] && printf '%s' "$*" | grep -q -- '--dry-run'; then
        printf 'DRYRUN:'
      fi
      cat
    fi
    ;;
  update)
    if [ -n "$BD_FAIL" ]; then
      echo "boom" >&2
      exit 1
    fi
    ;;
  *)
    : # comment/close/remember: succeed silently
    ;;
esac
`

// newFakeAdapter writes the fake bd script into a temp repo and returns an
// adapter pointed at it plus the argv-log path.
func newFakeAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	repo := t.TempDir()
	bin := filepath.Join(repo, "bd")
	if err := os.WriteFile(bin, []byte(fakeBD), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	log := filepath.Join(repo, "argv.log")
	t.Setenv("BD_ARGS_LOG", log)
	return &Adapter{RepoRoot: repo, BeadsDir: filepath.Join(repo, ".beads"), Bin: bin}, log
}

func lastArgs(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return lines
}

func TestVersion(t *testing.T) {
	a, _ := newFakeAdapter(t)
	got, err := a.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "bd 0.42.0" {
		t.Fatalf("version = %q, want first line only", got)
	}
}

func TestReady(t *testing.T) {
	a, log := newFakeAdapter(t)
	got, err := a.Ready(context.Background(), ReadyOpts{Parent: "epic-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d issues, want 2", len(got))
	}
	// Input order preserved (a-1 first even though a-2 is higher priority).
	if got[0].ID != "a-1" || got[1].ID != "a-2" {
		t.Fatalf("order not preserved: %+v", []string{got[0].ID, got[1].ID})
	}
	if got[0].Priority != 1 {
		t.Fatalf("int priority = %d, want 1", got[0].Priority)
	}
	if got[0].ParentID != "epic-1" {
		t.Fatalf("parent = %q", got[0].ParentID)
	}
	// null labels -> empty, non-nil slice.
	if got[1].Labels == nil || len(got[1].Labels) != 0 {
		t.Fatalf("null labels = %#v, want []string{}", got[1].Labels)
	}
	args := lastArgs(t, log)
	if args[0] != "ready --json --limit 0 --parent epic-1" {
		t.Fatalf("ready argv = %q", args[0])
	}
}

func TestReadyNoParent(t *testing.T) {
	a, log := newFakeAdapter(t)
	if _, err := a.Ready(context.Background(), ReadyOpts{}); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, log)[0]; got != "ready --json --limit 0" {
		t.Fatalf("ready argv = %q, want no --parent", got)
	}
}

func TestShowStringPriorityAndEnvelope(t *testing.T) {
	a, log := newFakeAdapter(t)
	got, err := a.Show(context.Background(), "a-9")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "a-9" {
		t.Fatalf("id = %q", got.ID)
	}
	if got.Priority != 2 {
		t.Fatalf("string priority %q parsed to %d, want 2", "P2", got.Priority)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q", got.Status)
	}
	if got.Notes != "pre-dispatch addendum" {
		t.Fatalf("notes = %q, want the bd notes field carried verbatim (koryph-o72)", got.Notes)
	}
	if args := lastArgs(t, log)[0]; args != "show a-9 --json" {
		t.Fatalf("show argv = %q", args)
	}
}

func TestListChildren(t *testing.T) {
	a, log := newFakeAdapter(t)
	got, err := a.ListChildren(context.Background(), "epic-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-1" {
		t.Fatalf("children = %+v", got)
	}
	if args := lastArgs(t, log)[0]; args != "list --parent epic-1 --json" {
		t.Fatalf("list argv = %q", args)
	}
}

func TestListChildrenAll(t *testing.T) {
	a, log := newFakeAdapter(t)
	got, err := a.ListChildrenAll(context.Background(), "epic-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-1" {
		t.Fatalf("children = %+v", got)
	}
	if args := lastArgs(t, log)[0]; args != "list --parent epic-1 --json --all --limit 0" {
		t.Fatalf("list argv = %q", args)
	}
}

func TestMutationVerbs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		call func(a *Adapter) error
		want string
	}{
		{"comment", func(a *Adapter) error { return a.Comment(ctx, "x-1", "hello world") }, "comment x-1 hello world"},
		{"appendnotes", func(a *Adapter) error { return a.AppendNotes(ctx, "x-1", "pre-dispatch nudge") }, "update x-1 --append-notes pre-dispatch nudge"},
		{"close", func(a *Adapter) error { return a.Close(ctx, "x-1", "done") }, "close x-1 --reason done"},
		{"claim", func(a *Adapter) error { return a.Claim(ctx, "x-1") }, "update x-1 --claim"},
		{"setstatus", func(a *Adapter) error { return a.SetStatus(ctx, "x-1", "blocked") }, "update x-1 --status blocked"},
		{"remember", func(a *Adapter) error { return a.Remember(ctx, "a note") }, "remember a note"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, log := newFakeAdapter(t)
			if err := tc.call(a); err != nil {
				t.Fatal(err)
			}
			if got := lastArgs(t, log)[0]; got != tc.want {
				t.Fatalf("argv = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCreateGraph(t *testing.T) {
	a, _ := newFakeAdapter(t)
	graph := `{"nodes":[{"id":"n1"}]}`

	dry, err := a.CreateGraph(context.Background(), graph, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry != "DRYRUN:"+graph {
		t.Fatalf("dry-run stdout = %q", dry)
	}

	live, err := a.CreateGraph(context.Background(), graph, false)
	if err != nil {
		t.Fatal(err)
	}
	if live != graph {
		t.Fatalf("live stdout = %q, want echoed stdin", live)
	}
}

func TestListByLabel(t *testing.T) {
	a, log := newFakeAdapter(t)
	got, err := a.ListByLabel(context.Background(), "gh-42")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-1" {
		t.Fatalf("list-by-label = %+v", got)
	}
	if args := lastArgs(t, log)[0]; args != "list --label gh-42 --json --limit 0 --all" {
		t.Fatalf("list-by-label argv = %q", args)
	}
}

func TestListByExternalRef(t *testing.T) {
	a, log := newFakeAdapter(t)
	got, err := a.ListByExternalRef(context.Background(), "gh-42")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-1" {
		t.Fatalf("list-by-external-ref = %+v", got)
	}
	if args := lastArgs(t, log)[0]; args != "list --external-ref gh-42 --json --limit 0 --all" {
		t.Fatalf("list-by-external-ref argv = %q", args)
	}
}

func TestCreate(t *testing.T) {
	a, log := newFakeAdapter(t)
	id, err := a.Create(context.Background(), CreateInput{
		Title:       "Crash on login",
		Description: "body text\nsecond line",
		Labels:      []string{"gh-34", "intake", "no-dispatch"},
		Priority:    1,
		IssueType:   "bug",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "bd-created-1" {
		t.Fatalf("create id = %q, want the --silent id", id)
	}
	args := lastArgs(t, log)[0]
	// Description is piped on stdin (not argv), so argv carries only flags.
	for _, want := range []string{
		"--silent",
		"--body-file -",
		"--priority 1",
		"--labels gh-34,intake,no-dispatch",
		"--type bug",
		// The (attacker-influenceable) title follows a `--` end-of-options
		// terminator so a title starting with `-` cannot inject a bd flag.
		"-- Crash on login",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("create argv %q missing %q", args, want)
		}
	}
	// The title must NOT appear as the immediate positional after `create`
	// (the option-injection-vulnerable form).
	if strings.Contains(args, "create Crash on login") {
		t.Fatalf("title must follow `--`, not sit adjacent to `create`: %q", args)
	}
	if strings.Contains(args, "body text") || strings.Contains(args, "second line") {
		t.Fatalf("description must be piped on stdin, not argv: %q", args)
	}
}

func TestCreateNoTypeNoLabels(t *testing.T) {
	a, log := newFakeAdapter(t)
	if _, err := a.Create(context.Background(), CreateInput{
		Title:       "Plain",
		Description: "d",
		Priority:    2,
	}); err != nil {
		t.Fatal(err)
	}
	args := lastArgs(t, log)[0]
	if strings.Contains(args, "--type") {
		t.Fatalf("no --type expected when IssueType empty: %q", args)
	}
	if strings.Contains(args, "--labels") {
		t.Fatalf("no --labels expected when Labels empty: %q", args)
	}
	if !strings.Contains(args, "--priority 2") {
		t.Fatalf("priority flag missing: %q", args)
	}
	if strings.Contains(args, "--external-ref") {
		t.Fatalf("no --external-ref expected when ExternalRef empty: %q", args)
	}
}

func TestCreateWithExternalRef(t *testing.T) {
	a, log := newFakeAdapter(t)
	if _, err := a.Create(context.Background(), CreateInput{
		Title:       "Tracked issue",
		Description: "body",
		Priority:    1,
		ExternalRef: "gh-99",
	}); err != nil {
		t.Fatal(err)
	}
	args := lastArgs(t, log)[0]
	if !strings.Contains(args, "--external-ref gh-99") {
		t.Fatalf("create argv missing --external-ref: %q", args)
	}
}

// hungBD is a stand-in `bd` that never returns (sleeps far longer than any test
// bound). It models a bd/dolt lock held by another process wedging the binary.
const hungBD = "#!/bin/sh\nsleep 300\n"

// newHungAdapter points an adapter at the hung stub with a tight Timeout so the
// timeout fires in milliseconds.
func newHungAdapter(t *testing.T, timeout time.Duration) *Adapter {
	t.Helper()
	repo := t.TempDir()
	bin := filepath.Join(repo, "bd")
	if err := os.WriteFile(bin, []byte(hungBD), 0o755); err != nil {
		t.Fatalf("write hung bd: %v", err)
	}
	return &Adapter{RepoRoot: repo, BeadsDir: filepath.Join(repo, ".beads"), Bin: bin, Timeout: timeout}
}

// TestHungBdReadTimesOutWithinBound proves the core koryph-1dg guarantee: a
// loop-critical read (Show — called by beadClosedMidFlight before every requeue)
// against a wedged bd binary cannot stall past the adapter's timeout, and the
// timeout surfaces distinctly via errors.Is(err, execx.ErrTimeout).
func TestHungBdReadTimesOutWithinBound(t *testing.T) {
	bound := 100 * time.Millisecond
	a := newHungAdapter(t, bound)

	start := time.Now()
	_, err := a.Show(context.Background(), "x-1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error from a hung bd, got nil")
	}
	if !errors.Is(err, execx.ErrTimeout) {
		t.Fatalf("error does not wrap execx.ErrTimeout: %v", err)
	}
	// Generous slack for process spawn/kill scheduling, but far below the 300s
	// the stub would otherwise sleep — proving the call is genuinely bounded.
	if elapsed > 10*time.Second {
		t.Fatalf("Show took %s, expected it bounded near %s", elapsed, bound)
	}
}

// TestHungBdWriteTimesOut proves writes (Claim/Comment/AddLabel — best-effort on
// the dispatch/complete path) are bounded too and surface a timeout distinctly.
func TestHungBdWriteTimesOut(t *testing.T) {
	a := newHungAdapter(t, 100*time.Millisecond)
	start := time.Now()
	err := a.Claim(context.Background(), "x-1")
	if err == nil {
		t.Fatal("expected a timeout error from a hung bd claim, got nil")
	}
	if !errors.Is(err, execx.ErrTimeout) {
		t.Fatalf("claim error does not wrap execx.ErrTimeout: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Claim took %s, expected it bounded", elapsed)
	}
}

// TestAdapterTimeoutDefaults asserts the per-class defaults exist and reads are
// bounded no looser than writes (a stale read is cheap to retry next pass).
func TestAdapterTimeoutDefaults(t *testing.T) {
	a := &Adapter{} // no override
	if a.readTimeout() <= 0 || a.writeTimeout() <= 0 {
		t.Fatalf("default timeouts must be finite and positive: read=%s write=%s", a.readTimeout(), a.writeTimeout())
	}
	if a.readTimeout() > a.writeTimeout() {
		t.Fatalf("read timeout %s should not exceed write timeout %s", a.readTimeout(), a.writeTimeout())
	}
	a.Timeout = 5 * time.Second
	if a.readTimeout() != 5*time.Second || a.writeTimeout() != 5*time.Second {
		t.Fatalf("Adapter.Timeout override should apply to both classes: read=%s write=%s", a.readTimeout(), a.writeTimeout())
	}
}

func TestMergeSlotAcquireSuccess(t *testing.T) {
	a, log := newFakeAdapter(t)
	if err := a.MergeSlotAcquire(context.Background(), "gt:slot", "agent-7", 3); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, log)[0]; got != "update gt:slot --claim" {
		t.Fatalf("acquire argv = %q", got)
	}
}

func TestMergeSlotAcquireRetriesThenFails(t *testing.T) {
	// Shrink backoff so retries are fast.
	orig := mergeSlotBackoffBase
	mergeSlotBackoffBase = time.Millisecond
	defer func() { mergeSlotBackoffBase = orig }()

	a, log := newFakeAdapter(t)
	t.Setenv("BD_FAIL", "1")
	err := a.MergeSlotAcquire(context.Background(), "gt:slot", "agent-7", 2)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// retries=2 -> 3 attempts total.
	if n := len(lastArgs(t, log)); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

func TestMergeSlotNoOpWhenAbsent(t *testing.T) {
	a := &Adapter{RepoRoot: t.TempDir(), Bin: "/nonexistent/definitely-not-bd"}
	if a.Available() {
		t.Fatal("expected bd unavailable")
	}
	if err := a.MergeSlotAcquire(context.Background(), "gt:slot", "owner", 3); err != nil {
		t.Fatalf("acquire no-op returned error: %v", err)
	}
	if err := a.MergeSlotRelease(context.Background(), "gt:slot"); err != nil {
		t.Fatalf("release no-op returned error: %v", err)
	}
}

func TestMergeSlotRelease(t *testing.T) {
	a, log := newFakeAdapter(t)
	if err := a.MergeSlotRelease(context.Background(), "gt:slot"); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, log)[0]; got != "update gt:slot --status open" {
		t.Fatalf("release argv = %q", got)
	}
}

func TestSnapshot(t *testing.T) {
	a, _ := newFakeAdapter(t)
	beadsDir := filepath.Join(a.RepoRoot, ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "embeddeddolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "issues.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "keep.db"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "stale.lock"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".tar.gz") {
		t.Fatalf("snapshot path = %q", path)
	}
	if !strings.Contains(path, filepath.Join(".plan-logs", "beads-snapshots")) {
		t.Fatalf("snapshot not under expected dir: %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}

	// Confirm lock files were excluded.
	out, err := exec.Command("tar", "-tzf", path).Output()
	if err != nil {
		t.Fatalf("list archive: %v", err)
	}
	res := string(out)
	if strings.Contains(res, ".lock") {
		t.Fatalf("archive should not contain lock files:\n%s", res)
	}
	if !strings.Contains(res, "issues.jsonl") {
		t.Fatalf("archive missing issues.jsonl:\n%s", res)
	}
}

func TestAvailable(t *testing.T) {
	a, _ := newFakeAdapter(t)
	if !a.Available() {
		t.Fatal("fake bd should be available")
	}
}

// TestParsersDirect exercises the tolerant parsers across every accepted shape.
func TestParsersDirect(t *testing.T) {
	list, err := parseIssueList([]byte(`{"issues":[{"id":"z-1","priority":"P3","labels":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Priority != 3 || list[0].Labels == nil {
		t.Fatalf("envelope+string-priority+null-labels parse: %+v", list)
	}

	arr, err := parseIssueList([]byte(`[{"id":"z-2","priority":2}]`))
	if err != nil || len(arr) != 1 || arr[0].Priority != 2 {
		t.Fatalf("array parse: %+v err=%v", arr, err)
	}

	bare, err := parseIssue([]byte(`{"id":"z-3","priority":"p1"}`))
	if err != nil || bare.ID != "z-3" || bare.Priority != 1 {
		t.Fatalf("bare object parse: %+v err=%v", bare, err)
	}

	wrapped, err := parseIssue([]byte(`{"issue":{"id":"z-4","priority":0}}`))
	if err != nil || wrapped.ID != "z-4" {
		t.Fatalf("wrapped parse: %+v err=%v", wrapped, err)
	}
}
