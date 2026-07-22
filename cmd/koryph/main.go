// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Command koryph is the central multi-project koryph CLI: onboard and
// validate projects, drive the wave engine, and observe/operate live runs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the command mux. It returns the process exit code so main() is a thin
// os.Exit wrapper and every command is unit-testable via (args, stdout, stderr).
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return engine.ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help":
		usage(stdout)
		return 0
	case "help":
		// `koryph help` prints the global usage; `koryph help <cmd> [sub]`
		// routes to that command's own -h so a user never has to remember the
		// flag form.
		if len(rest) == 0 {
			usage(stdout)
			return 0
		}
		if rest[0] == "help" || isHelpArg(rest[0]) {
			usage(stdout)
			return 0
		}
		if rest[0] == "getting-started" {
			fmt.Fprint(stdout, gettingStartedText)
			return 0
		}
		return run(append(rest, "-h"), stdout, stderr)
	case completeVerb:
		// Hidden: the shell-completion resolver. Never mutates state.
		return cmdComplete(rest, stdout, stderr)
	}
	if c := lookupCommand(cmd); c != nil {
		return c.run(rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "koryph: unknown command %q\n\n", cmd)
	usage(stderr)
	return engine.ExitUsage
}

func init() {
	registerCmd(command{
		name:    "version",
		summary: "print the engine version",
		run:     cmdVersion,
		// No DocLinks: version has no associated concept or user-guide page.
	})
}

func cmdVersion(_ []string, stdout, _ io.Writer) int {
	// First line stays exactly "koryph <semver>": release tooling parses its
	// last field (goreleaser's version-parity hook, `make version-check`). The
	// build-provenance lines below appear only on a linker-stamped or VCS-stamped
	// binary, so `go run`/`go test` still emit the single canonical line.
	fmt.Fprintf(stdout, "koryph %s\n", engine.EngineVersion)
	if b := version.Build(); b != "" && b != "v"+engine.EngineVersion {
		fmt.Fprintf(stdout, "  build:  %s\n", b)
	}
	if c := version.Commit(); c != "" {
		fmt.Fprintf(stdout, "  commit: %s\n", c)
	}
	if d := version.Date(); d != "" {
		fmt.Fprintf(stdout, "  built:  %s\n", d)
	}
	return 0
}

// --- shared helpers --------------------------------------------------------

// openStore returns the initialized central registry store (honors
// KORYPH_HOME). Init is idempotent.
func openStore(ctx context.Context) (*registry.Store, error) {
	s := registry.NewStore()
	if err := s.Init(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// newFlagSet builds a ContinueOnError flag set whose parse errors go to stderr.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// errHelp is returned by parseFlags when the user asked for help (-h/--help).
// Callers map it to a clean exit 0 via flagExit; the help text has already
// been printed to stdout by the FlagSet's usage function.
var errHelp = errors.New("help requested")

// isHelpArg reports whether tok is one of the help tokens a parent command
// treats as a request to list its sub-verbs (-h, --help, or the word help).
func isHelpArg(tok string) bool {
	return tok == "-h" || tok == "--help" || tok == "help"
}

// setUsage installs a FlagSet usage function that prints a one-line purpose, a
// positional/flag synopsis, and (when the set defines flags) the flag
// defaults. It replaces stdlib's bare "Usage of X:" so every leaf command's
// -h is self-documenting. synopsis is the text after the command name (e.g.
// "--project ID [flags]" or "<root> [--force]"); pass "" for a flagless,
// positional-less command. Help output goes to stdout; genuine parse-error
// messages still go to the FlagSet's stderr output.
func setUsage(fs *flag.FlagSet, stdout io.Writer, purpose, synopsis string) {
	fs.Usage = func() {
		fmt.Fprintf(stdout, "koryph %s — %s\n\nUSAGE\n  koryph %s", fs.Name(), purpose, fs.Name())
		if synopsis != "" {
			fmt.Fprintf(stdout, " %s", synopsis)
		}
		fmt.Fprintln(stdout)
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(stdout, "\nFLAGS")
			fs.SetOutput(stdout)
			fs.PrintDefaults()
		}
	}
}

// flagGroup is a labeled subset of a command's flags for setGroupedUsage: a
// title and the flag names (without the -- prefix) shown under it, in order.
type flagGroup struct {
	title string
	names []string
}

// setGroupedUsage is setUsage for flag-heavy commands (>8 flags): instead of
// flag.PrintDefaults' single alphabetical list — the "help is a wall" failure
// mode on the flagship `run` — it prints each group under its title in the
// given order. Any flag not named in a group is collected under an OTHER
// heading so nothing is ever hidden. Help goes to stdout.
func setGroupedUsage(fs *flag.FlagSet, stdout io.Writer, purpose, synopsis string, groups []flagGroup) {
	fs.Usage = func() {
		fmt.Fprintf(stdout, "koryph %s — %s\n\nUSAGE\n  koryph %s", fs.Name(), purpose, fs.Name())
		if synopsis != "" {
			fmt.Fprintf(stdout, " %s", synopsis)
		}
		fmt.Fprintln(stdout)

		byName := map[string]*flag.Flag{}
		fs.VisitAll(func(f *flag.Flag) { byName[f.Name] = f })
		printed := map[string]bool{}
		writeGroup := func(title string, flags []*flag.Flag) {
			if len(flags) == 0 {
				return
			}
			fmt.Fprintf(stdout, "\n%s\n", title)
			for _, f := range flags {
				writeFlagDefault(stdout, f)
			}
		}
		for _, g := range groups {
			var rows []*flag.Flag
			for _, n := range g.names {
				if f, ok := byName[n]; ok {
					rows = append(rows, f)
					printed[n] = true
				}
			}
			writeGroup(g.title, rows)
		}
		var rest []*flag.Flag
		fs.VisitAll(func(f *flag.Flag) {
			if !printed[f.Name] {
				rest = append(rest, f)
			}
		})
		writeGroup("OTHER", rest)
	}
}

// writeFlagDefault renders one flag in flag.PrintDefaults' two-line style
// (name + value placeholder, then indented usage with a trailing default),
// used by setGroupedUsage so grouped output matches the stdlib look.
func writeFlagDefault(w io.Writer, f *flag.Flag) {
	valueName, usage := flag.UnquoteUsage(f)
	head := "  --" + f.Name
	if valueName != "" {
		head += " " + valueName
	}
	fmt.Fprintf(w, "%s\n    \t%s", head, usage)
	// Show a non-zero default, matching PrintDefaults' filtering of the zero
	// values (false / 0 / "").
	if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
		fmt.Fprintf(w, " (default %q)", f.DefValue)
	}
	fmt.Fprintln(w)
}

// flagCapture, when non-nil, diverts parseFlags: the fully-populated FlagSet is
// stashed for the completion engine and a sentinel error is returned so the
// calling command returns immediately (via flagExit) before any side effects.
// This lets `koryph __complete` enumerate a command's real flags without
// hand-duplicating any flag list. Access is single-threaded (the CLI is not
// concurrent), so a package var is sufficient.
var flagCapture *flag.FlagSet

// errCaptured is the sentinel parseFlags returns while flagCapture is active.
var errCaptured = errors.New("flagset captured for completion")

// captureFlags runs cmd with capture mode armed and returns the FlagSet the
// command built (nil for a command that defines no flags, e.g. a pure parent).
// Only flag registration runs — every command returns at its parseFlags guard
// before touching the registry, filesystem, or network.
func captureFlags(cmd func([]string, io.Writer, io.Writer) int) *flag.FlagSet {
	prev := flagCapture
	flagCapture = &flag.FlagSet{} // non-nil sentinel; replaced by the real set
	defer func() { flagCapture = prev }()
	cmd(nil, io.Discard, io.Discard)
	if flagCapture == nil {
		return nil
	}
	fs := flagCapture
	if fs.Name() == "" { // untouched sentinel: the command defined no flags
		return nil
	}
	return fs
}

// parseFlags parses args that may lead with positional arguments (stdlib flag
// stops at the first non-flag token, so leading positionals are lifted out
// first, then trailing fs.Args() are appended). A -h/--help request prints the
// usage (via fs.Usage) and returns errHelp so callers can exit 0.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	if flagCapture != nil {
		flagCapture = fs
		return nil, errCaptured
	}
	var pre []string
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		pre = append(pre, args[i])
		i++
	}
	if err := fs.Parse(args[i:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		return nil, err
	}
	return append(pre, fs.Args()...), nil
}

// flagExit maps a parseFlags error to a process exit code: a help request is a
// clean exit 0; any other parse error is a usage error. The usage/help text
// has already been printed.
func flagExit(err error) int {
	if errors.Is(err, errHelp) || errors.Is(err, errCaptured) {
		return 0
	}
	return engine.ExitUsage
}

// subVerb is one row in a parent command's sub-verb listing: the invocation
// synopsis and a one-line purpose.
type subVerb struct {
	syn     string
	purpose string
}

// parentHelp prints a parent command's sub-verb listing to stdout and is used
// when a parent is invoked bare or with -h/--help/help. purpose is the
// parent's one-liner; verbs are its sub-verbs.
func parentHelp(stdout io.Writer, parent, purpose string, verbs []subVerb) {
	fmt.Fprintf(stdout, "koryph %s — %s\n\nUSAGE\n  koryph %s <subcommand> [flags]\n\nSUBCOMMANDS\n", parent, purpose, parent)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, v := range verbs {
		fmt.Fprintf(tw, "  %s\t%s\n", v.syn, v.purpose)
	}
	tw.Flush()
	fmt.Fprintf(stdout, "\nRun `koryph %s <subcommand> -h` for a subcommand's flags.\n", parent)
}

// flagPassed reports whether flag name was explicitly set on fs.
func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// printJSON writes v as indented JSON with a trailing newline.
func printJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// fail prints an error to stderr and returns the fatal exit code.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "koryph:", err)
	return engine.ExitFatal
}

// usageErr prints a message to stderr and returns the usage exit code.
func usageErr(stderr io.Writer, msg string) int {
	fmt.Fprintln(stderr, "koryph:", msg)
	return engine.ExitUsage
}

// sortStrings sorts s in place (ascending) for stable table/summary output.
func sortStrings(s []string) { sort.Strings(s) }
