// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/koryph/koryph/internal/forge"
)

// githubPRSvc implements [forge.PRService] for GitHub using the gh CLI.
//
// All methods use explicit --repo owner/repo so the binary can be invoked
// from any working directory. The gh binary path is controlled by the
// KORYPH_GH_BIN environment variable (default: "gh").
//
// The merge-method/message seam in [Merge] is kept explicit so that
// koryph-ufy's PR-based merge flow can specify the strategy and commit
// message without re-interpreting them at this layer.
type githubPRSvc struct{}

func (s *githubPRSvc) ghBin() string {
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		return v
	}
	return "gh"
}

// List returns pull requests for owner/repo filtered by opts.
// opts.State defaults to "open" when empty; "all" returns every PR.
// opts.Limit of 0 requests the gh default (typically 30).
func (s *githubPRSvc) List(_ context.Context, owner, repo string, opts forge.ListPROptions) ([]forge.PR, error) {
	ownerRepo := owner + "/" + repo
	state := opts.State
	if state == "" {
		state = "open"
	}
	args := []string{
		"pr", "list",
		"--repo", ownerRepo,
		"--state", state,
		"--json", "number,title,url,state,labels,headRefName,headRefOid,author,isDraft",
	}
	if opts.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(opts.Limit))
	}
	for _, lbl := range opts.Labels {
		args = append(args, "--label", lbl)
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("gh pr list %s: %w\n%s", ownerRepo, err, strings.TrimSpace(string(out)))
	}
	var raw []struct {
		Number     int    `json:"number"`
		Title      string `json:"title"`
		URL        string `json:"url"`
		State      string `json:"state"`
		IsDraft    bool   `json:"isDraft"`
		HeadRefOid string `json:"headRefOid"`
		HeadRef    string `json:"headRefName"`
		Author     struct {
			Login string `json:"login"`
		} `json:"author"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh pr list %s: parse response: %w", ownerRepo, err)
	}
	prs := make([]forge.PR, 0, len(raw))
	for _, r := range raw {
		labels := make([]string, 0, len(r.Labels))
		for _, l := range r.Labels {
			labels = append(labels, l.Name)
		}
		prs = append(prs, forge.PR{
			Number:     r.Number,
			Title:      r.Title,
			URL:        r.URL,
			State:      strings.ToLower(r.State),
			Labels:     labels,
			HeadBranch: r.HeadRef,
			HeadSHA:    r.HeadRefOid,
			Author:     r.Author.Login,
			Draft:      r.IsDraft,
		})
	}
	return prs, nil
}

// Get returns one pull request by its sequential number.
func (s *githubPRSvc) Get(_ context.Context, owner, repo string, number int) (*forge.PR, error) {
	ownerRepo := owner + "/" + repo
	args := []string{
		"pr", "view", strconv.Itoa(number),
		"--repo", ownerRepo,
		"--json", "number,title,url,state,labels,headRefName,headRefOid,author,isDraft",
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("gh pr view %d %s: %w\n%s", number, ownerRepo, err, strings.TrimSpace(string(out)))
	}
	var raw struct {
		Number     int    `json:"number"`
		Title      string `json:"title"`
		URL        string `json:"url"`
		State      string `json:"state"`
		IsDraft    bool   `json:"isDraft"`
		HeadRefOid string `json:"headRefOid"`
		HeadRef    string `json:"headRefName"`
		Author     struct {
			Login string `json:"login"`
		} `json:"author"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh pr view %d %s: parse response: %w", number, ownerRepo, err)
	}
	labels := make([]string, 0, len(raw.Labels))
	for _, l := range raw.Labels {
		labels = append(labels, l.Name)
	}
	return &forge.PR{
		Number:     raw.Number,
		Title:      raw.Title,
		URL:        raw.URL,
		State:      strings.ToLower(raw.State),
		Labels:     labels,
		HeadBranch: raw.HeadRef,
		HeadSHA:    raw.HeadRefOid,
		Author:     raw.Author.Login,
		Draft:      raw.IsDraft,
	}, nil
}

// Create opens a new pull request from branch against base.
// The returned PR carries at minimum Number and URL.
func (s *githubPRSvc) Create(_ context.Context, owner, repo, branch, base, title, body string) (*forge.PR, error) {
	ownerRepo := owner + "/" + repo
	args := []string{
		"pr", "create",
		"--repo", ownerRepo,
		"--head", branch,
		"--base", base,
		"--title", title,
		"--body", body,
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("gh pr create %s: %w\n%s", ownerRepo, err, strings.TrimSpace(string(out)))
	}
	// gh pr create outputs the URL of the new PR on stdout.
	url := strings.TrimSpace(string(out))
	number, perr := parsePRNumberFromURL(url)
	if perr != nil {
		// URL parsing failed — return without a number; callers that only need
		// the URL are still served correctly.
		return &forge.PR{URL: url}, nil
	}
	return &forge.PR{Number: number, URL: url, HeadBranch: branch}, nil
}

// Close closes the PR without merging it.
func (s *githubPRSvc) Close(_ context.Context, owner, repo string, number int) error {
	ownerRepo := owner + "/" + repo
	args := []string{"pr", "close", strconv.Itoa(number), "--repo", ownerRepo}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("gh pr close %d %s: %w\n%s", number, ownerRepo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Reopen re-opens a previously closed PR.
func (s *githubPRSvc) Reopen(_ context.Context, owner, repo string, number int) error {
	ownerRepo := owner + "/" + repo
	args := []string{"pr", "reopen", strconv.Itoa(number), "--repo", ownerRepo}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("gh pr reopen %d %s: %w\n%s", number, ownerRepo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ListChecks returns the CI check-run status for the PR's head commit.
// Status values: "queued", "in_progress", "completed".
// Conclusion values: "success", "failure", "cancelled", "skipped", "" (when not yet completed).
func (s *githubPRSvc) ListChecks(_ context.Context, owner, repo string, number int) ([]forge.CheckRun, error) {
	ownerRepo := owner + "/" + repo
	args := []string{
		"pr", "checks", strconv.Itoa(number),
		"--repo", ownerRepo,
		"--json", "name,status,conclusion",
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("gh pr checks %d %s: %w\n%s", number, ownerRepo, err, strings.TrimSpace(string(out)))
	}
	var raw []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh pr checks %d %s: parse response: %w", number, ownerRepo, err)
	}
	runs := make([]forge.CheckRun, 0, len(raw))
	for _, r := range raw {
		runs = append(runs, forge.CheckRun{
			Name:       r.Name,
			Status:     r.Status,
			Conclusion: r.Conclusion,
		})
	}
	return runs, nil
}

// Merge lands the PR via the GitHub merge button.
//
// opts.Method selects the merge strategy: "merge" (merge commit), "squash",
// or "rebase". An empty Method uses the repository's default merge method.
// opts.CommitMessage overrides the merge-commit message when Method is
// "merge" or "squash"; it is ignored for "rebase".
//
// This is the explicit merge-method/message seam that koryph-ufy's PR-based
// merge flow MUST use — callers set Method and CommitMessage; this layer
// passes them through to gh without re-interpreting them.
func (s *githubPRSvc) Merge(_ context.Context, owner, repo string, number int, opts forge.MergeOptions) error {
	ownerRepo := owner + "/" + repo
	args := []string{"pr", "merge", strconv.Itoa(number), "--repo", ownerRepo}

	switch opts.Method {
	case "squash":
		args = append(args, "--squash")
	case "rebase":
		args = append(args, "--rebase")
	case "merge", "":
		args = append(args, "--merge")
	default:
		return fmt.Errorf("gh pr merge: unknown method %q (want merge|squash|rebase)", opts.Method)
	}
	if strings.TrimSpace(opts.CommitMessage) != "" && opts.Method != "rebase" {
		args = append(args, "--body", opts.CommitMessage)
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("gh pr merge %d %s: %w\n%s", number, ownerRepo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Approve registers an approving review on the PR as the authenticated
// (operator) identity. body is optional review comment text. GitHub rejects
// self-approval; callers must guard against this before calling Approve.
func (s *githubPRSvc) Approve(_ context.Context, owner, repo string, number int, body string) error {
	ownerRepo := owner + "/" + repo
	args := []string{"pr", "review", strconv.Itoa(number), "--approve", "--repo", ownerRepo}
	if strings.TrimSpace(body) != "" {
		args = append(args, "--body", body)
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("gh pr review --approve %d %s: %w\n%s", number, ownerRepo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// AddLabels attaches labels to the PR, creating them in the repository if
// needed. Existing labels are not removed.
func (s *githubPRSvc) AddLabels(_ context.Context, owner, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	ownerRepo := owner + "/" + repo
	args := []string{"pr", "edit", strconv.Itoa(number), "--repo", ownerRepo}
	for _, l := range labels {
		args = append(args, "--add-label", l)
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("gh pr edit --add-label %d %s: %w\n%s", number, ownerRepo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveLabels detaches labels from the PR. Labels not currently attached
// are silently ignored.
func (s *githubPRSvc) RemoveLabels(_ context.Context, owner, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	ownerRepo := owner + "/" + repo
	args := []string{"pr", "edit", strconv.Itoa(number), "--repo", ownerRepo}
	for _, l := range labels {
		args = append(args, "--remove-label", l)
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("gh pr edit --remove-label %d %s: %w\n%s", number, ownerRepo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------- helpers ----------------------------------------------------------

// parsePRNumberFromURL parses the PR number from a GitHub PR URL such as
// https://github.com/owner/repo/pull/42. Returns an error when the URL does
// not match the expected pattern.
func parsePRNumberFromURL(url string) (int, error) {
	const seg = "/pull/"
	idx := strings.LastIndex(url, seg)
	if idx < 0 {
		return 0, fmt.Errorf("parsePRNumberFromURL: no /pull/ in %q", url)
	}
	rest := strings.TrimSpace(url[idx+len(seg):])
	// The number may be followed by a newline or other suffix.
	rest = strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\n' || r == '\r' })[0]
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, fmt.Errorf("parsePRNumberFromURL: cannot parse number from %q: %w", rest, err)
	}
	return n, nil
}

// ---------- compile-time interface guard -------------------------------------

var _ forge.PRService = (*githubPRSvc)(nil)
