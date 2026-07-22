// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

// help.go — the global `koryph -h` listing, rendered from commandRegistry so it
// can never drift from the real command set (the hand-maintained wall it
// replaced had six working top-level commands and three subcommands missing).
//
// Each top-level command appears exactly once, in its section, with its
// one-line summary; subcommands are indented beneath their parent. Detail
// (flags, synopsis) lives one `koryph <cmd> -h` away — the global listing is a
// scannable map, not a manual. Any registered non-hidden command not named in a
// section falls into the trailing MORE section, so a new command is always
// visible even before it is slotted; TestUsageCoversRegistry enforces this.

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

// helpSection is one titled group of top-level command names in the global
// listing. Order within cmds is the display order; order of helpSections is the
// section display order.
type helpSection struct {
	title string
	desc  string // optional one-line section note, shown after the title
	cmds  []string
}

// helpSections is the single source of truth for how top-level commands are
// grouped in `koryph -h`. A command name listed here that is not registered is
// skipped; a registered command named in no section lands in MORE (below).
var helpSections = []helpSection{
	{title: "GETTING STARTED", cmds: []string{"init", "adopt", "project", "validate", "run"}},
	{title: "OPERATE", desc: "drive and steer a live run", cmds: []string{"intake", "nudge", "stop", "drain", "resize"}},
	{title: "LAND & REVIEW", cmds: []string{"merge", "land", "review-pr", "pr-sync"}},
	{title: "SUPPLY CHAIN", desc: "signing, release, and the release bot", cmds: []string{"signing", "sign", "bot", "ci", "release"}},
	{title: "OBSERVE", cmds: []string{"board", "roster", "status", "tail", "tui", "cockpit", "doctor", "plan", "obs", "metrics"}},
	{title: "GOVERN", desc: "machine-wide concurrency + per-account spend", cmds: []string{"governor", "quota"}},
	{title: "REPO & POSTURE", desc: "desired-state GitHub settings (IaC + named profiles)", cmds: []string{"repo", "posture"}},
	{title: "ADVANCED", cmds: []string{"onboard", "batch", "epic", "models", "gc", "version"}},
	{title: "SHELL COMPLETION", cmds: []string{"completion"}},
}

// alwaysHiddenFromUsage are internal verbs never shown in the listing regardless
// of the hidden flag (they are dispatched directly in run(), not via the
// registry's usual leaf path, or are pure tooling).
var alwaysHiddenFromUsage = map[string]bool{"__docgen": true, completeVerb: true}

// visibleTopLevel reports whether command name c should appear in usage(),
// completion, and the generated CLI reference.
func visibleTopLevel(c *command) bool {
	return !c.hidden && !alwaysHiddenFromUsage[c.name]
}

// usage prints the global command listing to w, rendered from commandRegistry.
func usage(w io.Writer) {
	fmt.Fprint(w, `koryph — central multi-project orchestrator for autonomous coding agents

USAGE
  koryph <command> [flags]         (run `+"`koryph <command> -h`"+` for a command's flags)

Get one wave running:  init → adopt → run   (adopt is the wizard: deps, beads, config, assets)
Reach full autonomy:   koryph help getting-started   (repo posture, signing, ci, release)

`)

	byName := make(map[string]*command, len(commandRegistry))
	for i := range commandRegistry {
		c := &commandRegistry[i]
		if visibleTopLevel(c) {
			byName[c.name] = c
		}
	}

	printed := make(map[string]bool, len(byName))
	for _, sec := range helpSections {
		var rows []*command
		for _, name := range sec.cmds {
			if c, ok := byName[name]; ok && !printed[name] {
				rows = append(rows, c)
				printed[name] = true
			}
		}
		if len(rows) == 0 {
			continue
		}
		if sec.desc != "" {
			fmt.Fprintf(w, "%s  (%s)\n", sec.title, sec.desc)
		} else {
			fmt.Fprintf(w, "%s\n", sec.title)
		}
		writeCommandRows(w, rows)
		fmt.Fprintln(w)
	}

	// MORE: any visible command not slotted into a section above — guarantees a
	// new command is discoverable before it is curated into helpSections.
	var extra []*command
	for name, c := range byName {
		if !printed[name] {
			extra = append(extra, c)
		}
	}
	if len(extra) > 0 {
		sort.Slice(extra, func(i, j int) bool { return extra[i].name < extra[j].name })
		fmt.Fprintln(w, "MORE")
		writeCommandRows(w, extra)
		fmt.Fprintln(w)
	}

	// ENVIRONMENT (shared source with the generated CLI reference).
	fmt.Fprintln(w, "ENVIRONMENT")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, ev := range envVars {
		fmt.Fprintf(tw, "  %s\t%s\n", ev.name, ev.desc)
	}
	tw.Flush()
	fmt.Fprint(w, `
HELP
  koryph help <command>       show a command's flags (same as `+"`koryph <command> -h`"+`)
  koryph help getting-started the full zero-to-autonomy sequence
  koryph doctor               check your installation and registered projects
`)
}

// writeCommandRows renders one section's command rows (name + summary, subs
// indented) through a tabwriter so summaries align in a column. A hidden sub
// (a back-compat two-word alias for a now-flattened single-verb command,
// koryph-b8g #24) is still dispatchable but omitted from the listing.
func writeCommandRows(w io.Writer, cmds []*command) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range cmds {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
		for i := range c.subs {
			sub := &c.subs[i]
			if sub.hidden {
				continue
			}
			fmt.Fprintf(tw, "  %s %s\t%s\n", c.name, sub.name, sub.summary)
		}
	}
	tw.Flush()
}

// gettingStartedText is the two-track onboarding walkthrough printed by
// `koryph help getting-started` — the honest path to the project's stated goal
// of autonomous merge/push/release-prep with only final release approval left
// to the operator (the four-verb banner alone reaches only one wave).
const gettingStartedText = `koryph — getting started

Two tracks. The first gets a single wave running; the second reaches full
autonomy (autonomous merge, push, and release prep — you approve only releases).

TRACK 1 — one wave
  1. koryph init                        create ~/.koryph, verify tools on PATH
  2. koryph adopt <root>                the wizard: install missing deps, init/harden
                                        the beads DB, derive account + gate/forge/
                                        area_map (confirmed, never guessed silently),
                                        install assets, validate green
     (flag-driven alternative:
      koryph project add <root> --account <personal|work> --identity <email>)
  3. koryph validate --project <id>     re-run the pre-dispatch gate any time
  4. koryph run --project <id> --once   execute one wave

TRACK 2 — full autonomy (per project)
  5. koryph repo check / repo apply     bring GitHub settings + rulesets to desired state
     koryph posture apply <profile>     (or a named posture profile)
  6. koryph signing setup / enable      vault-backed commit signing (required for the merge gate)
  7. koryph ci setup                    install the gate pipeline PRs are checked against
  8. koryph release setup               install the release workflow + release-please config
     koryph bot create / attach         provision the release bot (no-dance Release PRs)
  9. koryph run --project <id> \
       --auto-merge --review            continuous autonomous waves

Observe:  koryph board · koryph tui · koryph status · koryph doctor
Full reference:  docs/user-guide/zero-to-shipped.md  ·  koryph help <command>
`
