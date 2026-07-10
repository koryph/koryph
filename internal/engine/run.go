// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/epicreview"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/resmon"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/claude"
	"github.com/koryph/koryph/internal/signing"
	"github.com/koryph/koryph/internal/sysmem"
	"github.com/koryph/koryph/internal/version"
)

// resolvedRuntimeName is the runtime every project's engine run resolves to
// today (koryph-v8u.5): registry.Record.AccountFor and dispatch.CLIBackend's
// default both take "claude" from this single constant rather than
// duplicating the literal at each call site. Real per-project/per-bead
// runtime SELECTION (bead `runtime:<name>` label, project
// default_runtime/runtimes block) is koryph-v8u.3's job — until it lands,
// every project has exactly one implicit runtime, so this is not yet a
// meaningful choice.
const resolvedRuntimeName = "claude"

// Environment overrides (primarily for tests; production leaves them unset).
const (
	envClaudeBin  = "KORYPH_CLAUDE_BIN"  // claude binary for dispatch + review
	envBDBin      = "KORYPH_BD_BIN"      // bd binary for the beads adapter
	envPollSec    = "KORYPH_POLL_SEC"    // poll tick override (seconds)
	envStaggerSec = "KORYPH_STAGGER_SEC" // dispatch stagger override (seconds)
	envBackoffSec = "KORYPH_BACKOFF_SEC" // requeue backoff base override (seconds)
	// envResmon="off" disables per-slot resource sampling. Tests set it so the
	// poll loop never forks `ps` / scans /proc — sampling side effects must not
	// perturb the timing-sensitive wave/pacing integration tests.
	envResmon = "KORYPH_RESMON"
)

// Engine defaults.
const (
	defaultPollSec    = 10 // was 45; L3 fast-completion-detection (koryph-2im.2)
	defaultStuckSec   = 900
	defaultWaveWidth  = 3
	defaultBackoffSec = 15
)

// runner carries the state of one engine run.
type runner struct {
	opts     Options
	reg      *registry.Store
	rec      *registry.Record
	cfg      *project.Config
	adapter  WorkSource // bd by default; interface so the loop is testable without bd
	beadsDir string     // .beads path exported to agents (BEADS_DIR)
	store    *ledger.Store
	run      *ledger.Run
	profile  account.Profile
	// expectedIdentity is resolved via registry.Record.AccountFor(resolvedRuntimeName)
	// (koryph-v8u.5) rather than read as r.rec.ExpectedIdentity at each call
	// site — identical value for every project today (AccountFor's flat-field
	// fallback), but real once a project opts into runtime_accounts.
	expectedIdentity string
	// rt is the resolved runtime.Runtime adapter for this run (koryph-v8u.5):
	// identity verification and quota-governor capability gating both go
	// through it. Always "claude" today — see resolvedRuntimeName's doc for
	// why real selection is out of scope here.
	rt       runtime.Runtime
	backend  dispatch.Backend
	quotaCfg *quota.Config
	gov      *govern.Store
	owner    string
	width    int

	dispatched         int
	govWarned          bool
	uncalibratedWarned bool // koryph-grz: loud uncalibrated-governor warning fired once this run
	issues             map[string]beads.Issue

	// memProbe reads current system memory (total + available) for the memory
	// admission gate (koryph-930). nil means "use the real platform probe"
	// (sysmem.Available); tests inject a stub. ok=false signals no usable
	// reading, on which the gate fails open. Total is needed to auto-size the
	// default floor to physical memory.
	memProbe func() (sysmem.Stat, bool)

	// resProbe takes the per-poll-pass process-table snapshot for per-slot
	// resource sampling (koryph process-metrics). nil means "use the real
	// platform probe" (resmon.Snapshot); tests inject a stub — returning
	// (nil, nil) to disable sampling, or a fixed table to assert the derived
	// ledger fields. Mirrors memProbe's seam.
	resProbe func(context.Context) (*resmon.ProcTable, error)

	// Health patrol state (koryph-gus).
	lastPatrolAt   time.Time
	patrolSeen     map[string]time.Time // finding key → last logged; throttles repeat findings
	lastQuotaLevel quota.Level          // cached from the most recent governorGate call
	lastQuotaUsage quota.Usage          // cached from the most recent governorGate call

	// Cached full issue listing for the epic-aware patrol checks (koryph-bbe's
	// completed-but-unvalidated sweep; koryph-wo0.7's parked/degraded WARN
	// check shares the same cache). Refreshed via one bd subprocess call at
	// most once per epicListCadence — see health.go's doc comment.
	epicPatrolAt       time.Time
	epicPatrolIssues   []beads.Issue
	epicPatrolFindings []patrolFinding // patrolCheckUnvalidatedEpics's cache from the last real scan

	// reportedSkips dedups structural-skip warnings so each non-dispatchable
	// ready bead is surfaced once per run, not every wave (koryph-6g2.1).
	reportedSkips map[string]bool

	// lastLearn throttles the wave-boundary learned-model pass
	// (koryph-qf6.6, see applyLearnedModels): rolling mode hits the boundary
	// on every freed slot, far more often than escalation evidence changes.
	lastLearn time.Time

	// Epic validation state (koryph-wo0.4, design §2/§4b). In-memory only:
	// `koryph epic validate` is the crash-recovery path.
	epicPending    map[string]bool                                           // epic id → completion candidate
	epicInFlight   string                                                    // epic id currently validating ("" = none)
	epicResults    chan epicValidationResult                                 // validator goroutine → tick loop
	epicValidateFn func(context.Context, epicreview.Opts) epicreview.Verdict // test seam; nil = epicreview.Validate

	// Billing for the current wave (refreshed by the governor each wave).
	billing account.BillingMode
	apiKey  string

	// sshAuthSock is the koryph scoped signing socket handed to dispatched
	// agents (empty when signing is not required). It holds ONLY the signing
	// key, so agents can sign without the operator's ambient agent.
	sshAuthSock string

	// wakeCh is a test seam overriding pollUntilIdle's SIGCHLD wake source
	// (koryph-2im.2); nil (the production default) means "register a real
	// signal.Notify(SIGCHLD) channel for the poll loop's duration".
	wakeCh chan os.Signal

	// resUsage holds the in-memory running resource Usage for each live slot
	// (keyed by phase id), folded from internal/resmon samples and mirrored to
	// the ledger slot's Peak/AvgRSSMB, CPUSeconds, and IO*MB fields (koryph
	// process-metrics). It is the accumulation source of truth; the ledger is a
	// derived snapshot. Each entry records the PID it is accumulating for so a
	// requeue (new PID, reset DispatchedAt) starts a FRESH Usage — keeping the
	// metrics per-attempt and consistent with the per-attempt wall window the
	// cockpit divides CPU seconds by. Lazily initialised by sampleSlotResources.
	resUsage map[string]*slotResUsage

	// lastResSampleAt throttles resource sampling to at most once per poll
	// interval, decoupling it from pollPass frequency (which also fires on every
	// SIGCHLD wake) so a burst of short-lived subprocess exits cannot trigger a
	// host-wide process sweep per signal.
	lastResSampleAt time.Time
}

// slotResUsage is one slot's in-memory resource accumulation plus the PID it is
// accumulating for, so a requeue to a new PID resets it (per-attempt metrics).
type slotResUsage struct {
	pid   int
	usage resmon.Usage
}

// Run executes one engine run over one project per the package contract in
// types.go: setup → (resume) → wave loop (scan → batch → preflight →
// dispatch → poll → review → merge → record).
func Run(ctx context.Context, opts Options) (Outcome, error) {
	// PollSec is intentionally NOT pre-defaulted here (unlike StuckSec below):
	// pollInterval() resolves it lazily against KORYPH_POLL_SEC env, then the
	// project config's poll_seconds (loaded further down), then the engine
	// default — so config participates. Pre-defaulting here would shadow the
	// config value with defaultPollSec before Load ever runs (koryph-2im.2).
	if opts.StuckSec <= 0 {
		opts.StuckSec = defaultStuckSec
	}
	switch opts.DispatchMode {
	case "", "wave", "rolling":
	default:
		return Outcome{Code: ExitUsage}, fmt.Errorf(
			"engine: --dispatch-mode must be wave|rolling, got %q", opts.DispatchMode)
	}

	reg := registry.NewStore()
	rec, err := reg.Get(opts.ProjectID)
	if err != nil {
		return Outcome{Code: ExitFatal}, err
	}
	if rec.MigrationStatus != registry.StatusValidated && !opts.AllowUnvalidated {
		return Outcome{Code: ExitFatal}, fmt.Errorf(
			"engine: project %s has migration status %q (want %q) — validate it or pass AllowUnvalidated for a canary run",
			opts.ProjectID, rec.MigrationStatus, registry.StatusValidated)
	}

	cfg, err := project.Load(rec.Root)
	if err != nil {
		return Outcome{Code: ExitFatal}, err
	}
	if ok, verr := version.Satisfied(EngineVersion, cfg.EngineVersion); verr != nil {
		return Outcome{Code: ExitFatal}, fmt.Errorf("engine: %w", verr)
	} else if !ok {
		return Outcome{Code: ExitFatal}, fmt.Errorf(
			"engine: koryph %s does not satisfy the project's engine_version %q — upgrade koryph",
			EngineVersion, cfg.EngineVersion)
	}
	if cfg.WorkSource != "bd" {
		return Outcome{Code: ExitFatal}, fmt.Errorf(
			"engine: work_source %q is not supported: legacy markdown projects run their project-local fork until migrated",
			cfg.WorkSource)
	}

	// ra resolves the effective account profile for this run's runtime
	// (koryph-v8u.5): registry.Record.AccountFor falls back to the flat
	// AccountProfile/ClaudeConfigDir/ExpectedIdentity fields when the project
	// has no runtime_accounts entry for resolvedRuntimeName, which is every
	// project today — so profile/expectedIdentity below are identical to
	// pre-v8u.5's rec.ClaudeConfigDir/rec.ExpectedIdentity reads.
	ra := rec.AccountFor(resolvedRuntimeName)
	profile := account.Profile{Name: rec.AccountProfile, ConfigDir: ra.ConfigDir}
	expectedIdentity := ra.ExpectedIdentity

	// rt is the resolved runtime adapter (see resolvedRuntimeName's doc).
	rt := claude.New(os.Getenv(envClaudeBin))

	// Run-level identity check, fail closed BEFORE any state is touched (no
	// lock, no run dir, no worktrees). The dispatch backend re-verifies per
	// dispatch as belt-and-braces. Reached through the runtime seam
	// (koryph-v8u.5): claude's VerifyIdentity delegates to
	// account.VerifyExpected, unchanged.
	if _, err := rt.VerifyIdentity(ctx, runtime.Profile{Name: profile.Name, ConfigDir: profile.ConfigDir}, expectedIdentity); err != nil {
		return Outcome{Code: ExitFatal}, err
	}

	// Signing preflight, fail closed BEFORE any dispatch. The engine only
	// touches ConfigureRepo/AgentReady/Verify — never the vault fetch path.
	if err := signingPreflight(ctx, opts.ProjectID, rec.Root, cfg.Signing); err != nil {
		return Outcome{Code: ExitFatal}, err
	}

	adapter := beads.New(rec.Root)
	if v := os.Getenv(envBDBin); v != "" {
		adapter.Bin = v
	}

	store := ledger.NewStore(rec.Root)
	lock, err := store.RunLock(opts.ProjectID)
	if err != nil {
		return Outcome{Code: ExitFatal}, err
	}
	defer func() { _ = lock.Unlock() }()

	r := &runner{
		opts:             opts,
		reg:              reg,
		rec:              rec,
		cfg:              cfg,
		adapter:          adapter,
		beadsDir:         adapter.BeadsDir,
		store:            store,
		profile:          profile,
		expectedIdentity: expectedIdentity,
		rt:               rt,
		backend:          &dispatch.CLIBackend{ClaudeBin: os.Getenv(envClaudeBin), Runtime: rt},
		gov:              govern.NewStore(),
		owner:            fmt.Sprintf("koryph@%s:%d", hostName(), os.Getpid()),
		width:            effectiveWidth(opts.Max, cfg.MaxConcurrentSlots),
		issues:           map[string]beads.Issue{},
		billing:          account.BillingSubscription,
	}
	if r.quotaCfg, err = quota.LoadConfig(r.quotaName()); err != nil {
		return Outcome{Code: ExitFatal}, err
	}
	// Dispatched agents sign via the koryph scoped signing socket, not the
	// operator's ambient agent (koryph-3vp.2).
	if r.requireSigned() && cfg.Signing.EffectiveMode() == signing.ModeSSH {
		r.sshAuthSock = paths.SigningAgentSock()
	}

	// Clear a stale operator-drain sentinel at process start (koryph-57v.1),
	// unconditionally (fresh run OR --resume): the sentinel is normally
	// consumed the moment the prior run's last active slot lands (see
	// governorGate), so one still present here can only be left over from a
	// run that never got back around to a boundary (e.g. killed out-of-band).
	// A leftover sentinel must never instantly drain-and-exit a fresh,
	// intentional invocation before it dispatches anything.
	if r.store.ConsumeDrain() {
		r.progress("cleared a stale operator-drain request from a previous run")
	}

	resumed := false
	if opts.Resume {
		resumed, err = r.resume(ctx)
		if err != nil {
			return Outcome{Code: ExitFatal}, err
		}
	}
	if !resumed {
		// Fresh run: reconcile beads stranded in_progress by a prior dead run
		// so they are re-dispatched rather than silently leaked (koryph-47n).
		if !opts.Manual {
			r.reconcileOrphans(ctx)
		}
		run, err := store.NewRun(opts.ProjectID, cfg.WorkSource, EngineVersion)
		if err != nil {
			return Outcome{Code: ExitFatal}, err
		}
		r.run = run
	}

	logRunStart(r.run.RunID, r.opts.ProjectID, r.dispatchMode())
	outcome, loopErr := r.loop(ctx)
	logRunEnd(r.run.RunID, r.opts.ProjectID, outcome.Reason, outcome.Drained, outcome.Dispatched, outcome.Merged)
	return outcome, loopErr
}

// signingPreflight enforces a Required signing policy at run setup:
// ConfigureRepo is applied idempotently (worktrees share the main repo's
// .git/config, so agent commits sign automatically), and for mode ssh the
// system SSH agent must already hold the signing key — the engine never
// talks to the vault itself, so an unready agent fails closed with the
// operator command that fixes it. A nil or non-Required config is a no-op.
func signingPreflight(ctx context.Context, projectID, repoRoot string, sc *signing.Config) error {
	if sc == nil || !sc.Required {
		return nil
	}
	if err := signing.ConfigureRepo(ctx, repoRoot, sc); err != nil {
		return fmt.Errorf("engine: signing: %w", err)
	}
	if sc.EffectiveMode() == signing.ModeSSH && !signing.ScopedAgentReady(ctx, sc.PublicKey) {
		return fmt.Errorf(
			"engine: signing is required but the koryph signing agent does not hold the signing key — run `koryph signing enable --project %s`",
			projectID)
	}
	return nil
}

// requireSigned reports whether merges for this project must verify commit
// signatures.
func (r *runner) requireSigned() bool {
	return r.cfg.Signing != nil && r.cfg.Signing.Required
}

// effectiveWidth computes the wave width: opts.Max, falling back to the
// project's MaxConcurrentSlots, falling back to the engine default — and
// never above the project cap.
func effectiveWidth(optMax, cfgMax int) int {
	w := optMax
	if w <= 0 {
		w = cfgMax
	}
	if w <= 0 {
		w = defaultWaveWidth
	}
	if cfgMax > 0 && w > cfgMax {
		w = cfgMax
	}
	return w
}

// quotaName resolves the governor profile name for this project.
func (r *runner) quotaName() string {
	if r.rec.QuotaProfile != "" {
		return r.rec.QuotaProfile
	}
	return r.rec.AccountProfile
}

// progress writes one human-readable line to opts.Out (nil-safe) and emits
// a structured INFO record via the engine logger for correlation in log
// pipelines (Section O2: engine instrumentation).
func (r *runner) progress(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if r.opts.Out != nil {
		fmt.Fprintln(r.opts.Out, msg)
	}
	log.Info(msg, r.runLogAttrs()...)
}

// runLogAttrs returns run-scoped slog attributes: run_id when a run is active,
// project always. Callers pass this to log calls that happen in run context.
func (r *runner) runLogAttrs() []any {
	if r.run != nil {
		return []any{
			slog.String(obs.KeyRunID, r.run.RunID),
			slog.String(obs.KeyProject, r.opts.ProjectID),
		}
	}
	return []any{slog.String(obs.KeyProject, r.opts.ProjectID)}
}

// outcome summarizes the run from its slot statuses.
func (r *runner) outcome(code int, reason string, drained bool) Outcome {
	o := Outcome{
		Code:       code,
		Reason:     reason,
		Drained:    drained,
		Dispatched: r.dispatched,
	}
	if r.run != nil {
		o.RunID = r.run.RunID
		for _, sl := range r.run.Slots {
			if sl == nil {
				continue
			}
			switch sl.Status {
			case ledger.SlotMerged:
				o.Merged++
			case ledger.SlotPROpened:
				o.PROpened++
			case ledger.SlotFailed, ledger.SlotConflict:
				o.Failed++
			case ledger.SlotBlocked:
				o.Blocked++
			}
		}
	}
	return o
}

// interrupted checkpoints every non-terminal slot's manifest and leaves the
// run in status running so a later --resume can classify and re-adopt it.
func (r *runner) interrupted() (Outcome, error) {
	for _, id := range r.activePhaseIDs() {
		r.checkpointSlot(r.run.Slots[id], "interrupted")
	}
	_ = r.store.SaveRun(r.run)
	r.progress("interrupted: run %s left running for --resume", r.run.RunID)
	return r.outcome(ExitOK, "interrupted", false), nil
}

// activeIDs returns the set of non-terminal slot phase ids.
func (r *runner) activeIDs() map[string]bool {
	ids := map[string]bool{}
	for id, sl := range r.run.Slots {
		if sl != nil && !ledger.Terminal(sl.Status) {
			ids[id] = true
		}
	}
	return ids
}

// activeCount is the number of non-terminal slots.
func (r *runner) activeCount() int { return len(r.activeIDs()) }

// activePhaseIDs returns the non-terminal slot ids in deterministic order.
func (r *runner) activePhaseIDs() []string {
	var ids []string
	for id := range r.activeIDs() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// newSessionID returns a random UUID v4 (crypto/rand, no dependencies).
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand never fails on supported platforms
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// envInt reads an integer environment override; ok is false when unset or
// unparseable.
func envInt(name string) (int, bool) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// pollInterval is the poll tick, resolved lazily (not pre-defaulted in Run())
// so the project config can participate. Precedence, highest first
// (koryph-2im.2):
//  1. KORYPH_POLL_SEC env — operator/test override, always wins.
//  2. Options.PollSec, when the caller set it (>0) — an explicit programmatic
//     override (e.g. a test fixture) survives even though Run() no longer
//     force-defaults it.
//  3. The project config's poll_seconds (>0).
//  4. defaultPollSec.
func (r *runner) pollInterval() time.Duration {
	if v, ok := envInt(envPollSec); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	if r.opts.PollSec > 0 {
		return time.Duration(r.opts.PollSec) * time.Second
	}
	if r.cfg != nil && r.cfg.PollSeconds > 0 {
		return time.Duration(r.cfg.PollSeconds) * time.Second
	}
	return time.Duration(defaultPollSec) * time.Second
}

// dispatchMode resolves the effective dispatch loop (koryph-2im.3, design L1).
// Precedence, highest first: Options.DispatchMode (the --dispatch-mode run
// flag) > the project config's dispatch_mode > "rolling". Both inputs are
// validated (Run's own switch; project.Config.Validate) before this ever
// runs, so any non-empty value seen here is guaranteed to be "wave" or
// "rolling".
//
// Rolling became the default after the 2026-07-03 burn-in (koryph-2im.8): a
// full self-build canary drained clean through the rolling pipeline (10 beads
// merged, refill-on-free and blocked-slot refill observed live, zero
// incorrect merges), with every failure root-caused to non-scheduler issues
// that were fixed on main (pre-composition footprints, a flaky dispatch
// test, a stray build artifact). Wave mode remains fully supported via
// dispatch_mode: "wave" or --dispatch-mode wave.
func (r *runner) dispatchMode() string {
	if r.opts.DispatchMode != "" {
		return r.opts.DispatchMode
	}
	if r.cfg != nil && r.cfg.DispatchMode != "" {
		return r.cfg.DispatchMode
	}
	return "rolling"
}

// staggerDelay is the pause between dispatches: env override, else project
// config.
func (r *runner) staggerDelay() time.Duration {
	if v, ok := envInt(envStaggerSec); ok && v >= 0 {
		return time.Duration(v) * time.Second
	}
	if r.cfg.DispatchStaggerSeconds > 0 {
		return time.Duration(r.cfg.DispatchStaggerSeconds) * time.Second
	}
	return 0
}

// backoffSleep pauses attempts*base seconds before a requeue (ctx-aware).
func (r *runner) backoffSleep(ctx context.Context, attempts int) {
	base := defaultBackoffSec
	if v, ok := envInt(envBackoffSec); ok && v >= 0 {
		base = v
	}
	d := time.Duration(attempts*base) * time.Second
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// hostName returns the machine hostname or "unknown".
func hostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// shortSHA trims a full SHA for progress lines.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
