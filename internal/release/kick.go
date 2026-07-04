// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package release

// kick.go — `koryph release kick`: close+reopen the open Release PR so that
// GitHub fires check workflows under the user's real gh auth token (a real
// actor, so checks fire — unlike GITHUB_TOKEN-caused events which never
// trigger workflows).
//
// This implements the bot-less rung-2 fallback: projects that cannot or choose
// not to install a GitHub App still get full check coverage by running one
// kick command per release.
//
// Guard rails:
//   - When --pr is not given, kick auto-detects the Release PR by the
//     "autorelease: pending" label. It refuses to operate on PRs that lack
//     this label unless --pr was given explicitly.
//   - The operation is idempotent: if the PR is already closed for some reason
//     (e.g. a partial prior run), reopen is still attempted.
//   - With --wait, kick polls check runs after reopening until all
//     conclusions are non-pending (or timeout elapses).

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// ReleasePRLabel is the GitHub label release-please applies to the open
// Release PR while it is waiting to be merged.
const ReleasePRLabel = "autorelease: pending"

// PRSummary is a minimal representation of a GitHub PR for kick purposes.
type PRSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	State string `json:"state"`
}

// IsReleasePR returns true when the PR carries the release-please label.
func (p *PRSummary) IsReleasePR() bool {
	for _, l := range p.Labels {
		if l.Name == ReleasePRLabel {
			return true
		}
	}
	return false
}

// KickOptions configures a single `koryph release kick` invocation.
type KickOptions struct {
	// Repo is the "owner/repo" GitHub slug (required).
	Repo string
	// PR is the explicit PR number. When 0, kick auto-detects the Release PR
	// by the "autorelease: pending" label. When set, the guard is relaxed:
	// a non-release-please PR is accepted (but a warning is printed).
	PR int
	// Wait causes kick to poll check conclusions after reopening until all
	// are non-pending or WaitTimeout elapses.
	Wait bool
	// WaitTimeout is the maximum polling duration when Wait is true
	// (default: 10 minutes).
	WaitTimeout time.Duration
	// WaitInterval is the poll interval when Wait is true (default: 15 s).
	WaitInterval time.Duration

	// Stdout/Stderr for human-readable progress.
	Stdout io.Writer
	Stderr io.Writer

	// Injectable seams (nil = use real gh CLI).
	GHPRList   func(repo, jqFilter string) ([]PRSummary, error)
	GHPRGet    func(repo string, number int) (*PRSummary, error)
	GHPRClose  func(repo string, number int) error
	GHPRReopen func(repo string, number int) error
	GHPRChecks func(repo string, number int) ([]CheckRun, error)
}

// CheckRun is a minimal check-run descriptor used by --wait polling.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func (o *KickOptions) stdout() io.Writer {
	if o.Stdout != nil {
		return o.Stdout
	}
	return io.Discard
}

func (o *KickOptions) stderr() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return io.Discard
}

func (o *KickOptions) waitTimeout() time.Duration {
	if o.WaitTimeout > 0 {
		return o.WaitTimeout
	}
	return 10 * time.Minute
}

func (o *KickOptions) waitInterval() time.Duration {
	if o.WaitInterval > 0 {
		return o.WaitInterval
	}
	return 15 * time.Second
}

func (o *KickOptions) ghPRList(repo, jqFilter string) ([]PRSummary, error) {
	if o.GHPRList != nil {
		return o.GHPRList(repo, jqFilter)
	}
	return defaultGHPRList(repo, jqFilter)
}

func (o *KickOptions) ghPRGet(repo string, number int) (*PRSummary, error) {
	if o.GHPRGet != nil {
		return o.GHPRGet(repo, number)
	}
	return defaultGHPRGet(repo, number)
}

func (o *KickOptions) ghPRClose(repo string, number int) error {
	if o.GHPRClose != nil {
		return o.GHPRClose(repo, number)
	}
	return defaultGHPRClose(repo, number)
}

func (o *KickOptions) ghPRReopen(repo string, number int) error {
	if o.GHPRReopen != nil {
		return o.GHPRReopen(repo, number)
	}
	return defaultGHPRReopen(repo, number)
}

func (o *KickOptions) ghPRChecks(repo string, number int) ([]CheckRun, error) {
	if o.GHPRChecks != nil {
		return o.GHPRChecks(repo, number)
	}
	return defaultGHPRChecks(repo, number)
}

// KickResult summarises what kick did.
type KickResult struct {
	// PR is the PR that was kicked.
	PR PRSummary
	// Closed is true when the close step ran (false only when PR was already closed).
	Closed bool
	// Reopened is true when the reopen step ran.
	Reopened bool
	// ChecksConclusion summarises the final check state when --wait was used.
	// Empty when --wait was not requested.
	ChecksConclusion string
}

// Kick executes the close+reopen dance for the Release PR. It:
//
//  1. Auto-detects or validates the target PR.
//  2. Guards against non-release-please PRs (unless PR was explicitly given).
//  3. Closes then reopens the PR so GitHub fires check workflows.
//  4. Optionally polls until checks conclude (--wait).
func Kick(opts KickOptions) (*KickResult, error) {
	if opts.Repo == "" {
		return nil, fmt.Errorf("release kick: --repo OWNER/REPO is required")
	}

	stdout := opts.stdout()

	var pr PRSummary

	if opts.PR != 0 {
		// Explicit PR number: fetch it and relax the guard (just warn).
		got, err := opts.ghPRGet(opts.Repo, opts.PR)
		if err != nil {
			return nil, fmt.Errorf("release kick: fetch PR #%d: %w", opts.PR, err)
		}
		pr = *got
		if !pr.IsReleasePR() {
			fmt.Fprintf(opts.stderr(), "koryph: warning: PR #%d does not have the %q label — proceeding because --pr was explicit\n", opts.PR, ReleasePRLabel)
		}
	} else {
		// Auto-detect: find the open Release PR by label.
		prs, err := opts.ghPRList(opts.Repo, `[.[] | select(.state=="open")]`)
		if err != nil {
			return nil, fmt.Errorf("release kick: list PRs: %w", err)
		}
		var candidates []PRSummary
		for _, p := range prs {
			if p.IsReleasePR() {
				candidates = append(candidates, p)
			}
		}
		switch len(candidates) {
		case 0:
			return nil, fmt.Errorf("release kick: no open PR with label %q found in %s (use --pr N to specify one explicitly)", ReleasePRLabel, opts.Repo)
		case 1:
			pr = candidates[0]
		default:
			// More than one — unusual; pick the lowest number and warn.
			pr = candidates[0]
			for _, p := range candidates[1:] {
				if p.Number < pr.Number {
					pr = p
				}
			}
			fmt.Fprintf(opts.stderr(), "koryph: warning: multiple Release PRs found; using #%d (%s)\n", pr.Number, pr.Title)
		}
	}

	fmt.Fprintf(stdout, "release kick: targeting PR #%d — %s\n  %s\n", pr.Number, pr.Title, pr.URL)

	res := &KickResult{PR: pr}

	// Close (if currently open).
	if strings.EqualFold(pr.State, "open") {
		fmt.Fprintf(stdout, "closing PR #%d...\n", pr.Number)
		if err := opts.ghPRClose(opts.Repo, pr.Number); err != nil {
			return nil, fmt.Errorf("release kick: close PR #%d: %w", pr.Number, err)
		}
		res.Closed = true
	} else {
		fmt.Fprintf(stdout, "PR #%d is already closed; skipping close step\n", pr.Number)
	}

	// Reopen.
	fmt.Fprintf(stdout, "reopening PR #%d...\n", pr.Number)
	if err := opts.ghPRReopen(opts.Repo, pr.Number); err != nil {
		return nil, fmt.Errorf("release kick: reopen PR #%d: %w", pr.Number, err)
	}
	res.Reopened = true
	fmt.Fprintf(stdout, "PR #%d reopened — GitHub check workflows will now fire under your auth token.\n", pr.Number)

	// Optional: poll until checks conclude.
	if opts.Wait {
		conclusion, err := pollChecks(&opts, pr.Number)
		if err != nil {
			fmt.Fprintf(opts.stderr(), "koryph: warning: --wait polling: %v\n", err)
		}
		res.ChecksConclusion = conclusion
		if conclusion != "" {
			fmt.Fprintf(stdout, "checks concluded: %s\n", conclusion)
		}
	}

	return res, nil
}

// pollChecks blocks until all check runs on pr have a non-empty conclusion
// (success, failure, cancelled, etc.) or until the timeout elapses. It
// returns a human-readable summary of the conclusions.
func pollChecks(opts *KickOptions, prNumber int) (string, error) {
	deadline := time.Now().Add(opts.waitTimeout())
	interval := opts.waitInterval()
	stdout := opts.stdout()

	for {
		checks, err := opts.ghPRChecks(opts.Repo, prNumber)
		if err != nil {
			// Transient error — keep trying until deadline.
			fmt.Fprintf(stdout, "  polling checks: %v — retrying\n", err)
		} else if allConcluded(checks) {
			return summariseConclusions(checks), nil
		} else {
			pending := countPending(checks)
			fmt.Fprintf(stdout, "  %d check(s) still pending...\n", pending)
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %s waiting for checks to conclude", opts.waitTimeout())
		}
		time.Sleep(interval)
	}
}

func allConcluded(checks []CheckRun) bool {
	if len(checks) == 0 {
		return false // no checks yet — waiting for them to appear
	}
	for _, c := range checks {
		if c.Conclusion == "" || c.Status == "in_progress" || c.Status == "queued" {
			return false
		}
	}
	return true
}

func countPending(checks []CheckRun) int {
	n := 0
	for _, c := range checks {
		if c.Conclusion == "" || c.Status == "in_progress" || c.Status == "queued" {
			n++
		}
	}
	return n
}

func summariseConclusions(checks []CheckRun) string {
	counts := map[string]int{}
	for _, c := range checks {
		conclusion := c.Conclusion
		if conclusion == "" {
			conclusion = "pending"
		}
		counts[conclusion]++
	}
	var parts []string
	for k, v := range counts {
		parts = append(parts, fmt.Sprintf("%s:%d", k, v))
	}
	return strings.Join(parts, " ")
}

// --- default gh CLI implementations -----------------------------------------

// defaultGHPRList runs `gh pr list --repo <repo> --json ... --jq <filter>`
// and returns decoded PRSummary values.
func defaultGHPRList(repo, jqFilter string) ([]PRSummary, error) {
	out, err := exec.Command("gh", "pr", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", "20",
		"--json", "number,title,url,labels,state",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []PRSummary
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: decode: %w", err)
	}
	// Apply jqFilter server-side is complex; we filter client-side.
	_ = jqFilter
	return prs, nil
}

// defaultGHPRGet fetches a single PR by number.
func defaultGHPRGet(repo string, number int) (*PRSummary, error) {
	out, err := exec.Command("gh", "pr", "view",
		fmt.Sprint(number),
		"--repo", repo,
		"--json", "number,title,url,labels,state",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr view %d: %w", number, err)
	}
	var pr PRSummary
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, fmt.Errorf("gh pr view %d: decode: %w", number, err)
	}
	return &pr, nil
}

// defaultGHPRClose closes the PR.
func defaultGHPRClose(repo string, number int) error {
	out, err := exec.Command("gh", "pr", "close",
		fmt.Sprint(number),
		"--repo", repo,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// defaultGHPRReopen reopens the PR.
func defaultGHPRReopen(repo string, number int) error {
	out, err := exec.Command("gh", "pr", "reopen",
		fmt.Sprint(number),
		"--repo", repo,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// defaultGHPRChecks fetches check runs for the PR's head commit.
func defaultGHPRChecks(repo string, number int) ([]CheckRun, error) {
	out, err := exec.Command("gh", "pr", "checks",
		fmt.Sprint(number),
		"--repo", repo,
		"--json", "name,status,conclusion",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr checks %d: %w", number, err)
	}
	var checks []CheckRun
	if err := json.Unmarshal(out, &checks); err != nil {
		return nil, fmt.Errorf("gh pr checks %d: decode: %w", number, err)
	}
	return checks, nil
}
