// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package commandguard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/resmon"
	"golang.org/x/sys/unix"
)

const (
	commandGuardDirName  = ".koryph-command"
	commandEventsName    = "events.jsonl"
	commandSchema        = "koryph.command-record/v2"
	commandResultSchema  = "koryph.command-result/v1"
	commandSignatureLen  = 24
	commandGenerationLen = 32
	commandStateMaxBytes = 64 << 10
)

// CommandRole is assigned by a trusted binary caller, never parsed from a
// worker-controlled flag or environment variable.
type CommandRole string

const (
	CommandRoleWorker     CommandRole = "worker"
	CommandRoleValidation CommandRole = "validation"
)

type ProcessGuardAction string

const (
	ProcessGuardStart ProcessGuardAction = "start"
	ProcessGuardReuse ProcessGuardAction = "reuse"
	ProcessGuardDeny  ProcessGuardAction = "deny"
)

type ProcessGuardRequest struct {
	Argv       []string
	Role       CommandRole
	Identity   resmon.CommandIdentity
	OwnerLease *OwnerLease
}

type ProcessGuardDecision struct {
	Action   ProcessGuardAction
	Class    resmon.CommandClass
	Existing CommandOwner
	Reason   string
	lease    *OwnerLease
}

// OwnerLease is an open file description prepared by ProcessGuard. Trusted
// validation can pass it to a suspended supervisor before Acquire; workers
// inherit the lease returned on a start decision. Its fields are deliberately
// private so a caller cannot forge a lease for another phase or signature.
type OwnerLease struct {
	file      *os.File
	signature string
	guardPath string
}

// CommandOwner is the durable identity of one authoritative broad command.
// Generation prevents a late duplicate from accepting a newer invocation's
// result for the same command signature.
type CommandOwner struct {
	Schema     string                 `json:"schema"`
	Generation string                 `json:"generation"`
	Class      resmon.CommandScope    `json:"class"`
	Signature  string                 `json:"signature"`
	Role       CommandRole            `json:"role"`
	Identity   resmon.CommandIdentity `json:"identity"`
	Status     string                 `json:"status"`
	LogPath    string                 `json:"log_path"`
	StartedAt  time.Time              `json:"started_at"`
	UpdatedAt  time.Time              `json:"updated_at"`
	PeakRSSKB  int64                  `json:"peak_rss_kb"`
	CPUSeconds float64                `json:"cpu_seconds"`
	DurationMS int64                  `json:"duration_ms"`
}

type CommandResult struct {
	Schema     string                 `json:"schema"`
	Generation string                 `json:"generation"`
	Signature  string                 `json:"signature"`
	Role       CommandRole            `json:"role"`
	Identity   resmon.CommandIdentity `json:"identity"`
	Status     string                 `json:"status"`
	ExitCode   int                    `json:"exit_code"`
	LogPath    string                 `json:"log_path"`
	PeakRSSKB  int64                  `json:"peak_rss_kb"`
	CPUSeconds float64                `json:"cpu_seconds"`
	DurationMS int64                  `json:"duration_ms"`
	Completed  time.Time              `json:"completed_at"`
}

type commandInspector func(context.Context, resmon.CommandIdentity) (resmon.Sample, bool, error)

type ProcessGuard struct {
	phaseDir      string
	now           func() time.Time
	inspect       commandInspector
	newGeneration func() (string, error)
	waitPoll      time.Duration
	waitTimeout   time.Duration
	resultGrace   time.Duration
	drainPoll     time.Duration
	drainTimeout  time.Duration
}

func NewProcessGuard(phaseDir string) *ProcessGuard {
	return &ProcessGuard{
		phaseDir:      filepath.Clean(phaseDir),
		now:           time.Now,
		inspect:       resmon.InspectCommand,
		newGeneration: randomCommandGeneration,
		waitPoll:      500 * time.Millisecond,
		waitTimeout:   30 * time.Minute,
		resultGrace:   2 * time.Second,
		drainPoll:     20 * time.Millisecond,
		drainTimeout:  10 * time.Second,
	}
}

func randomCommandGeneration() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Acquire serializes owner publication and stale replacement with a
// signature-local syscall flock. No reader can observe a mkdir-before-owner
// gap. Missing or unknown roles fail closed.
func (g *ProcessGuard) Acquire(ctx context.Context, req ProcessGuardRequest) (ProcessGuardDecision, error) {
	class := resmon.ClassifyCommand(req.Argv)
	decision := ProcessGuardDecision{Action: ProcessGuardStart, Class: class}
	switch req.Role {
	case CommandRoleWorker, CommandRoleValidation:
	default:
		return ProcessGuardDecision{}, fmt.Errorf("command role %q is not trusted", req.Role)
	}
	if !validIdentity(req.Identity) {
		return ProcessGuardDecision{}, errors.New("command requires authenticated start/lease and independent process-group identity")
	}
	if class.Scope == resmon.CommandFocused {
		return decision, nil
	}
	if !validLowerHex(class.Signature, commandSignatureLen) {
		return ProcessGuardDecision{}, errors.New("broad command has an invalid signature")
	}

	err := g.withSignatureLock(class.Signature, func(fs *commandGuardFS) error {
		if req.OwnerLease != nil {
			if err := req.OwnerLease.validate(class.Signature, fs.path); err != nil {
				return fmt.Errorf("invalid prepared owner lease: %w", err)
			}
		}
		ownerName := ownerFileName(class.Signature)
		var owner CommandOwner
		exists, err := fs.readJSON(ownerName, &owner)
		if err != nil {
			return fmt.Errorf("read command owner: %w", err)
		}
		if exists {
			if err := validateCommandOwner(owner, class, fs.path); err != nil {
				return err
			}
			if owner.Status == "running" {
				live, err := fs.ownerLeaseHeld(class.Signature)
				if err != nil {
					return fmt.Errorf("authenticate command owner: %w", err)
				}
				if live {
					decision.Action = ProcessGuardReuse
					decision.Existing = owner
					event := resmon.NewCommandEvent("reuse", class, g.now())
					event.Role = string(req.Role)
					event.ExistingRole = string(owner.Role)
					event.Identity = req.Identity
					event.ExistingPID = owner.Identity.PID
					event.Existing = owner.Identity
					event.Generation = owner.Generation
					event.Status = owner.Status
					event.LogPath = owner.LogPath
					return fs.appendEvent(event)
				}
			}
		}

		// A worker may join an already-running trusted gate and reuse its
		// authoritative result, but can never publish a new full-gate owner.
		if req.Role == CommandRoleWorker && class.Scope == resmon.CommandGate {
			decision.Action = ProcessGuardDeny
			decision.Reason = "the authoritative full project gate is owned by Koryph validation"
			event := resmon.NewCommandEvent("denial", class, g.now())
			event.Role = string(req.Role)
			event.Identity = req.Identity
			event.Status = "denied"
			event.Reason = decision.Reason
			return fs.appendEvent(event)
		}

		lease := req.OwnerLease
		var held bool
		if lease == nil {
			lease, held, err = fs.tryOwnerLease(class.Signature)
		} else {
			held, err = lease.tryLock()
		}
		if err != nil {
			return fmt.Errorf("acquire command owner lease: %w", err)
		}
		if held {
			return errors.New("command owner lease is held without a valid running owner")
		}
		decision.lease = lease

		generation, err := g.newGeneration()
		if err != nil {
			return fmt.Errorf("generate command identity: %w", err)
		}
		if !validLowerHex(generation, commandGenerationLen) {
			return errors.New("invalid command generation")
		}
		// Validate the generation-specific result path before any real child
		// is spawned. A precreated symlink/non-regular file fails closed.
		if err := fs.validateOptionalRegular(resultFileName(class.Signature, generation)); err != nil {
			return err
		}
		now := g.now().UTC()
		owner = CommandOwner{
			Schema: commandSchema, Generation: generation,
			Class: class.Scope, Signature: class.Signature,
			Role: req.Role, Identity: req.Identity, Status: "running",
			LogPath:   filepath.Join(fs.path, logFileName(class.Signature, generation)),
			StartedAt: now, UpdatedAt: now,
		}
		if err := fs.writeJSON(ownerName, owner); err != nil {
			return fmt.Errorf("publish command owner: %w", err)
		}
		event := resmon.NewCommandEvent("start", class, now)
		event.Role = string(owner.Role)
		event.Identity = req.Identity
		event.Generation = owner.Generation
		event.Status = owner.Status
		event.LogPath = owner.LogPath
		if err := fs.appendEvent(event); err != nil {
			return err
		}
		decision.Existing = owner
		return nil
	})
	if err != nil && decision.lease != nil {
		_ = decision.lease.Close()
		decision.lease = nil
	}
	return decision, err
}

// PrepareOwnerLease opens the exact signature lease before a trusted
// validation supervisor starts. The supervisor inherits this open file
// description while suspended; Acquire subsequently locks the same
// description, so an orchestrator crash cannot release ownership while the
// validation cohort survives.
func (g *ProcessGuard) PrepareOwnerLease(argv []string) (*OwnerLease, error) {
	class := resmon.ClassifyCommand(argv)
	if class.Scope != resmon.CommandBroad && class.Scope != resmon.CommandGate {
		return nil, errors.New("owner lease requires a broad command")
	}
	fs, err := openCommandGuardFS(g.phaseDir)
	if err != nil {
		return nil, err
	}
	defer fs.close()
	file, err := fs.openLock(ownerLeaseFileName(class.Signature))
	if err != nil {
		return nil, err
	}
	return &OwnerLease{file: file, signature: class.Signature, guardPath: fs.path}, nil
}

// OwnerLeaseFile returns the descriptor a start decision's real command must
// inherit. Reuse and deny decisions never carry one.
func (d ProcessGuardDecision) OwnerLeaseFile() (*os.File, error) {
	if d.Action != ProcessGuardStart || d.lease == nil {
		return nil, errors.New("start decision has no owner lease")
	}
	return d.lease.ExtraFile()
}

// ExtraFile returns the descriptor that a command supervisor must inherit.
// exec.Cmd duplicates ExtraFiles without closing this parent descriptor.
func (l *OwnerLease) ExtraFile() (*os.File, error) {
	if l == nil || l.file == nil {
		return nil, errors.New("owner lease is closed")
	}
	return l.file, nil
}

// Close releases only this process's descriptor. It deliberately does not
// issue LOCK_UN: inherited cohort descriptors must keep the kernel lease live.
func (l *OwnerLease) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *OwnerLease) validate(signature, guardPath string) error {
	if l == nil || l.file == nil || l.signature != signature || l.guardPath != guardPath {
		return errors.New("owner lease does not match command signature and guard path")
	}
	return requireSingleRegular(l.file, ownerLeaseFileName(signature))
}

// tryLock locks a prepared descriptor. held is true when another open file
// description already owns the kernel lease.
func (l *OwnerLease) tryLock() (held bool, err error) {
	if l == nil || l.file == nil {
		return false, errors.New("owner lease is closed")
	}
	err = syscall.Flock(int(l.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	return false, err
}

// OwnerLeaseHeld reports whether the classified command still has a live
// owner/cohort lease. It is a read-only diagnostic used by release canaries.
func (g *ProcessGuard) OwnerLeaseHeld(argv []string) (bool, error) {
	class := resmon.ClassifyCommand(argv)
	if class.Scope != resmon.CommandBroad && class.Scope != resmon.CommandGate {
		return false, errors.New("owner lease probe requires a broad command")
	}
	return g.ownerLeaseHeld(class.Signature)
}

// OpenLog creates the generation-specific command log without following a
// precreated symlink or overwriting an existing file.
func (g *ProcessGuard) OpenLog(decision ProcessGuardDecision) (*os.File, error) {
	if decision.Action != ProcessGuardStart || decision.Class.Scope == resmon.CommandFocused {
		return nil, errors.New("only an authoritative broad command owns a log")
	}
	if err := g.validateDecisionShape(decision); err != nil {
		return nil, err
	}
	fs, err := openCommandGuardFS(g.phaseDir)
	if err != nil {
		return nil, err
	}
	defer fs.close()
	return fs.createExclusiveRegular(logFileName(decision.Class.Signature, decision.Existing.Generation), 0o600)
}

// Observe records whole-cohort resource use while the authenticated owner is
// live. Updates are serialized with Acquire/Complete.
func (g *ProcessGuard) Observe(ctx context.Context, decision ProcessGuardDecision) (bool, error) {
	if decision.Action != ProcessGuardStart || decision.Class.Scope == resmon.CommandFocused {
		return false, nil
	}
	if err := g.validateDecisionShape(decision); err != nil {
		return false, err
	}
	sample := resmon.Sample{}
	if !strings.HasPrefix(decision.Existing.Identity.StartID, "lease:") {
		observed, live, err := g.inspect(ctx, decision.Existing.Identity)
		if err != nil || !live {
			return live, err
		}
		sample = observed
	}
	err := g.withSignatureLock(decision.Class.Signature, func(fs *commandGuardFS) error {
		var owner CommandOwner
		exists, err := fs.readJSON(ownerFileName(decision.Class.Signature), &owner)
		if err != nil {
			return err
		}
		if exists {
			if err := validateCommandOwner(owner, decision.Class, fs.path); err != nil {
				return err
			}
		}
		if !exists || !sameOwner(owner, decision.Existing) {
			return errors.New("command owner identity or generation changed")
		}
		if owner.Status != "running" {
			return errors.New("cannot observe a completed command owner")
		}
		if sample.RSSKB > owner.PeakRSSKB {
			owner.PeakRSSKB = sample.RSSKB
		}
		if sample.CPUSeconds > owner.CPUSeconds {
			owner.CPUSeconds = sample.CPUSeconds
		}
		owner.UpdatedAt = g.now().UTC()
		owner.DurationMS = owner.UpdatedAt.Sub(owner.StartedAt).Milliseconds()
		decision.Existing = owner
		return fs.writeJSON(ownerFileName(decision.Class.Signature), owner)
	})
	return true, err
}

// Complete atomically publishes the result bound to the exact owner identity
// and generation. It never deletes another generation's state.
func (g *ProcessGuard) Complete(decision ProcessGuardDecision, status string, exitCode int) (CommandResult, error) {
	if decision.Action != ProcessGuardStart || decision.Class.Scope == resmon.CommandFocused {
		return CommandResult{}, errors.New("only an authoritative broad command may complete")
	}
	if err := g.validateDecisionShape(decision); err != nil {
		return CommandResult{}, err
	}
	if err := validateTerminalStatus(status, exitCode); err != nil {
		return CommandResult{}, err
	}
	var result CommandResult
	err := g.withSignatureLock(decision.Class.Signature, func(fs *commandGuardFS) error {
		if decision.lease == nil {
			return errors.New("command owner decision has no cohort lease")
		}
		if err := g.awaitCohortLeaseDrain(fs, decision.Class.Signature, decision.lease); err != nil {
			return err
		}
		// Release inside the signature serialization boundary on every
		// persistence path. Close, rather than LOCK_UN, so an unexpected
		// inherited descriptor would continue to hold the lease.
		defer decision.lease.Close()

		var owner CommandOwner
		exists, err := fs.readJSON(ownerFileName(decision.Class.Signature), &owner)
		if err != nil {
			return err
		}
		if exists {
			if err := validateCommandOwner(owner, decision.Class, fs.path); err != nil {
				return err
			}
		}
		if !exists || !sameOwner(owner, decision.Existing) {
			return errors.New("refusing to complete a different command owner")
		}
		if owner.Status != "running" {
			return errors.New("command owner is already complete")
		}
		now := g.now().UTC()
		owner.Status = status
		owner.UpdatedAt = now
		owner.DurationMS = now.Sub(owner.StartedAt).Milliseconds()
		result = CommandResult{
			Schema: commandResultSchema, Generation: owner.Generation,
			Signature: owner.Signature, Role: owner.Role, Identity: owner.Identity,
			Status: status, ExitCode: exitCode, LogPath: owner.LogPath,
			PeakRSSKB: owner.PeakRSSKB, CPUSeconds: owner.CPUSeconds,
			DurationMS: owner.DurationMS, Completed: now,
		}
		if err := fs.writeJSON(resultFileName(owner.Signature, owner.Generation), result); err != nil {
			return err
		}
		if err := fs.writeJSON(ownerFileName(owner.Signature), owner); err != nil {
			return err
		}
		event := resmon.NewCommandEvent("complete", decision.Class, now)
		event.Role = string(owner.Role)
		event.Identity = owner.Identity
		event.Generation = owner.Generation
		event.Status = status
		event.LogPath = owner.LogPath
		event.PeakRSSKB = owner.PeakRSSKB
		event.CPUSeconds = owner.CPUSeconds
		event.DurationMS = owner.DurationMS
		return fs.appendEvent(event)
	})
	return result, err
}

// awaitCohortLeaseDrain transfers the owner's lock from the original
// description (shared with the real command cohort) to a fresh parent-only
// description. The signature state lock is held by the caller throughout, so
// no replacement generation can publish in the transfer window.
func (g *ProcessGuard) awaitCohortLeaseDrain(
	fs *commandGuardFS,
	signature string,
	lease *OwnerLease,
) error {
	if err := lease.Close(); err != nil {
		return fmt.Errorf("close parent owner lease: %w", err)
	}
	timeout := g.drainTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	poll := g.drainPoll
	if poll <= 0 {
		poll = 20 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		next, held, err := fs.tryOwnerLease(signature)
		if err != nil {
			return fmt.Errorf("reacquire drained owner lease: %w", err)
		}
		if !held {
			*lease = *next
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("real command cohort retained its owner lease after direct-child exit")
		}
		time.Sleep(poll)
	}
}

// Wait returns only the result for the reused owner's full identity and
// generation. The wait is bounded and never signals either process.
func (g *ProcessGuard) Wait(ctx context.Context, decision ProcessGuardDecision) (CommandResult, error) {
	if decision.Action != ProcessGuardReuse {
		return CommandResult{}, errors.New("wait requires a reuse decision")
	}
	if err := g.validateDecisionShape(decision); err != nil {
		return CommandResult{}, err
	}
	timeout := g.waitTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	poll := g.waitPoll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var deadAt time.Time
	for {
		var result CommandResult
		var found bool
		err := g.withSignatureLock(decision.Class.Signature, func(fs *commandGuardFS) error {
			var err error
			found, err = fs.readJSON(resultFileName(decision.Class.Signature, decision.Existing.Generation), &result)
			return err
		})
		if err != nil {
			return CommandResult{}, err
		}
		if found {
			if err := validateCommandResult(result, decision.Existing); err != nil {
				return CommandResult{}, err
			}
			return result, nil
		}
		live, err := g.ownerLeaseHeld(decision.Class.Signature)
		if err != nil {
			return CommandResult{}, err
		}
		if !live {
			if deadAt.IsZero() {
				deadAt = time.Now()
			}
			grace := g.resultGrace
			if grace <= 0 {
				grace = 2 * time.Second
			}
			if time.Since(deadAt) >= grace {
				return CommandResult{}, errors.New("authoritative command exited without its generation-bound result")
			}
		} else {
			deadAt = time.Time{}
		}
		select {
		case <-waitCtx.Done():
			if !deadAt.IsZero() {
				return CommandResult{}, errors.New("authoritative command exited without its generation-bound result")
			}
			return CommandResult{}, fmt.Errorf("wait for authoritative command: %w", waitCtx.Err())
		case <-time.After(poll):
		}
	}
}

func validateCommandOwner(owner CommandOwner, class resmon.CommandClass, guardPath string) error {
	if owner.Schema != commandSchema ||
		!validLowerHex(owner.Generation, commandGenerationLen) ||
		!validLowerHex(owner.Signature, commandSignatureLen) ||
		owner.Signature != class.Signature ||
		owner.Class != class.Scope ||
		(owner.Class != resmon.CommandBroad && owner.Class != resmon.CommandGate) ||
		(owner.Role != CommandRoleWorker && owner.Role != CommandRoleValidation) ||
		!validIdentity(owner.Identity) ||
		!validOwnerStatus(owner.Status) ||
		owner.LogPath != filepath.Join(guardPath, logFileName(owner.Signature, owner.Generation)) ||
		!filepath.IsAbs(owner.LogPath) || filepath.Clean(owner.LogPath) != owner.LogPath ||
		owner.StartedAt.IsZero() || owner.UpdatedAt.IsZero() ||
		owner.UpdatedAt.Before(owner.StartedAt) ||
		owner.DurationMS != owner.UpdatedAt.Sub(owner.StartedAt).Milliseconds() ||
		owner.PeakRSSKB < 0 || owner.CPUSeconds < 0 ||
		math.IsNaN(owner.CPUSeconds) || math.IsInf(owner.CPUSeconds, 0) {
		return errors.New("invalid command owner record")
	}
	return nil
}

func validateCommandResult(result CommandResult, owner CommandOwner) error {
	if result.Schema != commandResultSchema ||
		!validLowerHex(result.Generation, commandGenerationLen) ||
		!validLowerHex(result.Signature, commandSignatureLen) ||
		result.Generation != owner.Generation ||
		result.Signature != owner.Signature ||
		result.Role != owner.Role ||
		result.Identity != owner.Identity ||
		result.LogPath != owner.LogPath ||
		result.PeakRSSKB < 0 || result.CPUSeconds < 0 ||
		math.IsNaN(result.CPUSeconds) || math.IsInf(result.CPUSeconds, 0) ||
		result.DurationMS < 0 || result.Completed.IsZero() ||
		result.DurationMS != result.Completed.Sub(owner.StartedAt).Milliseconds() {
		return errors.New("command result does not match authoritative owner identity/generation")
	}
	if err := validateTerminalStatus(result.Status, result.ExitCode); err != nil {
		return fmt.Errorf("invalid command result: %w", err)
	}
	return nil
}

func validIdentity(identity resmon.CommandIdentity) bool {
	return identity.PID > 0 &&
		identity.PID == identity.ProcessGroup &&
		identity.StartID != "" &&
		len(identity.StartID) <= 256 &&
		!strings.ContainsAny(identity.StartID, "\x00\r\n")
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validOwnerStatus(status string) bool {
	return status == "running" || status == "passed" || status == "failed"
}

func validateTerminalStatus(status string, exitCode int) error {
	if exitCode < 0 || exitCode > 255 {
		return errors.New("command exit code is outside 0..255")
	}
	switch status {
	case "passed":
		if exitCode != 0 {
			return errors.New("passed command must have exit code zero")
		}
	case "failed":
		if exitCode == 0 {
			return errors.New("failed command must have a nonzero exit code")
		}
	default:
		return errors.New("terminal command status must be passed or failed")
	}
	return nil
}

func sameOwner(a, b CommandOwner) bool {
	return a.Generation == b.Generation && a.Signature == b.Signature &&
		a.Role == b.Role && a.Identity == b.Identity
}

func (g *ProcessGuard) validateDecisionShape(decision ProcessGuardDecision) error {
	if decision.Class.Scope != resmon.CommandBroad && decision.Class.Scope != resmon.CommandGate {
		return errors.New("command decision is not broad")
	}
	guardPath := filepath.Join(g.phaseDir, commandGuardDirName)
	if err := validateCommandOwner(decision.Existing, decision.Class, guardPath); err != nil {
		return fmt.Errorf("invalid command decision owner: %w", err)
	}
	if decision.Existing.Status != "running" {
		return errors.New("command decision owner is not running")
	}
	return nil
}

func ownerFileName(signature string) string { return "owner-" + signature + ".json" }
func resultFileName(signature, generation string) string {
	return "result-" + signature + "-" + generation + ".json"
}
func logFileName(signature, generation string) string {
	return "command-" + signature + "-" + generation + ".log"
}
func lockFileName(signature string) string { return "lock-" + signature }
func ownerLeaseFileName(signature string) string {
	return "owner-lease-" + signature
}

func (g *ProcessGuard) ownerLeaseHeld(signature string) (bool, error) {
	fs, err := openCommandGuardFS(g.phaseDir)
	if err != nil {
		return false, err
	}
	defer fs.close()
	return fs.ownerLeaseHeld(signature)
}

func (g *ProcessGuard) withSignatureLock(signature string, fn func(*commandGuardFS) error) error {
	fs, err := openCommandGuardFS(g.phaseDir)
	if err != nil {
		return fmt.Errorf("open command guard: %w", err)
	}
	defer fs.close()
	lock, err := fs.openLock(lockFileName(signature))
	if err != nil {
		return fmt.Errorf("open command lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock command signature: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return fn(fs)
}

// commandGuardFS anchors every operation to an already-open, no-follow phase
// subdirectory. Worker-created symlinks cannot redirect writes outside it.
type commandGuardFS struct {
	path string
	dir  *os.File
}

func openCommandGuardFS(phaseDir string) (*commandGuardFS, error) {
	if phaseDir == "" || !filepath.IsAbs(phaseDir) {
		return nil, errors.New("phase directory must be absolute")
	}
	clean := filepath.Clean(phaseDir)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return nil, fmt.Errorf("resolve phase directory: %w", err)
	}
	if resolved != clean {
		return nil, fmt.Errorf("phase directory contains a symlink: %s", phaseDir)
	}
	info, err := os.Lstat(clean)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("phase path is not a real directory")
	}
	root := filepath.Join(clean, commandGuardDirName)
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("command guard root is not a real directory")
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open command guard root: %w", err)
	}
	return &commandGuardFS{path: root, dir: os.NewFile(uintptr(fd), root)}, nil
}

func (fs *commandGuardFS) close() { _ = fs.dir.Close() }

func (fs *commandGuardFS) validateOptionalRegular(name string) error {
	var st unix.Stat_t
	err := unix.Fstatat(int(fs.dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return fmt.Errorf("%s is a symlink, hardlink, or non-regular file", name)
	}
	return nil
}

func (fs *commandGuardFS) openLock(name string) (*os.File, error) {
	var fd int
	var err error
	for range 10 {
		fd, err = unix.Openat(int(fs.dir.Fd()), name,
			unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create %s beneath %s: %w", name, fs.path, err)
		}
		fd, err = unix.Openat(int(fs.dir.Fd()), name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err == nil {
			break
		}
		// A concurrent replacement can create an EEXIST/ENOENT window. Retry
		// publication, but never follow whatever replaced the lock path.
		if !errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("open %s beneath %s: %w", name, fs.path, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("open %s beneath %s after bounded retry: %w", name, fs.path, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if err := requireSingleRegular(f, name); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (fs *commandGuardFS) tryOwnerLease(signature string) (*OwnerLease, bool, error) {
	lease, err := fs.openLock(ownerLeaseFileName(signature))
	if err != nil {
		return nil, false, err
	}
	err = syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return &OwnerLease{file: lease, signature: signature, guardPath: fs.path}, false, nil
	}
	_ = lease.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil, true, nil
	}
	return nil, false, err
}

func (fs *commandGuardFS) ownerLeaseHeld(signature string) (bool, error) {
	lease, held, err := fs.tryOwnerLease(signature)
	if err != nil || held {
		return held, err
	}
	_ = lease.Close()
	return false, nil
}

func (fs *commandGuardFS) createExclusiveRegular(name string, perm uint32) (*os.File, error) {
	if err := fs.validateOptionalRegular(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(fs.dir.Fd()), name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, perm)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(fs.path, name))
	if err := requireSingleRegular(f, name); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func requireSingleRegular(f *os.File, name string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || st.Nlink != 1 {
		return fmt.Errorf("%s is not a single-link regular file", name)
	}
	return nil
}

func (fs *commandGuardFS) readJSON(name string, dst any) (bool, error) {
	fd, err := unix.Openat(int(fs.dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if err := requireSingleRegular(f, name); err != nil {
		return false, err
	}
	data, err := io.ReadAll(io.LimitReader(f, commandStateMaxBytes+1))
	if err != nil {
		return false, err
	}
	if len(data) > commandStateMaxBytes {
		return false, fmt.Errorf("%s exceeds %d-byte state limit", name, commandStateMaxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return false, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("command state contains a trailing JSON value")
		}
		return false, fmt.Errorf("decode trailing command state: %w", err)
	}
	return true, nil
}

func (fs *commandGuardFS) writeJSON(name string, value any) error {
	if err := fs.validateOptionalRegular(name); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp := ".tmp-" + name + "-" + fmt.Sprint(os.Getpid()) + "-" + fmt.Sprint(time.Now().UnixNano())
	fd, err := unix.Openat(int(fs.dir.Fd()), temp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), temp)
	cleanup := func() { _ = unix.Unlinkat(int(fs.dir.Fd()), temp, 0) }
	defer cleanup()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Recheck for an explicit attack. A symlink inserted after this check is
	// atomically replaced by Renameat; it is never followed.
	if err := fs.validateOptionalRegular(name); err != nil {
		return err
	}
	if err := unix.Renameat(int(fs.dir.Fd()), temp, int(fs.dir.Fd()), name); err != nil {
		return err
	}
	return fs.dir.Sync()
}

func (fs *commandGuardFS) appendEvent(event resmon.CommandEvent) error {
	if err := fs.validateOptionalRegular(commandEventsName); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	fd, err := unix.Openat(int(fs.dir.Fd()), commandEventsName,
		unix.O_CREAT|unix.O_APPEND|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), commandEventsName)
	defer f.Close()
	if err := requireSingleRegular(f, commandEventsName); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
