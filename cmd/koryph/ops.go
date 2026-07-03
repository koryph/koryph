// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// latestRun resolves the record and its latest run for a project.
func latestRun(ctx context.Context, store *registry.Store, projectID string) (*registry.Record, *ledger.Run, error) {
	rec, err := store.Get(projectID)
	if err != nil {
		return nil, nil, err
	}
	run, err := ledger.NewStore(rec.Root).LoadLatest()
	if err != nil {
		return rec, nil, fmt.Errorf("no runs found for %s: %w", projectID, err)
	}
	return rec, run, nil
}

// cmdTail prints the tail of a phase's session.log + stderr.log and the
// stream.jsonl path. With --follow it streams new lines as they are written,
// and surfaces INBOX.md nudges with a prominent banner.
func cmdTail(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("tail", stderr)
	projectID := fs.String("project", "", "project id (required)")
	n := fs.Int("n", 40, "number of trailing lines")
	follow := fs.Bool("follow", false, "stream new lines as they appear (Ctrl-C to stop)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return engine.ExitUsage
	}
	if *projectID == "" {
		return usageErr(stderr, "tail: --project is required")
	}
	if len(pos) < 1 {
		return usageErr(stderr, "tail: <phase-id> is required")
	}
	phaseID := pos[0]

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, run, err := latestRun(ctx, store, *projectID)
	if err != nil {
		return fail(stderr, err)
	}
	phaseDir := filepath.Join(paths.KoryphRoot(rec.Root), run.RunID, phaseID)

	fmt.Fprintf(stdout, "== %s / %s / %s ==\n", rec.ProjectID, run.RunID, phaseID)
	fmt.Fprintf(stdout, "-- session.log (last %d) --\n", *n)
	fmt.Fprintln(stdout, tailFile(filepath.Join(phaseDir, "session.log"), *n))
	fmt.Fprintf(stdout, "-- stderr.log (last %d) --\n", *n)
	fmt.Fprintln(stdout, tailFile(filepath.Join(phaseDir, "stderr.log"), *n))
	fmt.Fprintf(stdout, "stream: %s\n", filepath.Join(phaseDir, "stream.jsonl"))

	if !*follow {
		return 0
	}

	// Surface any INBOX content that already exists before entering the loop.
	inboxPath := filepath.Join(phaseDir, "INBOX.md")
	if data, rerr := os.ReadFile(inboxPath); rerr == nil && len(bytes.TrimSpace(data)) > 0 {
		fmt.Fprintln(stdout, "-- INBOX (current) --")
		fmt.Fprintln(stdout, string(data))
	}

	sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintln(stdout, "-- following (Ctrl-C to stop) --")
	return tailFollow(sctx, stdout, phaseDir)
}

// tailFollow streams new content from phaseDir's session.log, stderr.log, and
// INBOX.md until ctx is cancelled. It polls every 250 ms, advancing byte
// offsets so each line is printed exactly once.
func tailFollow(ctx context.Context, stdout io.Writer, phaseDir string) int {
	sessionPath := filepath.Join(phaseDir, "session.log")
	stderrPath := filepath.Join(phaseDir, "stderr.log")
	inboxPath := filepath.Join(phaseDir, "INBOX.md")

	// Start from the current end-of-file so only new bytes are shown.
	sessionOff := fileEnd(sessionPath)
	stderrOff := fileEnd(stderrPath)
	inboxOff := fileEnd(inboxPath)

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Poll before waiting, so a single pre-cancelled context still flushes
		// any content that arrived between the snapshot and now.
		sessionOff = printNewContent(stdout, sessionPath, sessionOff, "")
		stderrOff = printNewContent(stdout, stderrPath, stderrOff, "[stderr] ")
		inboxOff = printNewInbox(stdout, inboxPath, inboxOff)

		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

// fileEnd returns the current byte size of path (the offset of the "end"),
// returning 0 when the file does not exist yet.
func fileEnd(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// printNewContent reads bytes after offset from path, prints each complete
// line (prefixed by prefix) to stdout, and returns the new offset.  Incomplete
// trailing fragments (no terminating newline yet) are held back so they appear
// atomically on the next tick.
func printNewContent(stdout io.Writer, path string, offset int64, prefix string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() <= offset {
		return offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset
	}
	buf := make([]byte, fi.Size()-offset)
	n, _ := io.ReadFull(f, buf)
	if n <= 0 {
		return offset
	}
	buf = buf[:n]

	// Only advance to the last complete line.
	lastNL := bytes.LastIndexByte(buf, '\n')
	if lastNL < 0 {
		return offset // no complete line yet
	}
	complete := buf[:lastNL+1]
	for _, line := range strings.Split(string(bytes.TrimRight(complete, "\n")), "\n") {
		fmt.Fprintln(stdout, prefix+line)
	}
	return offset + int64(lastNL+1)
}

// printNewInbox reads bytes after offset from path and, when new content is
// found, prints it with a conspicuous banner so operators do not miss it.
// Returns the new offset.
func printNewInbox(stdout io.Writer, path string, offset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() <= offset {
		return offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset
	}
	buf := make([]byte, fi.Size()-offset)
	n, _ := io.ReadFull(f, buf)
	if n <= 0 {
		return offset
	}
	buf = buf[:n]
	newOffset := offset + int64(n)

	text := strings.TrimSpace(string(buf))
	if text == "" {
		return newOffset
	}
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, ">>> INBOX NUDGE <<<")
	fmt.Fprintln(stdout, text)
	fmt.Fprintln(stdout, ">>> END NUDGE <<<")
	fmt.Fprintln(stdout, "")
	return newOffset
}

// tailFile returns the last n lines of the file at path, or a placeholder.
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no " + filepath.Base(path) + ")"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// cmdNudge appends an operator note to a phase's INBOX and (best-effort) posts
// a bd comment.
func cmdNudge(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("nudge", stderr)
	projectID := fs.String("project", "", "project id (required)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return engine.ExitUsage
	}
	if *projectID == "" {
		return usageErr(stderr, "nudge: --project is required")
	}
	if len(pos) < 2 {
		return usageErr(stderr, `nudge: <phase-id> "text" are required`)
	}
	phaseID, text := pos[0], strings.Join(pos[1:], " ")

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, run, err := latestRun(ctx, store, *projectID)
	if err != nil {
		return fail(stderr, err)
	}
	phaseDir := filepath.Join(paths.KoryphRoot(rec.Root), run.RunID, phaseID)
	if err := os.MkdirAll(phaseDir, 0o755); err != nil {
		return fail(stderr, err)
	}
	entry := fmt.Sprintf("\n---\n[%s] operator:\n%s\n", time.Now().UTC().Format(time.RFC3339), text)
	f, err := os.OpenFile(filepath.Join(phaseDir, "INBOX.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fail(stderr, err)
	}
	if _, werr := f.WriteString(entry); werr != nil {
		f.Close()
		return fail(stderr, werr)
	}
	if cerr := f.Close(); cerr != nil {
		return fail(stderr, cerr)
	}

	// Best-effort bd comment.
	bd := beads.New(rec.Root)
	if v := os.Getenv("KORYPH_BD_BIN"); v != "" {
		bd.Bin = v
	}
	if bd.Available() {
		if cerr := bd.Comment(ctx, phaseID, "operator nudge: "+text); cerr != nil {
			fmt.Fprintln(stderr, "koryph: warning: bd comment failed:", cerr)
		}
	}
	fmt.Fprintf(stdout, "nudged %s (%s)\n", phaseID, filepath.Join(phaseDir, "INBOX.md"))
	return 0
}

// cmdStop sends a graceful SIGTERM to a phase's agent process group.
func cmdStop(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stop", stderr)
	projectID := fs.String("project", "", "project id (required unless --all)")
	all := fs.Bool("all", false, "stop active agents across ALL managed projects")
	force := fs.Bool("force", false, "SIGKILL instead of SIGTERM — uncommitted worktree work is LOST")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return engine.ExitUsage
	}

	stop, verb := dispatch.StopGraceful, "SIGTERM"
	if *force {
		stop, verb = dispatch.StopForce, "SIGKILL"
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}

	if *all {
		if *projectID != "" || len(pos) > 0 {
			return usageErr(stderr, "stop --all takes neither --project nor a phase-id")
		}
		return stopAll(ctx, store, stop, verb, stdout, stderr)
	}

	if *projectID == "" {
		return usageErr(stderr, "stop: --project is required (or use --all)")
	}
	if len(pos) < 1 {
		return usageErr(stderr, "stop: <phase-id> is required (or use --all)")
	}
	phaseID := pos[0]

	_, run, err := latestRun(ctx, store, *projectID)
	if err != nil {
		return fail(stderr, err)
	}
	sl := run.Slots[phaseID]
	if sl == nil {
		return fail(stderr, fmt.Errorf("no slot %q in run %s", phaseID, run.RunID))
	}
	if sl.PID <= 0 {
		fmt.Fprintf(stdout, "%s: no live pid recorded (status %s)\n", phaseID, sl.Status)
		return 0
	}
	if err := stop(sl.PID); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "sent %s to pid %d (process group) for %s\n", verb, sl.PID, phaseID)
	return 0
}

// stopAll signals every live, non-terminal agent across all managed projects.
// A project with no runs (or an unreadable ledger) is skipped, not fatal — one
// bad project must not stop the sweep.
func stopAll(ctx context.Context, store *registry.Store, stop func(int) error, verb string, stdout, stderr io.Writer) int {
	records, err := store.List()
	if err != nil {
		return fail(stderr, err)
	}
	stopped, projects := 0, 0
	for _, rec := range records {
		_, run, err := latestRun(ctx, store, rec.ProjectID)
		if err != nil || run == nil {
			continue
		}
		hit := false
		for _, sl := range run.Slots {
			if sl == nil || ledger.Terminal(sl.Status) || sl.PID <= 0 || !dispatch.Alive(sl.PID) {
				continue
			}
			if serr := stop(sl.PID); serr != nil {
				fmt.Fprintf(stderr, "stop %s/%s pid %d: %v\n", rec.ProjectID, sl.PhaseID, sl.PID, serr)
				continue
			}
			fmt.Fprintf(stdout, "%s %s/%s (pid %d)\n", verb, rec.ProjectID, sl.PhaseID, sl.PID)
			stopped++
			hit = true
		}
		if hit {
			projects++
		}
	}
	fmt.Fprintf(stdout, "stop --all: signalled %d agent(s) across %d project(s)\n", stopped, projects)
	return 0
}

// cmdMerge lands a finished agent branch on the default branch.
func cmdMerge(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("merge", stderr)
	projectID := fs.String("project", "", "project id (required)")
	push := fs.Bool("push", false, "push the default branch after merge")
	squash := fs.Bool("squash", false, "squash-merge instead of ff-only")
	keepWorktree := fs.Bool("keep-worktree", false, "keep the worktree + branch after merge")
	closeBead := fs.String("close-bead", "", "bead to close on a successful merge")
	reason := fs.String("reason", "", "close reason for --close-bead")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return engine.ExitUsage
	}
	if *projectID == "" {
		return usageErr(stderr, "merge: --project is required")
	}
	if len(pos) < 1 {
		return usageErr(stderr, "merge: <branch> is required")
	}
	branch := pos[0]

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, err := store.Get(*projectID)
	if err != nil {
		return fail(stderr, err)
	}
	cfg, err := project.Load(rec.Root)
	if err != nil {
		return fail(stderr, err)
	}

	res, merr := merge.Merge(ctx, merge.Opts{
		RepoRoot:      rec.Root,
		Branch:        branch,
		DefaultBranch: rec.DefaultBranch,
		Gate:          cfg.Gate,
		Extra:         cfg.ProtectedPaths,
		Squash:        *squash,
		KeepWorktree:  *keepWorktree,
		Push:          *push,
		Slot:          nil,
		RequireSigned: cfg.Signing != nil && cfg.Signing.Required,
	})
	if perr := printJSON(stdout, res); perr != nil {
		return fail(stderr, perr)
	}
	if merr != nil {
		// Infrastructure error after a successful merge (most commonly a push
		// failure). The JSON already records pushed:false so the caller can
		// inspect the result, but the process must exit non-zero so
		// orchestrators and scripts don't treat this as a clean success.
		fmt.Fprintln(stderr, "koryph merge:", merr)
		return engine.ExitFatal
	}
	if res.Status != "merged" {
		return engine.ExitFatal
	}
	if *closeBead != "" {
		bd := beads.New(rec.Root)
		if v := os.Getenv("KORYPH_BD_BIN"); v != "" {
			bd.Bin = v
		}
		if cerr := bd.Close(ctx, *closeBead, *reason); cerr != nil {
			fmt.Fprintln(stderr, "koryph: warning: close bead failed:", cerr)
		} else {
			fmt.Fprintf(stdout, "closed bead %s\n", *closeBead)
		}
	}
	return 0
}
