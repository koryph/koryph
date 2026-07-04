// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"fmt"
	"strings"
)

// TabID identifies a top-level TUI tab.
type TabID int

const (
	TabThreads TabID = iota
	// Future tabs (T2-T5 are not implemented in this bead):
	// TabQueue
	// TabBeadDetail
	// TabEfficiency
	// TabEvents
	tabCount
)

// tabLabel returns the display name for a tab.
func tabLabel(t TabID) string {
	switch t {
	case TabThreads:
		return "Threads"
	default:
		return fmt.Sprintf("Tab%d", t)
	}
}

// renderTabBar renders the top tab bar, highlighting the active tab.
func renderTabBar(active TabID, theme Theme, width int) string {
	var parts []string
	for i := TabID(0); i < tabCount; i++ {
		label := fmt.Sprintf(" %s ", tabLabel(i))
		if i == active {
			parts = append(parts, theme.TabActive.Render(label))
		} else {
			parts = append(parts, theme.TabInactive.Render(label))
		}
	}
	bar := strings.Join(parts, "")
	// Pad to full width.
	padding := width - lipglossLen(bar)
	if padding > 0 {
		bar += strings.Repeat(" ", padding)
	}
	return theme.TabBar.Render(bar)
}

// lipglossLen returns the visual width of a lipgloss-rendered string,
// stripping ANSI escape codes.
func lipglossLen(s string) int {
	// Count only printable runes (this is a rough approximation; bubbles
	// uses a more precise measure internally but this is adequate for padding).
	n := 0
	inEsc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case !inEsc:
			n++
		}
	}
	return n
}
