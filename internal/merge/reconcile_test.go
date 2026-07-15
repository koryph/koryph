// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A single-line divergent stand-in for a checksum-over-a-directory: any change
// rewrites the one line, so two divergent edits always conflict at rebase — the
// exact shape of a migrations lockfile (atlas.sum) that both sides regenerated.
const (
	sumBase  = "BASE\n"
	sumMain  = "MAIN\n"
	sumAgent = "AGENT\n"
)

// atlasRegenCmd regenerates the "checksum" from the post-merge tree — a
// single-line sorted listing of migrations/*.sql. It ignores the conflict
// stages entirely (the regenerate-from-tree idiom), mirroring
// `atlas migrate hash --dir file://migrations`.
const atlasRegenCmd = "ls migrations/*.sql | sort | tr '\\n' ',' > migrations/atlas.sum"

func commitFiles(t *testing.T, dir, msg string, files map[string]string) string {
	t.Helper()
	for p, c := range files {
		writeFile(t, filepath.Join(dir, p), c)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-qm", msg)
	return headOf(t, dir, "HEAD")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// seedMigrations establishes migrations/0001_seed.sql + a base atlas.sum on
// main, then returns a worktree cut from that base (so both sides diverge from
// the same lockfile).
func seedMigrations(t *testing.T, repo, branch string) string {
	t.Helper()
	commitFiles(t, repo, "seed migrations", map[string]string{
		"migrations/0001_seed.sql": "-- seed\n",
		"migrations/atlas.sum":     sumBase,
	})
	return worktreeOn(t, repo, branch).Path
}

func TestReconcile_SingleCollisionHeals(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := seedMigrations(t, repo, "agent/x")

	// main and the branch each add a distinct migration and regenerate the
	// lockfile to a distinct single line -> atlas.sum conflicts, the .sql files
	// do not.
	commitFiles(t, repo, "main migration", map[string]string{
		"migrations/0002_main.sql": "-- main\n",
		"migrations/atlas.sum":     sumMain,
	})
	commitFiles(t, wtPath, "branch migration", map[string]string{
		"migrations/0002_x.sql": "-- x\n",
		"migrations/atlas.sum":  sumAgent,
	})

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Reconcilers: []Reconciler{{Path: "migrations/atlas.sum", Command: atlasRegenCmd}},
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s output=%s)", err, res.Status, res.GateOutput)
	}
	if res.Status != StatusMerged {
		t.Fatalf("Status=%q, want merged (output=%s)", res.Status, res.GateOutput)
	}
	if got := strings.Join(res.Reconciled, ","); got != "migrations/atlas.sum" {
		t.Errorf("Reconciled=%v, want [migrations/atlas.sum]", res.Reconciled)
	}
	if res.ReconcileRounds != 1 {
		t.Errorf("ReconcileRounds=%d, want 1", res.ReconcileRounds)
	}
	// The merged lockfile reflects the UNION of both sides' migrations.
	sum := readFile(t, filepath.Join(repo, "migrations", "atlas.sum"))
	for _, want := range []string{"0001_seed.sql", "0002_main.sql", "0002_x.sql"} {
		if !strings.Contains(sum, want) {
			t.Errorf("healed atlas.sum %q missing %s", sum, want)
		}
	}
}

func TestReconcile_CascadeTwoRounds(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := seedMigrations(t, repo, "agent/x")

	// main advances once; the branch has TWO commits each touching the
	// lockfile, so each replayed commit re-conflicts against the healed
	// lockfile — the renumber-into-renumber cascade, bounded and quiet.
	commitFiles(t, repo, "main migration", map[string]string{
		"migrations/0002_main.sql": "-- main\n",
		"migrations/atlas.sum":     sumMain,
	})
	commitFiles(t, wtPath, "branch migration A", map[string]string{
		"migrations/0002_x.sql": "-- xA\n",
		"migrations/atlas.sum":  "AGENT-A\n",
	})
	commitFiles(t, wtPath, "branch migration B", map[string]string{
		"migrations/0003_x.sql": "-- xB\n",
		"migrations/atlas.sum":  "AGENT-B\n",
	})

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Reconcilers: []Reconciler{{Path: "migrations/atlas.sum", Command: atlasRegenCmd}},
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s)", err, res.Status)
	}
	if res.Status != StatusMerged {
		t.Fatalf("Status=%q, want merged (output=%s)", res.Status, res.GateOutput)
	}
	if res.ReconcileRounds != 2 {
		t.Errorf("ReconcileRounds=%d, want 2 (two-commit cascade)", res.ReconcileRounds)
	}
	sum := readFile(t, filepath.Join(repo, "migrations", "atlas.sum"))
	for _, want := range []string{"0001_seed.sql", "0002_main.sql", "0002_x.sql", "0003_x.sql"} {
		if !strings.Contains(sum, want) {
			t.Errorf("healed atlas.sum %q missing %s", sum, want)
		}
	}
}

func TestReconcile_StructuredUnionViaEnv(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()

	// A secrets-baseline stand-in (a newline set of findings): a blind rescan
	// would drop one side's reviewed entries, so the command must UNION the two
	// sides via the KORYPH_MERGE_OURS/_THEIRS contract.
	commitFiles(t, repo, "seed baseline", map[string]string{".secrets.baseline": "a\nb\n"})
	wtPath := worktreeOn(t, repo, "agent/x").Path
	commitFiles(t, repo, "main finding", map[string]string{".secrets.baseline": "a\nb\nc\n"})
	commitFiles(t, wtPath, "branch finding", map[string]string{".secrets.baseline": "a\nb\nd\n"})

	unionCmd := `cat "$KORYPH_MERGE_OURS" "$KORYPH_MERGE_THEIRS" | sort -u > "$KORYPH_MERGE_PATH"`
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Reconcilers: []Reconciler{{Path: ".secrets.baseline", Command: unionCmd}},
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s)", err, res.Status)
	}
	if res.Status != StatusMerged {
		t.Fatalf("Status=%q, want merged (output=%s)", res.Status, res.GateOutput)
	}
	got := readFile(t, filepath.Join(repo, ".secrets.baseline"))
	for _, want := range []string{"a", "b", "c", "d"} {
		if !strings.Contains(got, want) {
			t.Errorf("union baseline %q missing %s", got, want)
		}
	}
}

func TestReconcile_UncoveredPathAborts(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := seedMigrations(t, repo, "agent/x")

	// A genuine conflict on a hand-authored file rides alongside the lockfile
	// conflict. All-or-nothing (I1): nothing heals, the rebase aborts.
	commitFiles(t, repo, "main", map[string]string{
		"migrations/0002_main.sql": "-- main\n",
		"migrations/atlas.sum":     sumMain,
		"app.go":                   "package app // main\n",
	})
	commitFiles(t, wtPath, "branch", map[string]string{
		"migrations/0002_x.sql": "-- x\n",
		"migrations/atlas.sum":  sumAgent,
		"app.go":                "package app // agent\n",
	})
	agentTip := headOf(t, wtPath, "HEAD")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Reconcilers: []Reconciler{{Path: "migrations/atlas.sum", Command: atlasRegenCmd}},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusConflict {
		t.Fatalf("Status=%q, want conflict (mixed conflict must not heal)", res.Status)
	}
	if len(res.Reconciled) != 0 {
		t.Errorf("Reconciled=%v, want none on an aborted heal", res.Reconciled)
	}
	// The rebase was fully aborted — the worktree is back on its own tip, no
	// partial state.
	if got := headOf(t, wtPath, "HEAD"); got != agentTip {
		t.Errorf("worktree HEAD=%s, want restored to agent tip %s (rebase not cleanly aborted)", got, agentTip)
	}
}

func TestReconcile_CommandFailureAborts(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := seedMigrations(t, repo, "agent/x")

	commitFiles(t, repo, "main", map[string]string{
		"migrations/0002_main.sql": "-- main\n", "migrations/atlas.sum": sumMain,
	})
	commitFiles(t, wtPath, "branch", map[string]string{
		"migrations/0002_x.sql": "-- x\n", "migrations/atlas.sum": sumAgent,
	})

	// A reconciler that exits non-zero must fail safe to today's behavior (I2).
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Reconcilers: []Reconciler{{Path: "migrations/atlas.sum", Command: "exit 1"}},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusConflict {
		t.Fatalf("Status=%q, want conflict (a failing reconciler must not merge)", res.Status)
	}
}

func TestReconcile_LeftUnresolvedAborts(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := seedMigrations(t, repo, "agent/x")

	commitFiles(t, repo, "main", map[string]string{
		"migrations/0002_main.sql": "-- main\n", "migrations/atlas.sum": sumMain,
	})
	commitFiles(t, wtPath, "branch", map[string]string{
		"migrations/0002_x.sql": "-- x\n", "migrations/atlas.sum": sumAgent,
	})

	// A command that exits 0 but leaves conflict markers in place must not be
	// mistaken for a resolution (I6). `true` writes nothing, so the file keeps
	// git's conflict markers.
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Reconcilers: []Reconciler{{Path: "migrations/atlas.sum", Command: "true"}},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusConflict {
		t.Fatalf("Status=%q, want conflict (unresolved markers must not merge)", res.Status)
	}
}

func TestReconcile_GateCatchesBadHeal(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := seedMigrations(t, repo, "agent/x")

	commitFiles(t, repo, "main", map[string]string{
		"migrations/0002_main.sql": "-- main\n", "migrations/atlas.sum": sumMain,
	})
	commitFiles(t, wtPath, "branch", map[string]string{
		"migrations/0002_x.sql": "-- x\n", "migrations/atlas.sum": sumAgent,
	})
	mainTip := headOf(t, repo, "main")

	// The heal succeeds (rebase completes) but the gate rejects the artifact.
	// Reaching StatusGateFailed at all proves the conflict was healed first —
	// the gate is the backstop (I3), and main must NOT advance.
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"false"}, SlotOwner: "o", Slot: &fakeSlot{},
		Reconcilers: []Reconciler{{Path: "migrations/atlas.sum", Command: atlasRegenCmd}},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusGateFailed {
		t.Fatalf("Status=%q, want gate-failed (heal then gate rejects)", res.Status)
	}
	if got := headOf(t, repo, "main"); got != mainTip {
		t.Errorf("main advanced to %s past %s despite gate failure", got, mainTip)
	}
}

func TestReconcile_NoReconcilersUnchanged(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := seedMigrations(t, repo, "agent/x")

	commitFiles(t, repo, "main", map[string]string{
		"migrations/0002_main.sql": "-- main\n", "migrations/atlas.sum": sumMain,
	})
	commitFiles(t, wtPath, "branch", map[string]string{
		"migrations/0002_x.sql": "-- x\n", "migrations/atlas.sum": sumAgent,
	})

	// No reconcilers configured: byte-for-byte today's behavior (I5).
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusConflict {
		t.Fatalf("Status=%q, want conflict (no reconcilers = today's behavior)", res.Status)
	}
	if res.ConflictMD == "" {
		t.Error("expected CONFLICT.md path on an unhealed conflict")
	}
}
