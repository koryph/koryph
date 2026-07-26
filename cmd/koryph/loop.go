// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	koryphgc "github.com/koryph/koryph/internal/gc"
	"github.com/koryph/koryph/internal/ledger"
	loopsupervisor "github.com/koryph/koryph/internal/loop"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/metrics"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/procx"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
	"github.com/koryph/koryph/internal/sched"
	"github.com/koryph/koryph/internal/strictjson"
	"github.com/koryph/koryph/internal/sysmem"
	"github.com/koryph/koryph/internal/version"
)

var (
	loopInstalledCommit        = version.Commit
	loopBinaryVersion          = func() string { return engine.EngineVersion }
	loadExpectedAutonomyReport = metrics.LoadAutonomyReportExpected
)

func init() {
	registerCmd(command{
		name:    "loop",
		summary: "run the binary-native autonomous supervisor",
		run:     cmdLoop,
		DocLinks: []string{
			"user-guide/running-waves.md",
			"concepts/rolling-dispatch.md",
		},
	})
}

func cmdLoop(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return cmdLoopStatus(args[1:], stdout, stderr)
		case "drain":
			return cmdLoopControl(args[1:], stdout, stderr, true)
		case "stop":
			return cmdLoopControl(args[1:], stdout, stderr, false)
		case "inject":
			return cmdLoopInject(args[1:], stdout, stderr)
		}
	}

	fs := newFlagSet("loop", stderr)
	projectID := fs.String("project", "", "project id (default: project containing the current directory)")
	max := fs.Int("max", 0, "target project width (canary starts at exactly 2 and widens after five good outcomes)")
	parent := fs.String("parent", "", "epic scope for the bd frontier")
	budget := fs.Float64("budget", 0, "per-engine-run cost ceiling in USD (0 = unlimited)")
	defaultModel := fs.String("default-model", "", "model for label-less beads")
	runtimeOnly := fs.String("runtime-only", "", "dispatch only beads normally routed to this runtime")
	runtimeEquivalent := fs.String("runtime-equivalent", "", "force this runtime through equivalent capability tiers")
	autoMerge := fs.Bool("auto-merge", true, "allow auto-merge for merge:auto items")
	direct := fs.Bool("direct", false, "owner override: merge directly instead of opening PRs")
	review := fs.Bool("review", true, "require the post-validation semantic review")
	allowAPISpend := fs.Bool("allow-api-spend", false, "permit api-key billing at governor stop")
	noBillingGuard := fs.Bool("no-billing-guard", false, "make quota throttling advisory (usage is still measured)")
	requireCalibration := fs.Bool("require-calibration", false, "refuse dispatch while quota governor is uncalibrated")
	dispatchMode := fs.String("dispatch-mode", "", "engine dispatch mode: wave|rolling")
	canary := fs.String("canary-cohort", "", "fixed comma-separated canary bead IDs (starts at width 2)")
	idleMin := fs.Duration("idle-min", time.Second, "minimum model-free idle observation backoff")
	idleMax := fs.Duration("idle-max", time.Minute, "maximum model-free idle observation backoff")
	setGroupedUsage(fs, stdout,
		"run the binary-native autonomous supervisor; idle launches no model and creates no run ledger",
		"[--project ID] [flags] | status|drain|stop|inject",
		[]flagGroup{
			{title: "SCOPE", names: []string{"project", "parent", "max", "canary-cohort", "dispatch-mode"}},
			{title: "LAND & REVIEW", names: []string{"auto-merge", "review", "direct", "default-model", "runtime-only", "runtime-equivalent"}},
			{title: "BUDGET", names: []string{"budget", "allow-api-spend", "no-billing-guard", "require-calibration"}},
			{title: "IDLE", names: []string{"idle-min", "idle-max"}},
		})
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) > 0 {
		return usageErr(stderr, "loop: unexpected positional arguments: "+strings.Join(pos, " "))
	}
	if *runtimeOnly != "" && *runtimeEquivalent != "" {
		return usageErr(stderr, "loop: --runtime-only and --runtime-equivalent are mutually exclusive")
	}
	if *idleMin <= 0 || *idleMax < *idleMin {
		return usageErr(stderr, "loop: require 0 < --idle-min <= --idle-max")
	}
	canaryIDs := splitIDs(*canary)
	if len(canaryIDs) > 0 && (!*review || !*autoMerge) {
		return usageErr(stderr, "loop: canary mode requires --review=true and --auto-merge=true")
	}

	ctx := context.Background()
	reg, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, code := resolveProjectRecordCwd(stderr, reg, *projectID, "loop")
	if code != 0 {
		return code
	}
	registryIdentityDigest, err := registry.ValidationIdentityDigest(rec)
	if err != nil {
		return fail(stderr, fmt.Errorf("loop: authenticate registry identity: %w", err))
	}
	cfg := loopsupervisor.Config{
		ProjectID: rec.ProjectID,
		Parent:    *parent,
		Max:       *max,
		IdleMin:   *idleMin,
		IdleMax:   *idleMax,
	}
	enginePolicy := nativeCanaryEnginePolicy{
		Parent: *parent, Budget: *budget, DefaultModel: *defaultModel,
		RuntimeOnly: *runtimeOnly, RuntimeEquivalent: *runtimeEquivalent,
		AutoMerge: *autoMerge, Direct: *direct, Review: *review,
		AllowAPISpend: *allowAPISpend, NoBillingGuard: *noBillingGuard,
		RequireCalibration: *requireCalibration, DispatchMode: *dispatchMode,
	}
	store := loopsupervisor.NewStore(rec.Root)
	state, err := store.LoadState()
	if err != nil {
		return fail(stderr, fmt.Errorf("loop: load supervisor state: %w", err))
	}
	marker, markerExists, err := loadNativeCanaryPromotionMarker(rec.Root)
	if err != nil {
		return fail(stderr, fmt.Errorf("loop: load canary promotion marker: %w", err))
	}
	var admittedSpec *loopsupervisor.CanarySpec
	if len(canaryIDs) > 0 &&
		((state.Canary != nil && state.Canary.Decision == "passed") || markerExists) {
		handled, promoted, err := reconcilePassedNativeCanary(
			ctx, reg, rec, store, canaryIDs, canaryTargetWidth(canaryIDs, *max),
		)
		if err != nil {
			return fail(stderr, fmt.Errorf("loop: recover completed canary: %w", err))
		}
		if handled {
			if promoted {
				fmt.Fprintf(stdout, "loop %s: promoted migration_status migrated -> validated from immutable passing canary\n", rec.ProjectID)
			} else {
				fmt.Fprintf(stdout, "loop %s: authenticated completed canary and finalized steady-mode handoff\n", rec.ProjectID)
			}
			return engine.ExitOK
		}
		if markerExists {
			return fail(stderr, errors.New("loop: canary promotion marker could not be reconciled"))
		}
	}
	if len(canaryIDs) > 0 && state.Canary != nil &&
		state.Canary.Decision == "failed" {
		target := canaryTargetWidth(canaryIDs, *max)
		admittedSpec, err = nativeCanarySpecWithPolicy(
			rec.ProjectID, rec.Root, canaryIDs, target,
			registryIdentityDigest, cfg, enginePolicy,
		)
		if err != nil {
			return fail(stderr, err)
		}
		var retired bool
		state, retired, err = retireStaleNativeCanaryEvidence(
			ctx, rec.Root, rec.ProjectID, admittedSpec,
		)
		if err != nil {
			return fail(stderr, fmt.Errorf("loop: retire stale canary evidence: %w", err))
		}
		if !retired {
			return fail(stderr, errors.New(
				"loop: failed canary evidence belongs to the current generation; install a clean strict-descendant build before retrying",
			))
		}
		marker = nativeCanaryPromotionMarker{}
		markerExists = false
		fmt.Fprintf(stdout, "loop %s: archived stale canary generation before fresh bootstrap\n", rec.ProjectID)
	}
	promotionPending := markerExists &&
		(marker.Status == nativeCanaryPromotionPending ||
			!validNativeCanarySHA256(marker.Canary.RegistryIdentityDigest))
	// Steady autonomous mode has no break-glass path. A fixed-cohort canary is
	// the sole bootstrap from migrated to validated: it remains evidence-gated
	// and cannot broaden its admitted work while the project is unvalidated.
	if !loopPostureAllows(rec.MigrationStatus, len(canaryIDs) > 0, promotionPending) {
		return fail(stderr, fmt.Errorf(
			"loop: project %s has migration status %q (canary promotion pending=%t); steady autonomous mode requires %q with no pending promotion, and fixed-cohort canary bootstrap requires %q",
			rec.ProjectID, rec.MigrationStatus, promotionPending, registry.StatusValidated, registry.StatusMigrated,
		))
	}

	if len(canaryIDs) > 0 {
		target := canaryTargetWidth(canaryIDs, *max)
		var spec *loopsupervisor.CanarySpec
		if admittedSpec != nil {
			spec = admittedSpec
		} else if state.Canary != nil {
			spec, err = resumedNativeCanarySpecWithPolicy(
				rec.ProjectID, rec.Root, canaryIDs, target,
				registryIdentityDigest, state, cfg, enginePolicy,
			)
		} else {
			spec, err = nativeCanarySpecWithPolicy(
				rec.ProjectID, rec.Root, canaryIDs, target,
				registryIdentityDigest, cfg, enginePolicy,
			)
		}
		if err != nil {
			return fail(stderr, err)
		}
		cfg.Canary = spec
	}
	lstore := ledger.NewStore(rec.Root)
	observer := &loopProjectObserver{
		root:        rec.Root,
		bd:          beads.New(rec.Root),
		ledger:      lstore,
		controlPath: store.ControlPath(),
	}
	runner := &loopEngine{
		projectID:          rec.ProjectID,
		root:               rec.Root,
		nativeCanaryCohort: append([]string(nil), canaryIDs...),
		parent:             *parent,
		budget:             *budget,
		defaultModel:       *defaultModel,
		runtimeOnly:        *runtimeOnly,
		runtimeEquivalent:  *runtimeEquivalent,
		autoMerge:          *autoMerge,
		direct:             *direct,
		review:             *review,
		allowAPISpend:      *allowAPISpend,
		noBillingGuard:     *noBillingGuard,
		requireCalibration: *requireCalibration,
		dispatchMode:       *dispatchMode,
		out:                stdout,
	}
	supervisor := &loopsupervisor.Supervisor{
		Config:   cfg,
		Store:    store,
		Observer: observer,
		Engine:   runner,
		Tripwire: runner,
		Drainer:  loopDrainer{store: lstore},
		Maintainer: loopMaintainer{
			root: rec.Root,
		},
		Notifier: loopsupervisor.JSONLNotifier{Store: store},
		Publisher: loopsupervisor.NativeCanaryPublisher{
			RepoRoot: rec.Root,
			Ledger:   lstore,
		},
	}
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stdout, "loop %s: native supervisor starting (auto-merge=%t review=%t)\n", rec.ProjectID, *autoMerge, *review)
	if err := supervisor.Run(sigCtx); err != nil {
		fmt.Fprintln(stderr, "koryph loop:", err)
		if errors.Is(err, loopsupervisor.ErrAlreadyRunning) {
			return engine.ExitUsage
		}
		return engine.ExitFatal
	}
	if cfg.Canary != nil {
		handled, promoted, err := reconcilePassedNativeCanary(
			ctx, reg, rec, store, canaryIDs, canaryTargetWidth(canaryIDs, *max),
		)
		if err != nil {
			return fail(stderr, fmt.Errorf("loop: promote passing canary: %w", err))
		}
		if !handled {
			return fail(stderr, errors.New("loop: canary stopped without a passing immutable decision"))
		}
		if promoted {
			fmt.Fprintf(stdout, "loop %s: promoted migration_status migrated -> validated from immutable passing canary\n", rec.ProjectID)
		}
	}
	return engine.ExitOK
}

func loopPostureAllows(status string, canary, promotionPending bool) bool {
	if promotionPending {
		return false
	}
	return status == registry.StatusValidated ||
		(canary && status == registry.StatusMigrated)
}

const (
	nativeCanaryPromotionMarkerVersion = 3
	nativeCanaryPromotionPending       = "pending"
	nativeCanaryPromotionValidated     = "validated"
	nativeCanaryPromotionMarkerFile    = "canary-promotion.json"
	nativeCanaryHistoryDir             = "canary-history"
	nativeCanaryHistoryVersion         = loopsupervisor.CanaryGenerationArchiveVersion
)

type nativeCanaryPromotionMarker struct {
	SchemaVersion int                        `json:"schema_version"`
	ProjectID     string                     `json:"project_id"`
	Status        string                     `json:"status"`
	Canary        loopsupervisor.CanaryState `json:"canary"`
}

type nativeCanaryHistoryRecord = loopsupervisor.CanaryGenerationArchive

func nativeCanaryPromotionMarkerPath(root string) string {
	return filepath.Join(loopsupervisor.NewStore(root).Root, nativeCanaryPromotionMarkerFile)
}

func loadNativeCanaryPromotionMarker(root string) (nativeCanaryPromotionMarker, bool, error) {
	var marker nativeCanaryPromotionMarker
	loopRoot := loopsupervisor.NewStore(root).Root
	read, err := fsx.ReadRegularConfined(
		filepath.Join(loopRoot, nativeCanaryPromotionMarkerFile), 8<<20, loopRoot,
	)
	if errors.Is(err, os.ErrNotExist) {
		return marker, false, nil
	}
	if err != nil {
		return marker, false, err
	}
	if err := strictjson.Decode(read.Data, &marker); err != nil {
		return marker, false, err
	}
	legacyUnbound := (marker.SchemaVersion == 1 &&
		strings.TrimSpace(marker.Canary.RegistryIdentityDigest) == "") ||
		(marker.SchemaVersion == 2 &&
			strings.TrimSpace(marker.Canary.GenerationDigest) == "")
	if (marker.SchemaVersion != nativeCanaryPromotionMarkerVersion &&
		!legacyUnbound) ||
		marker.ProjectID == "" || marker.Canary.CohortDigest == "" ||
		(!legacyUnbound &&
			!validNativeCanarySHA256(marker.Canary.RegistryIdentityDigest)) ||
		marker.Canary.ReportDigest == "" ||
		(marker.SchemaVersion == nativeCanaryPromotionMarkerVersion &&
			!validNativeCanarySHA256(marker.Canary.GenerationDigest)) ||
		(marker.Status != nativeCanaryPromotionPending &&
			marker.Status != nativeCanaryPromotionValidated) {
		return marker, false, errors.New("loop: invalid canary promotion marker")
	}
	return marker, true, nil
}

func saveNativeCanaryPromotionMarker(root string, marker nativeCanaryPromotionMarker) error {
	marker.SchemaVersion = nativeCanaryPromotionMarkerVersion
	return fsx.WriteJSONAtomicPerm(nativeCanaryPromotionMarkerPath(root), marker, 0o600)
}

func retireStaleNativeCanaryEvidence(
	ctx context.Context,
	root string,
	projectID string,
	current *loopsupervisor.CanarySpec,
) (loopsupervisor.State, bool, error) {
	if current == nil {
		return loopsupervisor.State{}, false, errors.New("current canary generation is required")
	}
	store := loopsupervisor.NewStore(root)
	lock, err := acquireLoopReconciliationLease(ctx, store)
	if err != nil {
		return loopsupervisor.State{}, false, err
	}
	defer lock.Unlock() //nolint:errcheck

	state, err := store.LoadState()
	if err != nil {
		return state, false, err
	}
	marker, markerExists, err := loadNativeCanaryPromotionMarker(root)
	if err != nil {
		return state, false, err
	}
	var canary *loopsupervisor.CanaryState
	durableProjectID := state.ProjectID
	if state.Canary != nil {
		copy := *state.Canary
		canary = &copy
	}
	if markerExists {
		if canary != nil && !sameNativeCanaryIdentity(canary, &marker.Canary) {
			return state, false, errors.New("durable canary state and promotion marker disagree")
		}
		if durableProjectID != "" && marker.ProjectID != durableProjectID {
			return state, false, errors.New("durable canary project and promotion marker disagree")
		}
		if canary == nil {
			copy := marker.Canary
			canary = &copy
		}
		durableProjectID = marker.ProjectID
	}
	if canary == nil {
		return state, false, nil
	}
	if durableProjectID != "" && durableProjectID != projectID {
		return state, false, errors.New("durable canary belongs to a different project")
	}
	if canary.Decision == "passed" || markerExists {
		return state, false, errors.New("passing or publication-pending canary must be reconciled before rollover")
	}
	if canary.Decision != "failed" || canary.EndedAt == "" ||
		canary.ReportDigest == "" || canary.ReportGeneratedAt == "" ||
		canary.PublishedAt == "" || procx.Alive(state.PID) ||
		(state.Mode != loopsupervisor.ModeCircuitOpen &&
			state.Mode != loopsupervisor.ModeStopped) {
		return state, false, errors.New("only a terminal failed canary generation may be archived")
	}

	expectedReportPath := filepath.Join(
		root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
	)
	if filepath.Clean(canary.ReportPath) != filepath.Clean(expectedReportPath) ||
		filepath.Clean(current.ReportPath) != filepath.Clean(expectedReportPath) {
		return state, false, errors.New("canary report path is not the fixed native path")
	}

	// Schema-v3 checkpoints did not persist the policy/generation digests. The
	// one-time upgrade binds their otherwise complete identity to the exact
	// current execution policy; raw cohort/contract/registry fields below must
	// still match, and same-commit rebuilds remain ineligible.
	if canary.AutonomyPolicyDigest == "" {
		canary.AutonomyPolicyDigest = current.AutonomyPolicyDigest
	}
	if canary.ExecutionPolicyDigest == "" {
		canary.ExecutionPolicyDigest = current.ExecutionPolicyDigest
	}
	previousGenerationDigest, err := loopsupervisor.CanaryStateGenerationDigest(
		projectID, canary,
	)
	if err != nil ||
		(canary.GenerationDigest != "" &&
			canary.GenerationDigest != previousGenerationDigest) {
		return state, false, errors.New("stored canary generation identity is invalid")
	}
	canary.GenerationDigest = previousGenerationDigest
	currentGenerationDigest, err := loopsupervisor.CanaryGenerationDigest(projectID, *current)
	if err != nil || current.GenerationDigest != currentGenerationDigest {
		return state, false, errors.New("current canary generation identity is invalid")
	}
	if previousGenerationDigest == currentGenerationDigest {
		return state, false, nil
	}
	if !sameNativeCanaryNonBinaryPolicy(canary, current) {
		return state, false, errors.New("canary cohort, contract, or execution policy drift requires operator resolution")
	}
	registryChanged := canary.RegistryIdentityDigest != current.RegistryIdentityDigest
	binaryUnchanged := canary.InstalledCommit == current.InstalledCommit &&
		canary.BinaryVersion == current.BinaryVersion &&
		canary.BuildIdentity == current.BuildIdentity
	binaryAdvanced := false
	if !binaryUnchanged {
		if canary.InstalledCommit == current.InstalledCommit ||
			canary.BuildIdentity == current.BuildIdentity {
			return state, false, errors.New("same-commit rebuild cannot supersede failed canary evidence")
		}
		if err := authenticateNativeCanaryCheckout(root, current.InstalledCommit); err != nil {
			return state, false, fmt.Errorf("authenticate rollover checkout: %w", err)
		}
		if err := authenticateStrictDescendantCommit(
			root, canary.InstalledCommit, current.InstalledCommit,
		); err != nil {
			return state, false, err
		}
		binaryAdvanced = true
	}
	if !registryChanged && !binaryAdvanced {
		return state, false, nil
	}

	keyMaterial := strings.Join([]string{
		projectID,
		previousGenerationDigest,
		canary.StartedAt,
		canary.ReportDigest,
		canary.ReportPath,
	}, "\x00")
	keySum := sha256.Sum256([]byte(keyMaterial))
	historyKey := hex.EncodeToString(keySum[:])
	loopRoot := store.Root
	historyPath := filepath.Join(loopRoot, nativeCanaryHistoryDir, historyKey+".json")
	archiveDir := filepath.Join(
		root, ".plan-logs", "koryph", "canary", "history", historyKey,
	)
	archiveReportPath := filepath.Join(archiveDir, filepath.Base(expectedReportPath))

	reportPathToLoad := expectedReportPath
	report, err := metrics.LoadAutonomyReport(reportPathToLoad)
	if errors.Is(err, os.ErrNotExist) {
		reportPathToLoad = archiveReportPath
		report, err = metrics.LoadAutonomyReport(reportPathToLoad)
	}
	if err != nil {
		return state, false, fmt.Errorf("authenticate failed canary report: %w", err)
	}
	if err := authenticateRetirableCanaryReport(
		projectID, canary, previousGenerationDigest, report,
	); err != nil {
		return state, false, err
	}

	sourceReport, err := fsx.ReadRegularConfined(expectedReportPath, 64<<20, root)
	sourceReportPresent := true
	if errors.Is(err, os.ErrNotExist) {
		sourceReportPresent = false
		sourceReport, err = fsx.ReadRegularConfined(archiveReportPath, 64<<20, root)
	}
	if err != nil {
		return state, false, fmt.Errorf("read failed canary report for archive: %w", err)
	}
	reportArchiveDigest, err := archiveNativeCanaryFile(
		archiveReportPath, sourceReport, root,
	)
	if err != nil {
		return state, false, fmt.Errorf("archive failed canary report: %w", err)
	}

	stateRead, stateErr := fsx.ReadRegularConfined(store.StatePath(), 16<<20, loopRoot)
	if stateErr != nil {
		return state, false, fmt.Errorf("read failed canary state for archive: %w", stateErr)
	}
	archiveStatePath := filepath.Join(archiveDir, filepath.Base(store.StatePath()))
	stateArchiveDigest, err := archiveNativeCanaryFile(archiveStatePath, stateRead, root)
	if err != nil {
		return state, false, fmt.Errorf("archive failed canary state: %w", err)
	}

	history := nativeCanaryHistoryRecord{
		SchemaVersion:                  nativeCanaryHistoryVersion,
		RetiredAt:                      time.Now().UTC().Format(time.RFC3339Nano),
		Reason:                         nativeCanaryRolloverReason(registryChanged, binaryAdvanced),
		ProjectID:                      projectID,
		PreviousGenerationDigest:       previousGenerationDigest,
		CurrentGenerationDigest:        currentGenerationDigest,
		PreviousRegistryIdentityDigest: canary.RegistryIdentityDigest,
		CurrentRegistryIdentityDigest:  current.RegistryIdentityDigest,
		Canary:                         *canary,
		OriginalStatePath:              store.StatePath(),
		ArchivedStatePath:              archiveStatePath,
		ArchivedStateDigest:            stateArchiveDigest,
		OriginalReportPath:             expectedReportPath,
		ArchivedReportPath:             archiveReportPath,
		ArchivedReportDigest:           reportArchiveDigest,
	}
	if markerExists {
		history.MarkerStatus = marker.Status
	}
	markerPath := nativeCanaryPromotionMarkerPath(root)
	if markerExists {
		markerRead, markerErr := fsx.ReadRegularConfined(markerPath, 8<<20, loopRoot)
		if markerErr != nil {
			return state, false, fmt.Errorf("read canary marker for archive: %w", markerErr)
		}
		archiveMarkerPath := filepath.Join(archiveDir, filepath.Base(markerPath))
		markerDigest, markerErr := archiveNativeCanaryFile(
			archiveMarkerPath, markerRead, root,
		)
		if markerErr != nil {
			return state, false, fmt.Errorf("archive canary marker: %w", markerErr)
		}
		history.OriginalMarkerPath = markerPath
		history.ArchivedMarkerPath = archiveMarkerPath
		history.ArchivedMarkerDigest = markerDigest
	}
	if existing, err := fsx.ReadRegularConfined(historyPath, 16<<20, loopRoot); err == nil {
		var prior nativeCanaryHistoryRecord
		if err := strictjson.Decode(existing.Data, &prior); err != nil ||
			!sameNativeCanaryHistory(prior, history) {
			return state, false, errors.New("stale canary history record is inconsistent")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := fsx.WriteJSONAtomicNoClobberPerm(historyPath, history, 0o600); err != nil {
			return state, false, fmt.Errorf("persist stale canary history: %w", err)
		}
	} else {
		return state, false, fmt.Errorf("load stale canary history: %w", err)
	}

	if sourceReportPresent {
		currentReport, err := fsx.ReadRegularConfined(expectedReportPath, 64<<20, root)
		if err != nil || currentReport.Digest != sourceReport.Digest {
			return state, false, errors.New("failed canary report changed during archive transaction")
		}
		if err := fsx.RemoveDurable(expectedReportPath); err != nil {
			return state, false, fmt.Errorf("retire stale canary report: %w", err)
		}
	}
	if state.Canary != nil {
		state.Canary = nil
		state.Mode = loopsupervisor.ModeStopped
		state.PID = 0
		if err := store.SaveState(state); err != nil {
			return state, false, fmt.Errorf("retire stale canary checkpoint: %w", err)
		}
	}
	if markerExists {
		if err := fsx.RemoveDurable(nativeCanaryPromotionMarkerPath(root)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return state, false, fmt.Errorf("retire stale canary marker: %w", err)
		}
	}
	return state, true, nil
}

func sameNativeCanaryHistory(left, right nativeCanaryHistoryRecord) bool {
	// Retirement time records the first successful manifest publication and
	// naturally differs during crash replay. Every identity, disposition,
	// embedded state field, path, and artifact digest must remain exact.
	left.RetiredAt = ""
	right.RetiredAt = ""
	return reflect.DeepEqual(left, right)
}

func sameNativeCanaryNonBinaryPolicy(
	previous *loopsupervisor.CanaryState,
	current *loopsupervisor.CanarySpec,
) bool {
	if previous == nil || current == nil {
		return false
	}
	previousCohort := append([]string(nil), previous.Cohort...)
	currentCohort := append([]string(nil), current.Cohort...)
	sort.Strings(previousCohort)
	sort.Strings(currentCohort)
	return slices.Equal(previousCohort, currentCohort) &&
		previous.CohortDigest == cohortDigestForNativeCanary(currentCohort) &&
		previous.TargetWidth == current.TargetWidth &&
		previous.InactivityLimitMS == current.InactivityLimit.Milliseconds() &&
		slices.Equal(
			loopsupervisor.CanonicalCanaryHardStops(previous.HardStops),
			loopsupervisor.CanonicalCanaryHardStops(current.HardStops),
		) &&
		previous.ContractDigest == current.ContractDigest &&
		previous.AutonomyPolicyDigest == current.AutonomyPolicyDigest &&
		previous.ExecutionPolicyDigest == current.ExecutionPolicyDigest &&
		filepath.Clean(previous.ReportPath) == filepath.Clean(current.ReportPath)
}

func cohortDigestForNativeCanary(cohort []string) string {
	digest, _ := metrics.AutonomyCohortDigest(cohort)
	return digest
}

func authenticateStrictDescendantCommit(root, previous, current string) error {
	if !validNativeCanaryCommit(previous) || !validNativeCanaryCommit(current) ||
		previous == current {
		return errors.New("canary rollover requires a new clean installed commit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: root, Name: "git",
		Args: []string{"merge-base", "--is-ancestor", previous, current},
	}); err != nil {
		return fmt.Errorf("installed canary commit is not a strict descendant: %w", err)
	}
	return nil
}

func authenticateRetirableCanaryReport(
	projectID string,
	canary *loopsupervisor.CanaryState,
	generationDigest string,
	report *metrics.AutonomyReport,
) error {
	if report == nil || report.Decision.Passed ||
		report.ProjectID != projectID ||
		report.InstalledCommit != canary.InstalledCommit ||
		report.BinaryVersion != canary.BinaryVersion ||
		report.BuildIdentity != canary.BuildIdentity ||
		report.ContractDigest != canary.ContractDigest ||
		report.RegistryIdentityDigest != canary.RegistryIdentityDigest ||
		report.CohortDigest != canary.CohortDigest ||
		report.StartedAt != canary.StartedAt ||
		report.EvidenceDigest != canary.ReportDigest ||
		(report.GenerationDigest != "" &&
			report.GenerationDigest != generationDigest) ||
		(report.ExecutionPolicyDigest != "" &&
			report.ExecutionPolicyDigest != canary.ExecutionPolicyDigest) {
		return errors.New("failed canary report does not match durable generation identity")
	}
	return nil
}

func archiveNativeCanaryFile(
	path string,
	source fsx.ConfinedRead,
	root string,
) (string, error) {
	existing, err := fsx.ReadRegularConfined(path, int64(len(source.Data))+1, root)
	if err == nil {
		if existing.Digest != source.Digest {
			return "", errors.New("existing canary archive differs from source")
		}
		return "sha256:" + existing.Digest, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := fsx.WriteAtomicNoClobber(path, source.Data, 0o600); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		existing, readErr := fsx.ReadRegularConfined(
			path, int64(len(source.Data))+1, root,
		)
		if readErr != nil || existing.Digest != source.Digest {
			return "", errors.New("concurrent canary archive differs from source")
		}
	}
	return "sha256:" + source.Digest, nil
}

func nativeCanaryRolloverReason(registryChanged, binaryAdvanced bool) string {
	switch {
	case registryChanged && binaryAdvanced:
		return "installed binary and registry validation identity changed"
	case binaryAdvanced:
		return "installed binary advanced"
	default:
		return "registry validation identity changed"
	}
}

func sameNativeCanaryIdentity(left, right *loopsupervisor.CanaryState) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.RegistryIdentityDigest == right.RegistryIdentityDigest &&
		left.GenerationDigest == right.GenerationDigest &&
		left.CohortDigest == right.CohortDigest &&
		left.InstalledCommit == right.InstalledCommit &&
		left.BuildIdentity == right.BuildIdentity &&
		left.ContractDigest == right.ContractDigest &&
		left.ExecutionPolicyDigest == right.ExecutionPolicyDigest &&
		left.ReportPath == right.ReportPath &&
		left.ReportDigest == right.ReportDigest
}

func validNativeCanarySHA256(value string) bool {
	raw := strings.TrimPrefix(strings.TrimSpace(value), "sha256:")
	if len(raw) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == sha256.Size
}

type projectRecordStore interface {
	Get(string) (*registry.Record, error)
	CompareAndSetMigrationStatus(
		context.Context,
		string,
		string,
		string,
		string,
	) (*registry.Record, error)
}

type loopStateSaver interface {
	SaveState(loopsupervisor.State) error
}

func reconcilePassedNativeCanary(
	ctx context.Context,
	reg projectRecordStore,
	rec *registry.Record,
	store *loopsupervisor.Store,
	requestedCohort []string,
	requestedTarget int,
) (bool, bool, error) {
	lock, err := acquireLoopReconciliationLease(ctx, store)
	if err != nil {
		return false, false, err
	}
	defer lock.Unlock() //nolint:errcheck

	state, err := store.LoadState()
	if err != nil {
		return false, false, err
	}
	marker, markerExists, err := loadNativeCanaryPromotionMarker(rec.Root)
	if err != nil {
		return false, false, err
	}
	archivedOnly := state.Canary == nil
	if state.Canary == nil {
		if !markerExists {
			return false, false, nil
		}
		archived := marker.Canary
		state.ProjectID = marker.ProjectID
		state.Canary = &archived
	}
	if state.Canary.Decision != "passed" {
		return false, false, nil
	}
	current, err := reg.Get(rec.ProjectID)
	if err != nil {
		return true, false, err
	}
	if archivedOnly && marker.Status == nativeCanaryPromotionValidated {
		if err := validatePassedNativeCanaryRequest(
			current, state, requestedCohort, requestedTarget,
		); err != nil {
			return true, false, err
		}
		if current.MigrationStatus != registry.StatusValidated {
			return true, false, fmt.Errorf(
				"validated canary archive cannot promote current migration status %q",
				current.MigrationStatus,
			)
		}
		return true, false, nil
	}
	promoted, err := completePassedNativeCanary(
		ctx, reg, current, store, state, requestedCohort, requestedTarget,
	)
	return true, promoted, err
}

func acquireLoopReconciliationLease(
	ctx context.Context,
	store *loopsupervisor.Store,
) (*loopsupervisor.Lock, error) {
	for {
		lock, err := store.Acquire()
		if !errors.Is(err, loopsupervisor.ErrAlreadyRunning) {
			return lock, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func completePassedNativeCanary(
	ctx context.Context,
	reg projectRecordStore,
	rec *registry.Record,
	store loopStateSaver,
	state loopsupervisor.State,
	requestedCohort []string,
	requestedTarget int,
) (bool, error) {
	if err := validatePassedNativeCanaryRequest(
		rec, state, requestedCohort, requestedTarget,
	); err != nil {
		return false, err
	}
	marker, markerExists, err := loadNativeCanaryPromotionMarker(rec.Root)
	if err != nil {
		return false, err
	}
	if markerExists && (marker.ProjectID != state.ProjectID ||
		marker.Canary.CohortDigest != state.Canary.CohortDigest ||
		marker.Canary.ReportDigest != state.Canary.ReportDigest) {
		return false, errors.New("canary promotion marker identity differs from completed canary")
	}
	recoveringPending := markerExists && marker.Status == nativeCanaryPromotionPending
	if err := saveNativeCanaryPromotionMarker(rec.Root, nativeCanaryPromotionMarker{
		ProjectID: state.ProjectID,
		Status:    nativeCanaryPromotionPending,
		Canary:    *state.Canary,
	}); err != nil {
		return false, fmt.Errorf("persist fail-closed canary promotion marker: %w", err)
	}

	promoted := false
	switch rec.MigrationStatus {
	case registry.StatusMigrated:
		updated, updateErr := reg.CompareAndSetMigrationStatus(
			ctx,
			rec.ProjectID,
			registry.StatusMigrated,
			registry.StatusValidated,
			state.Canary.RegistryIdentityDigest,
		)
		err = updateErr
		if err != nil {
			return false, fmt.Errorf("save validated registry posture: %w", err)
		}
		rec.MigrationStatus = updated.MigrationStatus
		promoted = true
	case registry.StatusValidated:
		// A prior registry Save may have persisted validated before returning
		// an audit/commit error. The pending marker keeps steady mode closed;
		// the immutable report above authenticates this recovery.
		if recoveringPending {
			if _, err := reg.CompareAndSetMigrationStatus(
				ctx,
				rec.ProjectID,
				registry.StatusValidated,
				registry.StatusValidated,
				state.Canary.RegistryIdentityDigest,
			); err != nil {
				return false, fmt.Errorf("complete validated registry audit and commit: %w", err)
			}
		}
	default:
		return false, fmt.Errorf(
			"completed canary cannot promote migration status %q", rec.MigrationStatus,
		)
	}

	completed := *state.Canary
	state.Canary = nil
	state.Mode = loopsupervisor.ModeStopped
	state.PID = 0
	if err := store.SaveState(state); err != nil {
		return false, fmt.Errorf("finalize steady-mode supervisor state: %w", err)
	}
	if err := saveNativeCanaryPromotionMarker(rec.Root, nativeCanaryPromotionMarker{
		ProjectID: state.ProjectID,
		Status:    nativeCanaryPromotionValidated,
		Canary:    completed,
	}); err != nil {
		return false, fmt.Errorf("resolve canary promotion marker: %w", err)
	}
	return promoted, nil
}

func validatePassedNativeCanaryRequest(
	rec *registry.Record,
	state loopsupervisor.State,
	requestedCohort []string,
	requestedTarget int,
) error {
	if state.Canary == nil || state.Canary.Decision != "passed" {
		return errors.New("native canary has no completed passing decision")
	}
	requestedDigest, err := metrics.AutonomyCohortDigest(requestedCohort)
	if err != nil || requestedDigest != state.Canary.CohortDigest ||
		requestedTarget != state.Canary.TargetWidth {
		return loopsupervisor.ErrCanaryCohortDrift
	}
	currentRegistryDigest, err := registry.ValidationIdentityDigest(rec)
	if err != nil || currentRegistryDigest != state.Canary.RegistryIdentityDigest {
		return errors.New("passing canary registry identity is stale")
	}
	return authenticatePassedNativeCanary(rec, state)
}

func authenticatePassedNativeCanary(
	rec *registry.Record,
	state loopsupervisor.State,
) error {
	if rec == nil || state.Canary == nil || state.Canary.Decision != "passed" {
		return errors.New("native canary has no completed passing decision")
	}
	canary := state.Canary
	if state.ProjectID != rec.ProjectID || canary.HardStop != "" {
		return errors.New("passing canary state identity is inconsistent")
	}
	reportPath := filepath.Join(
		rec.Root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
	)
	if filepath.Clean(canary.ReportPath) != filepath.Clean(reportPath) {
		return errors.New("passing canary report path is not the fixed native path")
	}
	expected, err := nativeCanaryReportExpectation(state.ProjectID, canary)
	if err != nil {
		return err
	}
	report, err := loadExpectedAutonomyReport(reportPath, expected)
	if err != nil {
		return fmt.Errorf("authenticate immutable report: %w", err)
	}
	if !report.Decision.Passed {
		return errors.New("immutable canary report decision did not pass")
	}
	return nil
}

func nativeCanaryReportExpectation(
	projectID string,
	canary *loopsupervisor.CanaryState,
) (metrics.AutonomyReportExpectation, error) {
	if canary == nil {
		return metrics.AutonomyReportExpectation{}, errors.New("missing native canary identity")
	}
	cohortDigest, err := metrics.AutonomyCohortDigest(canary.Cohort)
	if err != nil || cohortDigest != canary.CohortDigest {
		return metrics.AutonomyReportExpectation{}, errors.New("passing canary cohort identity is invalid")
	}
	freshAt, err := time.Parse(time.RFC3339Nano, canary.PublishedAt)
	if err != nil {
		return metrics.AutonomyReportExpectation{}, errors.New("passing canary publication time is invalid")
	}
	return metrics.AutonomyReportExpectation{
		ProjectID:              projectID,
		InstalledCommit:        canary.InstalledCommit,
		BinaryVersion:          canary.BinaryVersion,
		BuildIdentity:          canary.BuildIdentity,
		ContractDigest:         canary.ContractDigest,
		RegistryIdentityDigest: canary.RegistryIdentityDigest,
		GenerationDigest:       canary.GenerationDigest,
		ExecutionPolicyDigest:  canary.ExecutionPolicyDigest,
		Cohort:                 canary.Cohort,
		CohortDigest:           cohortDigest,
		Thresholds:             metrics.DefaultAutonomyThresholds(),
		CanaryStartedAt:        canary.StartedAt,
		EvidenceDigest:         canary.ReportDigest,
		GeneratedAt:            canary.ReportGeneratedAt,
		FreshAt:                freshAt,
		MaxAge:                 autonomyDoctorMaxAge,
		MaxFutureSkew:          autonomyDoctorMaxFutureSkew,
	}, nil
}

func cmdLoopStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("loop status", stderr)
	projectID := fs.String("project", "", "project id (default: project containing the current directory)")
	asJSON := fs.Bool("json", false, "emit supervisor state as JSON")
	setUsage(fs, stdout, "show durable native-loop supervisor state", "[--project ID] [--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	rec, code := resolveLoopProject(*projectID, "loop status", stderr)
	if code != 0 {
		return code
	}
	state, err := loopsupervisor.NewStore(rec.Root).LoadState()
	if err != nil {
		return fail(stderr, err)
	}
	if state.ProjectID == "" {
		fmt.Fprintf(stdout, "loop %s: never started\n", rec.ProjectID)
		return 0
	}
	if *asJSON {
		if err := printJSON(stdout, state); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "loop %s: %s", state.ProjectID, state.Mode)
	if state.PID > 0 {
		fmt.Fprintf(stdout, " pid=%d", state.PID)
	}
	if state.CurrentRunID != "" {
		fmt.Fprintf(stdout, " run=%s", state.CurrentRunID)
	}
	if state.CircuitReason != "" {
		fmt.Fprintf(stdout, " circuit=%q", state.CircuitReason)
	}
	if state.Canary != nil {
		fmt.Fprintf(stdout, " canary-width=%d/%d good-streak=%d",
			state.Canary.CurrentWidth, state.Canary.TargetWidth, state.Canary.ConsecutiveGood)
		decision := state.Canary.Decision
		if decision == "" {
			decision = "pending"
		}
		fmt.Fprintf(stdout, " report=%s decision=%s",
			state.Canary.ReportPath, decision)
	}
	fmt.Fprintln(stdout)
	return 0
}

func cmdLoopControl(args []string, stdout, stderr io.Writer, drain bool) int {
	name := "loop stop"
	purpose := "request a graceful supervisor stop"
	if drain {
		name = "loop drain"
		purpose = "drain the active engine and stop the supervisor"
	}
	fs := newFlagSet(name, stderr)
	projectID := fs.String("project", "", "project id (default: project containing the current directory)")
	setUsage(fs, stdout, purpose, "[--project ID]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	rec, code := resolveLoopProject(*projectID, name, stderr)
	if code != 0 {
		return code
	}
	store := loopsupervisor.NewStore(rec.Root)
	var err error
	if drain {
		err = store.RequestDrain()
	} else {
		err = store.RequestStop()
	}
	if err == nil {
		// Reuse the engine's live drain sidecar. If an engine is active it
		// observes this at its next scheduling boundary; if idle, the
		// supervisor consumes its own control sidecar without starting one.
		err = ledger.NewStore(rec.Root).RequestDrain()
	}
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "%s requested for %s\n", map[bool]string{true: "drain", false: "stop"}[drain], rec.ProjectID)
	return 0
}

func cmdLoopInject(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("loop inject", stderr)
	projectID := fs.String("project", "", "project id (default: project containing the current directory)")
	setUsage(fs, stdout, "inject one bead into the native loop frontier", "[--project ID] <bead-id>")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 1 {
		return usageErr(stderr, "loop inject: exactly one <bead-id> is required")
	}
	rec, code := resolveLoopProject(*projectID, "loop inject", stderr)
	if code != 0 {
		return code
	}
	bd := beads.New(rec.Root)
	if _, err := bd.Show(context.Background(), pos[0]); err != nil {
		return fail(stderr, fmt.Errorf("loop inject: bead %s not found: %w", pos[0], err))
	}
	store := loopsupervisor.NewStore(rec.Root)
	state, err := store.LoadState()
	if err != nil {
		return fail(stderr, err)
	}
	if state.Canary != nil {
		return fail(stderr, errors.New("loop inject: operator intervention is forbidden during a fixed canary"))
	}
	if err := store.RequestInject(pos[0]); err != nil {
		return fail(stderr, err)
	}
	// Also use the established live-engine override sidecar when a run exists.
	// Failure is harmless: the supervisor control remains durable for the next
	// observation, including while no engine run exists.
	lstore := ledger.NewStore(rec.Root)
	if run, loadErr := lstore.LoadLatest(); loadErr == nil && run != nil {
		_ = lstore.RecordInjection(run.RunID, pos[0])
	}
	fmt.Fprintf(stdout, "injected %s into loop %s\n", pos[0], rec.ProjectID)
	return 0
}

func resolveLoopProject(projectID, command string, stderr io.Writer) (*registry.Record, int) {
	store, err := openStore(context.Background())
	if err != nil {
		return nil, fail(stderr, err)
	}
	return resolveProjectRecordCwd(stderr, store, projectID, command)
}

type loopProjectObserver struct {
	root        string
	bd          *beads.Adapter
	ledger      *ledger.Store
	controlPath string
}

func (o *loopProjectObserver) Observe(ctx context.Context, scope loopsupervisor.Scope) (loopsupervisor.Observation, error) {
	run, loadErr := o.ledger.LoadLatest()
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return loopsupervisor.Observation{}, fmt.Errorf("loop: load latest recovery ledger: %w", loadErr)
	}
	allow := idSet(scope.FixedCohort)
	active := map[string]bool{}
	observation := loopsupervisor.Observation{DrainRequested: o.ledger.DrainRequested()}
	if run != nil {
		observation.WakeToken = run.RunID + ":" + run.UpdatedAt
		for id, slot := range run.Slots {
			if slot == nil || ledger.Terminal(slot.Status) {
				continue
			}
			if len(allow) > 0 && !allow[id] {
				continue
			}
			active[id] = true
			observation.Recovery = true
			observation.RecoveryRunID = run.RunID
		}
	}
	// A stale in-progress Bead may outlive the latest resumable ledger (for
	// example, an older wrapper-created run became non-latest). It is still
	// recovery work, not idle. Trigger a fresh engine transaction so its
	// existing reconcileOrphans path can restore the claim before scanning.
	all, listErr := o.bd.List(ctx)
	if listErr != nil {
		return loopsupervisor.Observation{}, listErr
	}
	for _, issue := range all {
		if len(allow) > 0 && !allow[issue.ID] {
			continue
		}
		if issue.Status == "in_progress" && !active[issue.ID] {
			observation.Reconcile = true
			break
		}
	}

	ready, err := o.bd.Ready(ctx, beads.ReadyOpts{Parent: scope.Parent})
	if err != nil {
		return loopsupervisor.Observation{}, err
	}
	// Injections can be outside a parent scope, so fold ready injected items
	// from the unscoped frontier without mutating Beads.
	if scope.Parent != "" && len(scope.PendingInjections) > 0 {
		global, globalErr := o.bd.Ready(ctx, beads.ReadyOpts{})
		if globalErr != nil {
			return loopsupervisor.Observation{}, globalErr
		}
		for _, issue := range global {
			if containsID(scope.PendingInjections, issue.ID) && !containsIssue(ready, issue.ID) {
				ready = append(ready, issue)
			}
		}
	}
	for _, issue := range ready {
		if len(allow) > 0 && !allow[issue.ID] {
			continue
		}
		if ok, _ := sched.Eligible(issue, active); !ok {
			continue
		}
		children, childErr := o.bd.ListChildren(ctx, issue.ID)
		if childErr != nil {
			return loopsupervisor.Observation{}, childErr
		}
		if len(children) > 0 {
			continue
		}
		observation.ReadyIDs = append(observation.ReadyIDs, issue.ID)
	}
	sort.Strings(observation.ReadyIDs)
	sum := sha256.Sum256([]byte(observation.WakeToken + "\x00" + strings.Join(observation.ReadyIDs, "\x00") +
		"\x00" + strings.Join(scope.PendingInjections, "\x00") + "\x00" +
		strconv.FormatBool(observation.DrainRequested) + "\x00" + strconv.FormatBool(observation.Reconcile)))
	observation.WakeToken = hex.EncodeToString(sum[:])
	return observation, nil
}

// Wait performs a zero-model filesystem wake. It polls only small control and
// top-level task/ledger metadata, returning at the bounded idle deadline even
// on filesystems that do not surface directory mtimes for nested writes.
func (o *loopProjectObserver) Wait(ctx context.Context, _ loopsupervisor.Scope, previous loopsupervisor.Observation, max time.Duration) error {
	deadline := time.NewTimer(max)
	defer deadline.Stop()
	interval := time.Second
	if max < interval {
		interval = max
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	before := o.fileWakeToken()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		case <-ticker.C:
			if o.fileWakeToken() != before || o.ledger.DrainRequested() || previous.WakeToken == "" {
				return nil
			}
		}
	}
}

func (o *loopProjectObserver) fileWakeToken() string {
	var parts []string
	for _, path := range []string{
		filepath.Join(o.root, ".beads"),
		o.ledger.KoryphRoot,
		o.controlPath,
	} {
		info, err := os.Stat(path)
		if err == nil {
			parts = append(parts, path, info.ModTime().UTC().Format(time.RFC3339Nano), strconv.FormatInt(info.Size(), 10))
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

type loopEngine struct {
	projectID          string
	root               string
	nativeCanaryCohort []string
	parent             string
	budget             float64
	defaultModel       string
	runtimeOnly        string
	runtimeEquivalent  string
	autoMerge          bool
	direct             bool
	review             bool
	allowAPISpend      bool
	noBillingGuard     bool
	requireCalibration bool
	dispatchMode       string
	out                io.Writer
	run                func(context.Context, engine.Options) (engine.Outcome, error)

	tripwireMu   sync.Mutex
	tripwireNext uint64
	tripwireSubs map[uint64]chan loopsupervisor.Tripwire
}

func (e *loopEngine) Run(ctx context.Context, req loopsupervisor.RunRequest) (loopsupervisor.RunResult, error) {
	only := req.Only
	nativeCanary := len(e.nativeCanaryCohort) > 0
	if nativeCanary && !fixedCanaryRequestMatches(e.nativeCanaryCohort, req) {
		return loopsupervisor.RunResult{Code: engine.ExitFatal},
			errors.New("loop: native canary engine request escaped its immutable cohort")
	}
	run := e.run
	if run == nil {
		run = engine.Run
	}
	outcome, err := run(ctx, engine.Options{
		ProjectID:          e.projectID,
		Max:                req.Max,
		Once:               true,
		Resume:             req.Resume,
		RecoveryRunID:      req.RecoveryRunID,
		Parent:             e.parent,
		Only:               only,
		AllowedIDs:         append([]string(nil), req.AllowedIDs...),
		AuthoritativeWidth: req.AuthoritativeWidth,
		HardStopKinds:      engineHardStops(req.HardStops),
		BudgetUSD:          e.budget,
		DefaultModel:       e.defaultModel,
		RuntimeOnly:        e.runtimeOnly,
		RuntimeEquivalent:  e.runtimeEquivalent,
		AutoMerge:          e.autoMerge,
		Direct:             e.direct,
		Review:             e.review,
		AllowAPISpend:      e.allowAPISpend,
		NativeCanary:       nativeCanary,
		NoBillingGuard:     e.noBillingGuard,
		RequireCalibration: e.requireCalibration,
		DispatchMode:       e.dispatchMode,
		Out:                e.out,
		OnRunStart:         req.OnRunStart,
		OnSafetyTripwire:   e.publishTripwire,
	})
	result := loopsupervisor.RunResult{
		RunID:      outcome.RunID,
		Code:       outcome.Code,
		Reason:     outcome.Reason,
		Dispatched: outcome.Dispatched,
		Containment: loopsupervisor.ContainmentResult{
			Required:         outcome.Containment.Required,
			Complete:         outcome.Containment.Complete,
			ActiveWorkers:    outcome.Containment.ActiveWorkers,
			NonTerminalSlots: outcome.Containment.NonTerminalSlots,
			RemainingLeases:  outcome.Containment.RemainingLeases,
			Error:            outcome.Containment.Error,
		},
	}
	if stat, memErr := sysmem.Available(); memErr == nil {
		result.PressureNormal = stat.Pressure == sysmem.PressureNormal
		sampledAt := stat.SampledAt
		if sampledAt.IsZero() {
			sampledAt = time.Now().UTC()
		}
		level := stat.Pressure.String()
		if !stat.Pressure.Known() {
			level = "warning"
		}
		result.Pressure = loopsupervisor.PressureSample{
			At:    sampledAt.UTC().Format(time.RFC3339Nano),
			Level: level, AvailableMB: int(stat.AvailableMB()),
		}
	}
	if run, loadErr := ledger.NewStore(e.root).LoadRun(outcome.RunID); loadErr == nil {
		if run.TokenSemantics != ledger.CurrentTokenSemantics {
			result.HardStop = "token-semantics-inconsistent"
		}
		for id, slot := range run.Slots {
			if slot == nil || !ledger.Terminal(slot.Status) {
				continue
			}
			result.Terminal = append(result.Terminal, loopsupervisor.TerminalOutcome{
				BeadID: id,
				Status: slot.Status,
				Good:   terminalHasFullQualityEvidence(ctx, e.root, run, slot, e.review),
			})
		}
		sort.Slice(result.Terminal, func(i, j int) bool { return result.Terminal[i].BeadID < result.Terminal[j].BeadID })
	}
	return result, err
}

func fixedCanaryRequestMatches(
	cohort []string,
	req loopsupervisor.RunRequest,
) bool {
	expected := splitIDs(strings.Join(cohort, ","))
	actual := splitIDs(strings.Join(req.AllowedIDs, ","))
	if len(expected) < 2 || len(actual) != len(expected) ||
		!req.AuthoritativeWidth || req.Max < 2 || req.Max > len(expected) {
		return false
	}
	for i := range expected {
		if expected[i] != actual[i] {
			return false
		}
	}
	if req.Only == "" {
		return true
	}
	for _, id := range expected {
		if id == req.Only {
			return true
		}
	}
	return false
}

type nativeCanaryEnginePolicy struct {
	Parent             string  `json:"parent,omitempty"`
	Budget             float64 `json:"budget"`
	DefaultModel       string  `json:"default_model,omitempty"`
	RuntimeOnly        string  `json:"runtime_only,omitempty"`
	RuntimeEquivalent  string  `json:"runtime_equivalent,omitempty"`
	AutoMerge          bool    `json:"auto_merge"`
	Direct             bool    `json:"direct"`
	Review             bool    `json:"review"`
	AllowAPISpend      bool    `json:"allow_api_spend"`
	NoBillingGuard     bool    `json:"no_billing_guard"`
	RequireCalibration bool    `json:"require_calibration"`
	DispatchMode       string  `json:"dispatch_mode,omitempty"`
}

type nativeCanaryExecutionPolicy struct {
	SchemaVersion        string                          `json:"schema_version"`
	Supervisor           loopsupervisor.SupervisorPolicy `json:"supervisor"`
	Engine               nativeCanaryEnginePolicy        `json:"engine"`
	TargetWidth          int                             `json:"target_width"`
	InactivityLimitMS    int64                           `json:"inactivity_limit_ms"`
	HardStops            []string                        `json:"hard_stops"`
	ReportPath           string                          `json:"report_path"`
	AutonomyPolicyDigest string                          `json:"autonomy_policy_digest"`
}

func nativeCanaryExecutionPolicyDigest(
	config loopsupervisor.Config,
	spec loopsupervisor.CanarySpec,
	enginePolicy nativeCanaryEnginePolicy,
	autonomyPolicyDigest string,
) (string, error) {
	policy := nativeCanaryExecutionPolicy{
		SchemaVersion:        "koryph.canary-execution-policy/v1",
		Supervisor:           loopsupervisor.EffectiveSupervisorPolicy(config),
		Engine:               enginePolicy,
		TargetWidth:          spec.TargetWidth,
		InactivityLimitMS:    spec.InactivityLimit.Milliseconds(),
		HardStops:            append([]string(nil), spec.HardStops...),
		ReportPath:           filepath.Clean(spec.ReportPath),
		AutonomyPolicyDigest: autonomyPolicyDigest,
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// nativeCanarySpec keeps the narrow package-test seam; production admission
// always uses nativeCanarySpecWithPolicy with the resolved project and command
// policy.
func nativeCanarySpec(
	root string,
	cohort []string,
	target int,
	registryIdentityDigest string,
) (*loopsupervisor.CanarySpec, error) {
	config := loopsupervisor.Config{ProjectID: "demo"}
	return nativeCanarySpecWithPolicy(
		"demo", root, cohort, target, registryIdentityDigest, config,
		nativeCanaryEnginePolicy{AutoMerge: true, Review: true},
	)
}

func nativeCanarySpecWithPolicy(
	projectID string,
	root string,
	cohort []string,
	target int,
	registryIdentityDigest string,
	config loopsupervisor.Config,
	enginePolicy nativeCanaryEnginePolicy,
) (*loopsupervisor.CanarySpec, error) {
	commit := strings.TrimSpace(loopInstalledCommit())
	if !validNativeCanaryCommit(commit) {
		return nil, errors.New("loop: canary requires an installed binary with a clean source commit")
	}
	if err := authenticateNativeCanaryCheckout(root, commit); err != nil {
		return nil, err
	}
	contract, err := fsx.ReadRegularConfined(filepath.Join(root, "AGENTS.md"), 1<<20, root)
	if err != nil {
		return nil, fmt.Errorf("loop: authenticate AGENTS.md: %w", err)
	}
	reportPath, err := filepath.Abs(filepath.Join(
		root, filepath.FromSlash(".plan-logs/koryph/canary/autonomous-loop-reliability.json"),
	))
	if err != nil {
		return nil, err
	}
	buildIdentity, err := version.BuildIdentity(loopBinaryVersion())
	if err != nil {
		return nil, fmt.Errorf("loop: authenticate installed binary: %w", err)
	}
	autonomyPolicyDigest, err := metrics.AutonomyThresholdsDigest(
		metrics.DefaultAutonomyThresholds(),
	)
	if err != nil {
		return nil, fmt.Errorf("loop: authenticate autonomy thresholds: %w", err)
	}
	spec := &loopsupervisor.CanarySpec{
		Cohort: cohort, TargetWidth: target,
		InactivityLimit:        loopsupervisor.DefaultCanaryInactivityLimit,
		HardStops:              loopsupervisor.CanonicalCanaryHardStops(nil),
		InstalledCommit:        commit,
		BinaryVersion:          loopBinaryVersion(),
		BuildIdentity:          buildIdentity,
		ContractDigest:         "sha256:" + contract.Digest,
		RegistryIdentityDigest: registryIdentityDigest,
		AutonomyPolicyDigest:   autonomyPolicyDigest,
		ReportPath:             reportPath,
	}
	spec.ExecutionPolicyDigest, err = nativeCanaryExecutionPolicyDigest(
		config, *spec, enginePolicy, autonomyPolicyDigest,
	)
	if err != nil {
		return nil, fmt.Errorf("loop: authenticate canary execution policy: %w", err)
	}
	spec.GenerationDigest, err = loopsupervisor.CanaryGenerationDigest(projectID, *spec)
	if err != nil {
		return nil, err
	}
	return spec, nil
}

func resumedNativeCanarySpec(
	root string,
	cohort []string,
	target int,
	registryIdentityDigest string,
	state loopsupervisor.State,
) (*loopsupervisor.CanarySpec, error) {
	projectID := strings.TrimSpace(state.ProjectID)
	if projectID == "" {
		projectID = "demo"
	}
	config := loopsupervisor.Config{ProjectID: projectID}
	return resumedNativeCanarySpecWithPolicy(
		projectID, root, cohort, target, registryIdentityDigest, state, config,
		nativeCanaryEnginePolicy{AutoMerge: true, Review: true},
	)
}

func resumedNativeCanarySpecWithPolicy(
	projectID string,
	root string,
	cohort []string,
	target int,
	registryIdentityDigest string,
	state loopsupervisor.State,
	config loopsupervisor.Config,
	enginePolicy nativeCanaryEnginePolicy,
) (*loopsupervisor.CanarySpec, error) {
	if state.Canary == nil {
		return nil, errors.New("loop: no durable canary identity to resume")
	}
	canary := state.Canary
	digest, err := metrics.AutonomyCohortDigest(cohort)
	if err != nil || digest != canary.CohortDigest || target != canary.TargetWidth {
		return nil, loopsupervisor.ErrCanaryCohortDrift
	}
	commit := strings.TrimSpace(loopInstalledCommit())
	binaryVersion := strings.TrimSpace(loopBinaryVersion())
	buildIdentity, err := version.BuildIdentity(binaryVersion)
	if err != nil {
		return nil, fmt.Errorf("loop: authenticate installed binary: %w", err)
	}
	if commit != canary.InstalledCommit ||
		binaryVersion != canary.BinaryVersion ||
		buildIdentity != canary.BuildIdentity {
		return nil, loopsupervisor.ErrCanaryIdentityDrift
	}
	if registryIdentityDigest != canary.RegistryIdentityDigest {
		return nil, loopsupervisor.ErrCanaryIdentityDrift
	}
	if err := authenticateNativeCanaryResumeCheckout(root, commit); err != nil {
		return nil, err
	}
	contract, err := fsx.ReadRegularConfined(filepath.Join(root, "AGENTS.md"), 1<<20, root)
	if err != nil {
		return nil, fmt.Errorf("loop: authenticate AGENTS.md: %w", err)
	}
	reportPath, err := filepath.Abs(filepath.Join(
		root, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath),
	))
	if err != nil {
		return nil, err
	}
	if canary.ContractDigest != "sha256:"+contract.Digest ||
		filepath.Clean(canary.ReportPath) != filepath.Clean(reportPath) {
		return nil, loopsupervisor.ErrCanaryIdentityDrift
	}
	spec := &loopsupervisor.CanarySpec{
		Cohort:                 cohort,
		TargetWidth:            target,
		InactivityLimit:        time.Duration(canary.InactivityLimitMS) * time.Millisecond,
		HardStops:              canary.HardStops,
		InstalledCommit:        canary.InstalledCommit,
		BinaryVersion:          canary.BinaryVersion,
		BuildIdentity:          canary.BuildIdentity,
		ContractDigest:         canary.ContractDigest,
		RegistryIdentityDigest: canary.RegistryIdentityDigest,
		AutonomyPolicyDigest:   canary.AutonomyPolicyDigest,
		ExecutionPolicyDigest:  canary.ExecutionPolicyDigest,
		GenerationDigest:       canary.GenerationDigest,
		ReportPath:             canary.ReportPath,
	}
	expectedAutonomyPolicy, err := metrics.AutonomyThresholdsDigest(
		metrics.DefaultAutonomyThresholds(),
	)
	if err != nil || expectedAutonomyPolicy != spec.AutonomyPolicyDigest {
		return nil, loopsupervisor.ErrCanaryIdentityDrift
	}
	expectedExecutionPolicy, err := nativeCanaryExecutionPolicyDigest(
		config, *spec, enginePolicy, expectedAutonomyPolicy,
	)
	if err != nil || expectedExecutionPolicy != spec.ExecutionPolicyDigest {
		return nil, loopsupervisor.ErrCanaryIdentityDrift
	}
	expectedGeneration, err := loopsupervisor.CanaryGenerationDigest(projectID, *spec)
	if err != nil || expectedGeneration != spec.GenerationDigest {
		return nil, loopsupervisor.ErrCanaryIdentityDrift
	}
	return spec, nil
}

func authenticateNativeCanaryResumeCheckout(root, installedCommit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: root, Name: "git",
		Args: []string{"status", "--porcelain=v1", "--untracked-files=all"},
	})
	if err != nil {
		return fmt.Errorf("loop: authenticate clean resumed canary checkout: %w", err)
	}
	if nativeCanaryCheckoutDirty(status.Stdout) {
		return errors.New("loop: resumed canary checkout has tracked or untracked changes")
	}
	if _, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: root, Name: "git",
		Args: []string{"merge-base", "--is-ancestor", installedCommit, "HEAD"},
	}); err != nil {
		return fmt.Errorf(
			"loop: resumed canary checkout HEAD is not descended from installed commit: %w",
			err,
		)
	}
	return nil
}

func validNativeCanaryCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(commit)
	return err == nil && len(decoded)*2 == len(commit) && commit == strings.ToLower(commit)
}

func authenticateNativeCanaryCheckout(root, installedCommit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	head, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: root, Name: "git", Args: []string{"rev-parse", "--verify", "HEAD^{commit}"},
	})
	if err != nil {
		return fmt.Errorf("loop: authenticate canary checkout HEAD: %w", err)
	}
	if strings.TrimSpace(head.Stdout) != installedCommit {
		return errors.New("loop: canary installed binary commit does not match checkout HEAD")
	}
	status, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: root, Name: "git",
		Args: []string{"status", "--porcelain=v1", "--untracked-files=all"},
	})
	if err != nil {
		return fmt.Errorf("loop: authenticate clean canary checkout: %w", err)
	}
	if nativeCanaryCheckoutDirty(status.Stdout) {
		return errors.New("loop: canary checkout has tracked or untracked changes")
	}
	return nil
}

func nativeCanaryCheckoutDirty(status string) bool {
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		if line == "" {
			continue
		}
		if len(line) < 4 || line[:2] != "??" {
			return true
		}
		path := filepath.ToSlash(strings.TrimSpace(line[3:]))
		if path != ".koryph" && !strings.HasPrefix(path, ".koryph/") &&
			path != ".plan-logs/koryph" &&
			!strings.HasPrefix(path, ".plan-logs/koryph/") {
			return true
		}
	}
	return false
}

func engineHardStops(kinds []string) []engine.SafetyTripwireKind {
	out := make([]engine.SafetyTripwireKind, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, engine.SafetyTripwireKind(kind))
	}
	return out
}

func terminalHasFullQualityEvidence(
	ctx context.Context,
	root string,
	run *ledger.Run,
	slot *ledger.Slot,
	requireReview bool,
) bool {
	if run == nil || slot == nil || slot.Status != ledger.SlotMerged ||
		slot.CandidateGeneration == "" || slot.CandidateResultPath == "" ||
		slot.DispatchGeneration == "" || slot.DispatchBaseSHA == "" ||
		!slot.CompletionAccounted || slot.GateEvidencePath == "" ||
		slot.GateEvidenceDigest == "" {
		return false
	}
	store := ledger.NewStore(root)
	phaseDir := filepath.Join(store.KoryphRoot, run.RunID, slot.PhaseID)
	if filepath.Clean(slot.CandidateResultPath) != phasecontrol.ResultPath(phaseDir) {
		return false
	}
	result, err := phasecontrol.LoadResult(phaseDir)
	if err != nil || result.State != "done" || result.RunID != run.RunID ||
		result.PhaseID != slot.PhaseID ||
		result.Generation != slot.CandidateGeneration ||
		result.Generation != slot.DispatchGeneration ||
		result.BaseSHA != slot.DispatchBaseSHA ||
		result.CommitCount < 1 || !result.WorktreeClean {
		return false
	}
	evidenceDir := store.EvidenceDir(run.RunID, slot.PhaseID)
	gateRead, err := fsx.ReadRegularConfined(slot.GateEvidencePath, 64<<10, evidenceDir)
	if err != nil || "sha256:"+gateRead.Digest != slot.GateEvidenceDigest {
		return false
	}
	var gate merge.GateEvidence
	if err := json.Unmarshal(gateRead.Data, &gate); err != nil ||
		merge.ValidateGateEvidence(&gate) != nil {
		return false
	}
	if !requireReview {
		return true
	}
	if slot.GeneralReviewArtifactPath == "" ||
		slot.GeneralReviewArtifactDigest == "" ||
		slot.GeneralReviewCandidateSHA != gate.CandidateSHA ||
		slot.GeneralReviewBaseSHA != gate.BaseSHA {
		return false
	}
	general, ok := authenticatedTerminalReview(
		evidenceDir,
		slot.GeneralReviewArtifactPath,
		slot.GeneralReviewArtifactDigest,
		review.ReviewKindGeneral,
		gate.CandidateSHA,
		gate.BaseSHA,
	)
	if !ok || general.Verdict.Blocking || general.Verdict.Degraded {
		return false
	}
	securityRequired, authenticated := terminalSecurityReviewRequired(ctx, root, slot, &gate, general)
	if !authenticated {
		return false
	}
	if !securityRequired {
		return true
	}
	if slot.SecurityReviewArtifactPath == "" ||
		slot.SecurityReviewArtifactDigest == "" ||
		slot.SecurityReviewCandidateSHA != gate.CandidateSHA ||
		slot.SecurityReviewBaseSHA != gate.BaseSHA {
		return false
	}
	security, ok := authenticatedTerminalReview(
		evidenceDir,
		slot.SecurityReviewArtifactPath,
		slot.SecurityReviewArtifactDigest,
		review.ReviewKindSecurity,
		gate.CandidateSHA,
		gate.BaseSHA,
	)
	return ok && !security.Verdict.Blocking && !security.Verdict.Degraded
}

func authenticatedTerminalReview(
	evidenceDir, path, digest, kind, candidateSHA, baseSHA string,
) (review.Artifact, bool) {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(digest) == "" {
		return review.Artifact{}, false
	}
	read, err := fsx.ReadRegularConfined(path, 1<<20, evidenceDir)
	if err != nil || "sha256:"+read.Digest != strings.TrimSpace(digest) {
		return review.Artifact{}, false
	}
	artifact, err := review.ParseArtifact(read.Data, kind)
	if err != nil ||
		artifact.CandidateSHA != candidateSHA ||
		artifact.BaseSHA != baseSHA {
		return review.Artifact{}, false
	}
	return artifact, true
}

var terminalHighRiskReviewPrefixes = []string{
	".github/",
	"hooks/",
	"internal/account/",
	"internal/auth",
	"internal/commandguard/",
	"internal/merge/",
	"internal/signing/",
	"internal/vault/",
}

func terminalSecurityReviewRequired(
	ctx context.Context,
	root string,
	slot *ledger.Slot,
	gate *merge.GateEvidence,
	general review.Artifact,
) (required, authenticated bool) {
	if general.Verdict.SecurityReviewRequired || terminalSecurityFieldsPresent(slot) {
		return true, true
	}
	for _, label := range slot.BeadLabels {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "security-risk", "risk:security", "security":
			return true, true
		}
	}
	cfg, err := project.Load(root)
	if err != nil {
		return false, false
	}
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: root, Name: "git",
		Args: []string{"diff", "--name-only", gate.BaseSHA + "..." + gate.CandidateSHA},
	})
	if err != nil {
		return false, false
	}
	prefixes := append([]string(nil), terminalHighRiskReviewPrefixes...)
	prefixes = append(prefixes, cfg.EffectiveReview().HighRiskPaths...)
	for _, raw := range strings.Split(res.Stdout, "\n") {
		path := strings.Trim(strings.ToLower(filepath.ToSlash(strings.TrimSpace(raw))), "/")
		for _, rawPrefix := range prefixes {
			prefix := strings.Trim(strings.ToLower(filepath.ToSlash(strings.TrimSpace(rawPrefix))), "/")
			if path != "" && prefix != "" &&
				(path == prefix || strings.HasPrefix(path, prefix+"/")) {
				return true, true
			}
		}
	}
	return false, true
}

func terminalSecurityFieldsPresent(slot *ledger.Slot) bool {
	return slot.SecurityReviewArtifactPath != "" ||
		slot.SecurityReviewArtifactDigest != "" ||
		slot.SecurityReviewCandidateSHA != "" ||
		slot.SecurityReviewBaseSHA != ""
}

// Events implements loop.TripwireSource. Each supervisor run gets a bounded
// subscription registered before its engine goroutine starts. One queued event
// is sufficient to drain the run; later events remain in the durable engine
// evidence rather than blocking its decision path.
func (e *loopEngine) Events(ctx context.Context) <-chan loopsupervisor.Tripwire {
	ch := make(chan loopsupervisor.Tripwire, 1)
	e.tripwireMu.Lock()
	e.tripwireNext++
	id := e.tripwireNext
	if e.tripwireSubs == nil {
		e.tripwireSubs = make(map[uint64]chan loopsupervisor.Tripwire)
	}
	e.tripwireSubs[id] = ch
	e.tripwireMu.Unlock()
	go func() {
		<-ctx.Done()
		e.tripwireMu.Lock()
		if registered, ok := e.tripwireSubs[id]; ok {
			delete(e.tripwireSubs, id)
			close(registered)
		}
		e.tripwireMu.Unlock()
	}()
	return ch
}

func (e *loopEngine) publishTripwire(event engine.SafetyTripwire) {
	wire := loopsupervisor.Tripwire{
		Kind: string(event.Kind), RunID: event.RunID,
		BeadID: event.BeadID, Detail: event.Detail,
	}
	e.tripwireMu.Lock()
	defer e.tripwireMu.Unlock()
	for _, subscriber := range e.tripwireSubs {
		select {
		case subscriber <- wire:
		default:
		}
	}
}

type loopDrainer struct{ store *ledger.Store }

func (d loopDrainer) Drain(_ context.Context, _ string) error {
	return d.store.RequestDrain()
}

func (d loopDrainer) AcknowledgeDrain() {
	d.store.ConsumeDrain()
}

type loopMaintainer struct {
	root string
	run  func(koryphgc.Options) (*koryphgc.Result, error)
}

func (m loopMaintainer) Maintain(_ context.Context, _ loopsupervisor.Boundary) error {
	run := m.run
	if run == nil {
		run = koryphgc.Run
	}
	result, err := run(koryphgc.Options{RepoRoot: m.root})
	if err != nil {
		return err
	}
	var failures []string
	for _, class := range result.Classes {
		for _, classErr := range class.Errors {
			failures = append(failures, fmt.Sprintf("%s: %s", class.Class, classErr))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("artifact maintenance: %s", strings.Join(failures, "; "))
	}
	return nil
}

func splitIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range strings.Split(value, ",") {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func canaryTargetWidth(ids []string, requested int) int {
	target := len(ids)
	if requested > 0 && requested < target {
		target = requested
	}
	return target
}

func idSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func containsID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func containsIssue(issues []beads.Issue, id string) bool {
	for _, issue := range issues {
		if issue.ID == id {
			return true
		}
	}
	return false
}
