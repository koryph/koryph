// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/koryph/koryph/internal/bot"
)

// cmdBot dispatches the 'koryph bot' sub-verbs.
//
// Three replication scenarios are first-class:
//
//   - Private bot on the owning GitHub account (default):
//     koryph bot create --name <login>-release-bot
//     koryph bot install --name <login>-release-bot
//     (Only the owning account can install the app.)
//
//   - Public bot for guest-org repos (you admin repos but don't own the org):
//     koryph bot create --name <login>-release-bot --public
//     koryph bot install --name <login>-release-bot
//     (Repo admins in any org can scope-install the app to their repos.)
//
//   - Per-org private bot (you own the org):
//     koryph bot create --name <org>-release-bot --org <org>
//     koryph bot install --name <org>-release-bot
//     (Private app owned by the org; org members can install within that org.)
func cmdBot(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "bot", "provision and manage koryph GitHub App bots", []subVerb{
			{"create [--name N] [--org ORG] [--public]", "create a GitHub App via the manifest flow (one browser click)"},
			{"install --name N", "print/open the installation page for a provisioned bot"},
			{"list", "list provisioned bots in ~/.koryph/bots/"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return cmdBotCreate(rest, stdout, stderr)
	case "install":
		return cmdBotInstall(rest, stdout, stderr)
	case "list":
		return cmdBotList(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown bot subcommand %q", sub))
	}
}

// cmdBotCreate implements 'koryph bot create'.
//
// The GitHub App Manifest flow requires exactly ONE browser click on the
// confirmation page.  This command:
//
//  1. Starts a localhost redirect catcher on an ephemeral port.
//  2. Serves an HTML page that auto-POSTs the pre-filled manifest form to
//     GitHub (github.com/settings/apps/new, or the org equivalent).
//  3. Opens the browser (or prints the URL when TERM is unset / --headless).
//  4. Waits for GitHub to redirect back with ?code=XXX after the click.
//  5. Exchanges the code via the unauthenticated GitHub endpoint
//     POST /app-manifests/{code}/conversions.
//  6. Persists {name, app_id, slug, owner, public, pem} to
//     ~/.koryph/bots/<name>.json (mode 0600; never printed).
//
// Permissions granted: contents:write + pull_requests:write ONLY.
// No org permissions — this is what enables repo-admin installs in guest orgs.
// No webhook is registered.
func cmdBotCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot create", stderr)
	flagName := fs.String("name", "", "GitHub App name (e.g. mylogin-release-bot); defaults to <gh-login>-release-bot when omitted (requires gh CLI)")
	flagOrg := fs.String("org", "", "create the app under this GitHub organization (omit for personal account)")
	flagPublic := fs.Bool("public", false, "make the app publicly installable (required for guest-org repo-admin installs)")
	flagHeadless := fs.Bool("headless", false, "print the URL instead of opening the browser (set automatically when TERM is unset)")
	setUsage(fs, stdout,
		"create a GitHub App via the GitHub App Manifest flow (one browser click)",
		"[--name N] [--org ORG] [--public] [--headless]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	name := *flagName
	if name == "" {
		// Best-effort: derive from gh CLI.
		resolved, err := resolveDefaultBotName(*flagOrg)
		if err != nil {
			return usageErr(stderr, "bot create: --name is required (could not auto-detect GitHub login: "+err.Error()+")")
		}
		name = resolved
		fmt.Fprintf(stdout, "bot create: using name %q (from gh auth status)\n", name)
	}
	if err := bot.ValidateName(name); err != nil {
		return usageErr(stderr, "bot create: "+err.Error())
	}

	// Refuse to overwrite an existing credential file without an explicit flag.
	if _, err := bot.Load(name); err == nil {
		return usageErr(stderr,
			fmt.Sprintf("bot create: bot %q already exists at %s\n  delete the file manually or choose a different --name to create a new app", name, bot.BotPath(name)))
	}

	headless := *flagHeadless

	ctx := context.Background()
	cfg, err := bot.Create(ctx, bot.CreateOptions{
		Name:     name,
		Org:      *flagOrg,
		Public:   *flagPublic,
		Headless: headless,
		Out:      stdout,
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("bot create: %w", err))
	}

	fmt.Fprintf(stdout, "\nGitHub App created successfully.\n")
	fmt.Fprintf(stdout, "  name:   %s\n", cfg.Name)
	fmt.Fprintf(stdout, "  app_id: %d\n", cfg.AppID)
	fmt.Fprintf(stdout, "  slug:   %s\n", cfg.Slug)
	fmt.Fprintf(stdout, "  owner:  %s\n", cfg.Owner)
	fmt.Fprintf(stdout, "  public: %v\n", cfg.Public)
	fmt.Fprintf(stdout, "  creds:  %s (0600)\n\n", bot.BotPath(cfg.Name))

	printBotNextSteps(stdout, cfg)
	return 0
}

// cmdBotInstall implements 'koryph bot install --name N'.
//
// Installation is always a browser action (GitHub requires it).  This command
// prints the installation URL and explains the three replication scenarios:
//
//   - Private bot: only the owning account can install; the install page
//     shows the user's personal repos and any orgs they own.
//   - Public bot (--public was passed at create time): any user who is a
//     repo admin can scope-install the app to individual repos.  If the org
//     policy requires approval, GitHub shows an "approval request" flow
//     instead of an immediate install — the org owner must approve.
//   - Org-owned private bot: behaves like personal-account private, but the
//     install page is scoped to the org.
func cmdBotInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot install", stderr)
	flagName := fs.String("name", "", "bot name (required)")
	setUsage(fs, stdout,
		"print/open the GitHub App installation page for a provisioned bot",
		"--name N")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *flagName == "" {
		return usageErr(stderr, "bot install: --name is required")
	}

	cfg, err := bot.Load(*flagName)
	if err != nil {
		return fail(stderr, err)
	}

	installURL := bot.InstallURL(cfg)
	fmt.Fprintf(stdout, "Installation URL: %s\n\n", installURL)
	printInstallGuidance(stdout, cfg)
	return 0
}

// cmdBotList implements 'koryph bot list'.
func cmdBotList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot list", stderr)
	setUsage(fs, stdout, "list provisioned bots in ~/.koryph/bots/", "")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	names, err := bot.List()
	if err != nil {
		return fail(stderr, err)
	}
	if len(names) == 0 {
		fmt.Fprintln(stdout, "no bots provisioned (run `koryph bot create` to create one)")
		return 0
	}
	for _, n := range names {
		cfg, err := bot.Load(n)
		if err != nil {
			fmt.Fprintf(stdout, "  %s  (load error: %v)\n", n, err)
			continue
		}
		vis := "private"
		if cfg.Public {
			vis = "public"
		}
		fmt.Fprintf(stdout, "  %-30s  app_id=%-8d  owner=%-20s  %s\n",
			cfg.Name, cfg.AppID, cfg.Owner, vis)
	}
	return 0
}

// --- helpers ----------------------------------------------------------------

// resolveDefaultBotName derives a default bot name from gh CLI output.
// Org non-empty: <org>-release-bot. Personal: <gh-login>-release-bot.
func resolveDefaultBotName(org string) (string, error) {
	if org != "" {
		return org + "-release-bot", nil
	}
	// Try gh CLI to get the authenticated username.
	login, err := ghAuthLogin()
	if err != nil {
		return "", err
	}
	return login + "-release-bot", nil
}

// ghAuthLogin invokes 'gh auth status --show-token' and extracts the login.
// It parses the line "Logged in to github.com account <login> (…)".
func ghAuthLogin() (string, error) {
	out, err := runGH("auth", "status")
	if err != nil {
		return "", fmt.Errorf("gh auth status: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// e.g.: "✓ Logged in to github.com account mctocat (keyring)"
		if strings.Contains(line, "Logged in to github.com account") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "account" && i+1 < len(parts) {
					return strings.TrimRight(parts[i+1], " ()"), nil
				}
			}
		}
	}
	return "", fmt.Errorf("could not extract GitHub login from gh auth status output")
}

// printBotNextSteps prints the user's next steps after creating a bot.
func printBotNextSteps(w io.Writer, cfg *bot.Config) {
	installURL := bot.InstallURL(cfg)
	fmt.Fprintln(w, "NEXT STEPS")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "1. Install the app (one browser click):")
	fmt.Fprintf(w, "     koryph bot install --name %s\n", cfg.Name)
	fmt.Fprintf(w, "   or open directly: %s\n", installURL)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "2. Wire the bot to a project:")
	fmt.Fprintf(w, "     koryph release setup --project <id> --bot %s\n", cfg.Name)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "3. Verify the installation:")
	fmt.Fprintf(w, "     koryph doctor --project <id>\n")
	fmt.Fprintln(w, "")
	if cfg.Public {
		fmt.Fprintln(w, "NOTE: This is a PUBLIC app.  Any repo admin can install it on repos")
		fmt.Fprintln(w, "in organizations you don't own (guest-org scenario).  When the org's")
		fmt.Fprintln(w, "policy requires approval, the repo admin's request goes to the org owner")
		fmt.Fprintln(w, "for approval before the app is activated.")
	} else {
		fmt.Fprintln(w, "NOTE: This is a PRIVATE app.  It can only be installed on repos owned")
		fmt.Fprintln(w, "by the creating account (or the owning org if --org was specified).")
		fmt.Fprintln(w, "To install in guest orgs, re-create with --public.")
	}
}

// runGH runs the gh CLI binary (honouring KORYPH_GH_BIN) with the given
// arguments and returns combined stdout output.  It is used only for
// read-only queries such as 'gh auth status'.
func runGH(args ...string) (string, error) {
	bin := "gh"
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		bin = v
	}
	cmd := exec.Command(bin, args...) //nolint:gosec // user-controlled bin is intentional
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

// printInstallGuidance explains the installation scenarios.
func printInstallGuidance(w io.Writer, cfg *bot.Config) {
	fmt.Fprintln(w, "Visit the URL above to install the GitHub App.  GitHub will redirect you")
	fmt.Fprintln(w, "to a page where you can choose which repositories to grant access to.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "INSTALLATION SCENARIOS")
	fmt.Fprintln(w, "")
	if cfg.Public {
		fmt.Fprintln(w, "  PUBLIC app — three ways to install:")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  A. Personal account or owned org:")
		fmt.Fprintln(w, "       Open the URL above; choose repos from the dropdown; click Install.")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  B. Guest org (you are a repo admin, not an org owner):")
		fmt.Fprintln(w, "       Open the URL above; select the guest org from the account dropdown;")
		fmt.Fprintln(w, "       choose the specific repos you administer; click Install.")
		fmt.Fprintln(w, "       GitHub creates a REPO-SCOPED installation — you are not granting")
		fmt.Fprintln(w, "       access to every repo in the org, only the ones you select.")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  C. Guest org with approval policy:")
		fmt.Fprintln(w, "       If the org requires app approvals, your install click sends an")
		fmt.Fprintln(w, "       approval request to the org owner.  The bot activates after they")
		fmt.Fprintln(w, "       approve.  Check with the org owner if the install appears to hang.")
	} else {
		fmt.Fprintln(w, "  PRIVATE app — only the creating account can install this app.")
		fmt.Fprintln(w, "")
		fmt.Fprintf(w, "  Owner: %s\n", cfg.Owner)
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  The installation page shows repositories owned by the account or org")
		fmt.Fprintln(w, "  that created the app.  Select the repos to grant access, then click")
		fmt.Fprintln(w, "  Install.  For guest-org installs, re-create the bot with --public.")
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "After installing, run:")
	fmt.Fprintf(w, "  koryph release setup --project <id> --bot %s\n", cfg.Name)
}
