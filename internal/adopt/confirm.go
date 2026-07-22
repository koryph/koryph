// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"fmt"
	"strings"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/registry"
)

// This file holds the CONFIRM phase's pure, non-interactive resolvers (design
// §3.3): given what Detect found plus whatever explicit flags the operator
// passed, decide whether a value confirmation is UNAMBIGUOUS enough to accept
// without a prompt. --yes and a non-TTY stdin both route through these
// functions; an interactive terminal instead prompts the operator and only
// falls back to these functions' explicit-flag branch. Every failure names
// the exact flag that would resolve it, per the design's "fails closed with
// the flag to set" contract.

// AccountChoice is the resolved account/identity a project will register
// under.
type AccountChoice struct {
	Profile    string
	ConfigDir  string
	Identity   string
	Provenance string
	// AuthMode and Credential carry the CLI's --auth-mode/--credential-*
	// flags through to registration (koryph-i3b, design
	// docs/designs/2026-07-api-key-auth.md §8). Empty AuthMode is the
	// default subscription mode, with Credential ignored. Neither field is
	// set by this file's resolvers (ResolveAccountNonInteractive,
	// promptAccount pick Profile/Identity/ConfigDir/Provenance only) — the
	// CLI layer sets them directly from the parsed flags before calling
	// RegisterAndConfigure, so adopt never infers a billing mode itself.
	AuthMode   string
	Credential *registry.Credential
}

// ResolveAccountNonInteractive resolves the account decision for --yes /
// non-TTY mode: explicit --account+--identity always wins (config-dir is
// optional, e.g. the personal profile has none); otherwise exactly ONE
// verified account candidate is accepted. Zero or multiple verified
// candidates both fail closed, naming --account/--identity.
func ResolveAccountNonInteractive(candidates []account.Candidate, explicitProfile, explicitIdentity, explicitConfigDir string) (AccountChoice, error) {
	if explicitProfile != "" || explicitIdentity != "" {
		if explicitProfile == "" || explicitIdentity == "" {
			return AccountChoice{}, fmt.Errorf("adopt: --account and --identity must be passed together")
		}
		return AccountChoice{
			Profile: explicitProfile, Identity: explicitIdentity, ConfigDir: explicitConfigDir,
			Provenance: "explicit --account/--identity",
		}, nil
	}

	var verified []account.Candidate
	for _, c := range candidates {
		if c.Verified {
			verified = append(verified, c)
		}
	}
	switch len(verified) {
	case 0:
		return AccountChoice{}, fmt.Errorf(
			"adopt: no verified account candidate found — non-interactive mode fails closed; " +
				"pass --account/--identity, or run `claude auth login` first")
	case 1:
		c := verified[0]
		return AccountChoice{Profile: c.Profile.Name, Identity: c.Identity, ConfigDir: c.Profile.ConfigDir, Provenance: c.Provenance}, nil
	default:
		names := make([]string, len(verified))
		for i, c := range verified {
			names[i] = fmt.Sprintf("%s <%s>", c.Profile.Name, c.Identity)
		}
		return AccountChoice{}, fmt.Errorf(
			"adopt: %d verified account candidates found (%s) — ambiguous in non-interactive mode; "+
				"pass --account/--identity to choose one", len(verified), strings.Join(names, ", "))
	}
}

// ResolveGateNonInteractive resolves the gate command list for --yes /
// non-TTY mode: an explicit --gate list always wins; otherwise the inferred
// proposals are accepted only when there is at least one candidate (an
// unambiguous ecosystem match — InferGate already returns a definite,
// ordered list per design §6, never a menu of alternatives to choose among).
// No evidence at all fails closed, naming --gate.
func ResolveGateNonInteractive(proposals []onboard.Proposal, explicit []string) ([]string, error) {
	if len(explicit) > 0 {
		return explicit, nil
	}
	if len(proposals) == 0 {
		return nil, fmt.Errorf(
			"adopt: no gate could be inferred — non-interactive mode fails closed; " +
				`pass --gate "cmd1;;cmd2" (or repeat --gate)`)
	}
	out := make([]string, len(proposals))
	for i, p := range proposals {
		out[i] = p.Value
	}
	return out, nil
}

// ResolveForgeNonInteractive resolves the forge provider for --yes / non-TTY
// mode: an explicit --forge always wins; an inferred value is accepted as
// unambiguous. A remote that exists but matches no known forge host is
// ambiguous and fails closed naming --forge. An ABSENT remote (a genuinely
// local-only repo, or --no-remote) is not ambiguous — there is nothing to be
// wrong about — so it resolves to "" without error, matching design §6's
// "remote host match … else ask/omit" for the inference itself.
func ResolveForgeNonInteractive(proposal onboard.Proposal, explicit, remoteURL string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if proposal.Value != "" {
		return proposal.Value, nil
	}
	if strings.TrimSpace(remoteURL) == "" {
		return "", nil
	}
	return "", fmt.Errorf(
		"adopt: remote %q does not match a known forge — non-interactive mode fails closed; "+
			"pass --forge github|gitlab", remoteURL)
}
