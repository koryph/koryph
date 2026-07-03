// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/koryph/koryph/internal/execx"
)

// ghIssue is the subset of `gh issue list --json ...` fields intake consumes.
type ghIssue struct {
	Number int       `json:"number"`
	Title  string    `json:"title"`
	Body   string    `json:"body"`
	Labels []ghLabel `json:"labels"`
	Author ghAuthor  `json:"author"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghAuthor struct {
	Login string `json:"login"`
}

// ghClient runs the GitHub CLI in a project's root directory and implements
// the Source interface. The binary is `gh` unless overridden by KORYPH_GH_BIN.
type ghClient struct {
	bin string
	dir string
}

func newGH(dir string) *ghClient {
	bin := "gh"
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		bin = v
	}
	return &ghClient{bin: bin, dir: dir}
}

// List implements Source. It calls `gh issue list` and converts the results to
// provider-neutral SourceIssues.
func (g *ghClient) List(ctx context.Context, owner, repo, label string, limit int) ([]SourceIssue, error) {
	raw, err := g.listIssues(ctx, owner, repo, label, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SourceIssue, 0, len(raw))
	for _, iss := range raw {
		labels := make([]string, 0, len(iss.Labels))
		for _, l := range iss.Labels {
			labels = append(labels, l.Name)
		}
		out = append(out, SourceIssue{
			Number: iss.Number,
			Title:  iss.Title,
			Body:   iss.Body,
			Labels: labels,
			Author: iss.Author.Login,
		})
	}
	return out, nil
}

// Comment implements Source. It calls `gh issue comment`.
func (g *ghClient) Comment(ctx context.Context, owner, repo string, number int, body string) error {
	_, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir:  g.dir,
		Name: g.bin,
		Args: []string{
			"issue", "comment", strconv.Itoa(number),
			"--repo", owner + "/" + repo,
			"--body", body,
		},
	})
	return err
}

// Provenance implements Source. Returns "gh-<owner>/<repo>#<number>" so that
// issue numbers from different repositories never collide in the bead store.
func (g *ghClient) Provenance(owner, repo string, number int) string {
	return fmt.Sprintf("gh-%s/%s#%d", owner, repo, number)
}

// legacyProvenance returns the pre-v1 key format ("gh-<number>") used by beads
// created before repository qualification was introduced. It is used ONLY as a
// backward-compatibility fallback during deduplication and must never be used
// when creating new beads.
func (g *ghClient) legacyProvenance(number int) string {
	return fmt.Sprintf("gh-%d", number)
}

// listIssues runs `gh issue list` for the trigger label and parses the JSON.
func (g *ghClient) listIssues(ctx context.Context, owner, repo, label string, limit int) ([]ghIssue, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir:  g.dir,
		Name: g.bin,
		Args: []string{
			"issue", "list",
			"--repo", owner + "/" + repo,
			"--label", label,
			"--state", "open",
			"--limit", strconv.Itoa(limit),
			"--json", "number,title,body,labels,author",
		},
	})
	if err != nil {
		return nil, err
	}
	return parseGHIssues([]byte(res.Stdout))
}

// parseGHIssues decodes the `gh issue list --json` array (empty stdout → nil).
func parseGHIssues(data []byte) ([]ghIssue, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var out []ghIssue
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("intake: parse gh issue list json: %w", err)
	}
	return out, nil
}
