// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import "github.com/charmbracelet/lipgloss"

// Theme holds the colorblind-safe color palette and derived styles.
// Colors are chosen from the Paul Tol "muted" qualitative palette
// (https://personal.sron.nl/~pault/), which is safe for deuteranopia,
// protanopia, and tritanopia.
type Theme struct {
	// Base palette (Tol muted).
	Blue   lipgloss.Color
	Cyan   lipgloss.Color
	Green  lipgloss.Color
	Yellow lipgloss.Color
	Red    lipgloss.Color
	Purple lipgloss.Color
	Gray   lipgloss.Color
	White  lipgloss.Color

	// Semantic aliases.
	Running  lipgloss.Color // active/healthy (Tol blue)
	Warning  lipgloss.Color // requeue/stuck (Tol yellow)
	Error    lipgloss.Color // failed/conflict (Tol red)
	Done     lipgloss.Color // merged/done (Tol green)
	Inactive lipgloss.Color // queued/deferred (Tol gray)
	Accent   lipgloss.Color // header/tab highlight (Tol cyan)

	// Styles assembled from the palette.
	Header      lipgloss.Style
	TabActive   lipgloss.Style
	TabInactive lipgloss.Style
	TabBar      lipgloss.Style
	TableHeader lipgloss.Style
	TableRow    lipgloss.Style
	TableRowAlt lipgloss.Style
	StatusBar   lipgloss.Style
	HelpKey     lipgloss.Style
	HelpDesc    lipgloss.Style
	HelpBorder  lipgloss.Style
}

// DefaultTheme returns a colorblind-safe theme suitable for most terminals.
func DefaultTheme() Theme {
	t := Theme{
		// Tol muted palette hex codes.
		Blue:   lipgloss.Color("#4477AA"),
		Cyan:   lipgloss.Color("#66CCEE"),
		Green:  lipgloss.Color("#228833"),
		Yellow: lipgloss.Color("#CCBB44"),
		Red:    lipgloss.Color("#EE6677"),
		Purple: lipgloss.Color("#AA3377"),
		Gray:   lipgloss.Color("#BBBBBB"),
		White:  lipgloss.Color("#FFFFFF"),
	}
	// Semantic mapping.
	t.Running = t.Blue
	t.Warning = t.Yellow
	t.Error = t.Red
	t.Done = t.Green
	t.Inactive = t.Gray
	t.Accent = t.Cyan

	// Assembled styles.
	t.Header = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.White).
		Background(lipgloss.Color("#222222")).
		Padding(0, 1)

	t.TabActive = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.White).
		Background(t.Blue).
		Padding(0, 2)

	t.TabInactive = lipgloss.NewStyle().
		Foreground(t.Gray).
		Padding(0, 2)

	t.TabBar = lipgloss.NewStyle().
		Background(lipgloss.Color("#111111"))

	t.TableHeader = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Accent)

	t.TableRow = lipgloss.NewStyle()

	t.TableRowAlt = lipgloss.NewStyle().
		Background(lipgloss.Color("#1A1A1A"))

	t.StatusBar = lipgloss.NewStyle().
		Foreground(t.Gray).
		Background(lipgloss.Color("#111111")).
		Padding(0, 1)

	t.HelpKey = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Cyan)

	t.HelpDesc = lipgloss.NewStyle().
		Foreground(t.Gray)

	t.HelpBorder = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Blue).
		Padding(1, 2)

	return t
}

// StatusColor returns the theme color for a ledger slot status string.
func (t Theme) StatusColor(status string) lipgloss.Color {
	switch status {
	case "running", "dispatching":
		return t.Running
	case "review", "merge-pending":
		return t.Accent
	case "merged", "done", "pr-opened":
		return t.Done
	case "failed", "conflict", "blocked":
		return t.Error
	case "stuck":
		return t.Warning
	default:
		return t.Inactive
	}
}
