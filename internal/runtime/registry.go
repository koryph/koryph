// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

import (
	"fmt"
	"sort"
	"sync"
)

// Registry holds the set of Runtime implementations known to one koryph
// process, keyed by Runtime.Name. It is the lookup path bead labels
// (`runtime:<name>`) and koryph.project.json's `runtimes:{}` block resolve
// through (koryph-v8u.1 phase 1; the wiring itself lands in a later
// koryph-v8u bead — this type is registered but not yet consulted by the
// engine). The zero value is not usable; construct with NewRegistry.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Runtime
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]Runtime)}
}

// Default is the process-wide Registry real adapters self-register into at
// init (koryph-v8u.2 — see internal/runtime/claude's register.go for the
// first real entry). It exists as a package-level singleton, rather than
// requiring every adapter package's caller to construct and thread through a
// *Registry by hand, so importing an adapter package for its Runtime methods
// (e.g. internal/dispatch importing internal/runtime/claude to delegate
// Command/ParseEvents) is enough to also make that adapter discoverable —
// the same "import for side effect" pattern database/sql drivers use.
// Nothing consults Default for dispatch decisions yet; that is
// koryph-v8u.3's job (selection/routing). Tests that need an isolated
// registry should still use NewRegistry, never Default.
var Default = NewRegistry()

// Register adds rt under rt.Name(). It returns an error, and leaves the
// registry unchanged, when rt.Name() is empty or already registered —
// duplicate registration is a programming error (two adapters claiming the
// same runtime identity), never a silent overwrite, since a silent last-
// writer-wins would make Registry construction order-sensitive in a way
// that is very hard to debug at dispatch time.
func (r *Registry) Register(rt Runtime) error {
	if rt == nil {
		return fmt.Errorf("runtime: cannot register a nil Runtime")
	}
	name := rt.Name()
	if name == "" {
		return fmt.Errorf("runtime: cannot register a Runtime with an empty Name()")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[name]; exists {
		return fmt.Errorf("runtime: %q is already registered", name)
	}
	r.byID[name] = rt
	return nil
}

// Get returns the Runtime registered under name, and whether it was found.
func (r *Registry) Get(name string) (Runtime, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.byID[name]
	return rt, ok
}

// List returns every registered Runtime sorted by Name, for deterministic
// output in `koryph doctor`'s integration matrix and in tests.
func (r *Registry) List() []Runtime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byID))
	for name := range r.byID {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Runtime, 0, len(names))
	for _, name := range names {
		out = append(out, r.byID[name])
	}
	return out
}
