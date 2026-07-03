// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package project defines the per-project adapter configuration
// (koryph.project.json at the repo root). Everything that legitimately
// varies between projects lives here; everything else is engine behavior.
package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/signing"
)

// ConfigFileName is the adapter file at each managed repo's root.
const ConfigFileName = "koryph.project.json"

// Policy is how a bead's finished branch lands. It is the merge_policy default
// (an epic's merge:<policy> label overrides it). The constants are the whole
// vocabulary — the engine compares against them so a typo is a compile error.
type Policy string

const (
	PolicyManual Policy = "manual" // leave the branch for a human to land
	PolicyAuto   Policy = "auto"   // ff-merge onto the default branch when clean
	PolicyPR     Policy = "pr"     // push + open a PR for later landing
)

// FootprintRule maps a path glob (doublestar-lite: '*' within a segment,
// '**' across segments, evaluated by sched) to footprint tokens. Tokens
// prefixed "HOT:" conflict with everything sharing that token.
type FootprintRule struct {
	Pattern string   `json:"pattern"`
	Tokens  []string `json:"tokens"`
}

// PipelineStage is one post-implement stage in the project pipeline. Stages run
// sequentially in the implementer's worktree after its commits land and before
// review/merge, each as a persona agent that may add its own commits (docs,
// tests, changelog, ...).
type PipelineStage struct {
	// Name identifies the stage (e.g. "docs", "test"). When it matches a known
	// engine stage it inherits that stage's default persona and model tier via
	// modelroute; it is also the stage key for model:<stage>:<tier> labels.
	Name string `json:"name"`

	// Persona overrides the agent (.claude/agents/<persona>); default is
	// modelroute.PersonaFor(Name, Stages).
	Persona string `json:"persona,omitempty"`

	// Model overrides the tier for this stage (must be in AllowedModels);
	// default is the engine stage default resolved by modelroute.
	Model string `json:"model,omitempty"`

	// Effort overrides the reasoning-effort hint passed to the agent.
	Effort string `json:"effort,omitempty"`

	// Prompt is extra, stage-specific instruction text appended to the built
	// stage prompt.
	Prompt string `json:"prompt,omitempty"`

	// Optional stages log-and-continue on failure; a non-optional stage (the
	// default) that fails blocks the slot and stops the pipeline (fail closed).
	Optional bool `json:"optional,omitempty"`
}

// IntakeSource configures one issue-tracker source in the koryph.project.json
// intake list. Each entry drives one poll per `koryph intake` invocation.
type IntakeSource struct {
	// Provider is the issue-tracker type. Supported values: "github" (default),
	// "jira", "linear". The field defaults to "github" when omitted.
	Provider string `json:"provider,omitempty"`

	// Source identifies the target within the provider.
	//   GitHub: "owner/repo" (e.g. "acme/widgets").
	//   JIRA:   "<host>/<project-key>" (e.g. "acme.atlassian.net/ENG").
	//   Linear: "<team-key>" (e.g. "ENG").
	Source string `json:"source"`

	// Trigger is the label (GitHub) or JQL predicate (JIRA) or label/state
	// filter (Linear) that determines which open issues are candidates for
	// intake. Default: "triage".
	//   GitHub: label name, e.g. "triage".
	//   JIRA:   JQL clause AND-combined with "project = <key>", e.g. `status = "To Do"`.
	//   Linear: "label:<name>" or "state:<name>" or bare label name. When empty
	//           all open issues in the team are polled.
	Trigger string `json:"trigger,omitempty"`

	// Limit caps the number of open issues fetched per run. Default: 20.
	Limit int `json:"limit,omitempty"`

	// CommentBack posts the new bead ID back on each ingested issue.
	// Opt-in; mirrors the --comment flag. Default: false.
	CommentBack bool `json:"comment_back,omitempty"`

	// Mapping is reserved for future provider-specific field remapping.
	// Ignored in v1.
	Mapping map[string]string `json:"mapping,omitempty"`
}

// Config is the per-project adapter.
type Config struct {
	SchemaVersion int    `json:"schema_version"`
	ProjectID     string `json:"project_id"`

	// WorkSource is "bd" (beads ready-graph, preferred) or "markdown"
	// (legacy docs/plans phase docs; supported for un-migrated projects).
	WorkSource string `json:"work_source"`
	PlansDir   string `json:"plans_dir,omitempty"`

	// Footprint declares conflict domains. AreaMap maps an `area:<x>` bead
	// label to footprint tokens when no fp:* label is present.
	Footprint []FootprintRule     `json:"footprint,omitempty"`
	AreaMap   map[string][]string `json:"area_map,omitempty"`

	// Gate is the ordered green-gate command list run in the worktree after
	// rebase and before merge (each entry runs via `sh -c` under direnv when
	// available).
	Gate []string `json:"gate"`

	// Stages maps pipeline stage -> persona name in .claude/agents.
	// Tiers maps model tier -> persona for tier-driven dispatch.
	Stages map[string]string `json:"stages,omitempty"`
	Tiers  map[string]string `json:"tiers,omitempty"`

	// Pipeline lists post-implement stages executed sequentially in the
	// worktree after the implementer and before review/merge. Empty keeps the
	// classic implement -> (review) -> merge flow. See PipelineStage.
	Pipeline []PipelineStage `json:"pipeline,omitempty"`

	// Bootstrap commands run in a freshly created or re-attached worktree
	// before the agent starts (e.g. "pnpm install --frozen-lockfile").
	Bootstrap []string `json:"bootstrap,omitempty"`

	// Intake lists the issue-tracker sources polled by `koryph intake`.
	// Each entry drives one poll per run. When empty, intake falls back to
	// the project's registry remote with CLI flags.
	Intake []IntakeSource `json:"intake,omitempty"`

	// ProtectedPaths extend the engine's default protected list; diffs
	// touching them are never mergeable from a worktree.
	ProtectedPaths []string `json:"protected_paths,omitempty"`

	// Validation commands for `koryph validate` (beyond the engine checks).
	Validation []string `json:"validation,omitempty"`

	// EngineVersion is the minimum koryph engine this project requires,
	// minimum-style: "0.2+", "1+", ">=0.2.0", or a bare version (also a
	// minimum). Empty = any engine.
	EngineVersion string `json:"engine_version,omitempty"`

	// CommitStyle governs agent commit messages: "conventional" (default,
	// also when empty) is mechanically enforced at merge/PR time (every
	// commit subject in def..branch must match type(scope): subject);
	// "custom" (CommitTemplate required) governs via the template and is not
	// conventional-validated; "none" opts out of enforcement entirely.
	// Projects can additionally map Stages["commit"] to a persona whose
	// guidance agents consult for commit authoring.
	CommitStyle    string `json:"commit_style,omitempty"`
	CommitTemplate string `json:"commit_template,omitempty"`

	// MergePolicy default when the epic carries no merge:* label.
	MergePolicy Policy `json:"merge_policy"`

	// MergeMethod is how an engine-opened PR lands on the default branch:
	// "ff" (default, also when empty) preserves the exact gate-checked, signed
	// commit SHAs via a local fast-forward + push; "squash" collapses them into
	// one new commit. A non-ff method is refused while signing is required
	// (only ff preserves signatures). GitHub-native merge methods are never
	// used — they rewrite SHAs or add an unsigned merge commit.
	MergeMethod string `json:"merge_method,omitempty"`

	// RiskTierDefault is the recovery tier (0-3) for beads without rt:*.
	RiskTierDefault int `json:"risk_tier_default"`

	// Signing is the vault-backed commit/artifact signing policy
	// (nil = signing not configured; managed by `koryph signing setup`).
	Signing *signing.Config `json:"signing,omitempty"`

	// MaxConcurrentSlots caps wave width for this project (default 3).
	MaxConcurrentSlots int `json:"max_concurrent_slots,omitempty"`

	// DispatchStaggerSeconds between agent launches (default 8).
	DispatchStaggerSeconds int `json:"dispatch_stagger_seconds,omitempty"`
}

// Default returns a conservative baseline config.
func Default(projectID string) *Config {
	return &Config{
		SchemaVersion:          1,
		ProjectID:              projectID,
		WorkSource:             "bd",
		Gate:                   []string{"make lint", "make test"},
		MergePolicy:            PolicyManual,
		RiskTierDefault:        2,
		MaxConcurrentSlots:     3,
		DispatchStaggerSeconds: 8,
	}
}

// Load reads the adapter config from repoRoot.
func Load(repoRoot string) (*Config, error) {
	p := filepath.Join(repoRoot, ConfigFileName)
	var c Config
	if err := fsx.ReadJSON(p, &c); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("project not onboarded: %s missing (run `koryph onboard`)", p)
		}
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &c, nil
}

// Save writes the adapter config to repoRoot atomically.
func (c *Config) Save(repoRoot string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return fsx.WriteJSONAtomic(filepath.Join(repoRoot, ConfigFileName), c)
}

// Validate enforces internal consistency.
func (c *Config) Validate() error {
	switch c.WorkSource {
	case "bd", "markdown":
	default:
		return fmt.Errorf("work_source must be bd|markdown, got %q", c.WorkSource)
	}
	if c.WorkSource == "markdown" && c.PlansDir == "" {
		return fmt.Errorf("plans_dir required when work_source=markdown")
	}
	switch c.MergePolicy {
	case PolicyManual, PolicyAuto, PolicyPR:
	default:
		return fmt.Errorf("merge_policy must be manual|auto|pr, got %q", c.MergePolicy)
	}
	switch c.MergeMethod {
	case "", "ff", "squash":
	default:
		return fmt.Errorf("merge_method must be ff|squash, got %q", c.MergeMethod)
	}
	if c.RiskTierDefault < 0 || c.RiskTierDefault > 3 {
		return fmt.Errorf("risk_tier_default must be 0-3")
	}
	if len(c.Gate) == 0 {
		return fmt.Errorf("gate must have at least one command")
	}
	switch c.CommitStyle {
	case "", "conventional", "none":
	case "custom":
		if c.CommitTemplate == "" {
			return fmt.Errorf("commit_style custom requires commit_template")
		}
	default:
		return fmt.Errorf("commit_style must be conventional|custom|none, got %q", c.CommitStyle)
	}
	if c.Signing != nil {
		if err := c.Signing.Validate(); err != nil {
			return fmt.Errorf("signing: %w", err)
		}
	}
	if err := validatePipeline(c.Pipeline); err != nil {
		return err
	}
	if err := validateIntake(c.Intake); err != nil {
		return err
	}
	return nil
}

// EnforceConventional reports whether the merge/PR paths must validate commit
// subjects against the Conventional Commits grammar. It is ON by default
// (empty or "conventional") and disabled only by an explicit "none" opt-out;
// "custom" defers to CommitTemplate and is not conventional-validated.
func (c *Config) EnforceConventional() bool {
	return c.CommitStyle == "" || c.CommitStyle == "conventional"
}

// LandMethod is the effective PR-landing merge method, defaulting to "ff".
func (c *Config) LandMethod() string {
	if c.MergeMethod == "" {
		return "ff"
	}
	return c.MergeMethod
}

// LandMethodError validates a landing method (empty means the config default)
// and refuses a signature-breaking method while signing is required. Only "ff"
// preserves the exact signed SHAs; "squash" rewrites them into a new commit, so
// it is refused when Signing.Required is set.
func (c *Config) LandMethodError(method string) error {
	if method == "" {
		method = c.LandMethod()
	}
	switch method {
	case "ff", "squash":
	default:
		return fmt.Errorf("unknown merge_method %q (want ff|squash)", method)
	}
	if method != "ff" && c.Signing != nil && c.Signing.Required {
		return fmt.Errorf("merge_method %q rewrites the gate-checked signed commits, but signing.required is set; only ff preserves signatures", method)
	}
	return nil
}

// validateIntake enforces the intake source list contract: every source has a
// non-empty source field, the provider is "github", "jira", "linear", or empty
// (defaults to "github"), and limit (when set) is positive.
func validateIntake(sources []IntakeSource) error {
	for i, s := range sources {
		p := strings.TrimSpace(s.Provider)
		if p != "" && p != "github" && p != "jira" && p != "linear" {
			return fmt.Errorf("intake[%d]: provider %q is not supported (supported: github, jira, linear)", i, p)
		}
		if strings.TrimSpace(s.Source) == "" {
			return fmt.Errorf("intake[%d]: source is required", i)
		}
		if s.Limit < 0 {
			return fmt.Errorf("intake[%d]: limit must be >= 0, got %d", i, s.Limit)
		}
	}
	return nil
}

// validatePipeline enforces the post-implement stage contract: every stage has
// a name, names are unique, and the engine-managed "implement"/"review" stages
// may not be redeclared as pipeline steps. Model tiers are validated lazily at
// dispatch by modelroute (fail closed), keeping this package modelroute-free.
func validatePipeline(stages []PipelineStage) error {
	seen := make(map[string]bool, len(stages))
	for i, st := range stages {
		name := strings.TrimSpace(st.Name)
		if name == "" {
			return fmt.Errorf("pipeline[%d]: name is required", i)
		}
		if name == "implement" || name == "review" {
			return fmt.Errorf("pipeline stage %q is engine-managed and cannot be a post-implement stage", name)
		}
		if seen[name] {
			return fmt.Errorf("pipeline: duplicate stage %q", name)
		}
		seen[name] = true
	}
	return nil
}
