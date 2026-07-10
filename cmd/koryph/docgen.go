// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

// docgen.go — hidden __docgen command and the CLI-reference renderer.
//
// Run:
//
//	koryph __docgen > docs/reference/cli.md
//
// Drift check (green gate):
//
//	go test ./cmd/koryph/ -run TestCLIRefDrift
//
// The generator walks commandRegistry (the same data structure that drives
// shell completion) and captures each command's flag set via captureFlags().
// DocLinks on each command become cross-links to the relevant concept and
// user-guide pages.  Commands with no DocLinks emit a "TODO" warning in the
// generated output so coverage converges over time.

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

func init() {
	// Hidden: not listed in usage(); never returned by shell completion.
	registerCmd(command{
		name:    "__docgen",
		summary: "regenerate docs/reference/cli.md from the command registry (hidden)",
		run:     cmdDocGen,
	})
}

// cmdDocGen implements koryph __docgen — writes the CLI reference to stdout.
func cmdDocGen(args []string, stdout, stderr io.Writer) int {
	renderCLIDoc(stdout, stderr)
	return 0
}

// renderCLIDoc writes the full CLI reference page to w. Any structural
// warnings (missing DocLinks, unresolvable links) go to warnOut.
// This function is deterministic: the same registry always produces the same
// output. It is called both from cmdDocGen (live run) and from
// TestCLIRefDrift (drift check).
func renderCLIDoc(w io.Writer, warnOut io.Writer) {
	p := func(format string, a ...any) {
		fmt.Fprintf(w, format, a...)
	}

	// ---- header -------------------------------------------------------
	// REUSE-IgnoreStart
	p("<!-- SPDX-License-Identifier: Apache-2.0 -->\n")
	p("<!-- Copyright (c) 2026 The Koryph Developers -->\n")
	// REUSE-IgnoreEnd
	p("\n")
	p("<!--\n")
	p("  DO NOT EDIT MANUALLY — this file is auto-generated from the command registry.\n")
	p("\n")
	p("  Re-generate:  koryph __docgen > docs/reference/cli.md\n")
	p("  Drift check:  go test ./cmd/koryph/ -run TestCLIRefDrift\n")
	p("-->\n")
	p("\n")
	p("# CLI Reference\n")
	p("\n")
	p("koryph — central multi-project orchestrator for autonomous Claude Code agents.\n")
	p("\n")
	p("## Quick index\n")
	p("\n")

	// ---- build a deterministic command list in section order ----------
	// sectionOrder lists top-level command names in the logical order from
	// the global usage() text so the reference mirrors the mental model.
	sectionOrder := []string{
		// Getting started
		"init", "project", "validate", "run",
		// Operate
		"intake", "nudge", "stop", "drain", "resize",
		"merge", "land", "review-pr", "pr-sync",
		"bot", "signing", "sign", "release",
		// Observe
		"board", "roster", "status", "tail",
		"doctor", "plan",
		"governor", "quota", "metrics",
		// Repo IaC
		"repo",
		// Posture
		"posture",
		// Assets
		"agents", "commands", "rules",
		// Advanced
		"onboard", "batch", "version",
		// Shell completion
		"completion",
	}

	// Gather all registered top-level commands indexed by name for quick lookup.
	cmdByName := make(map[string]*command, len(commandRegistry))
	for i := range commandRegistry {
		c := &commandRegistry[i]
		if !visibleTopLevel(c) {
			// Hidden aliases (agents/commands/rules → project install-assets)
			// and internal verbs (__docgen/__complete) are omitted from the
			// generated reference, matching the interactive listing.
			continue
		}
		cmdByName[c.name] = c
	}

	// Collect ordered list; append any names not in sectionOrder at the end
	// (sorted alphabetically) so new commands always appear rather than being
	// silently dropped.
	ordered := make([]*command, 0, len(cmdByName))
	seen := make(map[string]bool, len(sectionOrder))
	for _, name := range sectionOrder {
		if c, ok := cmdByName[name]; ok {
			ordered = append(ordered, c)
			seen[name] = true
		}
	}
	var extra []string
	for name := range cmdByName {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		ordered = append(ordered, cmdByName[name])
	}

	// ---- TOC: two-column quick-index table ----------------------------
	p("| Command | Summary |\n")
	p("|---------|----------|\n")
	for _, c := range ordered {
		anchor := anchorFor(c.name)
		if len(c.subs) > 0 {
			p("| [`koryph %s`](#%s) | %s |\n", c.name, anchor, escape(c.summary))
			for i := range c.subs {
				sub := &c.subs[i]
				subAnchor := anchorFor(c.name + "-" + sub.name)
				p("| ↳ [`koryph %s %s`](#%s) | %s |\n",
					c.name, sub.name, subAnchor, escape(sub.summary))
			}
		} else {
			p("| [`koryph %s`](#%s) | %s |\n", c.name, anchor, escape(c.summary))
		}
	}
	p("\n")
	p("---\n")
	p("\n")

	// ---- per-command sections -----------------------------------------
	for _, c := range ordered {
		renderCommandSection(w, warnOut, c, "")
		for i := range c.subs {
			sub := &c.subs[i]
			renderCommandSection(w, warnOut, sub, c.name)
		}
		p("\n---\n\n")
	}

	// ---- environment section ------------------------------------------
	p("## Environment { #env }\n")
	p("\n")
	p("| Variable | Description |\n")
	p("|----------|-------------|\n")
	for _, ev := range envVars {
		p("| `%s` | %s |\n", ev.name, escape(ev.desc))
	}
	// No trailing blank line — end-of-file is the final newline after the
	// last table row, matching what the pre-commit end-of-file-fixer enforces.
}

// renderCommandSection writes one command's section to w.
// parent is the parent command name (empty for top-level commands).
func renderCommandSection(w io.Writer, warnOut io.Writer, c *command, parent string) {
	fullName := c.name
	if parent != "" {
		fullName = parent + " " + c.name
	}
	anchor := anchorFor(strings.ReplaceAll(fullName, " ", "-"))

	p := func(format string, a ...any) {
		fmt.Fprintf(w, format, a...)
	}

	// Heading with explicit anchor for stability.
	p("## `koryph %s` { #%s }\n", fullName, anchor)
	p("\n")
	// Summaries carry CLI usage syntax like "[--until <duration>]" — escape
	// it here too or zensical --strict reads it as an unresolved link
	// reference (same hazard escape() documents for table cells).
	p("%s\n", strings.ReplaceAll(c.summary, "[", "\\["))
	p("\n")

	// DocLinks cross-reference block.
	if len(c.DocLinks) > 0 {
		p("**See also:**")
		for i, link := range c.DocLinks {
			title := docLinkTitle(link)
			rel := "../" + link
			// Strip .md suffix for MkDocs clean URLs.
			rel = strings.TrimSuffix(rel, ".md")
			if i > 0 {
				p(" ·")
			}
			p(" [%s](%s)", title, rel)
		}
		p("\n\n")
	} else {
		// Warn: this command has no doc cross-links yet.
		fmt.Fprintf(warnOut, "WARNING: koryph %s has no DocLinks — add links to the command registration\n", fullName)
		p("<!-- TODO: add DocLinks to the `%s` command registration in cmd/koryph/ -->\n", fullName)
		p("\n")
	}

	// Flags via captureFlags.
	if c.run != nil {
		fs := captureFlags(c.run)
		if fs != nil {
			var flags []flagRow
			fs.VisitAll(func(f *flag.Flag) {
				flags = append(flags, flagRow{
					name:   f.Name,
					typ:    flagTypeName(f.Value),
					defVal: f.DefValue,
					usage:  f.Usage,
				})
			})
			if len(flags) > 0 {
				p("| Flag | Type | Default | Description |\n")
				p("|------|------|---------|-------------|\n")
				for _, f := range flags {
					def := ""
					if f.defVal != "" && f.defVal != "0" && f.defVal != "false" {
						def = "`" + escape(f.defVal) + "`"
					}
					p("| `--%s` | %s | %s | %s |\n",
						f.name, f.typ, def, escape(f.usage))
				}
				p("\n")
			} else {
				p("No flags.\n\n")
			}
		} else if len(c.subs) == 0 {
			// run is non-nil but captureFlags returned nil — the command
			// defines no flags (e.g. a simple positional-only command).
			p("No flags.\n\n")
		} else {
			// Parent command: no top-level flags; subs have their own sections.
			p("Run `koryph %s <subcommand> -h` for subcommand flags.\n\n", fullName)
		}
	} else if len(c.subs) > 0 {
		// No run function and no flags — just a grouping node.
		p("Run `koryph %s <subcommand> -h` for subcommand flags.\n\n", c.name)
	}
}

// flagRow is one flag's metadata for table rendering.
type flagRow struct {
	name   string
	typ    string
	defVal string
	usage  string
}

// flagTypeName returns a user-friendly type name for a flag.Value.
// It uses the type's string representation from fmt.Sprintf to avoid
// importing unexported flag types.
func flagTypeName(v flag.Value) string {
	t := fmt.Sprintf("%T", v)
	// Strip package prefix: "*flag.boolValue" → "boolValue"
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	// Strip pointer marker and "Value" suffix.
	t = strings.TrimPrefix(t, "*")
	t = strings.TrimSuffix(t, "Value")
	t = strings.TrimSuffix(t, "Flag") // for boolFlag
	// Normalize known names.
	switch t {
	case "bool":
		return "bool"
	case "int":
		return "int"
	case "int64":
		return "int64"
	case "uint":
		return "uint"
	case "uint64":
		return "uint64"
	case "float64":
		return "float64"
	case "string":
		return "string"
	case "duration":
		return "duration"
	default:
		// Custom types (e.g. multiFlag) — use the bare type name.
		return t
	}
}

// anchorFor returns a MkDocs-compatible anchor ID from a command path like
// "run" or "bot-create". Spaces and special chars are replaced with hyphens.
func anchorFor(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, " ", "-")
	// Remove characters that are invalid in HTML IDs.
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return "koryph-" + b.String()
}

// docLinkTitle derives a human-readable title from a docs/ relative path.
// e.g. "user-guide/running-waves.md" → "Running waves"
//
//	"concepts/beads.md"           → "Beads"
//	"architecture.md"             → "Architecture"
func docLinkTitle(path string) string {
	// Use basename without extension.
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".md")
	// Replace hyphens with spaces and title-case the first word only.
	words := strings.Split(base, "-")
	if len(words) > 0 {
		words[0] = strings.Title(words[0]) //nolint:staticcheck // Title is fine for ASCII slugs
	}
	return strings.Join(words, " ")
}

// escape replaces Markdown pipe characters in table cells and escapes
// square brackets so the output is valid Markdown table content. Brackets
// matter under the strict docs build: CLI usage text like
// "[--until <duration>]" otherwise parses as an unresolved Markdown link
// reference and fails zensical --strict (koryph-3l1.4 landing finding).
func escape(s string) string {
	// Escape pipe characters in table cells.
	s = strings.ReplaceAll(s, "|", "\\|")
	// Escape link-reference syntax: only the opening bracket needs it.
	s = strings.ReplaceAll(s, "[", "\\[")
	return s
}

// envVar is one documented environment variable.
type envVar struct {
	name string
	desc string
}

// envVars is the canonical list of koryph environment variables.
// Keep in sync with the ENVIRONMENT section of usage() in main.go.
var envVars = []envVar{
	{
		name: "KORYPH_HOME",
		desc: "central registry + governor root (default: ~/.koryph)",
	},
	{
		name: "KORYPH_BD_BIN",
		desc: "path to the bd (beads) binary (default: bd on PATH)",
	},
	{
		name: "KORYPH_GH_BIN",
		desc: "path to the gh (GitHub CLI) binary (default: gh on PATH)",
	},
	{
		name: "KORYPH_NO_NPX",
		desc: "set to any value to disable npx-based tool fallbacks (e.g. ccusage)",
	},
}
