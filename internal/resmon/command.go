// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package resmon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// CommandScope describes how much of a repository a command validates.
type CommandScope string

const (
	CommandFocused CommandScope = "focused"
	CommandBroad   CommandScope = "broad"
	CommandGate    CommandScope = "full-gate"
)

// CommandClass is the deterministic classification used by the phase command
// guard. Signature identifies an exact normalized broad command; focused
// commands deliberately have no signature because they are not single-flight.
type CommandClass struct {
	Scope     CommandScope `json:"scope"`
	Name      string       `json:"name"`
	Signature string       `json:"signature,omitempty"`
}

// ClassifyCommand recognizes repository-wide validation without treating a
// focused package test as a gate. argv must be exec-shaped: command first,
// followed by its arguments.
func ClassifyCommand(argv []string) CommandClass {
	argv = normalizeCommand(argv)
	if len(argv) == 0 {
		return CommandClass{Scope: CommandFocused}
	}
	name := filepath.Base(argv[0])
	if (name == "sh" || name == "bash" || name == "zsh") && len(argv) > 2 && argv[1] == "-c" {
		class := ClassifyShellCommand(strings.Join(argv[2:], " "))
		class.Name = name
		return class
	}
	scope := CommandFocused
	switch name {
	case "make", "gmake":
		for _, arg := range argv[1:] {
			target := strings.TrimSpace(strings.SplitN(arg, "=", 2)[0])
			switch target {
			case "gate", "gate-agent":
				scope = CommandGate
			case "test", "vet", "build", "lint", "lint-agent", "reuse":
				if scope != CommandGate {
					scope = CommandBroad
				}
			}
		}
	case "go":
		if len(argv) > 1 {
			switch argv[1] {
			case "test", "vet", "build":
				if hasRepositoryWidePackage(argv[2:]) {
					scope = CommandBroad
				}
			}
		}
	case "golangci-lint":
		if len(argv) > 1 && argv[1] == "run" {
			scope = CommandBroad
		}
	}

	class := CommandClass{Scope: scope, Name: name}
	if scope == CommandGate {
		class.Signature = commandSignature([]string{"koryph", "full-gate"})
	} else if scope != CommandFocused {
		class.Signature = commandSignature(argv)
	}
	return class
}

func commandSignature(argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	return hex.EncodeToString(sum[:12])
}

// ClassifyShellCommand classifies a simple command as received from a runtime
// hook. Compound shell programs are classified at the broadest segment. It is
// intentionally conservative: quoting is not reinterpreted and an ambiguous
// segment remains focused for the process supervisor to observe.
func ClassifyShellCommand(command string) CommandClass {
	broadest := CommandClass{Scope: CommandFocused}
	segments := strings.FieldsFunc(command, func(r rune) bool {
		return r == '\n' || r == ';' || r == '|' || r == '&'
	})
	for _, segment := range segments {
		class := ClassifyCommand(strings.Fields(segment))
		if class.Scope == CommandGate {
			return CommandClass{
				Scope: CommandGate, Name: class.Name,
				Signature: commandSignature([]string{"koryph", "full-gate"}),
			}
		}
		if class.Scope == CommandBroad {
			broadest = class
		}
	}
	if len(segments) > 1 && broadest.Scope == CommandBroad {
		broadest.Signature = commandSignature([]string{"sh", "-c", command})
	}
	return broadest
}

func normalizeCommand(argv []string) []string {
	if len(argv) > 0 && filepath.Base(strings.TrimSpace(argv[0])) == "env" {
		i := 1
		for i < len(argv) {
			arg := strings.TrimSpace(argv[i])
			if arg == "-u" && i+1 < len(argv) {
				i += 2
				continue
			}
			if strings.HasPrefix(arg, "-") || isEnvAssignment(arg) {
				i++
				continue
			}
			break
		}
		argv = argv[i:]
	}
	out := make([]string, 0, len(argv))
	seenCommand := false
	for _, arg := range argv {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		if !seenCommand && isEnvAssignment(arg) {
			continue
		}
		seenCommand = true
		out = append(out, arg)
	}
	return out
}

func isEnvAssignment(arg string) bool {
	if !strings.Contains(arg, "=") || strings.HasPrefix(arg, "=") {
		return false
	}
	name, _, ok := strings.Cut(arg, "=")
	return ok && validEnvName(name)
}

func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func hasRepositoryWidePackage(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "./...", "all":
			return true
		}
	}
	return false
}

// CommandIdentity authenticates a process with both its kernel start identity
// and process-group membership. Numeric PID alone never authorizes signalling.
type CommandIdentity struct {
	PID          int    `json:"pid"`
	StartID      string `json:"start_id"`
	ProcessGroup int    `json:"process_group"`
}

// CommandIdentity returns a signal-safe identity for pid.
func (t *ProcTable) CommandIdentity(pid int) (CommandIdentity, bool) {
	if t == nil {
		return CommandIdentity{}, false
	}
	p, ok := t.byPID[pid]
	if !ok || p.birthID == "" || p.pgid <= 0 {
		return CommandIdentity{}, false
	}
	return CommandIdentity{PID: pid, StartID: p.birthID, ProcessGroup: p.pgid}, true
}

// MatchesCommand reports whether all reusable identity components still
// identify the same live process.
func (t *ProcTable) MatchesCommand(want CommandIdentity) bool {
	got, ok := t.CommandIdentity(want.PID)
	return ok && want.StartID != "" && got == want
}

// InspectCommand takes one host snapshot and returns authenticated cohort
// usage. An identity mismatch is reported as not found so callers cannot act
// on a recycled PID.
func InspectCommand(ctx context.Context, want CommandIdentity) (Sample, bool, error) {
	table, err := Snapshot(ctx)
	if err != nil {
		return Sample{}, false, err
	}
	if !table.MatchesCommand(want) {
		return Sample{}, false, nil
	}
	sample, found := table.Aggregate(want.PID)
	return sample, found, nil
}

// CurrentCommandIdentity resolves pid into the stable identity required by
// the command guard.
func CurrentCommandIdentity(ctx context.Context, pid int) (CommandIdentity, error) {
	var lastErr error
	for range 10 {
		table, err := Snapshot(ctx)
		if err != nil {
			return CommandIdentity{}, err
		}
		if identity, ok := table.CommandIdentity(pid); ok {
			return identity, nil
		}
		lastErr = fmt.Errorf("process %d has no stable start/group identity", pid)
		select {
		case <-ctx.Done():
			return CommandIdentity{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return CommandIdentity{}, lastErr
}

// EnsureIndependentProcessGroup moves the current wrapper into its own process
// group and verifies that the kernel-reported stable identity agrees. It does
// not signal any process. Real tool children inherit this group, so one resmon
// Aggregate sample covers the complete command cohort.
func EnsureIndependentProcessGroup(ctx context.Context) (CommandIdentity, error) {
	pid := os.Getpid()
	pgid, err := unix.Getpgid(0)
	if err != nil {
		return CommandIdentity{}, fmt.Errorf("get command process group: %w", err)
	}
	if pgid != pid {
		if err := unix.Setpgid(0, 0); err != nil {
			return CommandIdentity{}, fmt.Errorf("create independent command process group: %w", err)
		}
	}
	identity, err := CurrentCommandIdentity(ctx, pid)
	if err != nil {
		return CommandIdentity{}, err
	}
	if identity.PID != pid || identity.ProcessGroup != pid {
		return CommandIdentity{}, fmt.Errorf("command process group is not independent: pid=%d pgid=%d", identity.PID, identity.ProcessGroup)
	}
	return identity, nil
}

// EnsureIndependentProcessGroupForGuard returns the strongest identity the
// host permits for a phase-local command guard. Normal hosts use the stable
// process-start identity above. Some macOS sandboxes deny both ps and
// KERN_PROC sysctl; after the independent group is established, those
// permission errors fall back to a process-local random lease identity.
//
// A lease identity must never authorize signalling or reattachment. The
// command guard authenticates its liveness with a kernel-released flock held
// by the owner process instead.
func EnsureIndependentProcessGroupForGuard(ctx context.Context) (CommandIdentity, error) {
	identity, err := EnsureIndependentProcessGroup(ctx)
	if err == nil {
		return identity, nil
	}
	if !errors.Is(err, os.ErrPermission) {
		return CommandIdentity{}, err
	}
	pid := os.Getpid()
	pgid, pgErr := unix.Getpgid(0)
	if pgErr != nil {
		return CommandIdentity{}, fmt.Errorf("get guarded command process group: %w", pgErr)
	}
	if pgid != pid {
		return CommandIdentity{}, fmt.Errorf("guarded command process group is not independent: pid=%d pgid=%d", pid, pgid)
	}
	var nonce [16]byte
	if _, randErr := rand.Read(nonce[:]); randErr != nil {
		return CommandIdentity{}, fmt.Errorf("generate guarded command lease identity: %w", randErr)
	}
	return CommandIdentity{
		PID: pid, ProcessGroup: pgid, StartID: "lease:" + hex.EncodeToString(nonce[:]),
	}, nil
}

// CommandEvent is the durable, provider-neutral command evidence envelope.
type CommandEvent struct {
	Schema       string          `json:"schema"`
	Event        string          `json:"event"`
	At           time.Time       `json:"at"`
	Class        CommandScope    `json:"class"`
	Signature    string          `json:"signature,omitempty"`
	Generation   string          `json:"generation,omitempty"`
	Role         string          `json:"role,omitempty"`
	ExistingRole string          `json:"existing_role,omitempty"`
	Identity     CommandIdentity `json:"identity,omitempty"`
	Existing     CommandIdentity `json:"existing_identity,omitempty"`
	Status       string          `json:"status,omitempty"`
	LogPath      string          `json:"log_path,omitempty"`
	PeakRSSKB    int64           `json:"peak_rss_kb"`
	CPUSeconds   float64         `json:"cpu_seconds"`
	DurationMS   int64           `json:"duration_ms"`
	Reason       string          `json:"reason,omitempty"`
	ExistingPID  int             `json:"existing_pid,omitempty"`
}

// NewCommandEvent supplies the stable schema and UTC timestamp.
func NewCommandEvent(kind string, class CommandClass, now time.Time) CommandEvent {
	return CommandEvent{
		Schema:    "koryph.command-event/v1",
		Event:     kind,
		At:        now.UTC(),
		Class:     class.Scope,
		Signature: class.Signature,
	}
}
