// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// RedactingHandler wraps any slog.Handler and scrubs every record through
// RedactRecord before forwarding it. All component loggers use this wrapper.
type RedactingHandler struct {
	inner slog.Handler
}

// NewRedactingHandler wraps h with redaction.
func NewRedactingHandler(h slog.Handler) *RedactingHandler {
	return &RedactingHandler{inner: h}
}

func (r *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return r.inner.Enabled(ctx, level)
}

func (r *RedactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	return r.inner.Handle(ctx, RedactRecord(rec))
}

func (r *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Redact attrs that are added via With(…) as well.
	cleaned := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		cleaned[i] = RedactAttr(a)
	}
	return &RedactingHandler{inner: r.inner.WithAttrs(cleaned)}
}

func (r *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{inner: r.inner.WithGroup(name)}
}

// sharedLevel is a heap-allocated level gate shared between a LevelHandler and
// all copies produced by WithAttrs/WithGroup. Clones read/write the same
// *sharedLevel so SetLevel is visible to every derived handler.
type sharedLevel struct {
	mu  sync.RWMutex
	val slog.Level
}

func (sl *sharedLevel) get() slog.Level {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return sl.val
}

func (sl *sharedLevel) set(l slog.Level) {
	sl.mu.Lock()
	sl.val = l
	sl.mu.Unlock()
}

// LevelHandler gates records by a dynamic minimum level. The level may be
// updated at runtime via SetLevel without recreating loggers. Clones produced
// by WithAttrs/WithGroup share the same level reference, so SetLevel is
// visible to every derived handler.
type LevelHandler struct {
	shared *sharedLevel
	inner  slog.Handler
}

// NewLevelHandler creates a LevelHandler with initial level l wrapping h.
func NewLevelHandler(l slog.Level, h slog.Handler) *LevelHandler {
	return &LevelHandler{shared: &sharedLevel{val: l}, inner: h}
}

// SetLevel updates the minimum level. Safe for concurrent use.
// The update is visible to every handler derived from this one via WithAttrs
// or WithGroup.
func (lh *LevelHandler) SetLevel(l slog.Level) {
	lh.shared.set(l)
}

// Level returns the current minimum level.
func (lh *LevelHandler) Level() slog.Level {
	return lh.shared.get()
}

func (lh *LevelHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= lh.shared.get()
}

func (lh *LevelHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < lh.shared.get() {
		return nil
	}
	return lh.inner.Handle(ctx, r)
}

func (lh *LevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LevelHandler{shared: lh.shared, inner: lh.inner.WithAttrs(attrs)}
}

func (lh *LevelHandler) WithGroup(name string) slog.Handler {
	return &LevelHandler{shared: lh.shared, inner: lh.inner.WithGroup(name)}
}

// MultiHandler fans a single record out to multiple handlers. All handlers
// receive the record even if one returns an error; errors are joined.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler creates a MultiHandler that writes to all given handlers.
func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	return &MultiHandler{handlers: handlers}
}

func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []string
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("obs: handler errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: hs}
}

func (m *MultiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: hs}
}

// ConsoleHandler writes human-readable log lines to w, preserving the style
// of today's koryph console output. Format: "LEVEL  msg  key=val …"
type ConsoleHandler struct {
	mu  sync.Mutex
	w   io.Writer
	min slog.Level
}

// NewConsoleHandler creates a ConsoleHandler writing to w at min level.
func NewConsoleHandler(w io.Writer, min slog.Level) *ConsoleHandler {
	return &ConsoleHandler{w: w, min: min}
}

func (c *ConsoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= c.min
}

func (c *ConsoleHandler) Handle(_ context.Context, r slog.Record) error {
	// Build a minimal human-readable line.
	var sb strings.Builder

	levelStr := LevelString(r.Level)
	// Pad to 5 chars for alignment.
	sb.WriteString(levelStr)
	for i := len(levelStr); i < 5; i++ {
		sb.WriteByte(' ')
	}
	sb.WriteByte(' ')

	// Timestamp only at DEBUG/TRACE so INFO output stays clean.
	if r.Level <= slog.LevelDebug {
		sb.WriteString(r.Time.Format(time.RFC3339))
		sb.WriteByte(' ')
	}

	sb.WriteString(r.Message)

	r.Attrs(func(a slog.Attr) bool {
		sb.WriteByte(' ')
		sb.WriteString(a.Key)
		sb.WriteByte('=')
		fmt.Fprintf(&sb, "%v", a.Value.Any())
		return true
	})
	sb.WriteByte('\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := io.WriteString(c.w, sb.String())
	return err
}

func (c *ConsoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// ConsoleHandler is stateless w.r.t. attrs — forward to a JSONHandler
	// wrapper for simplicity, or inline. We keep it simple: create a new
	// handler wrapping a slog.JSONHandler for structured sub-loggers.
	inner := slog.NewTextHandler(c.w, &slog.HandlerOptions{
		Level:       c.min,
		ReplaceAttr: levelReplacer,
	}).WithAttrs(attrs)
	return inner
}

func (c *ConsoleHandler) WithGroup(name string) slog.Handler {
	inner := slog.NewTextHandler(c.w, &slog.HandlerOptions{
		Level:       c.min,
		ReplaceAttr: levelReplacer,
	}).WithGroup(name)
	return inner
}

// levelReplacer replaces the slog "level" attribute with our custom level name
// so TRACE appears as "TRACE" rather than "DEBUG-4".
func levelReplacer(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey {
		if l, ok := a.Value.Any().(slog.Level); ok {
			return slog.String(slog.LevelKey, LevelString(l))
		}
	}
	return a
}

// NewJSONHandler returns a slog.JSONHandler writing to w with the obs level
// replacer applied so custom levels render by name.
func NewJSONHandler(w io.Writer, min slog.Level) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       min,
		ReplaceAttr: levelReplacer,
	})
}

// NewTextHandler returns a slog.TextHandler writing to w with the obs level
// replacer applied.
func NewTextHandler(w io.Writer, min slog.Level) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{
		Level:       min,
		ReplaceAttr: levelReplacer,
	})
}

// OpenFileHandler opens (or creates) a JSON log file at path and returns
// a handler plus a close function. The caller is responsible for calling close.
func OpenFileHandler(path string, min slog.Level) (slog.Handler, func() error, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("obs: open log file %q: %w", path, err)
	}
	h := NewJSONHandler(f, min)
	return h, f.Close, nil
}
