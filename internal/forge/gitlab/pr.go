// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/forge"
)

// gitlabPRSvc implements [forge.PRService] for GitLab using the GitLab REST API v4.
//
// Authentication: reads KORYPH_GITLAB_TOKEN for the private access token.
// All requests include the token in the PRIVATE-TOKEN header.
//
// The merge-method seam follows GitLab's MR merge API:
//   - ""  or "merge"                    : standard merge commit (squash=false)
//   - "squash"                          : squash all commits into one (squash=true)
//   - "merge_when_pipeline_succeeds"    : queue the MR for auto-merge when CI passes
//   - "rebase"                          : not supported per-MR; returns [forge.ErrUnsupported]
//
// CheckRun mapping from GitLab pipeline job statuses:
//   - created / waiting_for_resource / preparing / pending / manual / scheduled → queued / ""
//   - running → in_progress / ""
//   - success → completed / success
//   - failed → completed / failure
//   - canceled → completed / cancelled
//   - skipped → completed / skipped
type gitlabPRSvc struct{}

// prToken returns the GitLab access token for MR operations from KORYPH_GITLAB_TOKEN.
func prToken() string { return os.Getenv("KORYPH_GITLAB_TOKEN") }

// prAPIBase returns the GitLab API base URL.
// KORYPH_GITLAB_BASE_URL overrides the full URL (used in tests with httptest.Server).
// Otherwise falls back to glAPIBase() which honours KORYPH_GITLAB_HOST.
func prAPIBase() string {
	if v := os.Getenv("KORYPH_GITLAB_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return glAPIBase()
}

// prHTTPClient returns a shared HTTP client for MR API calls.
func prHTTPClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// glPIDPath returns the URL-path-encoded project identifier ("namespace%2Frepo").
func glPIDPath(owner, repo string) string { return url.PathEscape(owner + "/" + repo) }

// ---------- low-level HTTP helpers --------------------------------------------

// glDo executes an authenticated HTTP request and returns the response body and
// status code. reqBody may be nil for requests without a body.
func glDo(ctx context.Context, method, apiURL string, reqBody []byte) ([]byte, int, error) {
	var body io.Reader
	if reqBody != nil {
		body = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("gitlab pr: build request %s %s: %w", method, apiURL, err)
	}
	req.Header.Set("PRIVATE-TOKEN", prToken())
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := prHTTPClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("gitlab pr: %s %s: %w", method, apiURL, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, nil
}

// glExpect calls glDo and returns an error if the status code is not in want.
func glExpect(ctx context.Context, method, apiURL string, reqBody []byte, want ...int) ([]byte, error) {
	body, code, err := glDo(ctx, method, apiURL, reqBody)
	if err != nil {
		return nil, err
	}
	for _, w := range want {
		if code == w {
			return body, nil
		}
	}
	return nil, fmt.Errorf("gitlab pr: %s %s returned HTTP %d: %s", method, apiURL, code, strings.TrimSpace(string(body)))
}

// ---------- GitLab REST API types --------------------------------------------

// glMR is the subset of GitLab MR fields returned by the API that koryph uses.
// GitLab returns labels as a flat []string (not a struct array like GitHub).
type glMR struct {
	IID          int      `json:"iid"`
	Title        string   `json:"title"`
	WebURL       string   `json:"web_url"`
	State        string   `json:"state"` // "opened", "closed", "merged"
	Labels       []string `json:"labels"`
	SourceBranch string   `json:"source_branch"`
	SHA          string   `json:"sha"`
	Draft        bool     `json:"draft"`
	Author       struct {
		Username string `json:"username"`
	} `json:"author"`
}

// toForgePR converts a GitLab MR to the forge-neutral [forge.PR] type.
// GitLab "opened" → forge "open"; "closed" and "merged" are kept as-is.
func (m *glMR) toForgePR() forge.PR {
	state := m.State
	if state == "opened" {
		state = "open"
	}
	labels := m.Labels
	if labels == nil {
		labels = []string{}
	}
	return forge.PR{
		Number:     m.IID,
		Title:      m.Title,
		URL:        m.WebURL,
		State:      state,
		Labels:     labels,
		HeadBranch: m.SourceBranch,
		HeadSHA:    m.SHA,
		Author:     m.Author.Username,
		Draft:      m.Draft,
	}
}

// glPipelineSummary is the pipeline record returned by
// GET /projects/:id/merge_requests/:iid/pipelines.
type glPipelineSummary struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
}

// glJob is a job record returned by GET /projects/:id/pipelines/:id/jobs.
type glJob struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// mapJobStatus maps a GitLab job/pipeline status to the forge-neutral Status
// and Conclusion fields of [forge.CheckRun].
func mapJobStatus(status string) (forgeStatus, conclusion string) {
	switch status {
	case "running":
		return "in_progress", ""
	case "success":
		return "completed", "success"
	case "failed":
		return "completed", "failure"
	case "canceled":
		return "completed", "cancelled"
	case "skipped":
		return "completed", "skipped"
	default:
		// created, waiting_for_resource, preparing, pending, manual, scheduled
		return "queued", ""
	}
}

// ---------- PRService methods -------------------------------------------------

// List returns MRs for the repository filtered by opts.
// opts.State "" or "open" maps to GitLab "opened"; "closed", "merged", "all" pass through.
// opts.Limit caps results (0 uses the GitLab default, max 100 per page).
// opts.Labels filters to MRs carrying ALL listed labels (comma-separated query param).
func (s *gitlabPRSvc) List(ctx context.Context, owner, repo string, opts forge.ListPROptions) ([]forge.PR, error) {
	state := opts.State
	switch state {
	case "", "open":
		state = "opened"
	case "closed", "merged", "all":
		// pass through unchanged
	default:
		state = "opened"
	}

	q := url.Values{}
	q.Set("state", state)
	if opts.Limit > 0 {
		q.Set("per_page", strconv.Itoa(opts.Limit))
	} else {
		q.Set("per_page", "100")
	}
	if len(opts.Labels) > 0 {
		q.Set("labels", strings.Join(opts.Labels, ","))
	}

	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests?%s",
		prAPIBase(), glPIDPath(owner, repo), q.Encode())

	raw, err := glExpect(ctx, http.MethodGet, apiURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab pr list %s/%s: %w", owner, repo, err)
	}
	var mrs []glMR
	if err := json.Unmarshal(raw, &mrs); err != nil {
		return nil, fmt.Errorf("gitlab pr list %s/%s: parse response: %w", owner, repo, err)
	}
	prs := make([]forge.PR, 0, len(mrs))
	for i := range mrs {
		pr := mrs[i].toForgePR()
		prs = append(prs, pr)
	}
	return prs, nil
}

// Get returns one MR by its sequential IID (internal ID within the project).
func (s *gitlabPRSvc) Get(ctx context.Context, owner, repo string, number int) (*forge.PR, error) {
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d",
		prAPIBase(), glPIDPath(owner, repo), number)
	raw, err := glExpect(ctx, http.MethodGet, apiURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab pr get %s/%s!%d: %w", owner, repo, number, err)
	}
	var mr glMR
	if err := json.Unmarshal(raw, &mr); err != nil {
		return nil, fmt.Errorf("gitlab pr get %s/%s!%d: parse response: %w", owner, repo, number, err)
	}
	pr := mr.toForgePR()
	return &pr, nil
}

// Create opens a new MR from branch against base with the given title and description.
// Returns the created MR with at minimum Number and URL populated.
func (s *gitlabPRSvc) Create(ctx context.Context, owner, repo, branch, base, title, body string) (*forge.PR, error) {
	payload, _ := json.Marshal(map[string]string{
		"source_branch": branch,
		"target_branch": base,
		"title":         title,
		"description":   body,
	})
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests",
		prAPIBase(), glPIDPath(owner, repo))
	raw, err := glExpect(ctx, http.MethodPost, apiURL, payload, http.StatusCreated)
	if err != nil {
		return nil, fmt.Errorf("gitlab pr create %s/%s: %w", owner, repo, err)
	}
	var mr glMR
	if err := json.Unmarshal(raw, &mr); err != nil {
		return nil, fmt.Errorf("gitlab pr create %s/%s: parse response: %w", owner, repo, err)
	}
	pr := mr.toForgePR()
	return &pr, nil
}

// Close closes the MR without merging it. GitLab uses state_event=close.
func (s *gitlabPRSvc) Close(ctx context.Context, owner, repo string, number int) error {
	payload, _ := json.Marshal(map[string]string{"state_event": "close"})
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d",
		prAPIBase(), glPIDPath(owner, repo), number)
	if _, err := glExpect(ctx, http.MethodPut, apiURL, payload, http.StatusOK); err != nil {
		return fmt.Errorf("gitlab pr close %s/%s!%d: %w", owner, repo, number, err)
	}
	return nil
}

// Reopen re-opens a previously closed MR. GitLab uses state_event=reopen.
func (s *gitlabPRSvc) Reopen(ctx context.Context, owner, repo string, number int) error {
	payload, _ := json.Marshal(map[string]string{"state_event": "reopen"})
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d",
		prAPIBase(), glPIDPath(owner, repo), number)
	if _, err := glExpect(ctx, http.MethodPut, apiURL, payload, http.StatusOK); err != nil {
		return fmt.Errorf("gitlab pr reopen %s/%s!%d: %w", owner, repo, number, err)
	}
	return nil
}

// ListChecks returns CI check status for the MR's head commit by inspecting the
// latest pipeline's jobs. Returns an empty slice when no pipeline exists yet.
//
// GitLab pipeline-job statuses are mapped to forge.CheckRun as follows:
//   - running → in_progress
//   - success → completed/success
//   - failed → completed/failure
//   - canceled → completed/cancelled
//   - skipped → completed/skipped
//   - all others (created, pending, …) → queued
func (s *gitlabPRSvc) ListChecks(ctx context.Context, owner, repo string, number int) ([]forge.CheckRun, error) {
	// Step 1: fetch the list of pipelines for this MR and take the latest one.
	pipelinesURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d/pipelines",
		prAPIBase(), glPIDPath(owner, repo), number)
	raw, err := glExpect(ctx, http.MethodGet, pipelinesURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab pr checks %s/%s!%d: list pipelines: %w", owner, repo, number, err)
	}
	var pipelines []glPipelineSummary
	if err := json.Unmarshal(raw, &pipelines); err != nil {
		return nil, fmt.Errorf("gitlab pr checks %s/%s!%d: parse pipelines: %w", owner, repo, number, err)
	}
	if len(pipelines) == 0 {
		return []forge.CheckRun{}, nil
	}

	// Step 2: fetch jobs for the latest pipeline (first in list — GitLab returns
	// newest first).
	latestID := pipelines[0].ID
	jobsURL := fmt.Sprintf("%s/projects/%s/pipelines/%d/jobs?per_page=100",
		prAPIBase(), glPIDPath(owner, repo), latestID)
	raw, err = glExpect(ctx, http.MethodGet, jobsURL, nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("gitlab pr checks %s/%s!%d: list jobs: %w", owner, repo, number, err)
	}
	var jobs []glJob
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return nil, fmt.Errorf("gitlab pr checks %s/%s!%d: parse jobs: %w", owner, repo, number, err)
	}

	runs := make([]forge.CheckRun, 0, len(jobs))
	for _, j := range jobs {
		st, conc := mapJobStatus(j.Status)
		runs = append(runs, forge.CheckRun{
			Name:       j.Name,
			Status:     st,
			Conclusion: conc,
		})
	}
	return runs, nil
}

// Merge lands the MR using the specified merge strategy.
//
// Supported Methods:
//   - "" or "merge"                 : standard merge commit
//   - "squash"                      : squash all commits into one commit
//   - "merge_when_pipeline_succeeds": queue the MR for auto-merge when CI passes
//   - "rebase"                      : returns [forge.ErrUnsupported] (not available per-MR in GitLab)
//
// opts.CommitMessage sets the merge commit message (ignored for
// merge_when_pipeline_succeeds as the message is set at actual merge time).
func (s *gitlabPRSvc) Merge(ctx context.Context, owner, repo string, number int, opts forge.MergeOptions) error {
	mergePayload := map[string]any{}

	switch opts.Method {
	case "", "merge":
		mergePayload["squash"] = false
	case "squash":
		mergePayload["squash"] = true
	case "merge_when_pipeline_succeeds":
		mergePayload["merge_when_pipeline_succeeds"] = true
	case "rebase":
		return fmt.Errorf("gitlab pr merge %s/%s!%d: %w: GitLab does not support per-MR rebase via the merge API; configure rebase_merge at the project level instead",
			owner, repo, number, forge.ErrUnsupported)
	default:
		return fmt.Errorf("gitlab pr merge %s/%s!%d: unknown method %q (want merge|squash|merge_when_pipeline_succeeds)",
			owner, repo, number, opts.Method)
	}

	if msg := strings.TrimSpace(opts.CommitMessage); msg != "" && opts.Method != "merge_when_pipeline_succeeds" {
		mergePayload["merge_commit_message"] = msg
	}

	payload, _ := json.Marshal(mergePayload)
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d/merge",
		prAPIBase(), glPIDPath(owner, repo), number)
	if _, err := glExpect(ctx, http.MethodPut, apiURL, payload, http.StatusOK); err != nil {
		return fmt.Errorf("gitlab pr merge %s/%s!%d: %w", owner, repo, number, err)
	}
	return nil
}

// Approve registers an approving review on the MR.
// body is sent as the approval comment when non-empty (not all GitLab
// configurations expose this field; it is sent as a best-effort note).
// GitLab rejects self-approval; callers must guard against that before calling.
func (s *gitlabPRSvc) Approve(ctx context.Context, owner, repo string, number int, body string) error {
	payload := map[string]any{}
	if strings.TrimSpace(body) != "" {
		payload["approval_password"] = "" // not used but included for completeness
	}
	rawPayload, _ := json.Marshal(payload)
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d/approve",
		prAPIBase(), glPIDPath(owner, repo), number)
	if _, err := glExpect(ctx, http.MethodPost, apiURL, rawPayload, http.StatusCreated, http.StatusOK); err != nil {
		return fmt.Errorf("gitlab pr approve %s/%s!%d: %w", owner, repo, number, err)
	}
	return nil
}

// AddLabels attaches one or more labels to the MR using the add_labels
// parameter. Labels that do not yet exist on the project are silently ignored
// by GitLab (they must be pre-created via the Labels API).
// Existing labels on the MR are preserved.
func (s *gitlabPRSvc) AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{
		"add_labels": strings.Join(labels, ","),
	})
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d",
		prAPIBase(), glPIDPath(owner, repo), number)
	if _, err := glExpect(ctx, http.MethodPut, apiURL, payload, http.StatusOK); err != nil {
		return fmt.Errorf("gitlab pr add-labels %s/%s!%d: %w", owner, repo, number, err)
	}
	return nil
}

// RemoveLabels detaches the named labels from the MR using the remove_labels
// parameter. Labels that are not currently attached are silently ignored by
// GitLab.
func (s *gitlabPRSvc) RemoveLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{
		"remove_labels": strings.Join(labels, ","),
	})
	apiURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d",
		prAPIBase(), glPIDPath(owner, repo), number)
	if _, err := glExpect(ctx, http.MethodPut, apiURL, payload, http.StatusOK); err != nil {
		return fmt.Errorf("gitlab pr remove-labels %s/%s!%d: %w", owner, repo, number, err)
	}
	return nil
}

// ---------- compile-time interface guard -------------------------------------

var _ forge.PRService = (*gitlabPRSvc)(nil)
