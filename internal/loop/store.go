// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/strictjson"
)

const (
	stateFile   = "supervisor.json"
	controlFile = "control.json"
	alertFile   = "alerts.jsonl"
	lockFile    = "supervisor.lock"
	controlLock = "control.lock"
)

// Store owns supervisor-only state under <repo>/.koryph/loop. This is outside
// the engine ledger root, so idle observation cannot manufacture an empty run.
type Store struct {
	Root string
	now  func() time.Time
}

func NewStore(repoRoot string) *Store {
	return &Store{Root: filepath.Join(repoRoot, ".koryph", "loop")}
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) StatePath() string   { return filepath.Join(s.Root, stateFile) }
func (s *Store) ControlPath() string { return filepath.Join(s.Root, controlFile) }
func (s *Store) AlertPath() string   { return filepath.Join(s.Root, alertFile) }

func (s *Store) LoadState() (State, error) {
	var state State
	read, err := fsx.ReadRegularConfined(s.StatePath(), 8<<20, s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	if err := strictjson.Decode(read.Data, &state); err != nil {
		return State{}, fmt.Errorf("%s: %w", read.Path, err)
	}
	if state.SchemaVersion > SchemaVersion {
		return State{}, fmt.Errorf("loop: supervisor schema v%d is newer than supported v%d; upgrade koryph", state.SchemaVersion, SchemaVersion)
	}
	return state, nil
}

func (s *Store) SaveState(state State) error {
	state.SchemaVersion = SchemaVersion
	state.UpdatedAt = s.clock().Format(time.RFC3339Nano)
	return fsx.WriteJSONAtomicPerm(s.StatePath(), state, 0o600)
}

func (s *Store) LoadControl() (Control, error) {
	var control Control
	read, err := fsx.ReadRegularConfined(s.ControlPath(), 1<<20, s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return Control{SchemaVersion: SchemaVersion}, nil
	}
	if err != nil {
		return Control{}, err
	}
	if err := strictjson.Decode(read.Data, &control); err != nil {
		return Control{}, fmt.Errorf("%s: %w", read.Path, err)
	}
	if control.SchemaVersion > SchemaVersion {
		return Control{}, fmt.Errorf("loop: control schema v%d is newer than supported v%d; upgrade koryph", control.SchemaVersion, SchemaVersion)
	}
	return control, nil
}

func (s *Store) RequestStop() error {
	return s.updateControl(func(control *Control) { control.Stop = true })
}

func (s *Store) RequestDrain() error {
	return s.updateControl(func(control *Control) { control.Drain = true })
}

func (s *Store) RequestInject(id string) error {
	if id == "" {
		return errors.New("loop: empty injection id")
	}
	return s.updateControl(func(control *Control) {
		for _, existing := range control.Inject {
			if existing == id {
				return
			}
		}
		control.Inject = append(control.Inject, id)
	})
}

func (s *Store) AcknowledgeInjection(id string) error {
	return s.updateControl(func(control *Control) {
		out := control.Inject[:0]
		for _, existing := range control.Inject {
			if existing != id {
				out = append(out, existing)
			}
		}
		control.Inject = out
	})
}

func (s *Store) ClearControl() error {
	return s.withControlLock(func() error {
		err := os.Remove(s.ControlPath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

func (s *Store) updateControl(mut func(*Control)) error {
	return s.withControlLock(func() error {
		control, err := s.LoadControl()
		if err != nil {
			return err
		}
		mut(&control)
		control.SchemaVersion = SchemaVersion
		control.UpdatedAt = s.clock().Format(time.RFC3339Nano)
		return fsx.WriteJSONAtomicPerm(s.ControlPath(), control, 0o600)
	})
}

func (s *Store) withControlLock(fn func() error) error {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Root, controlLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

// Lock is the held supervisor singleton lease.
type Lock struct{ file *os.File }

func (s *Store) Acquire() (*Lock, error) {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Root, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return &Lock{file: f}, nil
}

func (l *Lock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

// JSONLNotifier appends structured alert evidence only. It deliberately has
// no source, branch, Beads, signing, policy, command, or repair dependency.
type JSONLNotifier struct{ Store *Store }

func (n JSONLNotifier) Emit(_ context.Context, alert Alert) error {
	if n.Store == nil {
		return errors.New("loop: nil notifier store")
	}
	if alert.SchemaVersion == 0 {
		alert.SchemaVersion = SchemaVersion
	}
	if alert.At == "" {
		alert.At = n.Store.clock().Format(time.RFC3339Nano)
	}
	line, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(n.Store.Root, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(n.Store.AlertPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
