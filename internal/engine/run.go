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
	defaultPollSec    = 45
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
	adapter  *beads.Adapter
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

	// Billing for the current wave (refreshed by the governor each wave).
	billing account.BillingMode
	apiKey  string
}

// Run executes one engine run over one project per the package contract in
// types.go: setup → (resume) → wave loop (scan → batch → preflight →
// dispatch → poll → review → merge → record).
func Run(ctx context.Context, opts Options) (Outcome, error) {
	if opts.PollSec <= 0 {
		opts.PollSec = defaultPollSec
	}
	if opts.StuckSec <= 0 {
		opts.StuckSec = defaultStuckSec
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
		opts:    opts,
		reg:     reg,
		rec:     rec,
		cfg:     cfg,
		adapter: adapter,
		store:   store,
		profile: profile,
		backend: &dispatch.CLIBackend{ClaudeBin: os.Getenv(envClaudeBin)},
		gov:     govern.NewStore(),
		owner:   fmt.Sprintf("koryph@%s:%d", hostName(), os.Getpid()),
		width:   effectiveWidth(opts.Max, cfg.MaxConcurrentSlots),
		issues:  map[string]beads.Issue{},
		billing: account.BillingSubscription,
	}
	if r.quotaCfg, err = quota.LoadConfig(r.quotaName()); err != nil {
		return Outcome{Code: ExitFatal}, err
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
	if sc.EffectiveMode() == signing.ModeSSH && !signing.AgentReady(ctx, sc.PublicKey) {
		return fmt.Errorf(
			"engine: signing is required but the SSH agent does not hold the signing key — run `koryph signing enable --project %s`",
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

// pollInterval is the poll tick: env override, else opts.PollSec.
func (r *runner) pollInterval() time.Duration {
	if v, ok := envInt(envPollSec); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	return time.Duration(r.opts.PollSec) * time.Second
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
