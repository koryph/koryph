// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/koryph/koryph/internal/cockpit"
	"github.com/koryph/koryph/internal/runtime"
)

// streamFixture writes a realistic stream.jsonl (content_block_start → deltas →
// content_block_stop, as every dispatched agent emits under
// --include-partial-messages) with one thinking block, one tool call, one
// assistant message, and a nested-subagent thinking block.
func streamFixture(t *testing.T) string {
	t.Helper()
	block := func(idx int, parent, cb, delta string) string {
		p := "null"
		if parent != "" {
			p = `"` + parent + `"`
		}
		return strings.Join([]string{
			fmt.Sprintf(`{"type":"stream_event","event":{"type":"content_block_start","index":%d,"content_block":%s},"parent_tool_use_id":%s}`, idx, cb, p),
			fmt.Sprintf(`{"type":"stream_event","event":{"type":"content_block_delta","index":%d,"delta":%s},"parent_tool_use_id":%s}`, idx, delta, p),
			fmt.Sprintf(`{"type":"stream_event","event":{"type":"content_block_stop","index":%d},"parent_tool_use_id":%s}`, idx, p),
		}, "\n")
	}
	lines := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		block(0, "", `{"type":"thinking"}`, `{"type":"thinking_delta","thinking":"planning the gateway fix"}`),
		block(1, "", `{"type":"tool_use","name":"Read","input":{}}`, `{"type":"input_json_delta","partial_json":"{\"file_path\":\"/repo/gateway.go\"}"}`),
		block(1, "", `{"type":"text"}`, `{"type":"text_delta","text":"Reading the gateway now."}`),
		block(0, "toolu_abcdefgh", `{"type":"thinking"}`, `{"type":"thinking_delta","thinking":"nested subagent probe"}`),
		`{"type":"result","total_cost_usd":0.1}`,
	}, "\n") + "\n"

	p := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatalf("write stream fixture: %v", err)
	}
	return p
}

func openActivityTail(t *testing.T, streamPath string) *detailModel {
	t.Helper()
	m := newDetailModel(DefaultTheme())
	m.Resize(100, 30)
	m.SetDetail(cockpit.BeadDetailSnapshot{BeadID: "b1", Title: "bead", StreamPath: streamPath})
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	return mm.(*detailModel)
}

// TestDetailActivityTail proves the koryph-xvk flow: T opens the activity tail
// (follow on, all categories), the header renders, and every kind — thinking,
// tool call (with name), assistant message, and the subagent divider — is
// visible under the default "all" filter. f toggles follow; esc returns.
func TestDetailActivityTail(t *testing.T) {
	dm := openActivityTail(t, streamFixture(t))
	if !dm.activityMode || !dm.activityFollow {
		t.Fatalf("T must enter activity mode with follow on (mode=%v follow=%v)", dm.activityMode, dm.activityFollow)
	}
	view := dm.View()
	for _, want := range []string{
		"Activity [",
		"planning the gateway fix", // thinking
		"Read",                     // tool name
		"gateway.go",               // tool input summary
		"Reading the gateway now.", // assistant message
		"subagent",                 // nested divider
	} {
		if !strings.Contains(view, want) {
			t.Errorf("activity view missing %q:\n%s", want, view)
		}
	}

	mm, _ := dm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	dm = mm.(*detailModel)
	if dm.activityFollow {
		t.Error("f must toggle follow off")
	}

	mm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	dm = mm.(*detailModel)
	if dm.activityMode {
		t.Error("esc must exit activity mode")
	}
	if v := dm.View(); !strings.Contains(v, "b1") || !strings.Contains(v, "bead") {
		t.Errorf("after esc the detail panel must render again:\n%s", v)
	}
}

func TestDetailActivityTailSelectsCodexProjector(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-7"}`,
		`{"type":"item.completed","item":{"type":"reasoning","text":"Codex reasoning signal"}}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"go test ./..."}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Codex message signal"}}`,
		`{"type":"turn.completed"}`,
		`{"type":"turn.failed","error":{"message":"Codex error signal"}}`,
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "codex-stream.jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newDetailModel(DefaultTheme())
	m.Resize(100, 30)
	m.SetDetail(cockpit.BeadDetailSnapshot{
		BeadID: "b-codex", Title: "codex bead", Runtime: "codex", StreamPath: path,
	})
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	view := mm.(*detailModel).View()
	for _, want := range []string{
		"Codex reasoning signal", "go test", "Codex message signal",
		"turn completed", "Codex error signal",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("Codex activity view missing %q:\n%s", want, view)
		}
	}
}

func TestDetailActivityTailUnsupportedRuntimeIsNeutral(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newDetailModel(DefaultTheme())
	m.Resize(100, 30)
	m.SetDetail(cockpit.BeadDetailSnapshot{
		BeadID: "future", Runtime: "future-runtime", StreamPath: path,
	})
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	if view := mm.(*detailModel).View(); !strings.Contains(view, "no activity projector") {
		t.Errorf("unsupported runtime view =\n%s", view)
	}
}

// TestDetailActivityFilter proves the filter keys isolate each category and
// that 0/a returns to all.
func TestDetailActivityFilter(t *testing.T) {
	dm := openActivityTail(t, streamFixture(t))

	press := func(r rune) {
		mm, _ := dm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		dm = mm.(*detailModel)
	}

	// 1 = thinking only.
	press('1')
	if dm.activityFilter != filterThinking {
		t.Fatalf("1 must select thinking, got %v", dm.activityFilter)
	}
	v := dm.View()
	if !strings.Contains(v, "planning the gateway fix") {
		t.Errorf("thinking filter must show reasoning:\n%s", v)
	}
	if strings.Contains(v, "Reading the gateway now.") {
		t.Errorf("thinking filter must hide assistant messages:\n%s", v)
	}

	// 2 = tools only.
	press('2')
	v = dm.View()
	if !strings.Contains(v, "Read") || !strings.Contains(v, "gateway.go") {
		t.Errorf("tools filter must show the tool call:\n%s", v)
	}
	if strings.Contains(v, "planning the gateway fix") {
		t.Errorf("tools filter must hide thinking:\n%s", v)
	}

	// 3 = messages only.
	press('3')
	v = dm.View()
	if !strings.Contains(v, "Reading the gateway now.") {
		t.Errorf("messages filter must show assistant text:\n%s", v)
	}
	if strings.Contains(v, "planning the gateway fix") {
		t.Errorf("messages filter must hide thinking:\n%s", v)
	}

	// 0 = all again.
	press('0')
	if dm.activityFilter != filterAll {
		t.Fatalf("0 must select all, got %v", dm.activityFilter)
	}
	v = dm.View()
	if !strings.Contains(v, "planning the gateway fix") || !strings.Contains(v, "Reading the gateway now.") {
		t.Errorf("all filter must show every category:\n%s", v)
	}
}

// TestDetailActivityScrollbackPausesFollow proves the scroll-back UX: scrolling
// up off the bottom pauses follow so scrollback is readable, and f re-enables
// follow (jumping back to the bottom).
func TestDetailActivityScrollbackPausesFollow(t *testing.T) {
	// A tall stream so the tail viewport is scrollable.
	var b strings.Builder
	b.WriteString(`{"type":"system","subtype":"init"}` + "\n")
	for i := range 40 {
		fmt.Fprintln(&b, `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}},"parent_tool_use_id":null}`)
		fmt.Fprintf(&b, `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning step %d"}},"parent_tool_use_id":null}`+"\n", i)
		fmt.Fprintln(&b, `{"type":"stream_event","event":{"type":"content_block_stop","index":0},"parent_tool_use_id":null}`)
	}
	p := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := newDetailModel(DefaultTheme())
	m.Resize(100, 12) // small viewport → content overflows, scrollable
	m.SetDetail(cockpit.BeadDetailSnapshot{BeadID: "b1", Title: "bead", StreamPath: p})
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	dm := mm.(*detailModel)
	if !dm.activityFollow || !dm.activityVP.AtBottom() {
		t.Fatalf("opening must follow at the bottom (follow=%v atBottom=%v)", dm.activityFollow, dm.activityVP.AtBottom())
	}

	// Scroll up a page — follow must auto-pause.
	mm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	dm = mm.(*detailModel)
	if dm.activityFollow {
		t.Error("scrolling up must auto-pause follow")
	}
	if v := dm.View(); !strings.Contains(v, "[paused]") {
		t.Errorf("paused follow must show a [paused] indicator:\n%s", v)
	}

	// f re-enables follow and jumps back to the bottom.
	mm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	dm = mm.(*detailModel)
	if !dm.activityFollow || !dm.activityVP.AtBottom() {
		t.Errorf("f must resume follow at the bottom (follow=%v atBottom=%v)", dm.activityFollow, dm.activityVP.AtBottom())
	}
}

// appendThinkingSteps appends steps [from,to) as realistic thinking blocks to
// path (creating it if absent), simulating a running agent's growing stream.
func appendThinkingSteps(t *testing.T, path string, from, to int) {
	t.Helper()
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintln(&b, `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}},"parent_tool_use_id":null}`)
		fmt.Fprintf(&b, `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning step %d"}},"parent_tool_use_id":null}`+"\n", i)
		fmt.Fprintln(&b, `{"type":"stream_event","event":{"type":"content_block_stop","index":0},"parent_tool_use_id":null}`)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
}

// TestDetailActivityAutoScrollsOnGrowth proves the live-follow behavior the
// operator sees: while following, each RefreshTail (the App's per-tick driver)
// re-reads the grown stream and keeps the viewport pinned to the new bottom, so
// the newest activity stays on screen and older lines scroll off. Once follow is
// paused (by scrolling up) further growth must not move the viewport.
func TestDetailActivityAutoScrollsOnGrowth(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stream.jsonl")
	appendThinkingSteps(t, p, 0, 40) // taller than the viewport

	m := newDetailModel(DefaultTheme())
	m.Resize(100, 12)
	m.SetDetail(cockpit.BeadDetailSnapshot{BeadID: "b1", Title: "bead", StreamPath: p})
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	dm := mm.(*detailModel)
	if !dm.activityFollow || !dm.activityVP.AtBottom() {
		t.Fatalf("open: follow=%v atBottom=%v", dm.activityFollow, dm.activityVP.AtBottom())
	}
	if !strings.Contains(dm.View(), "reasoning step 39") {
		t.Fatalf("open: newest step must be visible:\n%s", dm.View())
	}

	// The stream grows; RefreshTail is what the App calls each tick.
	appendThinkingSteps(t, p, 40, 80)
	dm.RefreshTail()
	if !dm.activityVP.AtBottom() {
		t.Error("following: viewport must stay pinned to the bottom as the stream grows")
	}
	if v := dm.View(); !strings.Contains(v, "reasoning step 79") || strings.Contains(v, "reasoning step 39") {
		t.Errorf("following: newest (79) must be visible and old bottom (39) scrolled off:\n%s", v)
	}

	// Scroll up → follow pauses; subsequent growth must NOT yank the view.
	mm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	dm = mm.(*detailModel)
	if dm.activityFollow {
		t.Fatal("scroll up must pause follow")
	}
	off := dm.activityVP.YOffset
	appendThinkingSteps(t, p, 80, 120)
	dm.RefreshTail()
	if dm.activityVP.YOffset != off {
		t.Errorf("paused: growth must not move the viewport (was %d now %d)", off, dm.activityVP.YOffset)
	}
}

func thinkingBlockLine(text string) string {
	return strings.Join([]string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}},"parent_tool_use_id":null}`,
		fmt.Sprintf(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}},"parent_tool_use_id":null}`, text),
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"parent_tool_use_id":null}`,
	}, "\n") + "\n"
}

func entriesContain(entries []claudeActivityEntry, sub string) bool {
	for _, e := range entries {
		if strings.Contains(e.Text, sub) {
			return true
		}
	}
	return false
}

// claudeActivityEntry aliases the entry type so the helper signature reads
// cleanly in this test file.
type claudeActivityEntry = runtime.ActivityEntry

// TestDetailActivityFullHistory proves the 'h' toggle: the default tail window
// cannot reach an entry emitted more than 512 KB ago, but switching to full
// history loads the whole stream from byte 0 so the earliest entry becomes
// available, and the header reflects the active scope.
func TestDetailActivityFullHistory(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"type":"system","subtype":"init"}` + "\n")
	b.WriteString(thinkingBlockLine("EARLIEST-MARKER"))
	pad := strings.Repeat("x", 400)
	for i := 0; b.Len() < 600*1024; i++ {
		b.WriteString(thinkingBlockLine(fmt.Sprintf("pad %d %s", i, pad)))
	}
	b.WriteString(thinkingBlockLine("LATEST-MARKER"))

	p := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	dm := openActivityTail(t, p) // opens in the default 512 KB tail mode

	// Tail window: the newest is present, the earliest (>512 KB back) is not.
	if !entriesContain(dm.activityEntries, "LATEST-MARKER") {
		t.Error("tail window must include the newest entry")
	}
	if entriesContain(dm.activityEntries, "EARLIEST-MARKER") {
		t.Error("tail window must NOT reach an entry emitted >512 KB ago")
	}
	if v := dm.View(); !strings.Contains(v, "tail 512K") {
		t.Errorf("header must show the tail scope:\n%s", firstLine(v))
	}

	// h → full history: the earliest entry is now loaded.
	mm, _ := dm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	dm = mm.(*detailModel)
	if !dm.activityFull {
		t.Fatal("h must enable full-history mode")
	}
	if !entriesContain(dm.activityEntries, "EARLIEST-MARKER") {
		t.Error("full history must load the earliest entry (byte 0)")
	}
	if !entriesContain(dm.activityEntries, "LATEST-MARKER") {
		t.Error("full history must still include the newest entry")
	}
	if v := dm.View(); !strings.Contains(v, "full history") {
		t.Errorf("header must show the full-history scope:\n%s", firstLine(v))
	}

	// h again → back to the bounded tail window.
	mm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	dm = mm.(*detailModel)
	if dm.activityFull {
		t.Fatal("h must toggle full-history back off")
	}
	if entriesContain(dm.activityEntries, "EARLIEST-MARKER") {
		t.Error("returning to tail mode must drop the out-of-window earliest entry")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestDetailActivityTailNoStream proves the graceful path when the bead has no
// slot stream (never dispatched): entering the mode shows a hint, not a crash.
func TestDetailActivityTailNoStream(t *testing.T) {
	m := newDetailModel(DefaultTheme())
	m.Resize(100, 30)
	m.SetDetail(cockpit.BeadDetailSnapshot{BeadID: "b2", Title: "bead"})

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	dm := mm.(*detailModel)
	if !dm.activityMode {
		t.Fatal("T must enter activity mode even without a stream")
	}
	if view := dm.View(); !strings.Contains(view, "Activity [") {
		t.Errorf("view missing header:\n%s", view)
	}
}
