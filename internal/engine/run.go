// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/signing"
	"github.com/koryph/koryph/internal/version"
)

// Environment overrides (primarily for tests; production leaves them unset).
const (
	envClaudeBin  = "KORYPH_CLAUDE_BIN"  // claude binary for dispatch + review
	envBDBin      = "KORYPH_BD_BIN"      // bd binary for the beads adapter
	envPollSec    = "KORYPH_POLL_SEC"    // poll tick override (seconds)
	envStaggerSec = "KORYPH_STAGGER_SEC" // dispatch stagger override (seconds)
	envBackoffSec = "KORYPH_BACKOFF_SEC" // requeue backoff base override (seconds)
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
	backend  dispatch.Backend
	quotaCfg *quota.Config
	gov      *govern.Store
	owner    string
	width    int

	dispatched int
	govWarned  bool
	issues     map[string]beads.Issue

	// reportedSkips dedups structural-skip warnings so each non-dispatchable
	// ready bead is surfaced once per run, not every wave (koryph-6g2.1).
	reportedSkips map[string]bool

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

	profile := account.Profile{Name: rec.AccountProfile, ConfigDir: rec.ClaudeConfigDir}

	// Run-level identity check, fail closed BEFORE any state is touched (no
	// lock, no run dir, no worktrees). The dispatch backend re-verifies per
	// dispatch as belt-and-braces.
	if _, err := account.VerifyExpected(ctx, profile, rec.ExpectedIdentity); err != nil {
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
		opts:     opts,
		reg:      reg,
		rec:      rec,
		cfg:      cfg,
		adapter:  adapter,
		beadsDir: adapter.BeadsDir,
		store:    store,
		profile:  profile,
		backend:  &dispatch.CLIBackend{ClaudeBin: os.Getenv(envClaudeBin)},
		gov:      govern.NewStore(),
		owner:    fmt.Sprintf("koryph@%s:%d", hostName(), os.Getpid()),
		width:    effectiveWidth(opts.Max, cfg.MaxConcurrentSlots),
		issues:   map[string]beads.Issue{},
		billing:  account.BillingSubscription,
	}
	if r.quotaCfg, err = quota.LoadConfig(r.quotaName()); err != nil {
		return Outcome{Code: ExitFatal}, err
	}
	// Dispatched agents sign via the koryph scoped signing socket, not the
	// operator's ambient agent (koryph-3vp.2).
	if r.requireSigned() && cfg.Signing.EffectiveMode() == signing.ModeSSH {
		r.sshAuthSock = paths.SigningAgentSock()
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

	return r.loop(ctx)
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

// progress writes one human-readable line to opts.Out (nil-safe).
func (r *runner) progress(format string, args ...any) {
	if r.opts.Out == nil {
		return
	}
	fmt.Fprintf(r.opts.Out, format+"\n", args...)
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
// flag) > the project config's dispatch_mode > "wave". Both inputs are
// validated (Run's own switch; project.Config.Validate) before this ever
// runs, so any non-empty value seen here is guaranteed to be "wave" or
// "rolling".
func (r *runner) dispatchMode() string {
	if r.opts.DispatchMode != "" {
		return r.opts.DispatchMode
	}
	if r.cfg != nil && r.cfg.DispatchMode != "" {
		return r.cfg.DispatchMode
	}
	return "wave"
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
