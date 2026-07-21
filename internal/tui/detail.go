// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// detail.go implements the Detail tab model for the koryph TUI cockpit
// (koryph-9af.3). It renders the full bead detail panel including metadata,
// dependency graph, attempt history, and log-tail shortcut.
//
// Navigation:
//   - j/↓ and k/↑ move keyboard focus through dep/reverse-dep rows.
//   - Enter on a focused dep row pushes the current bead onto the navigation
//     stack and opens the selected dep bead.
//   - Backspace/Esc pops the navigation stack; when the stack is empty an
//     Esc emits detailBackMsg so the App returns to the previous tab.
//   - 't' toggles the raw log-tail viewport (viewport follows on each tick).
//   - 'T' toggles the activity-tail viewport (koryph-xvk): the agent's live
//     stream.jsonl parsed into classified entries — thinking, tool calls, and
//     assistant messages — filterable with 0/1/2/3 (all/thinking/tools/msgs).
//     'h' toggles between the fast 512 KB tail window and the whole run (full
//     history, parsed incrementally). Subagent segments are labeled. Both tails
//     follow live and auto-pause follow when the operator scrolls up (see
//     RefreshTail for the tick wiring).
//   - Mouse clicks on dep rows via bubblezone set keyboard focus.
package tui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"

	"github.com/koryph/koryph/internal/cockpit"
	"github.com/koryph/koryph/internal/runtime/claude"
)

func init() {
	registerTab(TabDef{
		Name:  "Detail",
		Order: 99,
		// Hidden: the Detail panel is an overlay reached only by pressing Enter
		// on a Threads or Queue row, never a standalone tab. It is kept out of
		// the tab bar and Tab-cycling; Esc/Backspace returns to the originating
		// tab (see App.detailBackMsg handling).
		Hidden: true,
		New:    func(theme Theme, _ bool) TabModel { return newDetailModel(theme) },
	})
}

// detailRow is a navigable row in the detail panel (e.g. a dep link).
type detailRow struct {
	id        string // bead ID this row points to
	label     string // "dep" or "rdep"
	isBlocker bool   // true when dep is not yet closed (blocks this bead)
	zoneID    string // stable zone ID for mouse-click hit testing
}

// detailModel is the Bubble Tea model for the Detail tab.
type detailModel struct {
	theme  Theme
	width  int
	height int

	// beadID is the currently focused bead. Empty means nothing is selected.
	beadID string

	// projectID is the active project, used to strip the redundant
	// "<project>-" prefix from bead ids when the breadcrumb needs to fit a
	// narrow terminal. Set from every SetSnapshot; empty is a safe no-op
	// fallback (ids render unchanged) before the first snapshot arrives.
	projectID string

	// navStack is the bead-ID history before the current bead. Backspace pops.
	navStack []string

	// detail is the last fetched detail snapshot.
	detail cockpit.BeadDetailSnapshot

	// rows holds navigable rows (dep links) for keyboard/mouse selection.
	rows   []detailRow
	cursor int

	// bodyVP is the scrollable viewport for the detail body — descriptions
	// and plans can be arbitrarily long, and before this viewport existed
	// everything below the terminal height was silently clipped with no way
	// to scroll (the "can't read the plan" issue). j/k scroll it; n/N cycle
	// dep-row focus with auto-scroll to keep the focused row visible.
	bodyVP viewport.Model

	// depLines maps rows[i] → its line index in the body content, for
	// focus-follow scrolling.
	depLines []int

	// logMode is true when the log-tail viewport is shown instead of the detail.
	logMode bool

	// logVP is the viewport component used for the log-tail.
	logVP viewport.Model

	// logFollow enables auto-scroll to bottom on each tick.
	logFollow bool

	// activityMode is true when the activity-tail viewport is shown
	// (koryph-xvk): the agent's live stream.jsonl parsed into classified
	// entries — thinking, tool calls, and assistant messages — that the
	// operator filters between (all/thinking/tools/messages), mirroring the
	// raw log-tail mode above.
	activityMode bool

	// activityVP is the viewport for the activity tail — separate from logVP so
	// each mode keeps its own scroll position.
	activityVP viewport.Model

	// activityFollow enables auto-scroll to bottom on each refresh. It is
	// auto-paused when the operator scrolls up (so scrollback is readable) and
	// auto-resumed when they scroll back to the bottom; 'f' toggles it manually.
	activityFollow bool

	// activityFilter selects which entry kinds the activity tail shows.
	activityFilter activityFilter

	// activityFull switches the tail from the fast 512 KB window to the whole
	// stream from byte 0. 'h' toggles it. Full mode reads incrementally via
	// activityScanner so a large, growing stream is parsed once, not per tick.
	activityFull bool

	// activityScanner is the persistent incremental parser used in full-history
	// mode; nil in (or on entry to) tail mode. activityOffset is the byte count
	// already fed to it, and activityScanPath the stream it is bound to — a
	// change in either resets the scan.
	activityScanner  *claude.ActivityScanner
	activityOffset   int64
	activityScanPath string

	// activityEntries is the last parse of the slot's stream.jsonl (the 512 KB
	// window in tail mode, the whole stream in full mode), cached so a filter
	// change re-renders without re-reading the file.
	activityEntries []claude.ActivityEntry

	// zonePrefix is unique to this model instance to avoid zone ID collisions.
	zonePrefix string
}

// activityFilter selects which kinds of streamed activity the tail shows.
type activityFilter int

const (
	filterAll activityFilter = iota
	filterThinking
	filterTools
	filterMessages
)

// label returns the short name shown in the tail header/footer.
func (f activityFilter) label() string {
	switch f {
	case filterThinking:
		return "thinking"
	case filterTools:
		return "tools"
	case filterMessages:
		return "messages"
	default:
		return "all"
	}
}

// matches reports whether an entry of kind k is shown under this filter.
func (f activityFilter) matches(k claude.ActivityKind) bool {
	switch f {
	case filterThinking:
		return k == claude.ActThinking
	case filterTools:
		return k == claude.ActToolUse
	case filterMessages:
		return k == claude.ActMessage
	default:
		return true
	}
}

// newDetailModel creates an empty detail model.
func newDetailModel(theme Theme) *detailModel {
	// bubblezone's package-level functions require NewGlobal() to have been
	// called first; calling it multiple times is a no-op.
	zone.NewGlobal()
	return &detailModel{
		theme:      theme,
		width:      80,
		zonePrefix: zone.NewPrefix(),
		logVP:      viewport.New(80, 20),
		activityVP: viewport.New(80, 20),
		bodyVP:     viewport.New(80, 20),
	}
}

// Init implements TabModel.
func (m *detailModel) Init() tea.Cmd { return nil }

// IsCapturingInput implements TabModel. Detail tab has no text inputs.
func (m *detailModel) IsCapturingInput() bool { return false }

// SetBead sets the focused bead ID and clears stale detail.
// Called by App when showDetailMsg is received.
func (m *detailModel) SetBead(beadID string) {
	m.beadID = beadID
	m.detail = cockpit.BeadDetailSnapshot{}
	m.rows = nil
	m.cursor = 0
	m.logMode = false
	m.activityMode = false
	m.activityEntries = nil
	m.activityFull = false
	m.resetActivityScan()
	m.bodyVP.GotoTop()
	m.refreshBody()
}

// SetDetail stores a freshly-assembled detail snapshot.
// Called by App when detailReadyMsg is received.
func (m *detailModel) SetDetail(d cockpit.BeadDetailSnapshot) {
	m.detail = d
	m.beadID = d.BeadID
	m.rebuildRows()
	m.refreshBody()
}

// SetSnapshot implements TabModel. Refreshes the detail if a new snapshot
// carries an updated detail for our focused bead.
func (m *detailModel) SetSnapshot(snap cockpit.Snapshot) {
	m.projectID = snap.ProjectID
	if snap.Detail.BeadID != "" && snap.Detail.BeadID == m.beadID {
		m.detail = snap.Detail
		m.rebuildRows()
		m.refreshBody()
	}
	// Re-read the active live tail (log or thinking) so a fresh snapshot also
	// pulls in the latest file content. The primary live-follow driver is the
	// App's per-tick RefreshTail call (see below) — SetSnapshot fires only when
	// snapshotUnchanged sees a change, which is too coarse to follow a running
	// agent's continuously-growing stream on its own.
	m.RefreshTail()
}

// RefreshTail re-reads whichever live-tail file is currently open (the log tail
// or the thinking tail) and updates its viewport. The App calls this on every
// refresh tick while the Detail overlay is active, independent of the
// snapshotUnchanged dedup that gates SetSnapshot: a running agent's session.log
// and stream.jsonl grow continuously (thinking deltas arrive many times a
// second), while its status.json heartbeat — the field snapshotUnchanged keys
// on — only changes once per step. Without a tick-driven re-read the tail would
// freeze on whatever was captured the instant 't'/'T' was pressed and never
// follow the reasoning, which is the "can't tail the train of thought" bug.
// A no-op when no tail is open or the path is unknown.
func (m *detailModel) RefreshTail() {
	if m.logMode && m.detail.LogPath != "" {
		m.refreshLog()
	}
	if m.activityMode && m.detail.StreamPath != "" {
		m.refreshActivity()
	}
}

// Resize implements TabModel.
func (m *detailModel) Resize(w, h int) {
	m.width = w
	m.height = h
	m.logVP.Width = w
	m.logVP.Height = h - 4 // leave room for header/footer
	m.activityVP.Width = w
	m.activityVP.Height = h - 4
	m.bodyVP.Width = w
	m.bodyVP.Height = h - 3 // header line + footer hint + spacer
	if m.bodyVP.Height < 3 {
		m.bodyVP.Height = 3
	}
	m.refreshBody()
}

// rebuildRows rebuilds the navigable rows from the current detail snapshot.
// Deps (blockers for this bead) come first, then reverse-deps.
func (m *detailModel) rebuildRows() {
	m.rows = nil
	for i, dep := range m.detail.Deps {
		m.rows = append(m.rows, detailRow{
			id:        dep,
			label:     "dep",
			isBlocker: true, // forward dep = this bead is blocked by it
			zoneID:    fmt.Sprintf("%sdep-%d", m.zonePrefix, i),
		})
	}
	for i, rdep := range m.detail.ReverseDeps {
		m.rows = append(m.rows, detailRow{
			id:     rdep,
			label:  "rdep",
			zoneID: fmt.Sprintf("%srdep-%d", m.zonePrefix, i),
		})
	}
	if m.cursor >= len(m.rows) {
		m.cursor = 0
	}
}

// logTailBytes is the maximum number of bytes read from a log file on each
// refresh tick. Caps the read at 32 KB to avoid re-reading arbitrarily large
// files on every 100 ms interval.
const logTailBytes = 32 * 1024

// refreshLog re-reads the tail of the log file and updates the viewport.
func (m *detailModel) refreshLog() {
	if m.detail.LogPath == "" {
		return
	}
	f, err := os.Open(m.detail.LogPath)
	if err != nil {
		m.logVP.SetContent(fmt.Sprintf("(log unavailable: %v)", err))
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		m.logVP.SetContent(fmt.Sprintf("(log stat error: %v)", err))
		return
	}
	size := fi.Size()
	offset := size - logTailBytes
	if offset < 0 {
		offset = 0
	}
	if offset > 0 {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			m.logVP.SetContent(fmt.Sprintf("(log seek error: %v)", err))
			return
		}
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		m.logVP.SetContent(fmt.Sprintf("(log read error: %v)", err))
		return
	}
	content := string(buf)
	if offset > 0 {
		// Align to a newline boundary so the first visible line is not a
		// partial UTF-8 sequence or mid-line fragment from the seek offset.
		if nl := strings.IndexByte(content, '\n'); nl >= 0 {
			content = content[nl+1:]
		}
		content = "[\u2026truncated\u2026]\n" + content
	}
	m.logVP.SetContent(content)
	if m.logFollow {
		m.logVP.GotoBottom()
	}
}

// activityTailBytes is the maximum number of bytes read from the tail of
// stream.jsonl on each refresh tick (koryph-xvk). Larger than logTailBytes
// because thinking/message deltas share the stream with much chattier tool-use
// events (input_json_delta dominates by volume) — a 32 KB window would often
// hold only seconds of activity.
const activityTailBytes = 512 * 1024

// refreshActivity re-reads the slot's stream.jsonl, parses it into classified
// activity entries, caches them, and re-renders under the current filter — the
// activity sibling of refreshLog. It dispatches on the window mode: the fast
// fixed-size tail (default) or the whole history (full mode, 'h').
func (m *detailModel) refreshActivity() {
	if m.detail.StreamPath == "" {
		return
	}
	if m.activityFull {
		m.refreshActivityFull()
		return
	}
	m.refreshActivityTail()
}

// resetActivityScan discards the incremental full-history parser so the next
// full refresh re-reads from byte 0. Called when the window mode or focused
// bead changes.
func (m *detailModel) resetActivityScan() {
	m.activityScanner = nil
	m.activityOffset = 0
	m.activityScanPath = ""
}

// refreshActivityTail reads the last activityTailBytes of the stream (a fresh,
// stateless parse each call) and re-renders. On a read error the cached entries
// are left intact so a transient stat/seek failure mid-write doesn't blank a
// working tail; only a successful parse replaces the cache.
func (m *detailModel) refreshActivityTail() {
	f, err := os.Open(m.detail.StreamPath)
	if err != nil {
		m.activityVP.SetContent(fmt.Sprintf("(stream unavailable: %v)", err))
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		m.activityVP.SetContent(fmt.Sprintf("(stream stat error: %v)", err))
		return
	}
	offset := fi.Size() - activityTailBytes
	if offset < 0 {
		offset = 0
	}
	if offset > 0 {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			m.activityVP.SetContent(fmt.Sprintf("(stream seek error: %v)", err))
			return
		}
		// Skip the partial line the seek landed inside — the parser scans whole
		// JSON lines (the scanner it wraps handles window-edge blocks anyway).
		br := bufio.NewReaderSize(f, 64*1024)
		if _, err := br.ReadString('\n'); err != nil {
			m.activityEntries = nil
			m.renderActivity()
			return
		}
		m.activityEntries = claude.ExtractActivity(br)
		m.renderActivity()
		return
	}
	m.activityEntries = claude.ExtractActivity(f)
	m.renderActivity()
}

// refreshActivityFull accumulates the whole stream from byte 0 via a persistent
// incremental scanner, feeding only the bytes appended since the last refresh —
// so a large, still-growing stream is parsed once, not re-parsed per tick. When
// no new bytes have arrived it is a no-op (the cached render stands), keeping a
// finished or quiet stream free of per-tick work.
func (m *detailModel) refreshActivityFull() {
	path := m.detail.StreamPath
	f, err := os.Open(path)
	if err != nil {
		m.activityVP.SetContent(fmt.Sprintf("(stream unavailable: %v)", err))
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		m.activityVP.SetContent(fmt.Sprintf("(stream stat error: %v)", err))
		return
	}
	size := fi.Size()

	// (Re)bind the scanner if it is unset, points at another stream, or the file
	// shrank under our offset (truncation/rotation) — any of which invalidates
	// the accumulated parse.
	if m.activityScanner == nil || m.activityScanPath != path || size < m.activityOffset {
		m.activityScanner = claude.NewActivityScanner()
		m.activityOffset = 0
		m.activityScanPath = path
	}
	if m.activityOffset > 0 && size == m.activityOffset {
		return // nothing new appended — the cached render already reflects it
	}
	if m.activityOffset > 0 {
		if _, err = f.Seek(m.activityOffset, io.SeekStart); err != nil {
			// Fall back to a clean re-scan from the start.
			m.activityScanner = claude.NewActivityScanner()
			m.activityOffset = 0
			if _, err = f.Seek(0, io.SeekStart); err != nil {
				return // leave the cached entries intact
			}
		}
	}
	buf, err := io.ReadAll(f)
	if err != nil && len(buf) == 0 {
		return // leave the cached entries intact
	}
	m.activityScanner.Write(buf)
	m.activityOffset += int64(len(buf))
	m.activityEntries = m.activityScanner.Entries()
	m.renderActivity()
}

// renderActivity renders the cached activity entries under the current filter
// into the viewport, then applies the follow scroll. Split from refreshActivity
// so a filter change re-renders instantly without re-reading the file.
func (m *detailModel) renderActivity() {
	w := m.activityVP.Width - 2
	if w < 20 {
		w = 20
	}

	dim := lipgloss.NewStyle().Foreground(m.theme.Gray)
	toolStyle := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Cyan)
	msgStyle := lipgloss.NewStyle().Foreground(m.theme.White)
	divStyle := lipgloss.NewStyle().Foreground(m.theme.Blue)

	var b strings.Builder
	curParent, started := "", false
	shown := 0
	for _, e := range m.activityEntries {
		if !m.activityFilter.matches(e.Kind) {
			continue
		}
		// Subagent attribution: a divider whenever activity crosses between the
		// main agent and a nested subagent (parent_tool_use_id transitions), so
		// interleaved nested work stays attributable.
		if !started || e.Parent != curParent {
			if started {
				b.WriteByte('\n')
			}
			if e.Parent != "" {
				b.WriteString(divStyle.Render("── subagent "+shortToolUseID(e.Parent)+" ──") + "\n")
			} else if started {
				b.WriteString(divStyle.Render("── main agent ──") + "\n")
			}
			curParent = e.Parent
		}
		started = true
		shown++

		switch e.Kind {
		case claude.ActToolUse:
			head := toolStyle.Render("⚙ " + e.Tool)
			if arg := toolArgSummary(e.Text); arg != "" {
				head += " " + dim.Render(truncate(arg, w-lipgloss.Width(head)-1))
			}
			b.WriteString(head + "\n")
		case claude.ActMessage:
			for _, line := range wrapText(e.Text, w) {
				b.WriteString(msgStyle.Render(line) + "\n")
			}
		default: // thinking
			for _, line := range wrapText(e.Text, w) {
				b.WriteString(dim.Render(line) + "\n")
			}
		}
	}

	if shown == 0 {
		var hint string
		if len(m.activityEntries) == 0 {
			hint = "(no activity in the stream tail yet — the agent may be starting up)"
		} else {
			hint = fmt.Sprintf("(no %s in the stream tail yet — try another filter)", m.activityFilter.label())
		}
		m.activityVP.SetContent(dim.Render(hint))
		return
	}
	m.activityVP.SetContent(strings.TrimRight(b.String(), "\n"))
	if m.activityFollow {
		m.activityVP.GotoBottom()
	}
}

// setActivityFilter switches the active filter and re-renders from the cached
// entries (no file re-read). A filter change resets the scroll: it jumps to the
// bottom when following, else to the top so the operator sees the start of the
// newly-selected category.
func (m *detailModel) setActivityFilter(f activityFilter) {
	if m.activityFilter == f {
		return
	}
	m.activityFilter = f
	m.renderActivity()
	if m.activityFollow {
		m.activityVP.GotoBottom()
	} else {
		m.activityVP.GotoTop()
	}
}

// activityFilterBar renders the filter selector for the tail footer: each
// category with its live count, the active one highlighted, so the operator
// sees both which filter is on and how much of each kind exists.
func (m *detailModel) activityFilterBar() string {
	var nThink, nTool, nMsg int
	for _, e := range m.activityEntries {
		switch e.Kind {
		case claude.ActThinking:
			nThink++
		case claude.ActToolUse:
			nTool++
		case claude.ActMessage:
			nMsg++
		}
	}
	opts := []struct {
		key, label string
		f          activityFilter
		n          int
	}{
		{"0", "all", filterAll, nThink + nTool + nMsg},
		{"1", "think", filterThinking, nThink},
		{"2", "tools", filterTools, nTool},
		{"3", "msgs", filterMessages, nMsg},
	}
	active := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent)
	dim := lipgloss.NewStyle().Foreground(m.theme.Gray)
	parts := make([]string, 0, len(opts))
	for _, o := range opts {
		seg := fmt.Sprintf("%s:%s(%d)", o.key, o.label, o.n)
		if o.f == m.activityFilter {
			parts = append(parts, active.Render(seg))
		} else {
			parts = append(parts, dim.Render(seg))
		}
	}
	return strings.Join(parts, " ")
}

// activityScopeLabel names the window mode the 'h' key will switch TO, so the
// footer reads as the action it performs (showing "full" while in tail mode).
func activityScopeLabel(full bool) string {
	if full {
		return "tail"
	}
	return "full"
}

// shortToolUseID trims a subagent's parent tool-use id to a display-friendly
// suffix (the ids are long opaque "toolu_…" tokens whose tail distinguishes
// them), matching the divider format the claude activity extractor labels with.
func shortToolUseID(id string) string {
	if len(id) > 8 {
		return "…" + id[len(id)-8:]
	}
	return id
}

// toolArgSummary renders a tool's (possibly partial) input JSON as a compact
// single-line summary for the tail — the salient argument at a glance, not the
// full payload. Falls back to a flattened one-liner when the JSON is
// incomplete (the in-flight tool call whose input is still streaming).
func toolArgSummary(inputJSON string) string {
	s := strings.TrimSpace(inputJSON)
	if s == "" || s == "{}" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		// Partial/streaming JSON: collapse whitespace so it stays one line.
		return strings.Join(strings.Fields(s), " ")
	}
	// Prefer the argument operators most want to see first.
	for _, k := range []string{"command", "file_path", "path", "pattern", "query", "url", "description"} {
		if v, ok := m[k]; ok {
			if vs, ok := v.(string); ok && strings.TrimSpace(vs) != "" {
				return strings.Join(strings.Fields(vs), " ")
			}
		}
	}
	// Otherwise a compact key list so the call isn't opaque.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, " ")
}

// Update implements TabModel.
func (m *detailModel) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// In log-tail mode most keys are routed to the viewport.
		if m.logMode {
			switch msg.String() {
			case "t", "esc":
				m.logMode = false
				return m, nil
			case "f":
				m.logFollow = !m.logFollow
				if m.logFollow {
					m.logVP.GotoBottom()
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.logVP, cmd = m.logVP.Update(msg)
			// Scroll-back auto-pauses follow; scrolling back to the bottom
			// resumes it, so a live tail never yanks the operator off the line
			// they scrolled up to read.
			m.logFollow = m.logVP.AtBottom()
			return m, cmd
		}

		// Activity-tail mode (koryph-xvk): the classified thinking/tool/message
		// stream, filterable and independently scrollable.
		if m.activityMode {
			switch msg.String() {
			case "T", "esc":
				m.activityMode = false
				return m, nil
			case "f":
				m.activityFollow = !m.activityFollow
				if m.activityFollow {
					m.activityVP.GotoBottom()
				}
				return m, nil
			case "0", "a":
				m.setActivityFilter(filterAll)
				return m, nil
			case "1":
				m.setActivityFilter(filterThinking)
				return m, nil
			case "2":
				m.setActivityFilter(filterTools)
				return m, nil
			case "3":
				m.setActivityFilter(filterMessages)
				return m, nil
			case "h":
				// Toggle full-history vs the fast 512 KB tail window. Reset the
				// incremental scanner so the next refresh re-reads from the right
				// start, then re-read now for an immediate result.
				m.activityFull = !m.activityFull
				m.resetActivityScan()
				m.refreshActivity()
				if m.activityFollow {
					m.activityVP.GotoBottom()
				} else {
					m.activityVP.GotoTop()
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.activityVP, cmd = m.activityVP.Update(msg)
			m.activityFollow = m.activityVP.AtBottom()
			return m, cmd
		}

		// Normal detail mode key handling. j/k scroll the body viewport
		// (long descriptions/plans must be readable); n/N cycle dep-row
		// focus with the viewport following the focused row.
		switch msg.String() {
		case "t":
			// Toggle log-tail mode.
			m.logMode = true
			m.logFollow = true
			m.refreshLog()
			return m, nil

		case "T":
			// Toggle activity-tail mode (koryph-xvk).
			m.activityMode = true
			m.activityFollow = true
			m.refreshActivity()
			return m, nil

		case "j", "down":
			m.bodyVP.ScrollDown(1)
			return m, nil

		case "k", "up":
			m.bodyVP.ScrollUp(1)
			return m, nil

		case "ctrl+d", "pgdown":
			m.bodyVP.HalfPageDown()
			return m, nil

		case "ctrl+u", "pgup":
			m.bodyVP.HalfPageUp()
			return m, nil

		case "g":
			m.bodyVP.GotoTop()
			return m, nil

		case "G":
			m.bodyVP.GotoBottom()
			return m, nil

		case "n":
			m.focusDep(+1)
			return m, nil

		case "N":
			m.focusDep(-1)
			return m, nil

		case "enter":
			// Navigate into the focused dep/rdep row.
			if len(m.rows) > 0 && m.cursor >= 0 && m.cursor < len(m.rows) {
				targetID := m.rows[m.cursor].id
				// Push current bead onto the nav stack.
				if m.beadID != "" {
					m.navStack = append(m.navStack, m.beadID)
				}
				// Emit showDetailMsg to switch to that bead.
				id := targetID
				return m, func() tea.Msg { return showDetailMsg{beadID: id} }
			}

		case "backspace":
			// Pop the nav stack; if non-empty navigate to the previous bead.
			if len(m.navStack) > 0 {
				prev := m.navStack[len(m.navStack)-1]
				m.navStack = m.navStack[:len(m.navStack)-1]
				return m, func() tea.Msg { return showDetailMsg{beadID: prev} }
			}
			// Stack empty — emit detailBackMsg to return to the previous tab.
			return m, func() tea.Msg { return detailBackMsg{} }

		case "esc":
			// Esc always returns to the previous tab (clears nav stack too).
			m.navStack = nil
			return m, func() tea.Msg { return detailBackMsg{} }
		}

	case tea.MouseMsg:
		// Check if any dep/rdep zone was clicked.
		for i, row := range m.rows {
			zi := zone.Get(row.zoneID)
			if zi != nil && zi.InBounds(msg) {
				m.cursor = i
				m.refreshBody()
			}
		}
	}
	return m, nil
}

// focusDep moves the dep-row focus by delta (wrapping) and scrolls the body
// viewport so the focused row is visible.
func (m *detailModel) focusDep(delta int) {
	if len(m.rows) == 0 {
		return
	}
	m.cursor = ((m.cursor+delta)%len(m.rows) + len(m.rows)) % len(m.rows)
	m.refreshBody()
	if m.cursor < len(m.depLines) {
		line := m.depLines[m.cursor]
		if line < m.bodyVP.YOffset || line >= m.bodyVP.YOffset+m.bodyVP.Height {
			target := line - m.bodyVP.Height/2
			if target < 0 {
				target = 0
			}
			m.bodyVP.SetYOffset(target)
		}
	}
}

// View implements TabModel.
func (m *detailModel) View() string {
	dimStyle := lipgloss.NewStyle().Foreground(m.theme.Gray)

	// Log-tail mode: render the viewport.
	if m.logMode {
		followIndicator := ""
		if m.logFollow {
			followIndicator = "  [follow]"
		}
		hdr := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).
			Render(fmt.Sprintf("Log tail: %s%s", truncate(m.detail.LogPath, 60), followIndicator))
		ftr := dimStyle.Render("t/esc back  f toggle-follow  ↑/↓ scroll")
		return zone.Scan(hdr + "\n" + m.logVP.View() + "\n" + ftr)
	}

	// Activity-tail mode (koryph-xvk): the agent's live classified stream.
	if m.activityMode {
		followIndicator := "  [paused]"
		if m.activityFollow {
			followIndicator = "  [follow]"
		}
		scope := "tail 512K"
		if m.activityFull {
			scope = "full history"
		}
		hdr := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).
			Render(fmt.Sprintf("Activity [%s]: %s%s", scope, truncate(m.detail.StreamPath, 44), followIndicator))
		ftr := dimStyle.Render("T/esc back  f follow  h " + activityScopeLabel(m.activityFull) + "  " + m.activityFilterBar() + "  ↑/↓ scroll")
		return zone.Scan(hdr + "\n" + m.activityVP.View() + "\n" + ftr)
	}

	if m.beadID == "" {
		return lipgloss.NewStyle().
			Foreground(m.theme.Gray).
			Padding(1, 2).
			Render("No bead selected. Press Enter on a thread to view details.")
	}

	d := m.detail
	if d.BeadID == "" {
		return lipgloss.NewStyle().
			Foreground(m.theme.Gray).
			Padding(1, 2).
			Render(fmt.Sprintf("Loading detail for %s…", m.beadID))
	}

	w := m.width
	if w < 40 {
		w = 40
	}

	// --- Fixed header: breadcrumb (if any) + title line -----------------------
	var hdr strings.Builder
	if len(m.navStack) > 0 {
		crumb := renderBreadcrumb(m.projectID, m.navStack, d.BeadID, w-4)
		hdr.WriteString(dimStyle.Render("  "+crumb) + "\n")
	}
	statusColor := m.theme.StatusColor(d.Status)
	statusBadge := lipgloss.NewStyle().Foreground(statusColor).Bold(true).Render(d.Status)
	titleLine := lipgloss.NewStyle().Bold(true).Foreground(m.theme.White).Width(w - 2).
		Render(fmt.Sprintf("%s  %s  %s", truncate(d.Title, w-30), statusBadge, dimStyle.Render(d.BeadID)))
	hdr.WriteString(titleLine + "\n")

	// --- Scrollable body ------------------------------------------------------
	body := m.bodyVP.View()

	// --- Footer hint + scroll position ---------------------------------------
	pos := ""
	if m.bodyVP.TotalLineCount() > m.bodyVP.Height {
		pos = fmt.Sprintf("  %3.0f%%", m.bodyVP.ScrollPercent()*100)
	}
	hint := "j/k scroll  n/N deps  enter jump  ⌫ back  t log  T activity  esc return" + pos
	if len(m.navStack) > 0 {
		hint = "j/k scroll  n/N deps  enter jump  ⌫ pop stack  esc return to tab" + pos
	}

	return zone.Scan(hdr.String() + body + "\n" + dimStyle.Render(hint))
}

// refreshBody rebuilds the body viewport content from the current detail
// snapshot and cursor, recording each dep row's line index for focus-follow
// scrolling. Called on every detail/cursor/size change — never from View.
func (m *detailModel) refreshBody() {
	d := m.detail
	if d.BeadID == "" {
		m.bodyVP.SetContent("")
		m.depLines = nil
		return
	}

	w := m.width
	if w < 40 {
		w = 40
	}

	dimStyle := lipgloss.NewStyle().Foreground(m.theme.Gray)
	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Width(14)
	valueStyle := lipgloss.NewStyle().Foreground(m.theme.White)
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Cyan)
	selectedStyle := lipgloss.NewStyle().Foreground(m.theme.White).Background(m.theme.Blue).Bold(true)
	blockerStyle := lipgloss.NewStyle().Foreground(m.theme.Error)
	rdepStyle := lipgloss.NewStyle().Foreground(m.theme.Done)

	var lines []string
	m.depLines = make([]int, 0, len(m.rows))

	// --- Meta fields -------------------------------------------------------------
	lines = append(lines, labelStyle.Render("Type:")+" "+valueStyle.Render(d.IssueType))
	lines = append(lines, labelStyle.Render("Priority:")+" "+valueStyle.Render(fmt.Sprintf("P%d", d.Priority)))
	if d.ParentID != "" {
		lines = append(lines, labelStyle.Render("Parent:")+" "+valueStyle.Render(d.ParentID))
	}
	if len(d.Labels) > 0 {
		lines = append(lines, labelStyle.Render("Labels:")+" "+dimStyle.Render(strings.Join(d.Labels, "  ")))
	}
	if d.Branch != "" {
		lines = append(lines, labelStyle.Render("Branch:")+" "+valueStyle.Render(d.Branch))
	}
	if d.Worktree != "" {
		lines = append(lines, labelStyle.Render("Worktree:")+" "+dimStyle.Render(truncate(d.Worktree, w-20)))
	}
	if d.CostUSD > 0 || d.EstimateUSD > 0 {
		lines = append(lines, labelStyle.Render("Cost/Est:")+" "+valueStyle.Render(formatDetailCost(d.CostUSD, d.EstimateUSD)))
	}
	if d.DeathReason != "" {
		lines = append(lines, labelStyle.Render("Death:")+" "+
			lipgloss.NewStyle().Foreground(m.theme.Error).Render(d.DeathReason))
	}
	if d.SlotNote != "" {
		lines = append(lines, labelStyle.Render("Note:")+" "+valueStyle.Render(truncate(d.SlotNote, w-20)))
	}
	if d.LogPath != "" {
		lines = append(lines, labelStyle.Render("Log:")+" "+dimStyle.Render(truncate(d.LogPath, w-20)))
	}

	// --- Resources (koryph process-metrics) --------------------------------------
	// Per-bead clock times and process-cohort resource usage, so an operator can
	// calibrate orchestration against what each bead actually consumed.
	if !d.StartedAt.IsZero() || d.ResourceSamples > 0 {
		lines = append(lines, sectionStyle.Render("Resources"))
		if !d.StartedAt.IsZero() {
			lines = append(lines, labelStyle.Render("Started:")+" "+valueStyle.Render(formatTimestamp(d.StartedAt)))
		}
		finished := "running"
		if !d.FinishedAt.IsZero() {
			finished = formatTimestamp(d.FinishedAt)
		}
		lines = append(lines, labelStyle.Render("Finished:")+" "+valueStyle.Render(finished))
		if wall := detailWall(d); wall > 0 {
			lines = append(lines, labelStyle.Render("Wall:")+" "+valueStyle.Render(formatElapsed(wall)))
		}
		if d.ResourceSamples > 0 {
			mem := fmt.Sprintf("avg %d MB · peak %d MB", d.AvgRSSMB, d.PeakRSSMB)
			lines = append(lines, labelStyle.Render("Memory:")+" "+valueStyle.Render(mem))
			cpu := fmt.Sprintf("%.0fs · %.0f%% util", d.CPUSeconds, d.CPUUtilPct)
			lines = append(lines, labelStyle.Render("CPU:")+" "+valueStyle.Render(cpu))
			if d.IOReadMB > 0 || d.IOWriteMB > 0 {
				io := fmt.Sprintf("%.1f MB read · %.1f MB written", d.IOReadMB, d.IOWriteMB)
				lines = append(lines, labelStyle.Render("Disk I/O:")+" "+valueStyle.Render(io))
			} else {
				lines = append(lines, labelStyle.Render("Disk I/O:")+" "+dimStyle.Render("n/a on this platform"))
			}
		}
	}

	// --- Description -------------------------------------------------------------
	if d.Description != "" {
		lines = append(lines, sectionStyle.Render("Description"))
		for _, line := range wrapText(d.Description, w-4) {
			lines = append(lines, "  "+dimStyle.Render(line))
		}
	}

	// --- Acceptance criteria -----------------------------------------------------
	if d.Acceptance != "" {
		lines = append(lines, sectionStyle.Render("Acceptance"))
		for _, line := range wrapText(d.Acceptance, w-4) {
			lines = append(lines, "  "+dimStyle.Render(line))
		}
	}

	// --- Notes -------------------------------------------------------------------
	if d.Notes != "" {
		lines = append(lines, sectionStyle.Render("Notes"))
		for _, line := range wrapText(d.Notes, w-4) {
			lines = append(lines, "  "+dimStyle.Render(line))
		}
	}

	// --- Dependencies (navigable, blockers highlighted) --------------------------
	depOffset := 0
	if len(d.Deps) > 0 {
		lines = append(lines, sectionStyle.Render("Depends on"))
		for i, dep := range d.Deps {
			rowStr := fmt.Sprintf("  ← %s", dep)
			var rendered string
			if depOffset+i == m.cursor {
				rendered = zone.Mark(m.rows[i].zoneID, selectedStyle.Render(rowStr))
			} else {
				rendered = zone.Mark(m.rows[i].zoneID, blockerStyle.Render(rowStr))
			}
			m.depLines = append(m.depLines, len(lines))
			lines = append(lines, rendered)
		}
		depOffset += len(d.Deps)
	}

	// --- Reverse deps (navigable) ------------------------------------------------
	if len(d.ReverseDeps) > 0 {
		lines = append(lines, sectionStyle.Render("Blocked by this"))
		for i, rdep := range d.ReverseDeps {
			rowStr := fmt.Sprintf("  → %s", rdep)
			var rendered string
			if depOffset+i == m.cursor {
				rendered = zone.Mark(m.rows[depOffset+i].zoneID, selectedStyle.Render(rowStr))
			} else {
				rendered = zone.Mark(m.rows[depOffset+i].zoneID, rdepStyle.Render(rowStr))
			}
			m.depLines = append(m.depLines, len(lines))
			lines = append(lines, rendered)
		}
	}

	// --- Attempt history ---------------------------------------------------------
	if len(d.AttemptHistory) > 0 {
		lines = append(lines, sectionStyle.Render("Attempt history"))
		for _, rec := range d.AttemptHistory {
			cause := ""
			if rec.RequeueCause != "" {
				cause = "  requeue:" + rec.RequeueCause
			}
			line := fmt.Sprintf("  #%d  %-14s  %-12s  $%.3f  %s%s",
				rec.Attempt,
				rec.Status,
				truncate(rec.Model, 12),
				rec.CostUSD,
				formatElapsed(rec.Elapsed),
				cause,
			)
			lines = append(lines, dimStyle.Render(line))
		}
	}

	m.bodyVP.SetContent(strings.Join(lines, "\n"))
}

// wrapText wraps s at maxWidth characters, splitting on spaces.
func wrapText(s string, maxWidth int) []string {
	if maxWidth <= 0 {
		maxWidth = 60
	}
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		if len([]rune(para)) <= maxWidth {
			lines = append(lines, para)
			continue
		}
		words := strings.Fields(para)
		cur := ""
		for _, word := range words {
			if cur == "" {
				cur = word
			} else if len([]rune(cur))+1+len([]rune(word)) <= maxWidth {
				cur += " " + word
			} else {
				lines = append(lines, cur)
				cur = word
			}
		}
		if cur != "" {
			lines = append(lines, cur)
		}
	}
	return lines
}

// detailWall returns the bead's wall-clock duration: dispatch → finish when
// terminal, or dispatch → snapshot time while still live. Zero when the start
// is unknown or the computed span is negative.
func detailWall(d cockpit.BeadDetailSnapshot) time.Duration {
	if d.StartedAt.IsZero() {
		return 0
	}
	end := d.ComputedAt
	if !d.FinishedAt.IsZero() {
		end = d.FinishedAt
	}
	if w := end.Sub(d.StartedAt); w > 0 {
		return w
	}
	return 0
}

// formatDetailCost formats cost vs estimate for the detail panel.
func formatDetailCost(cost, estimate float64) string {
	if cost == 0 && estimate == 0 {
		return "—"
	}
	if estimate == 0 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.4f / $%.4f", cost, estimate)
}
