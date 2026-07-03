// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// openPR is the OpenPR divergence of Merge, reached only after the shared
// preflight (slot, protected-path, signing, sync, rebase, gate) has passed.
// It pushes the rebased branch to origin and opens (or reuses) a pull request
// against def. The worktree and branch are left intact for a later landing
// step. Missing prerequisites — no remote, no authenticated PR host — come
// back as a non-error Result the engine can surface as a clear block rather
// than a crash.
func openPR(ctx context.Context, o Opts, def string, hasRemote bool) (Result, error) {
	if !hasRemote {
		return Result{Status: "pr-no-remote"}, nil
	}
	host := o.PR
	if host == nil {
		host = GhCLI{}
	}
	if !host.Ready(ctx, o.RepoRoot) {
		return Result{Status: "pr-no-gh"}, nil
	}

	head, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.RepoRoot, Name: "git", Args: []string{"rev-parse", o.Branch},
	})
	if err != nil {
		return Result{Status: "error"}, err
	}
	if err := pushBranch(ctx, o.RepoRoot, o.Branch); err != nil {
		return Result{Status: "error"}, err
	}

	url, num, err := host.Open(ctx, o.RepoRoot, o.Branch, def, o.PRTitle, o.PRBody)
	if err != nil {
		return Result{Status: "error"}, err
	}
	return Result{
		Status:    "pr-opened",
		MergedSHA: strings.TrimSpace(head.Stdout),
		Pushed:    true,
		PRURL:     url,
		PRNumber:  num,
	}, nil
}

// pushBranch publishes an engine-owned agent branch to origin. A rebased
// re-run makes the branch a non-fast-forward of its earlier push; because
// agent/<bead-id> branches are written only by the engine, the force fallback
// clobbers nothing but our own prior push.
func pushBranch(ctx context.Context, dir, branch string) error {
	res, err := gitRun(ctx, dir, "push", "origin", branch)
	if err != nil {
		return err
	}
	if res.ExitCode == 0 {
		return nil
	}
	forced, err := gitRun(ctx, dir, "push", "--force", "origin", branch)
	if err != nil {
		return err
	}
	if forced.ExitCode != 0 {
		return fmt.Errorf("push %s to origin: %s", branch, strings.TrimSpace(tail(forced.Stderr, 400)))
	}
	return nil
}

// GhCLI opens pull requests through the `gh` command-line tool. It is the
// default PROpener; tests inject a fake to avoid a real GitHub round-trip.
type GhCLI struct{}

// Ready reports whether gh is installed and authenticated for the repo at dir.
func (GhCLI) Ready(ctx context.Context, dir string) bool {
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: []string{"auth", "status"}})
	return err == nil && res.ExitCode == 0
}

// Open returns the existing OPEN pull request for branch, or creates one
// against base with the given title and body.
func (GhCLI) Open(ctx context.Context, dir, branch, base, title, body string) (string, int, error) {
	if url, num, ok := ghExistingPR(ctx, dir, branch); ok {
		return url, num, nil
	}
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: []string{
		"pr", "create", "--head", branch, "--base", base, "--title", title, "--body", body,
	}})
	if err != nil {
		return "", 0, err
	}
	if res.ExitCode != 0 {
		return "", 0, fmt.Errorf("gh pr create: %s", strings.TrimSpace(tail(res.Stderr, 400)))
	}
	url := strings.TrimSpace(res.Stdout)
	return url, prNumberFromURL(url), nil
}

// ghExistingPR reports an already-OPEN pull request for branch (so a re-run is
// idempotent). A closed or merged PR, or no PR, returns ok=false.
func ghExistingPR(ctx context.Context, dir, branch string) (string, int, bool) {
	res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: []string{
		"pr", "view", branch, "--json", "url,number,state",
	}})
	if err != nil || res.ExitCode != 0 {
		return "", 0, false
	}
	var v struct {
		URL    string `json:"url"`
		Number int    `json:"number"`
		State  string `json:"state"`
	}
	if json.Unmarshal([]byte(res.Stdout), &v) != nil || v.State != "OPEN" {
		return "", 0, false
	}
	return v.URL, v.Number, true
}

// prNumberFromURL extracts the trailing pull-request number from a gh URL such
// as https://github.com/owner/repo/pull/123.
func prNumberFromURL(url string) int {
	i := strings.LastIndexByte(url, '/')
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(url[i+1:]))
	return n
}
