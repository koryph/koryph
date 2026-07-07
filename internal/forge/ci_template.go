// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package forge

import (
	"strings"
	"text/template"
)

// DefaultGateCommand is the gate command [CIService.Render]("gate") uses when
// a provider was not configured with an explicit override (the providers'
// respective WithGateCommand option). This is the single source of truth for
// the "make gate" default described in the design contract §2 — provider
// packages must reference this constant rather than redeclaring it.
const DefaultGateCommand = "make gate"

// ResolveGateCommand returns override if non-empty, otherwise
// [DefaultGateCommand]. Provider CIService implementations call this from
// their renderGate method to resolve the effective gate command.
func ResolveGateCommand(override string) string {
	if override == "" {
		return DefaultGateCommand
	}
	return override
}

// GateTemplateData is the view-model passed to a provider's gate
// workflow/pipeline template. Both the GitHub and GitLab gate templates
// reference only {{.GateCmd}}, so they share this type; it does not couple
// the two providers' distinct template sources together.
type GateTemplateData struct {
	// GateCmd is the shell command that runs the project's green gate.
	GateCmd string
}

// TemplateFuncs is the [text/template.FuncMap] shared by every provider's CI
// asset templates. It currently provides "join", the only helper referenced
// by the embedded workflow/pipeline templates.
var TemplateFuncs = template.FuncMap{
	// join concatenates ss with sep (mirrors strings.Join).
	"join": func(ss []string, sep string) string { return strings.Join(ss, sep) },
}
