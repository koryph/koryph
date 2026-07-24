// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package epicreview

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/agentjson"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/claude"
)

// Defaults per the package contract (§3 of 2026-07-epic-validation.md).
const (
	defaultPersona    = "koryph-epic-validator"
	defaultModel      = "opus"
	defaultClaudeBin  = "claude"
	defaultTimeoutSec = 420
	defaultAttempts   = 3
)

// Exponential backoff between validator attempts: the nth retry waits
// backoffUnit * 2^(n-1), capped at maxBackoff. Validator failures are dominated
// by API rate/usage limits; each retry backs off progressively. backoffUnit and
// maxBackoff are package vars so tests can shrink them; production keeps the
// real delays.
var (
	backoffUnit = 2 * time.Second
	maxBackoff  = 30 * time.Second
)

// backoffFor returns the exponential wait before the given retry (1-based):
// backoffUnit * 2^(retry-1), capped at maxBackoff. The cap also absorbs shift
// overflow for large retry counts.
func backoffFor(retry int) time.Duration {
	if retry < 1 {
		return 0
	}
	d := backoffUnit << (retry - 1)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

// outPath returns the default verdict file path for the given epic and round.
func outPath(repoRoot, outDir, epicID string, round int) string {
	dir := outDir
	if dir == "" {
		dir = filepath.Join(repoRoot, ".koryph", "epic-reviews")
	}
	name := fmt.Sprintf("%s-round%d.json", epicID, round)
	return filepath.Join(dir, name)
}

// Validate runs the epic-scoped validation pass for the epic described by o.
// It runs on main (o.RepoRoot, no worktree) since all children are already
// merged. A transient validator failure is retried up to o.Attempts times with
// exponential backoff; only when every attempt fails does it return
// Verdict{Degraded:true} carrying a Reason. It never panics the loop, but it
// never silently passes either — the caller decides policy.
func Validate(ctx context.Context, o Opts) Verdict {
	if o.Persona == "" {
		o.Persona = defaultPersona
	}
	if o.Model == "" {
		o.Model = defaultModel
	}
	if o.ClaudeBin == "" {
		o.ClaudeBin = defaultClaudeBin
	}
	if o.TimeoutSec <= 0 {
		o.TimeoutSec = defaultTimeoutSec
	}
	attempts := o.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	round := o.Round
	if round <= 0 {
		round = 1
	}

	prompt := buildPrompt(o)

	var last Verdict
	for i := 0; i < attempts; i++ {
		if i == 0 {
			// Pre-spawn launch line: tells the operator that a potentially
			// long-running frontier agent was just dispatched so the stream
			// does not look hung for the duration of TimeoutSec.
			if o.Progress != nil {
				o.Progress("epic validate: %s  round %d  model %s  persona %s  %d children  timeout %ds",
					o.EpicID, round, o.Model, o.Persona, len(o.Children), o.TimeoutSec)
			}
		} else {
			// Exponential backoff so a rate or usage limit — the dominant
			// transient validator failure — is given progressively more time
			// to clear instead of being hammered.
			select {
			case <-ctx.Done():
				last = degradedReason("context cancelled during validation retry")
				last.Attempts = i
				return last
			case <-time.After(backoffFor(i)):
			}
			if o.Progress != nil {
				o.Progress("epic validate: attempt %d/%d  reason: %s", i+1, attempts, last.Reason)
			}
		}
		v := attemptValidate(ctx, o, prompt)
		v.Attempts = i + 1
		if !v.Degraded {
			dest := outPath(o.RepoRoot, o.OutDir, o.EpicID, round)
			// Persist the raw Claude envelope beside the parsed verdict
			// (same pattern as stage-*.json, koryph-qbc) so usage/cost data
			// is available for audit. Best-effort: a write failure here is
			// non-fatal (we still have the parsed verdict).
			envelopeDest := strings.TrimSuffix(dest, ".json") + "-envelope.json"
			_ = fsx.WriteAtomic(envelopeDest, []byte(v.Envelope+"\n"), 0o644)
			if err := fsx.WriteAtomic(dest, []byte(v.Raw+"\n"), 0o644); err != nil {
				v = degradedReason("persist verdict JSON failed: " + err.Error())
				v.Attempts = i + 1
				return v
			}
			return v
		}
		last = v
	}
	return last
}

// attemptValidate runs one validator spawn + parse. On any failure it returns a
// degraded verdict whose Reason explains the failure so a degradation is never
// a black box.
func attemptValidate(ctx context.Context, o Opts, prompt string) Verdict {
	// Route the one-shot JSON validator spawn through the resolved Runtime seam
	// (koryph-fiv finding #1): read-only `--permission-mode plan`, no
	// fallback/max-budget, matching the pre-seam argv exactly.
	rt := o.Runtime
	if rt == nil {
		rt = claude.New(o.ClaudeBin)
	}
	spec := runtime.JSONSpec{
		RepoRoot:       o.RepoRoot,
		Persona:        o.Persona,
		Model:          o.Model,
		Effort:         o.Effort,
		PermissionMode: "plan",
		SpawnKind:      "epicreview",
		Profile:        runtime.Profile{Name: o.Profile.Name, ConfigDir: o.Profile.ConfigDir},
		Billing:        runtime.BillingSubscription,
		ProxyBaseURL:   o.ProxyBaseURL,
	}
	res, err := runtime.SpawnJSON(ctx, rt, spec, runtime.JSONExec{
		Dir:     o.RepoRoot,
		Stdin:   prompt,
		Timeout: time.Duration(o.TimeoutSec) * time.Second,
	})
	if err != nil {
		return degradedReason("validator spawn error: " + err.Error())
	}
	if res.ExitCode != 0 {
		// A signal kill from the context deadline surfaces as ExitCode -1 with
		// empty stderr (execx.Run, koryph-a59) — indistinguishable from a crash
		// unless TimedOut is consulted explicitly (koryph-hwlw). Name the
		// timeout so the degraded reason is never an empty "-1".
		if res.TimedOut {
			return degradedReason(fmt.Sprintf(
				"validator timed out after %ds (timeout_seconds=%d; large epics commonly need more)",
				o.TimeoutSec, o.TimeoutSec))
		}
		return degradedReason(fmt.Sprintf("validator exit %d: %s", res.ExitCode, strings.TrimSpace(agentjson.Tail(res.Stderr, 300))))
	}

	// The CLI emits a result envelope; its "result" field holds the model text,
	// which should itself be strict JSON. Extract the verdict schema-aware
	// (requiring the "met" key) so a stray brace token quoted from the design or
	// diff is never mistaken for the verdict.
	out := strings.TrimSpace(res.Stdout)
	raw, err := agentjson.ParseEnvelopeVerdict(out, "met")
	if err != nil {
		return degradedReason("validator " + err.Error())
	}

	var v Verdict
	if json.Unmarshal([]byte(raw), &v) != nil {
		return degradedReason("verdict JSON invalid: " + strings.TrimSpace(agentjson.Tail(raw, 300)))
	}
	v.Degraded = false
	v.Raw = raw
	// Capture the full Claude envelope so Validate can persist it for audit/metrics
	// beside the parsed verdict (koryph-qbc). res.Stdout is the raw --output-format
	// json output including usage and cost fields.
	v.Envelope = res.Stdout
	return v
}

// degradedReason builds a non-blocking, degraded verdict with a human-readable
// explanation. Blocking stays absent (epic validation does not use Blocking);
// the engine treats a degraded validation as a park trigger via policy.
func degradedReason(reason string) Verdict {
	return Verdict{Degraded: true, Reason: reason}
}

// buildPrompt renders the validator prompt: epic context, design doc, children,
// prior verdicts, and the strict-JSON response contract.
//
// The prompt drives two lenses (§3):
//  1. Completeness — did the union meet the epic's description and design doc,
//     in letter and spirit? Misses become gap follow-ups.
//  2. Structural health — duplicate helpers, copy-adapted blocks, library-shaped
//     code stranded in leaf packages, architecture/dependency violations.
func buildPrompt(o Opts) string {
	var b strings.Builder

	b.WriteString("# Epic validation\n\n")
	b.WriteString("You are reviewing the complete implementation of epic **")
	b.WriteString(o.EpicID)
	b.WriteString("** now that all child beads have merged to main.\n\n")

	// Epic metadata.
	b.WriteString("## Epic\n\n")
	b.WriteString("**ID:** ")
	b.WriteString(o.EpicID)
	b.WriteString("\n**Title:** ")
	b.WriteString(o.EpicTitle)
	b.WriteString("\n")
	if o.EpicDescription != "" {
		b.WriteString("\n### Description\n\n")
		b.WriteString(strings.TrimSpace(o.EpicDescription))
		b.WriteString("\n")
	}
	if o.EpicNotes != "" {
		b.WriteString("\n### Notes\n\n")
		b.WriteString(strings.TrimSpace(o.EpicNotes))
		b.WriteString("\n")
	}

	// Design doc.
	if o.DesignDocPath != "" {
		b.WriteString("\n## Design document\n\n")
		b.WriteString("The design document for this epic is at `")
		b.WriteString(o.DesignDocPath)
		b.WriteString("` in the repository. Read it now — the completeness lens judges the implementation against the goals and sections of this document.\n")
	}

	// Children.
	b.WriteString("\n## Completed child beads\n\n")
	if len(o.Children) == 0 {
		b.WriteString("(no children)\n")
	} else {
		for i, c := range o.Children {
			fmt.Fprintf(&b, "### Child %d: %s\n", i+1, c.ID)
			b.WriteString("**Title:** ")
			b.WriteString(c.Title)
			b.WriteString("\n")
			if c.MergeSHA != "" {
				b.WriteString("**Merge SHA:** ")
				b.WriteString(c.MergeSHA)
				b.WriteString("\n")
			}
			if c.CloseReason != "" {
				b.WriteString("**Close reason:** ")
				b.WriteString(c.CloseReason)
				b.WriteString("\n")
			}
			if len(c.Labels) > 0 {
				b.WriteString("**Labels:** ")
				b.WriteString(strings.Join(c.Labels, ", "))
				b.WriteString("\n")
			}
			if c.Description != "" {
				b.WriteString("\n")
				b.WriteString(strings.TrimSpace(c.Description))
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}

	// Prior verdicts (round context).
	if len(o.PriorVerdicts) > 0 {
		b.WriteString("## Prior validation verdicts\n\n")
		for i, pv := range o.PriorVerdicts {
			fmt.Fprintf(&b, "### Round %d verdict\n\n```json\n", i+1)
			b.WriteString(strings.TrimSpace(pv))
			b.WriteString("\n```\n\n")
		}
	}

	// Instructions for both lenses.
	round := o.Round
	if round <= 0 {
		round = 1
	}
	fmt.Fprintf(&b, "## Validation task (round %d)\n\n", round)
	b.WriteString(`Apply BOTH lenses to the work now on main:

### Lens 1 — Completeness
- Re-read the epic description and the design document.
- Check the union of the children's implementations against every goal and section of the design doc.
- Look for integration gaps: places where child A writes something child B never reads, or vice versa.
- Look for design drift: the design promised X; the sum of the children delivered a narrower X′.
- Look for spirit misses: acceptance criteria technically met while the motivating problem ("why" at the top of the design doc) is still reproducible.
- Each unmet goal or hole becomes a gap entry.

### Lens 2 — Structural health
Read the diff of each child's merge commit (use the merge SHAs above) and specifically hunt for:
- **Duplicate helpers** — nearly identical functions or types defined independently in two or more child packages.
- **Copy-adapted blocks** — code that was clearly copied from one child and minimally modified for another, which should be shared.
- **Library-shaped code stranded in leaf packages** — utilities or abstractions that belong in a shared internal package (e.g. internal/libs or a new package) but landed inside a leaf package where they cannot be reused.
- **Dependency-direction violations** — imports that violate the layer rules in docs/architecture.md (read that file).

Each structural finding is an improvement surfaced by the epic, NOT a failing of the epic: they do not affect the "met" field.

`)

	// JSON contract.
	b.WriteString(`## Response format

Respond with STRICT JSON only — no prose, no markdown fences — in exactly this shape:

{
  "met": <bool>,
  "summary": "<one paragraph: what the epic set out to do and what landed>",
  "gaps": [
    {
      "title": "<short title>",
      "why": "<which design goal or section is unmet and how>",
      "acceptance": "<what done looks like>",
      "type": "task|bug|chore",
      "labels": ["area:…", "fp:read:…"],
      "depends_on": ["<sibling gap 0-based index or existing bead id>"]
    }
  ],
  "structural": [
    {
      "category": "extract-common|architecture|duplication",
      "title": "<short title>",
      "why": "<what exists twice / what belongs in a shared package / which rule is violated, with file paths>",
      "acceptance": "<what done looks like>",
      "type": "chore|task",
      "labels": ["area:…"]
    }
  ]
}

Rules:
- Set "met" to true only when every completeness goal in the design doc is satisfied.
- "gaps" drives "met": even one gap entry means met=false.
- "structural" findings never affect "met".
- An empty "gaps" array with met=true means the epic is complete.
- Omit empty arrays.
- depends_on entries are 0-based indexes into the gaps array of this verdict, or existing bead IDs for beads already in the tracker.
`)

	return b.String()
}
