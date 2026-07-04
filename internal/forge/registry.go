// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package forge

import (
	"fmt"
	"sort"
	"sync"
)

// Registry is a thread-safe map from provider name to [Forge] implementation.
// Provider packages (internal/forge/github, internal/forge/gitlab) call
// [Default].Register in their init() functions.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Forge
}

// Default is the global provider registry. Provider init() functions
// register into this variable. Any package that imports a provider package
// as a side effect (e.g. `_ "github.com/koryph/koryph/internal/forge/github"`)
// ensures that provider is available at runtime.
var Default = &Registry{}

// Register adds a [Forge] provider under name. Panics if name is empty or
// already registered (the same init()-time contract as
// http.DefaultServeMux.Handle and database/sql.Register).
func (r *Registry) Register(name string, f Forge) {
	if name == "" {
		panic("forge: Register called with empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]Forge)
	}
	if _, dup := r.entries[name]; dup {
		panic(fmt.Sprintf("forge: Register called twice for provider %q", name))
	}
	r.entries[name] = f
}

// Get looks up a provider by name. Returns the [Forge] and true when found,
// or nil and false when the name is not registered.
func (r *Registry) Get(name string) (Forge, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.entries[name]
	return f, ok
}

// MustGet returns the [Forge] for name and panics when it is not registered.
// Intended for init-time wiring where a missing provider is a programming
// error.
func (r *Registry) MustGet(name string) Forge {
	f, ok := r.Get(name)
	if !ok {
		panic(fmt.Sprintf("forge: no provider registered for name %q", name))
	}
	return f
}

// Names returns the sorted list of registered provider names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for name := range r.entries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
