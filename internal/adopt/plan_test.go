// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/registry"
)

// --- fixtures ----------------------------------------------------------------

// setKoryphHome points KORYPH_HOME at a fresh temp dir for the duration of the
// test and, when withRegistry is true, pre-creates registry.d so buildHomeStep
// (which reads paths.KoryphHome() directly, not the Snapshot) sees an
// already-initialized home.
func setKoryphHome(t *testing.T, withRegistry bool) {
	t.Helper()
	home := t.TempDir()
	if withRegistry {
		if err := os.MkdirAll(filepath.Join(home, "registry.d"), 0o755); err != nil {
			t.Fatalf("mkdir registry.d: %v", err)
		}
	}
	t.Setenv("KORYPH_HOME", home)
}

// findStep returns the first step matching id, or fails the test.
func findStep(t *testing.T, steps []Step, id StepID) Step {
	t.Helper()
	for _, s := range steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step with id %q in plan: %+v", id, steps)
	return Step{}
}

// toolSteps returns every Step with ID == StepTools, in plan order.
func toolSteps(steps []Step) []Step {
	var out []Step
	for _, s := range steps {
		if s.ID == StepTools {
			out = append(out, s)
		}
	}
	return out
}

// --- BuildPlan: all-missing ---------------------------------------------------

// TestBuildPlan_AllMissing exercises a snapshot where nothing has been done
// yet: no tools, no beads, unregistered, no config, no assets. bd is
// deliberately OMITTED from Tools (rather than given Found: false) so this
// fixture exercises the general "everything needed" shape independently of
// the bd-blocks-beads interaction, which TestBuildPlan_BDNotFoundBlocksBeads
// and TestBuildPlan_BDTooOldNotBlocked cover directly.
func TestBuildPlan_AllMissing(t *testing.T) {
	setKoryphHome(t, false)

	snap := &Snapshot{
		Root:      "/repo",
		ProjectID: "acme",
		Tools: map[string]ToolStatus{
			"git":    {Name: "git", Found: false},
			"claude": {Name: "claude", Found: false},
			"gh":     {Name: "gh", Found: false},
		},
		Inventory: &onboard.Inventory{Root: "/repo"},
	}

	steps := BuildPlan(snap)

	tools := toolSteps(steps)
	if len(tools) != 3 {
		t.Fatalf("tools rows = %d, want 3 (git, claude, gh); steps=%+v", len(tools), tools)
	}
	for _, s := range tools {
		if s.State != StateNeeded {
			t.Errorf("tools row %+v State = %q, want needed", s, s.State)
		}
	}

	for _, id := range []StepID{StepHome, StepBeads, StepRegister, StepConfig, StepAssets} {
		if st := findStep(t, steps, id); st.State != StateNeeded {
			t.Errorf("step %q State = %q, want needed (detail: %s)", id, st.State, st.Detail)
		}
	}
	if st := findStep(t, steps, StepBeads); st.State == StateBlocked {
		t.Errorf("beads step blocked in an all-missing fixture with no bd tool entry at all; want needed")
	}

	if st := findStep(t, steps, StepSigning); st.State != StateOffer {
		t.Errorf("signing State = %q, want offer", st.State)
	}
	if st := findStep(t, steps, StepPosture); st.State != StateOffer {
		t.Errorf("posture State = %q, want offer", st.State)
	}
	if st := findStep(t, steps, StepCommit); st.State != StateNeeded {
		t.Errorf("commit State = %q, want needed (nothing is done yet)", st.State)
	}
	if st := findStep(t, steps, StepVerify); st.State != StateNeeded {
		t.Errorf("verify State = %q, want needed", st.State)
	}
}

// --- BuildPlan: all-present ----------------------------------------------------

// TestBuildPlan_AllPresent exercises a fully-adopted project: everything
// should read done except the always-optional offers (signing, posture) and
// verify (which needs a validated registry record, not merely a registered
// one) — and the commit step collapses to done ("nothing to commit") because
// every other required step is already satisfied.
func TestBuildPlan_AllPresent(t *testing.T) {
	setKoryphHome(t, true)

	snap := &Snapshot{
		Root:      "/repo",
		ProjectID: "acme",
		Tools: map[string]ToolStatus{
			"git":    {Name: "git", Found: true, VersionOK: true, Version: "2.49.0"},
			"claude": {Name: "claude", Found: true, VersionOK: true, Version: "2.1.0", Authed: true},
			"bd":     {Name: "bd", Found: true, VersionOK: true, Version: "1.1.0"},
			"gh":     {Name: "gh", Found: true, VersionOK: true, Version: "2.62.0"},
		},
		Inventory: &onboard.Inventory{
			Root:           "/repo",
			HasBeads:       true,
			BeadsHardened:  true,
			BeadsHooks:     true,
			ClaudeSettings: true,
			BDPrimeHook:    true,
			Personas:       []string{"koryph-implementer"},
			AdapterPresent: true,
		},
		ExistingRecord: &registry.Record{
			ProjectID:        "acme",
			AccountProfile:   "personal",
			ExpectedIdentity: "me@example.com",
			MigrationStatus:  registry.StatusRegistered, // not validated -> verify stays needed
		},
	}

	steps := BuildPlan(snap)

	tools := toolSteps(steps)
	if len(tools) != 1 {
		t.Fatalf("tools rows = %d, want exactly 1 combined done row; steps=%+v", len(tools), tools)
	}
	if tools[0].State != StateDone {
		t.Errorf("tools row State = %q, want done", tools[0].State)
	}

	for _, id := range []StepID{StepHome, StepBeads, StepRegister, StepConfig, StepAssets} {
		if st := findStep(t, steps, id); st.State != StateDone {
			t.Errorf("step %q State = %q, want done (detail: %s)", id, st.State, st.Detail)
		}
	}

	if st := findStep(t, steps, StepSigning); st.State != StateOffer {
		t.Errorf("signing State = %q, want offer", st.State)
	}
	if st := findStep(t, steps, StepPosture); st.State != StateOffer {
		t.Errorf("posture State = %q, want offer", st.State)
	}

	commit := findStep(t, steps, StepCommit)
	if commit.State != StateDone {
		t.Errorf("commit State = %q, want done", commit.State)
	}
	if !strings.Contains(commit.Detail, "nothing to commit") {
		t.Errorf("commit Detail = %q, want it to mention nothing to commit", commit.Detail)
	}

	if st := findStep(t, steps, StepVerify); st.State != StateNeeded {
		t.Errorf("verify State = %q, want needed (record is registered, not validated)", st.State)
	}
}

// --- bd version / presence interactions with the beads step -------------------

// TestBuildPlan_BDTooOldNotBlocked: bd IS found but fails the version gate.
// The tools category surfaces a needed row carrying the remediation text, and
// the beads step itself is merely needed (not blocked) — bd being present at
// all is what unblocks it, per buildBeadsStep's guard.
func TestBuildPlan_BDTooOldNotBlocked(t *testing.T) {
	setKoryphHome(t, true)

	const remediation = "bd 0.9.0 at /usr/local/bin/bd is older than the required 1.0.5: upgrade beads"
	snap := &Snapshot{
		Root:      "/repo",
		ProjectID: "acme",
		Tools: map[string]ToolStatus{
			"git":    {Name: "git", Found: true, VersionOK: true},
			"claude": {Name: "claude", Found: true, VersionOK: true},
			"bd":     {Name: "bd", Found: true, VersionOK: false, Remediation: remediation},
			"gh":     {Name: "gh", Found: true, VersionOK: true},
		},
		Inventory: &onboard.Inventory{Root: "/repo"},
	}

	steps := BuildPlan(snap)

	beads := findStep(t, steps, StepBeads)
	if beads.State == StateBlocked {
		t.Fatalf("beads step blocked despite bd being found (just too old); detail=%q", beads.Detail)
	}
	if beads.State != StateNeeded {
		t.Errorf("beads State = %q, want needed (HasBeads is false in this fixture)", beads.State)
	}

	tools := toolSteps(steps)
	var bdRow *Step
	for i := range tools {
		if strings.Contains(tools[i].Detail, remediation) {
			bdRow = &tools[i]
		}
	}
	if bdRow == nil {
		t.Fatalf("no tools row carries the bd remediation text %q; tools=%+v", remediation, tools)
	}
	if bdRow.State != StateNeeded {
		t.Errorf("bd tools row State = %q, want needed", bdRow.State)
	}
}

// TestBuildPlan_BDNotFoundBlocksBeads: bd is genuinely absent -> the beads
// step is blocked (distinct from merely needed), naming the tools step as the
// unblocking action.
func TestBuildPlan_BDNotFoundBlocksBeads(t *testing.T) {
	setKoryphHome(t, true)

	snap := &Snapshot{
		Root:      "/repo",
		ProjectID: "acme",
		Tools: map[string]ToolStatus{
			"bd": {Name: "bd", Found: false},
		},
		Inventory: &onboard.Inventory{Root: "/repo"},
	}

	steps := BuildPlan(snap)

	beads := findStep(t, steps, StepBeads)
	if beads.State != StateBlocked {
		t.Errorf("beads State = %q, want blocked (bd not found)", beads.State)
	}
	if !strings.Contains(beads.Detail, "not installed") {
		t.Errorf("beads Detail = %q, want it to mention bd is not installed", beads.Detail)
	}
}

// --- RenderPlan ----------------------------------------------------------------

// TestRenderPlan_NeededStepShowsWhy is a golden-ish check on the plain-text
// rendering: the header, at least one "needed" line, and an indented "why:"
// continuation line for a needed step (design §3.2's example block).
func TestRenderPlan_NeededStepShowsWhy(t *testing.T) {
	setKoryphHome(t, false)

	snap := &Snapshot{
		Root:      "/Users/me/src/myrepo",
		ProjectID: "myrepo",
		Tools: map[string]ToolStatus{
			"git": {Name: "git", Found: false},
		},
		Inventory: &onboard.Inventory{Root: "/Users/me/src/myrepo"},
	}
	steps := BuildPlan(snap)

	var buf bytes.Buffer
	RenderPlan(&buf, snap.ProjectID, snap.Root, steps)
	out := buf.String()

	if !strings.Contains(out, "ADOPTION PLAN") {
		t.Errorf("output missing header ADOPTION PLAN:\n%s", out)
	}
	if !strings.Contains(out, string(StateNeeded)) {
		t.Errorf("output missing a %q line:\n%s", StateNeeded, out)
	}
	if !strings.Contains(out, "why:") {
		t.Errorf("output missing an indented why: continuation line:\n%s", out)
	}
	// The why: line must be indented under the detail column, not flush left.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "why:") && !strings.HasPrefix(line, " ") {
			t.Errorf("why: line not indented: %q", line)
		}
	}
}
