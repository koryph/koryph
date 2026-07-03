// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package intake

import (
	"context"
	"fmt"
	"strings"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// SourceResult is the per-source outcome returned by RunMulti.
type SourceResult struct {
	// Provider and Source echo the IntakeSource that drove this run.
	Provider string
	Source   string
	// Result holds the ingested/skipped counts; Owner and Repo are
	// populated from the parsed source field.
	Result *Result
}

// MultiOptions configures a multi-source intake run driven by a project config.
type MultiOptions struct {
	// Project is the registry record for the project (used for Root and
	// default Remote when no intake sources are configured).
	Project *registry.Record

	// Sources is the list from project.Config.Intake.
	Sources []project.IntakeSource

	// Flag overrides — when non-zero/non-empty they replace the per-source
	// value for every source in this run.
	OverrideLabel       string // overrides Source.Trigger when non-empty
	OverrideLimit       int    // overrides Source.Limit when > 0
	OverrideCommentBack *bool  // overrides Source.CommentBack when non-nil
	DryRun              bool
}

// RunMulti iterates all configured intake sources in order, calling Run (for
// GitHub) or RunJIRA (for JIRA) per source, and returns a slice of
// SourceResults. It never stops early: an error from one source is collected
// and returned after all sources have been attempted. If every source succeeds
// the returned error is nil.
func RunMulti(ctx context.Context, opts MultiOptions) ([]SourceResult, error) {
	if opts.Project == nil {
		return nil, fmt.Errorf("intake: project record is required")
	}
	if len(opts.Sources) == 0 {
		return nil, fmt.Errorf("intake: RunMulti called with no sources configured")
	}

	results := make([]SourceResult, 0, len(opts.Sources))
	var errs []string

	for _, s := range opts.Sources {
		provider := strings.TrimSpace(s.Provider)
		if provider == "" {
			provider = "github"
		}

		// Apply per-source settings, then overlay run-level overrides.
		trigger := s.Trigger
		if opts.OverrideLabel != "" {
			trigger = opts.OverrideLabel
		}

		limit := s.Limit
		if limit <= 0 {
			limit = DefaultLimit
		}
		if opts.OverrideLimit > 0 {
			limit = opts.OverrideLimit
		}

		commentBack := s.CommentBack
		if opts.OverrideCommentBack != nil {
			commentBack = *opts.OverrideCommentBack
		}

		var res *Result
		var err error

		switch provider {
		case "github":
			res, err = runMultiGitHub(ctx, opts.Project, s.Source, trigger, limit, opts.DryRun, commentBack)
		case "jira":
			res, err = runMultiJIRA(ctx, opts.Project, s.Source, trigger, limit, opts.DryRun, commentBack)
		case "linear":
			res, err = runMultiLinear(ctx, opts.Project, s.Source, trigger, limit, opts.DryRun, commentBack)
		default:
			errs = append(errs, fmt.Sprintf("source %q: provider %q not supported (supported: github, jira, linear)", s.Source, provider))
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("source %q: %v", s.Source, err))
			continue
		}

		results = append(results, SourceResult{
			Provider: provider,
			Source:   s.Source,
			Result:   res,
		})
	}

	if len(errs) > 0 {
		return results, fmt.Errorf("intake: %s", strings.Join(errs, "; "))
	}
	return results, nil
}

// runMultiGitHub dispatches one GitHub source. It builds a synthetic remote
// from the "owner/repo" source field so the existing Run path can parse it.
func runMultiGitHub(ctx context.Context, project *registry.Record, source, label string, limit int, dryRun, commentBack bool) (*Result, error) {
	if label == "" {
		label = DefaultLabel
	}
	remote := "https://github.com/" + strings.TrimPrefix(source, "/")
	syntheticRec := &registry.Record{
		ProjectID: project.ProjectID,
		Root:      project.Root,
		Remote:    remote,
	}
	return Run(ctx, Options{
		Project:     syntheticRec,
		Label:       label,
		Limit:       limit,
		DryRun:      dryRun,
		CommentBack: commentBack,
	})
}

// runMultiJIRA dispatches one JIRA source. The source field must be
// "<host>/<project-key>" (e.g. "acme.atlassian.net/ENG").
func runMultiJIRA(ctx context.Context, project *registry.Record, source, jql string, limit int, dryRun, commentBack bool) (*Result, error) {
	host, projectKey, err := parseJIRASource(source)
	if err != nil {
		return nil, err
	}
	return RunJIRA(ctx, JIRAOptions{
		Project:     project,
		BaseURL:     "https://" + host,
		ProjectKey:  projectKey,
		JQL:         jql,
		Limit:       limit,
		DryRun:      dryRun,
		CommentBack: commentBack,
	})
}

// runMultiLinear dispatches one Linear source. The source field is the Linear
// team key (e.g. "ENG"). The trigger is passed verbatim to RunLinear where it
// is parsed as a label or state filter.
func runMultiLinear(ctx context.Context, project *registry.Record, source, trigger string, limit int, dryRun, commentBack bool) (*Result, error) {
	teamKey := strings.TrimSpace(source)
	if teamKey == "" {
		return nil, fmt.Errorf("linear: source (team key) is required")
	}
	return RunLinear(ctx, LinearOptions{
		Project:     project,
		TeamKey:     teamKey,
		Trigger:     trigger,
		Limit:       limit,
		DryRun:      dryRun,
		CommentBack: commentBack,
	})
}

// parseJIRASource splits "acme.atlassian.net/ENG" into ("acme.atlassian.net", "ENG").
// It also accepts "https://acme.atlassian.net/ENG" by stripping the scheme.
func parseJIRASource(source string) (host, projectKey string, err error) {
	s := strings.TrimSpace(source)
	// Strip scheme if present.
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.Trim(s, "/")
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("jira: source %q must be \"<host>/<project-key>\" (e.g. \"acme.atlassian.net/ENG\")", source)
	}
	return parts[0], strings.ToUpper(parts[1]), nil
}
