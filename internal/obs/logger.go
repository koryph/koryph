// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"context"
	"log/slog"
	"os"
	"sync"
)

// componentLogger is a per-component slog.Logger with a hot-swappable level
// gate. The LevelHandler wraps the shared root handler so log calls go through
// the same output pipeline but each component can be independently silenced or
// expanded without rebuilding handlers.
type componentLogger struct {
	lh     *LevelHandler
	logger *slog.Logger
}

// Registry tracks named component loggers and their level gates.
type Registry struct {
	mu         sync.RWMutex
	root       slog.Handler // shared, already wrapped with redaction
	components map[string]*componentLogger
	watcher    *Watcher
}

// NewRegistry creates a Registry backed by the given root handler (which
// should already be wrapped in a RedactingHandler) and a config Watcher.
// Call Sync() after construction to set initial per-component levels.
func NewRegistry(root slog.Handler, w *Watcher) *Registry {
	r := &Registry{
		root:       root,
		components: make(map[string]*componentLogger),
		watcher:    w,
	}
	return r
}

// For returns the slog.Logger for the named component, creating it on first
// call with the level from the current Config. Subsequent calls return the
// cached logger. The logger writes to the shared root handler gated by the
// component's LevelHandler.
func (r *Registry) For(name string) *slog.Logger {
	r.mu.RLock()
	cl, ok := r.components[name]
	r.mu.RUnlock()
	if ok {
		return cl.logger
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after acquiring write lock.
	if cl, ok = r.components[name]; ok {
		return cl.logger
	}

	level := r.watcher.ComponentLevel(name)
	lh := NewLevelHandler(level, r.root)
	logger := slog.New(lh).With(slog.String("component", name))
	cl = &componentLogger{lh: lh, logger: logger}
	r.components[name] = cl
	return logger
}

// SetLevel updates the minimum level for the named component. It creates the
// component entry if it doesn't exist. Returns the logger for chaining.
func (r *Registry) SetLevel(name string, level slog.Level) *slog.Logger {
	r.mu.Lock()
	cl, ok := r.components[name]
	if !ok {
		lh := NewLevelHandler(level, r.root)
		logger := slog.New(lh).With(slog.String("component", name))
		cl = &componentLogger{lh: lh, logger: logger}
		r.components[name] = cl
	} else {
		cl.lh.SetLevel(level)
	}
	logger := cl.logger
	r.mu.Unlock()
	return logger
}

// Sync re-reads the Watcher's Config and updates all existing component
// level gates without replacing the loggers. Called at each scheduler tick.
func (r *Registry) Sync() {
	cfg := r.watcher.Config()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for name, cl := range r.components {
		cl.lh.SetLevel(cfg.ComponentLevel(name))
	}
}

// defaultRegistry is the package-level singleton. Initialised by Init or
// lazily by For().
var (
	defaultRegistry *Registry
	defaultMu       sync.RWMutex
)

// Init initialises the package-level logger registry from cfg and root. It is
// idempotent: once set, subsequent Init calls are no-ops. Use ReInit to force
// replacement.
func Init(cfg Config, root slog.Handler) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultRegistry != nil {
		return
	}
	defaultRegistry = buildRegistry(cfg, root)
}

// ReInit replaces the package-level registry unconditionally. Use this when
// configuration changes require a new handler pipeline (e.g. a file path
// changed). Safe for concurrent use.
func ReInit(cfg Config, root slog.Handler) {
	reg := buildRegistry(cfg, root)
	defaultMu.Lock()
	defaultRegistry = reg
	defaultMu.Unlock()
}

func buildRegistry(cfg Config, root slog.Handler) *Registry {
	w := NewWatcher(cfg)
	rh := NewRedactingHandler(root)
	return NewRegistry(rh, w)
}

// For returns the component logger from the package-level registry.
// If Init has not been called, it lazily initialises from LoadConfig() with
// a console/text handler writing to stderr so the package is usable without
// explicit setup.
func For(component string) *slog.Logger {
	defaultMu.RLock()
	reg := defaultRegistry
	defaultMu.RUnlock()

	if reg == nil {
		// Lazy bootstrap: load config from disk, fall back to defaults.
		cfg, _ := LoadConfig()
		root := BuildPipeline(cfg, os.Stderr, "")
		defaultMu.Lock()
		if defaultRegistry == nil {
			defaultRegistry = buildRegistry(cfg, root)
		}
		reg = defaultRegistry
		defaultMu.Unlock()
	}

	return reg.For(component)
}

// Sync re-reads the Watcher in the package-level registry.
// The engine calls this at each scheduler tick.
func Sync() {
	defaultMu.RLock()
	reg := defaultRegistry
	defaultMu.RUnlock()
	if reg != nil {
		reg.Sync()
	}
}

// ReloadConfig reloads observability.json in the package-level Watcher and
// then syncs all component levels. Returns the new Config and any error.
func ReloadConfig() (Config, error) {
	defaultMu.RLock()
	reg := defaultRegistry
	defaultMu.RUnlock()
	if reg == nil {
		return defaultConfig(), nil
	}
	cfg, err := reg.watcher.Reload()
	if err == nil {
		reg.Sync()
	}
	return cfg, err
}

// Trace logs at TRACE level on the named component logger.
func Trace(component, msg string, args ...any) {
	For(component).Log(context.TODO(), LevelTrace, msg, args...)
}

// buildConsoleHandler creates the appropriate slog.Handler for cfg.
func buildConsoleHandler(cfg Config, w *os.File) slog.Handler {
	level := cfg.defaultSlogLevel()
	switch cfg.Format {
	case "json":
		return NewJSONHandler(w, level)
	default:
		return NewTextHandler(w, level)
	}
}

// BuildPipeline constructs the full handler pipeline from cfg:
//   - A console handler writing to w (text or JSON per cfg.Format).
//   - A telemetry JSONL file handler writing to telDir (or the canonical
//     telemetry directory when telDir is empty, resolved lazily at the first
//     write so KORYPH_HOME is read at write time rather than at construction).
//     On failure the file handler is silently skipped so telemetry is always
//     best-effort.
//   - When cfg.OTELEndpoint is non-empty, an OTLP/HTTP JSON handler that
//     forwards records to that endpoint.
//
// The returned pipeline is NOT wrapped in RedactingHandler; callers that use
// Init / ReInit receive a bare pipeline.  buildRegistry adds the redaction
// layer after this call.
func BuildPipeline(cfg Config, w *os.File, telDir string) slog.Handler {
	console := buildConsoleHandler(cfg, w)

	var handlers []slog.Handler
	handlers = append(handlers, console)

	// Telemetry file handler. When telDir is "" the fileWriter resolves the
	// directory lazily at the first write (see fileWriter.ensureOpen), which
	// means KORYPH_HOME is read at write time rather than at logger
	// construction. This prevents the package-level `var log = obs.For("engine")`
	// from baking the real ~/.koryph path into the file sink before tests have
	// had a chance to set KORYPH_HOME via t.Setenv.
	maxBytes := int64(cfg.TelemetryMaxSizeMB) * 1024 * 1024
	if maxBytes <= 0 {
		maxBytes = 50 * 1024 * 1024
	}
	fh := newFileJSONHandler(telDir, maxBytes, cfg.defaultSlogLevel())
	handlers = append(handlers, fh)

	// Optional OTLP/HTTP forwarding.
	if cfg.OTELEndpoint != "" {
		oh := NewOTLPHTTPHandler(cfg.OTELEndpoint, cfg.defaultSlogLevel())
		handlers = append(handlers, oh)
	}

	if len(handlers) == 1 {
		return handlers[0]
	}
	return NewMultiHandler(handlers...)
}
