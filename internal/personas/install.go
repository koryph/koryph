// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package personas installs fallback Claude sub-agent persona files into a
// project's .claude/agents directory. It uses the FS embedded in the
// koryph binary (github.com/koryph/koryph/agents) so no network
// access is required at onboard time, and the shared scaffold installer for
// the hash-aware, force-guarded copy policy.
//
// # Per-runtime rendering (koryph-v8u.12)
//
// A persona file's `model:` frontmatter scalar is a Claude-specific model
// pin (see agents/README.md's frontmatter contract); a codex/cursor/grok
// project must never receive a Claude model name it cannot honor. Install
// (equivalently InstallForRuntime(root, force, "claude")) keeps the
// pre-koryph-v8u.12 behavior of copying every embedded persona
// BYTE-IDENTICALLY — this is the default and the hard compatibility
// contract every existing caller and test depends on. InstallForRuntime
// with any OTHER registered runtime name rewrites each persona's `model:`
// value through that runtime's ModelMap, keyed by the SAME persona's
// `tier:` scalar, before writing it — see render.go for the (deliberately
// minimal, non-YAML-round-tripping) string substitution this performs.
package personas

import (
	"fmt"
	"path/filepath"

	"github.com/koryph/koryph/agents"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/scaffold"
)

// Install copies each embedded fallback persona into <root>/.claude/agents,
// byte-identical to the embedded source. Equivalent to
// InstallForRuntime(root, force, "claude"). A file that already exists with
// identical content is an idempotent no-op; a file with differing content is
// skipped (warned by the caller) unless force is set, in which case it is
// overwritten. See scaffold.CopyEmbed.
func Install(root string, force bool) ([]scaffold.Result, error) {
	return scaffold.CopyEmbed(agents.FS, filepath.Join(root, ".claude", "agents"), force, 0o644)
}

// InstallForRuntime is Install, generalized to render each persona's
// frontmatter for a target runtime OTHER than claude (koryph-v8u.12).
//
// runtimeName == "" or "claude" is IDENTICAL to Install: a verbatim copy of
// the embedded FS, byte-for-byte — no rendering pass runs at all for the
// claude path, so a rendering bug can never perturb today's exact output.
// Any other name must be registered in runtime.Default; an unregistered
// runtime is a hard error (fail closed) rather than a silent claude
// fallback — an installer that guessed wrong here would hand a project a
// model pin its runtime cannot honor.
//
// untiered lists the persona names (without ".md") that were installed
// UNCHANGED because they carry no `tier:` frontmatter scalar, or their tier
// is unmapped by runtimeName's ModelMap — the caller's cue to note that
// these personas still carry claude's legacy `model:` pin verbatim, which
// the target runtime may not honor. It is always nil for the claude path,
// since no rendering (and therefore no such gap) is possible there.
func InstallForRuntime(root string, force bool, runtimeName string) (results []scaffold.Result, untiered []string, err error) {
	if runtimeName == "" {
		runtimeName = "claude"
	}
	if runtimeName == "claude" {
		results, err = scaffold.CopyEmbed(agents.FS, filepath.Join(root, ".claude", "agents"), force, 0o644)
		return results, nil, err
	}

	rt, ok := runtime.Default.Get(runtimeName)
	if !ok {
		return nil, nil, fmt.Errorf("personas: unknown runtime %q (not registered in runtime.Default)", runtimeName)
	}

	rendered, untiered, err := renderPersonasFS(rt.ModelMap())
	if err != nil {
		return nil, nil, err
	}
	results, err = scaffold.CopyEmbed(rendered, filepath.Join(root, ".claude", "agents"), force, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return results, untiered, nil
}
