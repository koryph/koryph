// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/onboard"
)

// --- ResolveAccountNonInteractive ---------------------------------------------

func TestResolveAccountNonInteractive_ExplicitPairWins(t *testing.T) {
	// Candidates are present but must be ignored: explicit flags always win.
	candidates := []account.Candidate{
		{Profile: account.Profile{Name: "personal"}, Identity: "auto@example.com", Verified: true},
	}
	got, err := ResolveAccountNonInteractive(candidates, "work", "me@corp.com", "/cfg/work")
	if err != nil {
		t.Fatalf("ResolveAccountNonInteractive: %v", err)
	}
	want := AccountChoice{Profile: "work", Identity: "me@corp.com", ConfigDir: "/cfg/work", Provenance: "explicit --account/--identity"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveAccountNonInteractive_ExplicitHalfErrorsNamingBothFlags(t *testing.T) {
	cases := []struct {
		name              string
		profile, identity string
	}{
		{"profile only", "work", ""},
		{"identity only", "", "me@corp.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveAccountNonInteractive(nil, tc.profile, tc.identity, "")
			if err == nil {
				t.Fatal("expected an error for a half-specified --account/--identity pair")
			}
			for _, want := range []string{"--account", "--identity"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestResolveAccountNonInteractive_ZeroVerifiedErrorsMentioningAccountFlag(t *testing.T) {
	candidates := []account.Candidate{
		{Profile: account.Profile{Name: "personal"}, Verified: false, Err: "no ~/.claude.json yet"},
	}
	_, err := ResolveAccountNonInteractive(candidates, "", "", "")
	if err == nil {
		t.Fatal("expected an error when no candidate verified")
	}
	if !strings.Contains(err.Error(), "--account") {
		t.Errorf("error %q does not mention --account", err.Error())
	}
}

func TestResolveAccountNonInteractive_ExactlyOneVerifiedIsChosen(t *testing.T) {
	candidates := []account.Candidate{
		{
			Profile:    account.Profile{Name: "personal"},
			Identity:   "me@example.com",
			Verified:   true,
			Provenance: "derived from ~/.claude.json",
		},
	}
	got, err := ResolveAccountNonInteractive(candidates, "", "", "")
	if err != nil {
		t.Fatalf("ResolveAccountNonInteractive: %v", err)
	}
	want := AccountChoice{Profile: "personal", Identity: "me@example.com", ConfigDir: "", Provenance: "derived from ~/.claude.json"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveAccountNonInteractive_TwoVerifiedErrorsNamingBothCandidates(t *testing.T) {
	candidates := []account.Candidate{
		{Profile: account.Profile{Name: "personal"}, Identity: "a@example.com", Verified: true},
		{Profile: account.Profile{Name: "work"}, Identity: "b@example.com", Verified: true},
	}
	_, err := ResolveAccountNonInteractive(candidates, "", "", "")
	if err == nil {
		t.Fatal("expected an error when multiple candidates verified")
	}
	for _, want := range []string{"personal <a@example.com>", "work <b@example.com>", "--account"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// --- ResolveGateNonInteractive -------------------------------------------------

func TestResolveGateNonInteractive(t *testing.T) {
	cases := []struct {
		name      string
		proposals []onboard.Proposal
		explicit  []string
		want      []string
		wantErr   bool
	}{
		{
			name:      "explicit wins over proposals",
			proposals: []onboard.Proposal{{Value: "make gate"}},
			explicit:  []string{"custom cmd"},
			want:      []string{"custom cmd"},
		},
		{
			name:      "proposals accepted in order",
			proposals: []onboard.Proposal{{Value: "make test"}, {Value: "make lint"}},
			want:      []string{"make test", "make lint"},
		},
		{
			name:    "none errors naming --gate",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveGateNonInteractive(tc.proposals, tc.explicit)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), "--gate") {
					t.Errorf("error %q does not mention --gate", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveGateNonInteractive: %v", err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// --- ResolveForgeNonInteractive -------------------------------------------------

func TestResolveForgeNonInteractive(t *testing.T) {
	cases := []struct {
		name      string
		proposal  onboard.Proposal
		explicit  string
		remoteURL string
		want      string
		wantErr   bool
	}{
		{
			name:      "explicit wins even over a non-matching remote",
			proposal:  onboard.Proposal{},
			explicit:  "gitlab",
			remoteURL: "https://github.com/o/r.git",
			want:      "gitlab",
		},
		{
			name:      "inferred wins when no explicit override",
			proposal:  onboard.Proposal{Value: "github", Provenance: "host-matched"},
			remoteURL: "https://github.com/o/r.git",
			want:      "github",
		},
		{
			name: "no remote yields empty without error",
			want: "",
		},
		{
			name:      "unknown-host remote errors naming --forge",
			remoteURL: "https://git.example.corp/o/r.git",
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveForgeNonInteractive(tc.proposal, tc.explicit, tc.remoteURL)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), "--forge") {
					t.Errorf("error %q does not mention --forge", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveForgeNonInteractive: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
