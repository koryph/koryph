// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package forge_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is the module root relative to this package's directory.
const repoRoot = "../.."

// forgeCLIPatterns match the ways a package shells out to a forge CLI (gh or
// glab) or resolves its binary. The seam design (docs/designs/
// 2026-07-forge-providers.md §2) requires these to live only under
// internal/forge/** so the forge boundary stays sealed and every provider-
// specific edge is reachable through the Forge contract.
var forgeCLIPatterns = []*regexp.Regexp{
	// exec.Command("gh", …) / exec.CommandContext(ctx, "glab", …). The middle
	// class excludes quotes so we stop at the first string literal, and one
	// optional nested (…) group is tolerated so a resolver call before the
	// literal — exec.CommandContext(ctx, binPath(), "gh") — is still caught.
	regexp.MustCompile(`exec\.Command(Context)?\([^)"]*(\([^)"]*\))?[^)"]*"(gh|glab)"`),
	// execx.Cmd{… Name: "gh" …}
	regexp.MustCompile(`Name:\s*"(gh|glab)"`),
	// os.Getenv("KORYPH_GH_BIN") / os.Getenv("KORYPH_GLAB_BIN") — the point
	// where a package decides which forge CLI to invoke. Bare string mentions
	// of the env-var name in help text or docgen metadata do not match.
	regexp.MustCompile(`os\.Getenv\("KORYPH_(GH|GLAB)_BIN"\)`),
}

// preSeamAllowlist enumerates the call sites that still invoke gh/glab directly
// because their extraction into internal/forge/** has not landed yet. This list
// is a RATCHET: it may only shrink. Adding a new forge-CLI call site anywhere
// outside internal/forge/** must fail this test — extract it through a Forge
// provider instead. As the F2/F5-family extraction beads land, delete the
// corresponding entry here.
//
// Paths are slash-separated and relative to the module root.
var preSeamAllowlist = map[string]bool{
	"internal/posture/posture.go":      true, // hygiene → forge.Protection()/Repo()
	"internal/merge/pr.go":             true, // PR merge flow → forge.PRs()
	"internal/doctor/release_infra.go": true, // release-infra probe → forge.Secrets()/Releases()
	"internal/release/kick.go":         true, // release-please PR flow → forge.PRs()/Releases()
	"internal/intake/github.go":        true, // issue intake (separate provider family; see design §8)
	"internal/engine/reviewpr.go":      true, // review-pr flow → forge.PRs()
	"cmd/koryph/bot.go":                true, // bot identity → forge.Bot()
}

// TestForgeSeamSealed walks the Go sources under internal/ and cmd/ and fails
// if any file outside internal/forge/** invokes a forge CLI (gh/glab) unless
// it is on the shrinking pre-seam allowlist. This is the enforcement half of
// the forge-provider seam (F8): the seam stays sealed going forward even while
// the incumbent GitHub call sites are still being extracted.
func TestForgeSeamSealed(t *testing.T) {
	roots := []string{
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
	}

	var offenders []string
	seenAllowed := map[string]bool{}

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			// The seam itself is where forge CLIs legitimately live.
			if strings.HasPrefix(rel, "internal/forge/") {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !matchesForgeCLI(string(data)) {
				return nil
			}
			if preSeamAllowlist[rel] {
				seenAllowed[rel] = true
				return nil
			}
			offenders = append(offenders, rel)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("forge CLI (gh/glab) invoked outside internal/forge/**:\n  %s\n"+
			"Route forge-specific calls through a Forge provider (internal/forge/…). "+
			"See docs/user-guide/forges.md and docs/designs/2026-07-forge-providers.md §2.",
			strings.Join(offenders, "\n  "))
	}

	// Keep the allowlist honest: an entry that no longer matches means the
	// extraction landed and the entry should be deleted (the ratchet only
	// shrinks).
	for rel := range preSeamAllowlist {
		if !seenAllowed[rel] {
			t.Errorf("stale pre-seam allowlist entry %q no longer invokes a forge CLI; "+
				"delete it from preSeamAllowlist in seam_test.go", rel)
		}
	}
}

func matchesForgeCLI(src string) bool {
	for _, re := range forgeCLIPatterns {
		if re.MatchString(src) {
			return true
		}
	}
	return false
}

// TestMatchesForgeCLI locks the detector against the invocation shapes it must
// catch (including a resolver call before the literal — the nested-paren
// false-negative noted in review) and the shapes it must not flag.
func TestMatchesForgeCLI(t *testing.T) {
	mustMatch := []string{
		`exec.Command("gh", "pr", "list")`,
		`exec.CommandContext(ctx, "glab", "mr", "list")`,
		`exec.CommandContext(ctx, resolveBin(), "gh")`, // nested call before literal
		`execx.Run(ctx, execx.Cmd{Dir: dir, Name: "gh", Args: args})`,
		`if v := os.Getenv("KORYPH_GH_BIN"); v != "" {`,
		`os.Getenv("KORYPH_GLAB_BIN")`,
	}
	for _, src := range mustMatch {
		if !matchesForgeCLI(src) {
			t.Errorf("expected forge-CLI match for: %s", src)
		}
	}

	mustNotMatch := []string{
		`exec.Command(ghBin, "status")`,               // binary via variable, no literal
		`name: "KORYPH_GH_BIN"`,                       // docgen metadata, not a call
		`KORYPH_GH_BIN         path to the gh binary`, // help text mentioning the var
		`fmt.Println("use the gh CLI to authenticate")`,
	}
	for _, src := range mustNotMatch {
		if matchesForgeCLI(src) {
			t.Errorf("unexpected forge-CLI match for: %s", src)
		}
	}
}
