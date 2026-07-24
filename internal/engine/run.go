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
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/resmon"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtimecanary"
	"github.com/koryph/koryph/internal/runtimeconfig"
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
	defaultPollSec  = 10 // was 45; L3 fast-completion-detection (koryph-2im.2)
	defaultStuckSec = 900
	// defaultWaveWidth is the per-project wave-width fallback when neither
	// --max nor koryph.project.json's max_concurrent_slots is set (was 3,
	// koryph-4rk6.4). This value alone is NOT the 2026-07-21 OOM fix — a
	// wider per-project default is, taken alone, more concurrency, not
	// less. It is safe only paired with koryph-4rk6.2's machine-wide
	// max_machine_agents ceiling (default 8), which is what actually bounds
	// the cross-project sum that caused the incident: this default bounds
	// one project's common-case width, the ceiling bounds the total across
	// every concurrently-running project on the machine.
	defaultWaveWidth  = 4
	defaultBackoffSec = 15
	// defaultStaggerSec is the dispatch-stagger fallback when neither
	// KORYPH_STAGGER_SEC nor koryph.project.json's dispatch_stagger_seconds is
	// set (koryph-4rk6.3, anti-stampede floor). Below ~10s a wave of agents can
	// all land before the memory impact of the FIRST one registers in the
	// admission probe, so the whole batch is admitted against a stale
	// (pre-launch) footprint reading instead of each new admission observing
	// the steady-state footprint of the previous agent — a stampeding-herd
	// effect that defeats any per-agent floor. project.Default() carries the
	// same value for newly onboarded projects; this constant is what an
	// EXISTING project.json with the field omitted (zero value) falls back to.
	defaultStaggerSec = 10
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
	// authMode is ra.EffectiveAuthMode() (koryph-i3b, design §4), resolved
	// once at Run() setup alongside profile/expectedIdentity above:
	// registry.AuthModeSubscription (default), AuthModeAPIKey, or
	// AuthModeOAuthToken. billingFor reads this to decide whether THIS
	// account bills api-key from wave 1, independent of the legacy
	// --allow-api-spend governor-stop fallback (which stays keyed off
	// r.rec.APIFallback/APIKeyEnvVar and applies only to subscription
	// accounts).
	authMode string
	// credential/credentialEnvVar are the resolved, verified long-lived
	// credential for a first-class api-key/oauth-token account (koryph-i3b,
	// design §5/§6) — both empty for AuthModeSubscription. Resolved once at
	// Run() setup (account.ResolveCredential, after account.VerifyAuth
	// passes) and threaded onto every dispatch.Spec this run builds so
	// Command injects it under its canonical name. Never logged.
	credential       string
	credentialEnvVar string
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

	// systemTimeoutSec is the machine-wide default agent-facing wall timeout
	// (~/.koryph/config.json default_timeout_seconds) — the "system" tier of the
	// unified timeout hierarchy (koryph-w82i). 0 = unset (falls through to the
	// built-in 1200 in timeoutcfg.Resolve). Loaded once at Run() setup; read by
	// the review/stage/epic-validation call sites below the bead label and the
	// project config.
	systemTimeoutSec int

	dispatched         int
	govWarned          bool
	uncalibratedWarned bool // koryph-grz: loud uncalibrated-governor warning fired once this run
	staleResizeWarned  bool // koryph-bzf: fired once when an explicit --max overrode a stale persisted resize
	// startupResizeSetAt is the SetAt of the resize override present when this run
	// started ("" if none), snapshotted so governorGate can tell a live `koryph
	// resize` of THIS run (SetAt changed) from one inherited across runs (koryph-bzf).
	startupResizeSetAt string
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

	// staleClaimsAt/staleClaimsFindings cache patrolCheckStaleClaims's own
	// ledger scan (ListRuns + LoadRun per recent run — not free, unlike the
	// epic checks' pure in-memory work), independently throttled from
	// epicPatrolAt since a same-tick sibling call may already have consumed
	// that cache's refresh signal. See health.go's doc comment.
	staleClaimsAt       time.Time
	staleClaimsFindings []patrolFinding

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

	// phaseCanaries holds non-blocking cross-runtime capability probes keyed
	// by request id. The poll loop owns the map; each worker goroutine sends
	// exactly one sanitized result on its buffered channel.
	phaseCanaries    map[string]<-chan phaseCanaryCompletion
	runtimeCanaryRun func(context.Context, runtimecanary.Options) runtimecanary.Result

	// lastResSampleAt throttles resource sampling to at most once per poll
	// interval, decoupling it from pollPass frequency (which also fires on every
	// SIGCHLD wake) so a burst of short-lived subprocess exits cannot trigger a
	// host-wide process sweep per signal.
	lastResSampleAt time.Time

	// lastReadyCount is the eligible-frontier count from the most recent
	// frontier scan (waveLoop/rollingLoop), cached so the heartbeat (below) has
	// something to report on ticks where no scan ran this iteration. Written
	// only from the loop goroutine.
	lastReadyCount int

	// hb is the liveness-heartbeat snapshot (koryph-lwnq) — see heartbeat.go.
	// Safe for concurrent access by design: the loop goroutine writes it at
	// iteration checkpoints via setCounts/noteAction, the background heartbeat
	// goroutine only ever reads it via snapshot().
	hb heartbeatState
}

// slotResUsage is one slot's in-memory resource accumulation plus the PID it is
// accumulating for, so a requeue to a new PID resets it (per-attempt metrics).
type slotResUsage struct {
	pid   int
	usage resmon.Usage

	// lastCPU/lastActiveAt implement the implicit CPU heartbeat (koryph-2rf):
	// each poll pass that observes the cohort's cumulative CPU time advancing
	// stamps lastActiveAt. An agent blocked inside one long tool call (a
	// build, a Playwright e2e) cannot write the agent-authored JSON heartbeat
	// or commit — but its cohort burns CPU, and isStuck treats that as
	// activity. A 0%-CPU hung cohort stamps nothing and still trips stuck,
	// which is exactly the case worth flagging.
	lastCPU      float64
	lastActiveAt time.Time
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
	if opts.RuntimeOnly != "" && opts.RuntimeEquivalent != "" {
		return Outcome{Code: ExitUsage}, fmt.Errorf(
			"engine: --runtime-only and --runtime-equivalent are mutually exclusive")
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

	// The project default is the run's primary runtime. A runtime execution
	// policy intentionally replaces that baseline: both --runtime-only and
	// --runtime-equivalent can dispatch at most one runtime, so its account,
	// identity check, governor pool, and estimate table must be preflighted.
	// Without either flag, individual beads may still select another enabled
	// runtime at dispatch time, preserving existing mixed-runtime behavior.
	runRuntimeName, _ := modelroute.ResolveRuntimeName(nil, cfg.DefaultRuntime)
	if opts.RuntimeOnly != "" {
		runRuntimeName = opts.RuntimeOnly
	}
	if opts.RuntimeEquivalent != "" {
		runRuntimeName = opts.RuntimeEquivalent
	}
	rt, ok := runtimeForName(runRuntimeName)
	if !ok {
		return Outcome{Code: ExitFatal}, fmt.Errorf("engine: execution runtime %q is not registered", runRuntimeName)
	}
	if !runtimeEnabled(cfg, runRuntimeName) {
		return Outcome{Code: ExitFatal}, fmt.Errorf("engine: execution runtime %q is not enabled in koryph.project.json", runRuntimeName)
	}

	// ra resolves the effective account profile for this run's runtime
	// (koryph-v8u.5): registry.Record.AccountFor falls back to the flat
	// AccountProfile/ClaudeConfigDir/ExpectedIdentity fields when the project
	// has no runtime_accounts entry for resolvedRuntimeName, which is every
	// project today — so profile/expectedIdentity below are identical to
	// pre-v8u.5's rec.ClaudeConfigDir/rec.ExpectedIdentity reads.
	ra := rec.AccountFor(runRuntimeName)
	profile := account.Profile{Name: rec.AccountProfile, ConfigDir: ra.ConfigDir}
	expectedIdentity := ra.ExpectedIdentity
	authMode := ra.EffectiveAuthMode()

	// Run-level identity/credential check, fail closed BEFORE any state is
	// touched (no lock, no run dir, no worktrees). The dispatch backend
	// re-verifies per dispatch as belt-and-braces (koryph-i3b: for a
	// first-class non-subscription account, that re-check is cheap — it
	// only confirms the credential resolved below made it onto the Spec,
	// not a second live probe; see dispatch.CLIBackend.Dispatch's doc).
	//
	// credential/credentialEnvVar are threaded onto every dispatch.Spec this
	// run builds (wave.go/pipeline.go) so Command can inject them under
	// their canonical name — empty for AuthModeSubscription, which never
	// touches this path (see below).
	var credential, credentialEnvVar string
	switch authMode {
	case registry.AuthModeSubscription:
		// Reached through the runtime seam (koryph-v8u.5): claude's
		// VerifyIdentity delegates to account.VerifyExpected, unchanged —
		// BYTE FOR BYTE the pre-koryph-i3b check (design §5, §11 AC5).
		if _, err := rt.VerifyIdentity(ctx, runtime.Profile{Name: profile.Name, ConfigDir: profile.ConfigDir}, expectedIdentity); err != nil {
			return Outcome{Code: ExitFatal}, err
		}
	case registry.AuthModeAPIKey, registry.AuthModeOAuthToken:
		if rt.Name() != "claude" {
			return Outcome{Code: ExitFatal}, fmt.Errorf("engine: runtime %q does not support koryph-managed %s credentials; login with its native CLI in the configured account home", rt.Name(), authMode)
		}
		// First-class non-subscription account (koryph-i3b, design §5/§6):
		// there is no .claude.json to read — "verified" means the
		// credential resolves, fingerprint-matches the enrolled identity
		// (fail closed on a swap), and is live against Anthropic
		// (account.VerifyAuth). VerifyAuth never returns the raw secret (it
		// is not safe to carry on Identity), so the credential itself is
		// resolved a second time via ResolveCredential — cheap relative to
		// the liveness probe, and both run once per Run(), not per wave/slot.
		// ra.Credential is *registry.Credential, which account.Credential
		// aliases (both alias internal/authmode.Credential), so it passes
		// through with no conversion.
		cred := ra.Credential
		authSpec := account.AuthSpec{
			Mode:                account.AuthMode(authMode),
			ExpectedIdentity:    expectedIdentity,
			Credential:          cred,
			IdentityFingerprint: ra.IdentityFingerprint,
		}
		if _, err := account.VerifyAuth(ctx, profile, authSpec); err != nil {
			return Outcome{Code: ExitFatal}, err
		}
		envVar, value, err := account.ResolveCredential(ctx, account.AuthMode(authMode), cred)
		if err != nil {
			return Outcome{Code: ExitFatal}, err
		}
		credentialEnvVar, credential = envVar, value
	default:
		return Outcome{Code: ExitFatal}, fmt.Errorf(
			"engine: project %s has unrecognized auth_mode %q", opts.ProjectID, authMode)
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

	// bd capability preflight (non-fatal): a bd older than beads.MinVersion
	// omits `parent` from `bd list --json`, silently flattening the TUI queue
	// and any parent-linked view. It never errors, so warn loudly at startup so
	// the operator isn't left guessing why epic folds vanished.
	if info := beads.ProbeVersion(ctx); info.Found && !info.OK {
		fmt.Fprintf(opts.Out, "koryph: warning: %s\n", info.Remediation())
	}

	store := ledger.NewStore(rec.Root)
	lock, err := store.RunLock(opts.ProjectID)
	if err != nil {
		return Outcome{Code: ExitFatal}, err
	}
	defer func() { _ = lock.Unlock() }()

	// System-tier timeout default (koryph-w82i): the machine-wide
	// ~/.koryph/config.json default_timeout_seconds, resolved once. An absent or
	// unreadable global config is a non-fatal 0 — timeoutcfg falls through to the
	// built-in 1200.
	systemTimeoutSec := 0
	if gc, gerr := signing.LoadGlobalConfig(); gerr == nil {
		systemTimeoutSec = gc.DefaultTimeoutSeconds
	}

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
		authMode:         authMode,
		credential:       credential,
		credentialEnvVar: credentialEnvVar,
		rt:               rt,
		backend:          &dispatch.CLIBackend{ClaudeBin: os.Getenv(envClaudeBin), Runtime: rt},
		gov:              govern.NewStore(),
		owner:            fmt.Sprintf("koryph@%s:%d", hostName(), os.Getpid()),
		width:            effectiveWidth(opts.Max, cfg.MaxConcurrentSlots),
		systemTimeoutSec: systemTimeoutSec,
		issues:           map[string]beads.Issue{},
		billing:          account.BillingSubscription,
	}
	if r.quotaCfg, err = quota.LoadConfig(r.quotaName()); err != nil {
		return Outcome{Code: ExitFatal}, err
	}
	// Wire the per-account seeded-default concurrency cap (koryph-1o2.3) into
	// the global governor: govern must not import quota (layering), so the
	// engine hands it a closure over its own already-loaded r.quotaCfg instead.
	r.gov.SeedCap = r.seedCapForPool
	// Uniform memory floor (koryph-4rk6.1): repair a governor.json an older
	// koryph version already wrote with some pools carrying a
	// max_global_agents cap but no min_free_memory_mb at all — the exact gap
	// behind the 2026-07-21 OOM incident. Run once per engine load, not on
	// every governor write (Store.BackfillMemoryFloors doc), and fail open —
	// the governor is a safety rail, never a correctness dependency (I6).
	if changed, err := r.gov.BackfillMemoryFloors(); err != nil {
		r.progress("warning: memory-floor backfill failed (continuing): %v", err)
	} else if len(changed) > 0 {
		r.progress("governor: seeded default %d MB memory floor on pool(s) %v (previously unset)", govern.DefaultMinFreeMemoryMB, changed)
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
	// Same rationale for operator-stop markers (koryph-a1x, F1a): a marker
	// stranded by a prior run that never reached the phase's death must not
	// spuriously park that phase in this fresh, intentional run.
	r.store.ClearStops()

	// Snapshot the resize override present at run start (koryph-bzf): a resize
	// override persists across runs, so governorGate compares each boundary's
	// override against this snapshot to tell a live `koryph resize` of THIS run
	// (SetAt changed) from one merely inherited from a prior run — the latter
	// must yield to an explicit --max rather than silently pin the new run's width.
	if ov, ok := r.store.LoadResize(); ok {
		r.startupResizeSetAt = ov.SetAt
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
	// Liveness heartbeat (koryph-lwnq): runs on its own ticker for the whole
	// loop's lifetime, independent of whatever the loop goroutine is doing at
	// any instant — see heartbeat.go's doc comment for why that independence
	// is the point.
	stopHeartbeat := r.startHeartbeat(ctx)
	outcome, loopErr := r.loop(ctx)
	stopHeartbeat()
	logRunEnd(r.run.RunID, r.opts.ProjectID, outcome.Reason, outcome.Drained, outcome.Dispatched, outcome.Merged)
	return outcome, loopErr
}

// runtimeForName returns a configured runtime adapter. The two built-ins use
// their binary overrides; later adapters self-register in runtime.Default and
// need no engine branch.
func runtimeForName(name string) (runtime.Runtime, bool) {
	return runtimeconfig.Get(name)
}

func runtimeEnabled(cfg *project.Config, name string) bool {
	if name == "claude" {
		return true // existing projects predate explicit runtime opt-in
	}
	rc, configured := cfg.Runtimes[name]
	return configured && rc.Enabled
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

// quotaName resolves the governor profile name for this project. It defers to
// registry.Record.QuotaAccount, the sole definition of the
// QuotaProfile-else-AccountProfile rule (koryph-qta.11).
func (r *runner) quotaName() string {
	return r.rec.QuotaAccount()
}

// progress writes one human-readable line to the console sink (opts.Out). When
// no console sink is configured — a headless run — it falls back to the
// structured engine logger so the line is still captured somewhere. It
// deliberately does NOT emit to both: opts.Out (stdout) and the slog handler
// (stderr) are routinely merged into a single run log via `> run.log 2>&1`, and
// emitting the same string to both doubled every progress line there, inflating
// every tail/grep count (D8). Queryable telemetry comes from the dedicated,
// single-emission engine.* records in obs.go, which are unaffected.
//
// The message is redacted before either sink (koryph-mes, finding #53): this
// is the engine's dominant idiom for formatting raw errors (%v-wrapping
// git/gh/gate stderr, which can carry PEM blocks or tokens) into a log
// Message, and the opts.Out console path never passed through the slog
// handler's RedactRecord wrapper (obs/handler.go) — only the log.Info
// fallback did. RedactValue is a no-op on clean strings, so the common case
// is unaffected; the log.Info fallback re-applies it via RedactRecord, which
// is idempotent on already-redacted text.
func (r *runner) progress(format string, args ...any) {
	msg := obs.RedactValue(fmt.Sprintf(format, args...))
	// Feed the liveness heartbeat (koryph-lwnq): progress is the engine's
	// existing narration chokepoint — dispatch, merge, governor decisions,
	// drains, patrol findings all flow through it — so recording the latest
	// line here gives the heartbeat a "last action <what> <ago>" for free,
	// with no need to instrument every individual call site.
	r.hb.noteAction(msg, time.Now())
	if r.opts.Out != nil {
		fmt.Fprintln(r.opts.Out, msg)
		return
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

// liveActiveCount is the number of slots with a running (or about-to-run) agent:
// non-terminal AND not parked in the resume backlog (koryph-bzf). A SlotQueued
// slot still reserves its place in the width budget — so activeIDs counts it for
// frontier capacity and footprint gating — but has no agent to poll or wait on,
// so it must not keep pollUntilIdle spinning, and it must not be mistaken for a
// busy slot when drainResumeBacklog decides how many backlog beads a boundary
// may promote. With no backlog present (the common case) this equals
// activeCount, so every existing flow is unchanged.
func (r *runner) liveActiveCount() int {
	n := 0
	for _, sl := range r.run.Slots {
		if sl != nil && !ledger.Terminal(sl.Status) && sl.Status != ledger.SlotQueued {
			n++
		}
	}
	return n
}

// queuedResumeIDs returns, in deterministic order, the phase ids of slots parked
// in the resume backlog (SlotQueued) — stalled beads that resume() adopted but
// the effective width could not admit at once (koryph-bzf). drainResumeBacklog
// promotes them into live dispatches as width frees.
func (r *runner) queuedResumeIDs() []string {
	var ids []string
	for id, sl := range r.run.Slots {
		if sl != nil && sl.Status == ledger.SlotQueued {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

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
// config, else defaultStaggerSec — an unset/omitted config value is NOT "no
// stagger" (koryph-4rk6.3); it falls back to the same anti-stampede floor a
// freshly onboarded project gets from project.Default().
func (r *runner) staggerDelay() time.Duration {
	if v, ok := envInt(envStaggerSec); ok && v >= 0 {
		return time.Duration(v) * time.Second
	}
	if r.cfg != nil && r.cfg.DispatchStaggerSeconds > 0 {
		return time.Duration(r.cfg.DispatchStaggerSeconds) * time.Second
	}
	return time.Duration(defaultStaggerSec) * time.Second
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
