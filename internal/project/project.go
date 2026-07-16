// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package project defines the per-project adapter configuration
// (koryph.project.json at the repo root). Everything that legitimately
// varies between projects lives here; everything else is engine behavior.
package project

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/schemaver"
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

// ResourceSpec is one external resource kind's portable planning estimate in
// Config.Resources (koryph-4ql.3, design docs/designs/2026-07-resource-governor.md
// L2): the memory one unit of the kind is expected to consume when a bead
// declaring `res:<kind>` runs. It is the checked-in default that travels with
// the repo; the machine capacity ledger (governor.json) overrides mem_mb per
// host and adds capacity/ramp/probe. Additive/omitempty.
type ResourceSpec struct {
	// MemMB is the planning-time memory estimate in MB for one unit of the
	// kind. 0/absent means uncalibrated (no reservation from this vocabulary —
	// the machine ledger may still supply one).
	MemMB int `json:"mem_mb,omitempty"`
}

// MergeReconciler auto-heals a pre-merge rebase conflict confined to a known
// generated / derived file — a checksum-over-a-directory (a migrations
// lockfile like atlas.sum), a secrets baseline — by regenerating it from the
// post-merge tree instead of surfacing the line-level divergence as a fatal
// conflict (design docs/designs/2026-07-merge-reconcilers.md). Additive/omitempty:
// no entries = today's behavior (any conflict aborts to a requeue).
type MergeReconciler struct {
	// Path matches an unmerged path with path.Match semantics — an exact path
	// ("migrations/atlas.sum", ".secrets.baseline") or a glob
	// ("migrations/*.sum"); no "**".
	Path string `json:"path"`
	// Command reconciles the file. It runs via `sh -c` in the worktree with the
	// gate's allowlisted environment plus KORYPH_MERGE_PATH (the file to write)
	// and KORYPH_MERGE_OURS / _THEIRS / _BASE (temp files with the three
	// conflict stages), and must leave $KORYPH_MERGE_PATH a valid,
	// conflict-marker-free file. Two idioms: regenerate-from-tree (a checksum:
	// "atlas migrate hash --dir file://migrations", ignores the stage env) and
	// structured union (a secrets baseline: a helper reading _OURS/_THEIRS).
	// The healed tree still runs the project gate before merge, so put the
	// artifact's validator in the gate — that is what catches a bad regeneration.
	Command string `json:"command"`
}

// AdaptiveEscalation configures the learned-model auto-apply
// (koryph-qf6.6) — see Config.AdaptiveEscalation.
type AdaptiveEscalation struct {
	// Enabled turns on the engine's wave-boundary hook. Off by default: label
	// writes to beads are a visible, syncing side effect an operator should
	// opt into per project.
	Enabled bool `json:"enabled"`
	// MinEvidence is the minimum count of escalated-then-merged beads a
	// (area, size) bucket needs before its recommendation applies.
	// 0/absent = modellearn.DefaultMinEvidence (2).
	MinEvidence int `json:"min_evidence,omitempty" jsonschema:"minimum=0"`
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

	// TimeoutSec bounds the stage agent's run; <=0 uses the engine default
	// (stage.defaultTimeoutSec, 600s). Raise it for stages that legitimately
	// need longer — e.g. a docs sweep over a large rename (koryph-a59).
	TimeoutSec int `json:"timeout_sec,omitempty"`

	// Optional stages log-and-continue on failure; a non-optional stage (the
	// default) that fails blocks the slot and stops the pipeline (fail closed).
	Optional bool `json:"optional,omitempty"`
}

// GoreleaserBuild configures the GoReleaser build mode (mode A) for the
// project's release pipeline. The referenced .goreleaser.yaml must exist in
// the project root and configure artifact output to ArtifactsDir.
type GoreleaserBuild struct {
	// Version constrains the GoReleaser action version, e.g. "~> v2.16".
	// Defaults to "~> v2" when empty.
	Version string `json:"version,omitempty"`
}

// ReleaseBuildConfig is the build sub-block of ReleaseConfig: exactly one of
// Goreleaser or Commands must be set (enforced by project.Config.Validate).
//
// Mode A (Goreleaser != nil): GoReleaser drives cross-platform artifact
// creation via .goreleaser.yaml in the project root.
// Mode B (len(Commands) > 0): an ordered list of shell commands (each run via
// sh -c) builds and places artifacts into ArtifactsDir.
type ReleaseBuildConfig struct {
	// Goreleaser, when non-nil, selects mode A: GoReleaser-managed builds.
	Goreleaser *GoreleaserBuild `json:"goreleaser,omitempty"`

	// Commands, when non-empty, selects mode B: an ordered list of shell
	// commands (each run via sh -c) that build and stage artifacts.
	Commands []string `json:"commands,omitempty"`
}

// ReleaseConfig is the optional release sub-block of koryph.project.json. It
// drives `koryph release setup`, which renders and installs the caller GitHub
// Actions workflow (pointing at koryph/koryph's reusable release-train.yml)
// and the release-please config/manifest files into the target project.
//
// Exactly one build mode (Goreleaser or Commands) must be present in Build
// once this block is set (enforced by Validate).
type ReleaseConfig struct {
	// Type is the release-please release type, e.g. "go", "simple",
	// "node". See https://github.com/googleapis/release-please#release-types.
	Type string `json:"type"`

	// ExtraFiles lists additional files whose version strings release-please
	// should bump, e.g. ["internal/version/version.go"].
	ExtraFiles []string `json:"extra_files,omitempty"`

	// ArtifactsDir is the directory where build artifacts land (default:
	// "dist"). GoReleaser and mode-B commands should write outputs here.
	ArtifactsDir string `json:"artifacts_dir,omitempty"`

	// Build is the build configuration: exactly one of Goreleaser or
	// Commands must be set.
	Build ReleaseBuildConfig `json:"build"`

	// SBOM enables SBOM generation via anchore/sbom-action during the build.
	SBOM bool `json:"sbom,omitempty"`

	// Provenance enables SLSA provenance via
	// slsa-framework/slsa-github-generator (generic, level 3).
	Provenance bool `json:"provenance,omitempty"`
}

// Built-in defaults applied when a CopyrightConfig field is omitted. An
// unconfigured project attributes generated files to koryph — "(c) 2026 The
// Koryph Developers" — matching koryph's own source-header convention (see
// FileCopyrightText for the "(c)" prefix).
const (
	defaultCopyrightHolder = "The Koryph Developers"
	defaultCopyrightYear   = "2026"
	defaultLicenseID       = "Apache-2.0"
)

// CopyrightConfig configures the SPDX header koryph stamps onto files it
// GENERATES for a project (CI/release workflow assets). Every field is
// optional and falls back to a built-in default (see the constants above), so
// the block is purely additive — set it to make koryph attribute generated
// files to YOUR project instead of "The Koryph Developers".
type CopyrightConfig struct {
	// Holder is the copyright holder, e.g. "Acme, Inc." or "The Foo Authors".
	Holder string `json:"holder,omitempty"`
	// Year is the copyright year or range, e.g. "2026" or "2024-2026".
	Year string `json:"year,omitempty"`
	// License is the SPDX license identifier stamped into generated files
	// (default "Apache-2.0").
	License string `json:"license,omitempty"`
}

// FileCopyrightText returns the SPDX-FileCopyrightText value
// ("(c) <year> <holder>") for a generated file header, applying built-in
// defaults for omitted fields. The "(c)" prefix matches koryph's own
// source-header convention ("Copyright (c) 2026 The Koryph Developers"), so an
// unconfigured project attributes to "(c) 2026 The Koryph Developers". A nil
// receiver yields the full default, so callers may write
// cfg.Copyright.FileCopyrightText() without a nil check.
func (c *CopyrightConfig) FileCopyrightText() string {
	holder, year := defaultCopyrightHolder, defaultCopyrightYear
	if c != nil {
		if c.Holder != "" {
			holder = c.Holder
		}
		if c.Year != "" {
			year = c.Year
		}
	}
	return "(c) " + year + " " + holder
}

// LicenseID returns the SPDX license identifier for a generated file header,
// defaulting to Apache-2.0. Nil-safe like FileCopyrightText.
func (c *CopyrightConfig) LicenseID() string {
	if c != nil && c.License != "" {
		return c.License
	}
	return defaultLicenseID
}

// IntakeSource configures one issue-tracker source in the koryph.project.json
// intake list. Each entry drives one poll per `koryph intake` invocation.
type IntakeSource struct {
	// Provider is the issue-tracker type. Supported values: "github" (default),
	// "jira", "linear". The field defaults to "github" when omitted.
	Provider string `json:"provider,omitempty" jsonschema:"enum=github,enum=jira,enum=linear"`

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
	Limit int `json:"limit,omitempty" jsonschema:"minimum=0"`

	// CommentBack posts the new bead ID back on each ingested issue.
	// Opt-in; mirrors the --comment flag. Default: false.
	CommentBack bool `json:"comment_back,omitempty"`

	// Mapping is reserved for future provider-specific field remapping.
	// Ignored in v1.
	Mapping map[string]string `json:"mapping,omitempty"`
}

// RuntimeConfig is one runtime's per-project override (koryph-v8u.3), keyed
// by runtime name in Config.Runtimes. Deliberately minimal: an entry exists
// today only to (a) let an operator explicitly enable a registered runtime
// for this project, even though it may already be available process-wide,
// and (b) carry a per-runtime sparse tier->model override alongside the
// top-level ModelMap (which today only ever applies to the active/claude
// runtime). Richer per-runtime policy (its own Stages/Tiers/Gate) is future
// work once a second real adapter (koryph-v8u.6) lands and requirements are
// concrete.
type RuntimeConfig struct {
	// Enabled gates whether this project allows dispatching under this
	// runtime at all; a bead or default_runtime naming a disabled runtime is
	// refused the same as an unregistered one. False (the default, also when
	// the whole entry is omitted) requires an explicit per-runtime opt-in —
	// the safer default while only claude is wired end-to-end in the engine.
	Enabled bool `json:"enabled,omitempty"`

	// ModelMap sparsely overrides this runtime's own tier->model table
	// (mirrors Config.ModelMap's shape, scoped to just this runtime).
	ModelMap map[string]string `json:"model_map,omitempty"`
}

// EpicValidationConfig is the optional epic_validation sub-block of
// koryph.project.json. It controls whether and how the engine runs a
// whole-epic implementation review after all children of an epic close.
// When the block is absent, all fields take their documented defaults — call
// EpicValidationConfig.Effective() (or Config.EffectiveEpicValidation()) to
// resolve. See docs/designs/2026-07-epic-validation.md §5.
type EpicValidationConfig struct {
	// Enabled gates the in-loop automatic trigger (default true). When false
	// the engine skips post-epic validation; koryph epic validate still runs on
	// demand regardless. Per-epic opt-out: label no-validate on the epic bead.
	// A nil pointer means "not set" and resolves to the default of true via
	// Effective().
	Enabled *bool `json:"enabled,omitempty"`

	// Model is the tier or concrete model id for the validator agent (default
	// "opus" — frontier tier; judging epic spirit and scheduler-correctness of
	// gap-bead labels requires frontier quality).
	Model string `json:"model,omitempty"`

	// Persona is the agent file (.claude/agents/<persona>) used as the
	// validator (default "koryph-epic-validator").
	Persona string `json:"persona,omitempty"`

	// Effort overrides the reasoning-effort hint passed to the validator agent
	// (default: resolved from the validator persona's own frontmatter `effort:`
	// at the engine call site — koryph-77r.8 — since epic validation is
	// quality-critical and unmeasured by design, this is left empty here so
	// operators opt in explicitly rather than the engine guessing a default).
	Effort string `json:"effort,omitempty"`

	// MaxRounds caps how many validation rounds the engine runs per epic before
	// parking it (default 2). Beyond this cap the epic receives the label
	// validation:parked and waits for operator intervention. Must be >= 1 when
	// explicitly set (non-zero); zero resolves to the default via Effective().
	MaxRounds int `json:"max_rounds,omitempty"`

	// AutoClose, when true (default), has the engine close the epic after the
	// validator returns a "met" verdict. When false the epic stays open for
	// manual review even after a passing validation. A nil pointer means "not
	// set" and resolves to the default of true via Effective().
	AutoClose *bool `json:"auto_close,omitempty"`

	// TimeoutSeconds is the per-run wall-clock timeout for the validator agent
	// (default 420 s). Exceeding it is treated as a Degraded verdict. Zero
	// resolves to the default via Effective().
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// StructuralParent, when non-empty, is the bead ID of the epic (or
	// standalone container bead) under which structural follow-up beads are
	// filed. When empty (default) structural findings stand alone as top-level
	// beads.
	StructuralParent string `json:"structural_parent,omitempty"`

	// DocsUpdate configures the post-validation documentation stage (design
	// §4b): on a met verdict the engine files one docs-update child bead and
	// the epic closes when it merges. Nil resolves to the documented defaults
	// via EffectiveDocsUpdate().
	DocsUpdate *EpicDocsUpdateConfig `json:"docs_update,omitempty"`
}

// EpicDocsUpdateConfig is the docs_update sub-block of epic_validation.
type EpicDocsUpdateConfig struct {
	// Enabled gates the docs-update stage (default true). When false a met
	// verdict closes the epic immediately per auto_close.
	Enabled *bool `json:"enabled,omitempty"`

	// Labels are stamped on the auto-filed docs bead alongside
	// validation:docs (default ["area:docs"]; fp:docs-nav is koryph's own
	// addition — its nav block is a shared write).
	Labels []string `json:"labels,omitempty"`
}

// EffectiveDocsUpdate resolves the docs_update sub-block with defaults; safe
// on a nil receiver and a nil sub-block.
func (c *EpicValidationConfig) EffectiveDocsUpdate() EpicDocsUpdateConfig {
	var out EpicDocsUpdateConfig
	if c != nil && c.DocsUpdate != nil {
		out = *c.DocsUpdate
	}
	if out.Enabled == nil {
		t := true
		out.Enabled = &t
	}
	if len(out.Labels) == 0 {
		out.Labels = []string{"area:docs"}
	}
	return out
}

const (
	defaultEpicValidationModel     = "opus"
	defaultEpicValidationPersona   = "koryph-epic-validator"
	defaultEpicValidationMaxRounds = 2
	defaultEpicValidationTimeout   = 420
)

// Effective returns an EpicValidationConfig with documented defaults applied to
// any zero-value fields. It is safe to call on a nil receiver — the nil
// ("block absent") case returns the full set of defaults.
func (c *EpicValidationConfig) Effective() EpicValidationConfig {
	var out EpicValidationConfig
	if c != nil {
		out = *c
	}
	if out.Enabled == nil {
		t := true
		out.Enabled = &t
	}
	if out.AutoClose == nil {
		t := true
		out.AutoClose = &t
	}
	if out.Model == "" {
		out.Model = defaultEpicValidationModel
	}
	if out.Persona == "" {
		out.Persona = defaultEpicValidationPersona
	}
	if out.MaxRounds < 1 {
		out.MaxRounds = defaultEpicValidationMaxRounds
	}
	if out.TimeoutSeconds < 1 {
		out.TimeoutSeconds = defaultEpicValidationTimeout
	}
	return out
}

// Review-timeout bounds for the post-implementation reviewer. These mirror the
// authoritative constants in internal/review (the runtime enforcer): a drift
// guard in internal/engine asserts ReviewTimeoutHardCapSec stays equal to
// review.MaxTimeoutSec so the config-validation ceiling here and the runtime
// clamp there can never diverge.
const (
	// DefaultReviewTimeoutSec is the starting per-attempt reviewer timeout when
	// review.timeout_seconds is unset (mirrors review.defaultTimeoutSec).
	DefaultReviewTimeoutSec = 600
	// ReviewTimeoutHardCapSec is the hard 20-minute ceiling on any single
	// reviewer attempt. No review block may configure a timeout above it, and
	// the engine may not escalate past it — the "cannot be exceeded for any
	// single task" bound (mirrors review.MaxTimeoutSec).
	ReviewTimeoutHardCapSec = 1200
)

// ReviewConfig is the optional review sub-block of koryph.project.json. It tunes
// the post-implementation reviewer's wall-clock budget per project. The engine
// escalates the timeout toward MaxTimeoutSeconds when a review attempt runs out
// of wall-clock, so a large diff gets progressively more room on retry, bounded
// by the 20-minute hard ceiling (ReviewTimeoutHardCapSec). Nil = block absent;
// use EffectiveReview() to get the resolved values with defaults applied.
type ReviewConfig struct {
	// TimeoutSeconds is the STARTING per-attempt reviewer timeout in seconds
	// (default DefaultReviewTimeoutSec, 600). Zero/absent resolves to the
	// default. Must be <= ReviewTimeoutHardCapSec. The break-glass
	// KORYPH_REVIEW_TIMEOUT_SEC env override still takes precedence over this
	// value (same convention as KORYPH_POLL_SEC over poll_seconds).
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"minimum=0,maximum=1200"`

	// MaxTimeoutSeconds caps how far a timed-out review may escalate (default
	// and hard ceiling ReviewTimeoutHardCapSec, 1200 = 20 min). A project may
	// set a LOWER ceiling; it can never exceed 1200. Zero/absent resolves to
	// the default.
	MaxTimeoutSeconds int `json:"max_timeout_seconds,omitempty" jsonschema:"minimum=0,maximum=1200"`
}

// Effective resolves the review block with documented defaults and the hard
// 20-minute ceiling applied to any zero-value or out-of-range field. Safe on a
// nil receiver (nil = block absent = all defaults). The returned MaxTimeoutSeconds
// is always in (0, ReviewTimeoutHardCapSec] and TimeoutSeconds in (0,
// MaxTimeoutSeconds], so the caller can thread the values straight into
// review.Opts.
func (c *ReviewConfig) Effective() ReviewConfig {
	var out ReviewConfig
	if c != nil {
		out = *c
	}
	if out.MaxTimeoutSeconds <= 0 || out.MaxTimeoutSeconds > ReviewTimeoutHardCapSec {
		out.MaxTimeoutSeconds = ReviewTimeoutHardCapSec
	}
	if out.TimeoutSeconds <= 0 {
		out.TimeoutSeconds = DefaultReviewTimeoutSec
	}
	if out.TimeoutSeconds > out.MaxTimeoutSeconds {
		out.TimeoutSeconds = out.MaxTimeoutSeconds
	}
	return out
}

// PostureConfig is the optional desired-state posture sub-block of
// koryph.project.json. When set, koryph doctor --project reports drift between
// the live GitHub repo and the named profile as WARN, with the exact
// koryph posture apply command to remediate.
//
// `koryph project add` offers to populate this block interactively using the
// default profile (oss-solo-maintainer); --posture <name> / --no-posture
// control non-interactive mode.  A future `koryph new` command (koryph-om7,
// HELD) will populate this block unconditionally on freshly created repos;
// leave the field as the resolution point for that work.
type PostureConfig struct {
	// Profile is the named posture profile, e.g. "oss-solo-maintainer".
	// Must match a built-in profile name (koryph posture list) or a user
	// profile under ~/.koryph/postures/.
	Profile string `json:"profile"`
	// Parameters maps profile parameter names to their values, e.g.
	// {"required_checks": "pre-commit,make gate"}.  Omit or set to {} to
	// use the profile's defaults for all parameters.
	Parameters map[string]string `json:"parameters,omitempty"`
	// Fragments lists the security-scanner fragment names this project has
	// opted into (design §3.3).  Each name must match a built-in fragment
	// (see `koryph posture list --fragments`).  Opted-in fragments are:
	//   - installed into the project by `koryph posture apply`
	//   - drift-checked by `koryph doctor --project`
	// A profile's manifest.json may list recommended_fragments (informational
	// only — listing them here is what opts the project in).
	Fragments []string `json:"fragments,omitempty"`
	// Org, when non-empty, names the GitHub organisation whose org-level
	// rulesets should be drift-checked alongside the repo rulesets by
	// `koryph doctor --project`.  Requires org owner / admin access; the
	// doctor check degrades gracefully when permission is absent.
	Org string `json:"org,omitempty"`
}

// Config is the per-project adapter.
type Config struct {
	SchemaVersion int    `json:"schema_version"`
	ProjectID     string `json:"project_id"`

	// WorkSource is "bd" (beads ready-graph, preferred) or "markdown"
	// (legacy docs/plans phase docs; supported for un-migrated projects).
	WorkSource string `json:"work_source" jsonschema:"enum=bd,enum=markdown"`
	PlansDir   string `json:"plans_dir,omitempty"`

	// Footprint declares conflict domains. AreaMap maps an `area:<x>` bead
	// label to footprint tokens when no fp:* label is present.
	Footprint []FootprintRule     `json:"footprint,omitempty"`
	AreaMap   map[string][]string `json:"area_map,omitempty"`

	// Resources is the portable vocabulary of external runtime resource kinds a
	// bead may declare with a `res:<kind>` label (koryph-4ql.3, design
	// docs/designs/2026-07-resource-governor.md L2): kind → its checked-in
	// planning-time cost. It mirrors AreaMap's "portable label vocabulary" role
	// for footprints — footprints protect the merge, resources protect the
	// machine — while the per-machine capacity/override (and the ramp/probe)
	// live in governor.json (the top-level `resources` ledger), which wins over
	// this vocabulary. The engine sums each declared kind's mem_mb into a
	// reservation-aware memory floor at dispatch. Additive/omitempty: an absent
	// key is a nil map (no vocabulary), which is byte-for-byte today's behavior
	// (no `res:*` labels = no reservations).
	Resources map[string]ResourceSpec `json:"resources,omitempty"`

	// Gate is the ordered green-gate command list run in the worktree after
	// rebase and before merge (each entry runs via `sh -c` under direnv when
	// available).
	Gate []string `json:"gate"`

	// Stages maps pipeline stage -> persona name in .claude/agents.
	// Tiers maps model tier -> persona for tier-driven dispatch.
	Stages map[string]string `json:"stages,omitempty"`
	Tiers  map[string]string `json:"tiers,omitempty"`

	// ModelMap overrides the active runtime's tier -> concrete-model-id map
	// (koryph-v8u.10). Keys are the runtime-agnostic tier vocabulary a
	// persona's `tier:` frontmatter carries ("frontier", "standard",
	// "light" — see agents/README.md's frontmatter contract); values are
	// concrete model ids for whichever runtime is active (today, always
	// Claude: "opus"/"sonnet"/"haiku", or "fable" as an explicit frontier
	// override). Sparse: only the tiers an operator wants to re-map need be
	// present, e.g. {"frontier": "fable"} — every other tier keeps
	// runtime.ClaudeModelMap's default. A project-config host (rather than a
	// registry record) was chosen because AllowedModels/Tiers/Stages already
	// carry per-project model policy right here, and because this value is
	// read on every dispatch (modelroute.Resolve), the same hot path as
	// those fields — see internal/modelroute/route.go's effectiveModelMap
	// for how it overlays onto the runtime default.
	ModelMap map[string]string `json:"model_map,omitempty"`

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

	// MergeReconcilers auto-heal a pre-merge rebase conflict confined ENTIRELY
	// to known generated / derived files (a migrations lockfile, a secrets
	// baseline) by regenerating each from the post-merge tree and continuing,
	// instead of aborting and requeueing (design
	// docs/designs/2026-07-merge-reconcilers.md). A conflict touching any path
	// with no reconciler always aborts unchanged. Additive/omitempty: empty =
	// today's behavior. Footprints protect the merge; reconcilers heal the
	// derivatives.
	MergeReconcilers []MergeReconciler `json:"merge_reconcilers,omitempty"`

	// MergePrepare are commands run in the worktree AFTER the (possibly
	// reconciler-healed) rebase and BEFORE the gate — the merge-time
	// normalization seam (design docs/designs/2026-07-merge-reconcilers.md L6).
	// Their canonical use is renumbering a newly added migration to the next
	// free sequence against the branch it is landing on (KORYPH_DEFAULT_BRANCH
	// in the env), so two in-flight beads never land a duplicate number and no
	// renumber cascade is possible. koryph commits any change so it rides the
	// ff-merge and is gated; a command regression is a gate-shaped requeue.
	// Additive/omitempty: empty = no normalization (today's behavior).
	MergePrepare []string `json:"merge_prepare,omitempty"`

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
	CommitStyle    string `json:"commit_style,omitempty" jsonschema:"enum=conventional,enum=custom,enum=none"`
	CommitTemplate string `json:"commit_template,omitempty"`

	// MergePolicy default when the epic carries no merge:* label.
	MergePolicy Policy `json:"merge_policy" jsonschema:"enum=manual,enum=auto,enum=pr"`

	// MergeMethod is how an engine-opened PR lands on the default branch:
	// "ff" (default, also when empty) preserves the exact gate-checked, signed
	// commit SHAs via a local fast-forward + push; "squash" collapses them into
	// one new commit. A non-ff method is refused while signing is required
	// (only ff preserves signatures). GitHub-native merge methods are never
	// used — they rewrite SHAs or add an unsigned merge commit.
	MergeMethod string `json:"merge_method,omitempty" jsonschema:"enum=ff,enum=squash"`

	// RiskTierDefault is the recovery tier (0-3) for beads without rt:*.
	RiskTierDefault int `json:"risk_tier_default" jsonschema:"minimum=0,maximum=3"`

	// Vault sets the project-level default vault provider and container
	// (provider-native grouping: Proton Pass vault name, 1Password vault, file
	// directory, etc.). Commands that store or fetch secrets use this block when
	// no explicit flags are supplied. Falls back to the global
	// ~/.koryph/config.json vault block when absent.
	// Managed by `koryph signing setup` (sets provider/container on first run).
	Vault *signing.VaultDefaults `json:"vault,omitempty"`

	// Signing is the vault-backed commit/artifact signing policy
	// (nil = signing not configured; managed by `koryph signing setup`).
	Signing *signing.Config `json:"signing,omitempty"`

	// RequireCalibration hard-blocks dispatch while the quota governor is
	// uncalibrated (both ceilings 0), instead of the default advisory pass
	// (koryph-grz). Opt-in spend safety for this project: runs refuse to
	// dispatch until `koryph quota calibrate` sets a ceiling. The
	// `--require-calibration` run flag also sets it for a single run.
	RequireCalibration bool `json:"require_calibration,omitempty"`

	// AdaptiveEscalation gates the learned-model auto-apply (koryph-qf6.6):
	// when enabled, the engine's wave boundary mines run-ledger escalation
	// history (internal/modellearn) and stamps `model:<tier>` +
	// `model-learned:<date>` labels onto matching ready beads, so beads
	// similar to ones that only merged after escalating start on the stronger
	// tier first-pass. nil/absent = off (today's behavior); `koryph models
	// learn` offers the same pass manually regardless of this gate.
	AdaptiveEscalation *AdaptiveEscalation `json:"adaptive_escalation,omitempty"`

	// MaxConcurrentSlots caps wave width for this project (default 3).
	MaxConcurrentSlots int `json:"max_concurrent_slots,omitempty"`

	// DispatchStaggerSeconds between agent launches (default 8).
	DispatchStaggerSeconds int `json:"dispatch_stagger_seconds,omitempty"`

	// PollSeconds overrides the engine's slot poll tick for this project
	// (default 10 when zero/omitted). KORYPH_POLL_SEC and an explicit
	// programmatic Options.PollSec caller override still take precedence over
	// this value (see engine.runner.pollInterval; koryph-2im.2).
	PollSeconds int `json:"poll_seconds,omitempty" jsonschema:"minimum=0"`

	// HealthIntervalSeconds sets how often the engine's in-loop health patrol
	// fires for this project (default 600 = 10 minutes when zero/omitted).
	// KORYPH_HEALTH_INTERVAL_SEC env and Options.HealthIntervalSec take
	// precedence (koryph-gus).
	HealthIntervalSeconds int `json:"health_interval_seconds,omitempty" jsonschema:"minimum=0"`

	// StaleClaimWarnHours is the age (in hours, by bd's updated_at) an
	// in_progress bead must exceed, with no live agent PID found in any
	// recent run, before the health patrol's stale-claims check warns about
	// it (default 24 when zero/omitted). KORYPH_STALE_CLAIM_WARN_HOURS env
	// takes precedence. Report-only: never auto-resets the bead, since some
	// in_progress beads are correctly parked on a blocker bd cannot
	// represent (an operator action, unscoped future work) — see
	// patrolCheckStaleClaims's doc comment (2026-07-15/16 stampede-games
	// handoff).
	StaleClaimWarnHours int `json:"stale_claim_warn_hours,omitempty" jsonschema:"minimum=0"`

	// DispatchMode selects the engine's dispatch loop (koryph-2im.3,
	// docs/designs/2026-07-scheduler-throughput.md L1): "wave" (also
	// when empty) dispatches a fixed-width batch and blocks until every slot
	// in it lands before scanning the frontier again; "rolling" continuously
	// refills any slot that frees up without waiting for the rest of the
	// batch. A run's --dispatch-mode flag overrides this value; --once runs
	// today's wave semantics in both modes.
	DispatchMode string `json:"dispatch_mode,omitempty" jsonschema:"enum=wave,enum=rolling"`

	// DefaultRuntime selects the runtime (internal/runtime.Runtime) a bead
	// dispatches under when it carries no `runtime:<name>` label
	// (koryph-v8u.3). Empty means "claude" — today's only runtime the engine
	// actually dispatches through; internal/engine's dispatchBead blocks
	// (rather than silently substituting claude) any bead or default that
	// resolves to anything else. Must be "", "claude", or a name registered
	// in runtime.Default (enforced by Validate).
	DefaultRuntime string `json:"default_runtime,omitempty"`

	// Runtimes configures per-runtime settings for this project, keyed by
	// runtime name — the same string a bead's `runtime:<name>` label and
	// DefaultRuntime use, and runtime.Runtime.Name()'s value. See
	// RuntimeConfig's doc for how minimal this is today.
	Runtimes map[string]RuntimeConfig `json:"runtimes,omitempty"`

	// Release, when non-nil, configures the project's release pipeline
	// (managed by `koryph release setup`). It drives template rendering for
	// the caller GitHub Actions workflow and the release-please config.
	// Exactly one build mode (Build.Goreleaser or Build.Commands) must be
	// set when this block is present (enforced by Validate).
	Release *ReleaseConfig `json:"release,omitempty"`

	// Copyright configures the SPDX header koryph stamps onto files it
	// GENERATES for this project — the CI/release workflow assets written by
	// `koryph ci setup` / `koryph release setup` (koryph-s6g). It is
	// per-project so a repository koryph builds carries ITS OWN copyright and
	// license, not koryph's. Nil (and any omitted field) falls back to
	// koryph's built-in default, so an unconfigured project's generated output
	// is byte-for-byte unchanged.
	Copyright *CopyrightConfig `json:"copyright,omitempty"`

	// Posture, when non-nil, declares the desired-state posture profile for
	// this project's GitHub repository. koryph doctor --project reports any
	// drift between the live repo and the named profile as WARN, with the
	// exact koryph posture apply command to remediate.
	//
	// Managed by `koryph project add` (interactive offer) and the future
	// `koryph new` command (koryph-om7, HELD).  Nil means no profile is
	// declared and the drift check is silently skipped.
	Posture *PostureConfig `json:"posture,omitempty"`

	// Forge names the git forge provider for this project. Supported values:
	// "github" (default, also when empty — full back-compat; existing projects
	// without this field continue to behave as GitHub projects) and "gitlab".
	// Remote-URL sniffing may suggest a value during `koryph onboard`, but the
	// operator always makes the final selection.
	//
	// This field is the single source of truth for which internal/forge
	// provider is active; the registry record inherits it at
	// onboard/add time.
	Forge string `json:"forge,omitempty" jsonschema:"enum=github,enum=gitlab"`

	// EpicValidation configures the whole-epic implementation review that fires
	// once all children of an epic are closed (koryph-wo0.2,
	// docs/designs/2026-07-epic-validation.md §5). Nil = absent; use
	// EffectiveEpicValidation() to get the resolved values with defaults.
	EpicValidation *EpicValidationConfig `json:"epic_validation,omitempty"`

	// Review tunes the post-implementation reviewer's per-attempt wall-clock
	// budget for this project (starting timeout + escalation ceiling, bounded by
	// the 20-minute hard cap). Nil = absent; use EffectiveReview() to get the
	// resolved values with defaults.
	Review *ReviewConfig `json:"review,omitempty"`
}

// Default returns a conservative baseline config.
func Default(projectID string) *Config {
	return &Config{
		SchemaVersion:          schemaver.Current(schemaver.Project),
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
	if err := schemaver.CheckRead(schemaver.Project, c.SchemaVersion); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
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
	switch c.DispatchMode {
	case "", "wave", "rolling":
	default:
		return fmt.Errorf("dispatch_mode must be wave|rolling, got %q", c.DispatchMode)
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
	if c.Vault != nil {
		if err := c.Vault.Validate(); err != nil {
			return fmt.Errorf("vault: %w", err)
		}
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
	if err := validateDefaultRuntime(c.DefaultRuntime); err != nil {
		return err
	}
	if err := validateRelease(c.Release); err != nil {
		return err
	}
	if err := validatePosture(c.Posture); err != nil {
		return err
	}
	if err := validateForge(c.Forge); err != nil {
		return err
	}
	if err := validateEpicValidation(c.EpicValidation); err != nil {
		return err
	}
	if err := validateReview(c.Review); err != nil {
		return err
	}
	if err := validateMergeReconcilers(c.MergeReconcilers); err != nil {
		return err
	}
	for i, cmd := range c.MergePrepare {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("merge_prepare[%d]: command cannot be empty", i)
		}
	}
	return nil
}

// validateMergeReconcilers enforces that each generated-file reconciler names a
// non-empty path with a legal path.Match pattern and a non-empty command —
// rejected at config load rather than discovered mid-merge.
func validateMergeReconcilers(recs []MergeReconciler) error {
	for i, r := range recs {
		if strings.TrimSpace(r.Path) == "" {
			return fmt.Errorf("merge_reconcilers[%d]: path required", i)
		}
		if _, err := path.Match(r.Path, ""); err != nil {
			return fmt.Errorf("merge_reconcilers[%d]: invalid path glob %q: %w", i, r.Path, err)
		}
		if strings.TrimSpace(r.Command) == "" {
			return fmt.Errorf("merge_reconcilers[%d] (%s): command required", i, r.Path)
		}
	}
	return nil
}

// validateDefaultRuntime enforces koryph-v8u.3's default_runtime contract:
// empty and "claude" are always valid without a registry lookup (claude
// self-registers into runtime.Default at process start via
// internal/runtime/claude's init side effect, but Validate must not depend on
// that import having happened — e.g. a bare `koryph validate` binary build —
// so "claude" is special-cased here exactly as it is in
// modelroute.Resolve/ResolveRuntimeName); anything else must be a name
// actually registered in runtime.Default. Fail closed: a project must never
// be able to point default_runtime at a runtime dispatch cannot possibly
// select.
func validateDefaultRuntime(name string) error {
	if name == "" || name == "claude" {
		return nil
	}
	if _, ok := runtime.Default.Get(name); !ok {
		return fmt.Errorf(
			"default_runtime %q is not a registered runtime (want \"claude\", empty, or a name registered in runtime.Default)", name)
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

// validateRelease enforces the release block contract when non-nil:
//   - Type must be non-empty.
//   - Exactly one of Build.Goreleaser or Build.Commands must be set.
func validateRelease(r *ReleaseConfig) error {
	if r == nil {
		return nil
	}
	if strings.TrimSpace(r.Type) == "" {
		return fmt.Errorf("release.type is required")
	}
	hasGoreleaser := r.Build.Goreleaser != nil
	hasCommands := len(r.Build.Commands) > 0
	switch {
	case hasGoreleaser && hasCommands:
		return fmt.Errorf("release.build: only one build mode may be set (goreleaser or commands, not both)")
	case !hasGoreleaser && !hasCommands:
		return fmt.Errorf("release.build: exactly one build mode is required (goreleaser or commands)")
	}
	return nil
}

// PostureApplyCmd returns the exact shell command that would bring the live
// GitHub repo into conformance with the posture block, for use in doctor
// WARN messages. Returns an empty string when PostureConfig is nil.
func (p *PostureConfig) PostureApplyCmd() string {
	if p == nil {
		return ""
	}
	cmd := "koryph posture apply " + p.Profile
	for k, v := range p.Parameters {
		cmd += " --param " + k + "=" + v
	}
	if p.Org != "" {
		cmd += " --org " + p.Org
	}
	return cmd
}

// OrgPostureApplyCmd returns the exact shell command that would bring the live
// org-level rulesets into conformance with the posture block.  Returns an
// empty string when PostureConfig is nil or Org is not set.
func (p *PostureConfig) OrgPostureApplyCmd() string {
	if p == nil || p.Org == "" {
		return ""
	}
	cmd := "koryph posture apply " + p.Profile + " --org " + p.Org
	for k, v := range p.Parameters {
		cmd += " --param " + k + "=" + v
	}
	return cmd
}

// validatePosture enforces the posture block contract when non-nil:
// Profile must be non-empty (a specific profile name is required; the drift
// check cannot operate without one).
func validatePosture(p *PostureConfig) error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(p.Profile) == "" {
		return fmt.Errorf("posture.profile is required when posture block is present")
	}
	return nil
}

// validateForge enforces the forge field contract: empty means "github" (full
// back-compat); any non-empty value must be a recognised provider name.
func validateForge(name string) error {
	switch name {
	case "", "github", "gitlab":
		return nil
	default:
		return fmt.Errorf("forge must be github|gitlab (or empty for default github), got %q", name)
	}
}

// ResolvedForge returns the effective forge provider name for the config:
// the Forge field when non-empty, otherwise the default "github".
func (c *Config) ResolvedForge() string {
	if c.Forge != "" {
		return c.Forge
	}
	return "github"
}

// EffectiveEpicValidation returns the resolved EpicValidationConfig for this
// project, applying documented defaults to any zero-value or absent fields. It
// delegates to EpicValidationConfig.Effective(), which is safe to call on a nil
// receiver (nil = block absent = all defaults).
func (c *Config) EffectiveEpicValidation() EpicValidationConfig {
	return c.EpicValidation.Effective()
}

// validateEpicValidation enforces the epic_validation block contract: when the
// block is present, max_rounds must be >= 1 if explicitly set (non-zero).
// Zero (absent/omitted) is valid — Effective() resolves it to the default of 2.
func validateEpicValidation(c *EpicValidationConfig) error {
	if c == nil {
		return nil
	}
	if c.MaxRounds != 0 && c.MaxRounds < 1 {
		return fmt.Errorf("epic_validation.max_rounds must be >= 1, got %d", c.MaxRounds)
	}
	return nil
}

// EffectiveReview returns the resolved ReviewConfig for this project, applying
// documented defaults and the 20-minute hard ceiling to any zero-value or
// absent field. It delegates to ReviewConfig.Effective(), which is safe to call
// on a nil receiver (nil = block absent = all defaults).
func (c *Config) EffectiveReview() ReviewConfig {
	return c.Review.Effective()
}

// validateReview enforces the review block contract: timeouts are non-negative,
// neither exceeds the 20-minute per-task hard cap, and the starting timeout does
// not exceed the escalation ceiling. Zero (absent/omitted) is valid —
// Effective() resolves it to the defaults. Rejecting an over-cap value here
// gives a clear load-time error instead of a silent runtime clamp; the review
// package still clamps at spawn time as defense in depth.
func validateReview(c *ReviewConfig) error {
	if c == nil {
		return nil
	}
	if c.TimeoutSeconds < 0 {
		return fmt.Errorf("review.timeout_seconds must be >= 0, got %d", c.TimeoutSeconds)
	}
	if c.MaxTimeoutSeconds < 0 {
		return fmt.Errorf("review.max_timeout_seconds must be >= 0, got %d", c.MaxTimeoutSeconds)
	}
	if c.TimeoutSeconds > ReviewTimeoutHardCapSec {
		return fmt.Errorf("review.timeout_seconds must be <= %d (the %d-minute per-task hard ceiling), got %d",
			ReviewTimeoutHardCapSec, ReviewTimeoutHardCapSec/60, c.TimeoutSeconds)
	}
	if c.MaxTimeoutSeconds > ReviewTimeoutHardCapSec {
		return fmt.Errorf("review.max_timeout_seconds must be <= %d (the %d-minute per-task hard ceiling), got %d",
			ReviewTimeoutHardCapSec, ReviewTimeoutHardCapSec/60, c.MaxTimeoutSeconds)
	}
	if c.MaxTimeoutSeconds > 0 && c.TimeoutSeconds > c.MaxTimeoutSeconds {
		return fmt.Errorf("review.timeout_seconds (%d) must be <= review.max_timeout_seconds (%d)",
			c.TimeoutSeconds, c.MaxTimeoutSeconds)
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
