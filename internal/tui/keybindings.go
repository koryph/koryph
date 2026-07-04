// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import "github.com/charmbracelet/bubbles/key"

// KeyMap holds the application-wide key bindings.
type KeyMap struct {
	// Navigation.
	NextTab key.Binding
	PrevTab key.Binding
	Up      key.Binding
	Down    key.Binding
	Enter   key.Binding
	Back    key.Binding

	// Global actions.
	Help   key.Binding
	Quit   key.Binding
	Reload key.Binding

	// Project switcher.
	NextProject key.Binding
}

// DefaultKeyMap returns the default application key bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		NextTab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "next tab"),
		),
		PrevTab: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("shift+tab", "prev tab"),
		),
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "select"),
		),
		Back: key.NewBinding(
			key.WithKeys("backspace", "esc"),
			key.WithHelp("esc/⌫", "back"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		Reload: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "reload"),
		),
		NextProject: key.NewBinding(
			key.WithKeys("p"),
			key.WithHelp("p", "next project"),
		),
	}
}

// ShortHelp returns the abbreviated key list shown in the status bar.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Help, k.Quit, k.NextTab, k.NextProject}
}

// FullHelp returns the full key list shown in the help overlay.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.NextTab, k.PrevTab, k.NextProject},
		{k.Up, k.Down, k.Enter, k.Back},
		{k.Reload, k.Help, k.Quit},
	}
}
