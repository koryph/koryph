// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/koryph/koryph/internal/cockpit"
)

// TestDetailThinkingTail proves the koryph-xvk flow end-to-end at the model
// layer: T opens a live thinking tail parsed from the slot's stream.jsonl
// (thinking deltas only, subagent divider included), f toggles follow, and
// esc returns to the detail panel.
func TestDetailThinkingTail(t *testing.T) {
	stream := filepath.Join(t.TempDir(), "stream.jsonl")
	lines := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"planning the gateway fix"}},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"nested probe"}},"parent_tool_use_id":"toolu_abcdefgh"}`,
		`{"type":"result","total_cost_usd":0.1}`,
	}, "\n") + "\n"
	if err := os.WriteFile(stream, []byte(lines), 0o644); err != nil {
		t.Fatalf("write stream fixture: %v", err)
	}

	m := newDetailModel(DefaultTheme())
	m.Resize(100, 30)
	m.SetDetail(cockpit.BeadDetailSnapshot{BeadID: "b1", Title: "bead", StreamPath: stream})

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	dm := mm.(*detailModel)
	if !dm.thinkingMode || !dm.thinkingFollow {
		t.Fatalf("T must enter thinking mode with follow on (mode=%v follow=%v)", dm.thinkingMode, dm.thinkingFollow)
	}
	view := dm.View()
	if !strings.Contains(view, "Thinking tail:") {
		t.Errorf("view missing thinking header:\n%s", view)
	}
	if !strings.Contains(view, "planning the gateway fix") {
		t.Errorf("view missing extracted thinking:\n%s", view)
	}
	if !strings.Contains(view, "subagent …u_abcdefgh") && !strings.Contains(view, "subagent") {
		t.Errorf("view missing subagent divider:\n%s", view)
	}

	mm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	dm = mm.(*detailModel)
	if dm.thinkingFollow {
		t.Error("f must toggle follow off")
	}

	mm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	dm = mm.(*detailModel)
	if dm.thinkingMode {
		t.Error("esc must exit thinking mode")
	}
	if v := dm.View(); !strings.Contains(v, "[b1]") {
		t.Errorf("after esc the detail panel must render again:\n%s", v)
	}
}

// TestDetailThinkingTailNoStream proves the graceful path when the bead has
// no slot stream (never dispatched): entering the mode shows a hint, not a
// crash or a blank screen.
func TestDetailThinkingTailNoStream(t *testing.T) {
	m := newDetailModel(DefaultTheme())
	m.Resize(100, 30)
	m.SetDetail(cockpit.BeadDetailSnapshot{BeadID: "b2", Title: "bead"})

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	dm := mm.(*detailModel)
	if !dm.thinkingMode {
		t.Fatal("T must enter thinking mode even without a stream")
	}
	if view := dm.View(); !strings.Contains(view, "Thinking tail:") {
		t.Errorf("view missing header:\n%s", view)
	}
}
