// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sysdeps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
)

// beadsFlakeURL is the flake reference PlanFlakeBeads inserts for a repo with
// no existing beads input — the same repo the system route's nix-profile
// fallback installs from (plan.go's ToolBD ManagerNix route), kept in one
// place so the two routes agree on which beads flake they pin.
const beadsFlakeURL = "github:gastownhall/beads"

// FlakeEdit is a proposed, not-yet-applied modification to a repo's
// flake.nix. A nil *FlakeEdit from PlanFlakeBeads means "no flake.nix here".
// An edit with AlreadyIntegrated true (and empty Diff) means the flake
// already has the beads input + package — nothing to do.
type FlakeEdit struct {
	Path              string // absolute path to flake.nix
	AlreadyIntegrated bool
	Diff              string   // unified-style diff of the proposed change, for consent display
	NewText           string   // full post-edit file content
	Verify            []string // post-apply verification argv
}

// PlanFlakeBeads inspects <root>/flake.nix and proposes the minimal,
// structural edit that wires in the beads flake input and its `bd` package
// (docs/designs/2026-07-adopt.md §4.1). It never writes anything; it never
// reformats the file — only line insertions at points these rules can
// identify with confidence. Any flake shape these rules can't handle
// returns a descriptive error so the caller (the adopt wizard) can fall back
// to the system install route with consent, per the design.
func PlanFlakeBeads(root string) (*FlakeEdit, error) {
	path := filepath.Join(root, "flake.nix")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	origText := string(data)
	text := origText

	// Reuse the doctor's own beads-input detection (internal/beads/version.go)
	// rather than re-deriving the same regex here — the two must always agree
	// on what "the flake's beads input" means. internal/beads imports only
	// execx/version, never sysdeps, so this import creates no cycle.
	inputName, _, found := beads.FlakeBeadsInput(root)
	if found {
		if flakeReferencesBeadsPackage(text, inputName) {
			return &FlakeEdit{Path: path, AlreadyIntegrated: true}, nil
		}
		// The input is already there; only the devShell package entry is
		// missing. Fall through to insertBeadsPackage using the EXISTING
		// input name (it may not be literally "beads" if the repo customized
		// it) without touching `inputs = { ... }` again.
	} else {
		inputName = "beads"
		if text, err = insertBeadsInput(text, inputName); err != nil {
			return nil, err
		}
	}

	if text, err = insertBeadsPackage(text, inputName); err != nil {
		return nil, err
	}

	return &FlakeEdit{
		Path:    path,
		Diff:    diffLines(origText, text),
		NewText: text,
		Verify:  flakeVerifyArgv(root),
	}, nil
}

// ApplyFlakeEdit writes e.NewText over e.Path (atomically) and then refreshes
// flake.lock so the new input is pinned. A nil e or AlreadyIntegrated edit is
// a no-op: there is nothing to write and nothing to lock. The lock step goes
// through the stubable flakeLock hook so tests never shell out to a real nix.
func ApplyFlakeEdit(ctx context.Context, root string, e *FlakeEdit) error {
	if e == nil || e.AlreadyIntegrated {
		return nil
	}
	if err := fsx.WriteAtomic(e.Path, []byte(e.NewText), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", e.Path, err)
	}
	return flakeLock(ctx, root)
}

// flakeLock runs `nix flake lock` in root, refreshing flake.lock to include
// the beads input ApplyFlakeEdit just wrote. It is a package-level var so
// tests can stub it out and record the argv instead of invoking a real nix
// binary. On a non-zero exit, the combined stdout+stderr is folded into the
// returned error so the caller can surface it verbatim (there is no separate
// "output" return — the example this bead was speced against uses exactly
// this func(ctx, root) error shape).
var flakeLock = func(ctx context.Context, root string) error {
	res, err := execx.Run(ctx, execx.Cmd{Dir: root, Name: "nix", Args: []string{"flake", "lock"}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		combined := strings.TrimSpace(res.Stdout + res.Stderr)
		return fmt.Errorf("nix flake lock: exit %d: %s", res.ExitCode, combined)
	}
	return nil
}

// flakeVerifyArgv is the post-apply verification command (rule 6): prefer
// `direnv exec <root> bd version` when direnv is on PATH — most nix-flake
// repos wire direnv's `use flake` so this is the "as the user would run it"
// path — else fall back to `nix develop -c bd version`, which works whether
// or not direnv is set up.
func flakeVerifyArgv(root string) []string {
	if execx.LookPath("direnv") {
		return []string{"direnv", "exec", root, "bd", "version"}
	}
	return []string{"nix", "develop", "-c", "bd", "version"}
}

// flakeReferencesBeadsPackage reports whether text already wires the named
// input into a devShell's package list: either a line containing
// "<name>.packages." (the qualified form, `inputs.beads.packages.…` or the
// bare `beads.packages.…` an ellipsis/explicit-arg outputs binding produces)
// or a line that IS the bare input name (a package referenced directly
// without the `.packages.` accessor).
func flakeReferencesBeadsPackage(text, name string) bool {
	if strings.Contains(text, name+".packages.") {
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSuffix(strings.TrimSpace(line), ",")
		if t == name {
			return true
		}
	}
	return false
}

// reInputsBlock matches the opening line of a classic `inputs = { ... };`
// attrset, through its trailing newline — the point PlanFlakeBeads inserts
// the beads input as the block's new first sibling.
var reInputsBlock = regexp.MustCompile(`(?m)^[ \t]*inputs\s*=\s*\{[ \t]*\r?\n`)

// reInputsShorthand matches one top-level `inputs.<name>.url = "...";` line
// (through its trailing newline), capturing its leading indentation in group
// 1 so an inserted sibling line matches it exactly.
var reInputsShorthand = regexp.MustCompile(`(?m)^([ \t]*)inputs\.[A-Za-z_][A-Za-z0-9_-]*\.url[ \t]*=[^\n]*;[ \t]*\r?\n`)

// insertBeadsInput inserts the beads flake input under name, trying the
// classic `inputs = { ... };` block form first and the top-level shorthand
// `inputs.foo.url = ...;` form second. Neither form found is a hard error —
// rule 3: the caller falls back to the system install route.
func insertBeadsInput(text, name string) (string, error) {
	if loc := reInputsBlock.FindStringIndex(text); loc != nil {
		insertPos := loc[1]
		indent := siblingIndent(text[insertPos:], "    ")
		line := indent + name + `.url = "` + beadsFlakeURL + `";` + "\n"
		return text[:insertPos] + line + text[insertPos:], nil
	}
	if matches := reInputsShorthand.FindAllStringSubmatchIndex(text, -1); len(matches) > 0 {
		last := matches[len(matches)-1]
		indent := text[last[2]:last[3]] // group 1: the last shorthand line's indentation
		insertPos := last[1]            // end of the matched line, including its newline
		line := indent + "inputs." + name + `.url = "` + beadsFlakeURL + `";` + "\n"
		return text[:insertPos] + line + text[insertPos:], nil
	}
	return "", fmt.Errorf("flake.nix: no `inputs = { ... };` block or `inputs.<name>.url = ...;` " +
		"shorthand line found — cannot insert the beads input")
}

// packagesListRE matches the opening line of a `packages = [` or
// `buildInputs = [` list, through its trailing newline. It requires nothing
// but whitespace between `=` and `[` so it does not false-match a
// `buildInputs = pkgs.lib.optionals ... [ ... ]`-style expression (common in
// non-devShell derivations) where other tokens sit between them.
var packagesListRE = regexp.MustCompile(`(?m)^[ \t]*(?:packages|buildInputs)[ \t]*=[ \t]*\[[ \t]*\r?\n`)

// insertBeadsPackage inserts a reference to name's `bd` package into the
// first packages/buildInputs list found inside the flake's first mkShell
// block (rule 4: "the default devShell"). It requires the literal token
// "system" to already appear somewhere in the file before forming a
// `${system}` reference — with no such token in scope, the reference would
// be a guess, which rule 4 forbids.
func insertBeadsPackage(text, name string) (string, error) {
	if !strings.Contains(text, "mkShell") {
		return "", fmt.Errorf("flake.nix: no `mkShell` devShell found — cannot insert the beads package")
	}
	if !strings.Contains(text, "system") {
		return "", fmt.Errorf(
			"flake.nix: no `system` token in scope — cannot form a `${system}` package reference without guessing")
	}

	ref, text, err := beadsPackageRefAndArgs(text, name)
	if err != nil {
		return "", err
	}

	// Re-locate mkShell/the package list AFTER any outputs-arg-set edit above:
	// inserting `name, ` into the lambda header shifts every later byte offset,
	// so offsets computed before that edit would be stale.
	mkShellAt := strings.Index(text, "mkShell")
	rest := text[mkShellAt:]
	m := packagesListRE.FindStringIndex(rest)
	if m == nil {
		return "", fmt.Errorf(
			"flake.nix: no `packages = [` or `buildInputs = [` list found in the devShell — cannot insert the beads package")
	}
	insertPos := mkShellAt + m[1]
	indent := siblingIndent(text[insertPos:], "  ")
	line := indent + ref + "\n"
	return text[:insertPos] + line + text[insertPos:], nil
}

// beadsPackageRefAndArgs decides the devShell package reference expression
// for name and, when needed, edits the outputs lambda's argument set to make
// that reference resolvable (rule 4):
//
//   - `@inputs` (or `inputs@`) binding present → `inputs.<name>.packages.${system}.default`,
//     no arg-set edit needed (the catchall binding already exposes every input).
//   - otherwise (ellipsis or plain explicit args — both are handled the same
//     way, they just differ in whether `...` still follows) → `name` is added
//     as a plain lambda argument right after the first argument, and the
//     reference is the bare `<name>.packages.${system}.default`.
func beadsPackageRefAndArgs(text, name string) (ref, newText string, err error) {
	start, end, argSet, hasInputsBinding, ok := parseOutputsArgSet(text)
	if !ok {
		return "", "", fmt.Errorf("flake.nix: no `outputs = ...:` lambda header found — cannot form a beads package reference")
	}
	if hasInputsBinding {
		return "inputs." + name + ".packages.${system}.default", text, nil
	}

	newArgSet, err := insertArgAfterFirst(argSet, name)
	if err != nil {
		return "", "", fmt.Errorf("flake.nix: %w — cannot add `%s` as an outputs argument without a guess-edit", err, name)
	}
	newText = text[:start+1] + newArgSet + text[end-1:]
	return name + ".packages.${system}.default", newText, nil
}

// parseOutputsArgSet locates the outputs lambda's argument set — the
// `{ ... }` immediately after `outputs =`, whether bound via the
// binding-before form (`inputs@{ ... }`) or the binding-after form
// (`{ ... }@inputs`) — and reports whether it carries an "inputs" catchall
// binding. start/end are the byte offsets of the set's `{` and one past its
// matching `}`; ok is false when no `outputs = ...` lambda header is found
// at all (a flake.nix shape this package cannot edit).
func parseOutputsArgSet(text string) (start, end int, argSet string, hasInputsBinding, ok bool) {
	kw := regexp.MustCompile(`outputs\s*=`).FindStringIndex(text)
	if kw == nil {
		return 0, 0, "", false, false
	}
	i := kw[1]
	for i < len(text) && isSpaceByte(text[i]) {
		i++
	}

	// Binding-before form: `name@{ ... }`.
	j := i
	for j < len(text) && isIdentByte(text[j]) {
		j++
	}
	bindingBefore := ""
	if j > i && j < len(text) && text[j] == '@' {
		bindingBefore = text[i:j]
		i = j + 1
		for i < len(text) && isSpaceByte(text[i]) {
			i++
		}
	}

	if i >= len(text) || text[i] != '{' {
		return 0, 0, "", false, false
	}
	braceStart := i
	closeIdx, ok := matchBrace(text, braceStart)
	if !ok {
		return 0, 0, "", false, false
	}
	braceEnd := closeIdx + 1
	argSet = text[braceStart+1 : closeIdx]

	hasInputsBinding = bindingBefore == "inputs"
	if !hasInputsBinding {
		// Binding-after form: `}@inputs` or `} @ inputs` (rule 4 allows either
		// spacing).
		m := braceEnd
		for m < len(text) && isSpaceByte(text[m]) {
			m++
		}
		if m < len(text) && text[m] == '@' {
			m++
			for m < len(text) && isSpaceByte(text[m]) {
				m++
			}
			n := m
			for n < len(text) && isIdentByte(text[n]) {
				n++
			}
			if text[m:n] == "inputs" {
				hasInputsBinding = true
			}
		}
	}
	return braceStart, braceEnd, argSet, hasInputsBinding, true
}

// matchBrace returns the index of the `}` matching the `{` at text[openIdx],
// tracking nesting depth so an argument set containing its own nested
// attrset (a default value, in principle) is not truncated early.
func matchBrace(text string, openIdx int) (int, bool) {
	depth := 0
	for k := openIdx; k < len(text); k++ {
		switch text[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return k, true
			}
		}
	}
	return 0, false
}

// insertArgAfterFirst returns argSet (the raw text between an outputs
// lambda's `{` and `}`, e.g. " self, nixpkgs, ... ") with "name, " spliced in
// immediately after its first comma-separated argument, e.g.
// " self, name, nixpkgs, ... ". Used for both the ellipsis and plain
// explicit-argument outputs forms — they differ only in whether `...`
// follows, not in how the new argument is added.
func insertArgAfterFirst(argSet, name string) (string, error) {
	idx := strings.IndexByte(argSet, ',')
	if idx < 0 {
		return "", fmt.Errorf("outputs arg set %q has no comma-separated first argument to insert %q after", argSet, name)
	}
	return argSet[:idx+1] + " " + name + "," + argSet[idx+1:], nil
}

// siblingIndent returns the leading whitespace of the first line in `after`
// (the text immediately following a freshly opened block's `{`/`[` and its
// newline), so an inserted sibling line matches its neighbors' indentation
// instead of guessing a hardcoded width (rule 3/4: "sibling-matched
// indentation"). Falls back to `fallback` when the next line is blank or
// itself closes the block — there is no sibling to match against.
func siblingIndent(after, fallback string) string {
	line := after
	if i := strings.IndexByte(after, '\n'); i >= 0 {
		line = after[:i]
	}
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "}") || strings.HasPrefix(trimmed, "]") {
		return fallback
	}
	return line[:len(line)-len(trimmed)]
}

func isSpaceByte(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// diffLines builds a minimal, dependency-free unified-ish diff between
// oldText and newText for consent display (§3.3, rule 5: "check whether the
// repo already has a diff helper... if none, write a minimal one"). It is
// not a general LCS diff: it walks both line sequences in lockstep, and on a
// mismatch looks a short distance ahead in each sequence for a resync point,
// which is enough to isolate the handful of inserted lines nix.go ever
// produces (pure insertions, never reordering or reflow) without a
// third-party diff library.
func diffLines(oldText, newText string) string {
	oldLines := strings.Split(oldText, "\n")
	newLines := strings.Split(newText, "\n")

	const lookahead = 30
	var tagged []string
	oi, ni := 0, 0
	for oi < len(oldLines) || ni < len(newLines) {
		if oi < len(oldLines) && ni < len(newLines) && oldLines[oi] == newLines[ni] {
			tagged = append(tagged, "  "+oldLines[oi])
			oi++
			ni++
			continue
		}
		if oi < len(oldLines) {
			if j := indexWithin(newLines, ni, oldLines[oi], lookahead); j >= 0 {
				for k := ni; k < j; k++ {
					tagged = append(tagged, "+ "+newLines[k])
				}
				ni = j
				continue
			}
		}
		if ni < len(newLines) {
			if j := indexWithin(oldLines, oi, newLines[ni], lookahead); j >= 0 {
				for k := oi; k < j; k++ {
					tagged = append(tagged, "- "+oldLines[k])
				}
				oi = j
				continue
			}
		}
		if oi < len(oldLines) {
			tagged = append(tagged, "- "+oldLines[oi])
			oi++
		}
		if ni < len(newLines) {
			tagged = append(tagged, "+ "+newLines[ni])
			ni++
		}
	}
	return strings.Join(collapseContext(tagged, 2), "\n")
}

// indexWithin returns the index of the first line in lines[from:from+window]
// equal to target, or -1. Used by diffLines to resync after a mismatch —
// e.g. an inserted line reappearing a few lines later in the other sequence.
func indexWithin(lines []string, from int, target string, window int) int {
	limit := from + window
	if limit > len(lines) {
		limit = len(lines)
	}
	for i := from; i < limit; i++ {
		if lines[i] == target {
			return i
		}
	}
	return -1
}

// collapseContext elides long runs of unchanged ("  "-prefixed) lines down to
// ctx lines of leading/trailing context plus a "  ..." marker, so a diff
// spanning two far-apart edits in the same file (the input line near the top,
// the package line in the devShell) doesn't dump the whole file as "context"
// — only the changed regions and their immediate neighborhood.
func collapseContext(lines []string, ctx int) []string {
	var out []string
	i := 0
	for i < len(lines) {
		if !strings.HasPrefix(lines[i], "  ") {
			out = append(out, lines[i])
			i++
			continue
		}
		j := i
		for j < len(lines) && strings.HasPrefix(lines[j], "  ") {
			j++
		}
		run := lines[i:j]
		if len(run) <= 2*ctx+1 {
			out = append(out, run...)
		} else {
			out = append(out, run[:ctx]...)
			out = append(out, "  ...")
			out = append(out, run[len(run)-ctx:]...)
		}
		i = j
	}
	return out
}
