// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/koryph/koryph/internal/bot"
	"github.com/koryph/koryph/internal/forge"
	ghpkg "github.com/koryph/koryph/internal/forge/github"
	"github.com/koryph/koryph/internal/signing"
)

func init() {
	registerCmd(command{
		name:    "bot",
		summary: "provision and manage koryph GitHub App bots",
		run:     cmdBot,
		DocLinks: []string{
			"concepts/release-train.md",
			"user-guide/release-bot.md",
		},
		subs: []command{
			{
				name:     "create",
				summary:  "create a GitHub App via the manifest flow (one browser click)",
				run:      cmdBotCreate,
				DocLinks: []string{"user-guide/release-bot.md"},
			},
			{
				name:     "install",
				summary:  "print/open the installation page for a provisioned bot",
				run:      cmdBotInstall,
				DocLinks: []string{"user-guide/release-bot.md"},
			},
			{
				name:     "attach",
				summary:  "wire a repo to a bot: set secrets and enable Actions PR-approval toggle",
				run:      cmdBotAttach,
				DocLinks: []string{"user-guide/release-bot.md"},
			},
			{
				name:     "list",
				summary:  "list provisioned bots in ~/.koryph/bots/",
				run:      cmdBotList,
				DocLinks: []string{"user-guide/release-bot.md"},
			},
			{
				name:     "check",
				summary:  "run the bot validator chain (JWT, installation, secrets, Actions toggle)",
				run:      cmdBotCheck,
				DocLinks: []string{"user-guide/release-bot.md"},
			},
			{
				name:     "vault-migrate",
				summary:  "move a plaintext bot private key into a vault or encrypted file",
				run:      cmdBotVaultMigrate,
				DocLinks: []string{"user-guide/signing.md", "user-guide/release-bot.md"},
			},
		},
	})
}

// cmdBot dispatches the 'koryph bot' sub-verbs.
//
// Three replication scenarios are first-class:
//
//   - Private bot on the owning GitHub account (default):
//     koryph bot create --name <login>-release-bot
//     koryph bot install --name <login>-release-bot
//     koryph bot attach --name <login>-release-bot --repo OWNER/REPO
//     (Only the owning account can install the app.)
//
//   - Public bot for guest-org repos (you admin repos but don't own the org):
//     koryph bot create --name <login>-release-bot --public
//     koryph bot install --name <login>-release-bot
//     koryph bot attach --name <login>-release-bot --repo OWNER/REPO
//     (Repo admins in any org can scope-install the app to their repos.)
//
//   - Per-org private bot (you own the org):
//     koryph bot create --name <org>-release-bot --org <org>
//     koryph bot install --name <org>-release-bot
//     koryph bot attach --name <org>-release-bot --repo OWNER/REPO
//     (Private app owned by the org; org members can install within that org.)
func cmdBot(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "bot", "provision and manage koryph bots (GitHub App or GitLab access token)", []subVerb{
			{"create [--forge github|gitlab] [--name N] ...", "create a bot (GitHub: App manifest flow; GitLab: access-token flow)"},
			{"install --name N", "print/open the installation page for a GitHub App bot"},
			{"attach [--forge github|gitlab] --name N ...", "wire a repo/project to a bot: set secrets or CI variables"},
			{"list [--check] [--forge github|gitlab]", "list provisioned bots; --check does a live identity check"},
			{"check [--forge github|gitlab] --name N ...", "run the full validator chain"},
			{"vault-migrate --name N [--provider P] [--key-ref R]", "move a plaintext key into a vault or encrypted file"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]

	// Peek at --forge flag to dispatch to forge-specific handlers.
	forge := peekForgeFlag(rest)

	switch sub {
	case "create":
		if forge == "gitlab" {
			return cmdBotCreateGitLab(rest, stdout, stderr)
		}
		return cmdBotCreate(rest, stdout, stderr)
	case "install":
		return cmdBotInstall(rest, stdout, stderr)
	case "attach":
		if forge == "gitlab" {
			return cmdBotAttachGitLab(rest, stdout, stderr)
		}
		return cmdBotAttach(rest, stdout, stderr)
	case "list":
		return cmdBotList(rest, stdout, stderr)
	case "check":
		if forge == "gitlab" {
			return cmdBotCheckGitLab(rest, stdout, stderr)
		}
		return cmdBotCheck(rest, stdout, stderr)
	case "vault-migrate":
		return cmdBotVaultMigrate(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown bot subcommand %q", sub))
	}
}

// peekForgeFlag does a linear scan of args looking for --forge <value> or
// --forge=<value>. Returns "" when the flag is absent.
func peekForgeFlag(args []string) string {
	for i, a := range args {
		if a == "--forge" && i+1 < len(args) {
			return args[i+1]
		}
		if len(a) > 8 && a[:8] == "--forge=" {
			return a[8:]
		}
	}
	return ""
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
//  6. Stores the private key via the selected vault provider (no-vault fallback
//     ladder: configured vault > keychain (darwin) > encrypted-file) or inline
//     when --plaintext is explicitly requested.
//  7. Persists {name, app_id, slug, owner, public, provider, key_ref} (pointer
//     mode) or {name, app_id, slug, owner, public, pem} (inline, --plaintext)
//     to ~/.koryph/bots/<name>.json (mode 0600; private key never printed).
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
	flagVaultProvider := fs.String("provider", "", "vault provider for the private key (protonpass|onepassword|encrypted-file|keychain|file|…); auto-selects when omitted")
	fs.StringVar(flagVaultProvider, "vault-provider", "", "deprecated alias for --provider")
	hideFlag(fs, "vault-provider")
	flagKeyRef := fs.String("key-ref", "", "provider-specific reference for the key (e.g. pass:// URI, op:// ref, or file path); auto-derived when omitted")
	flagPlaintext := fs.Bool("plaintext", false, "store the private key inline as plaintext PEM (legacy; prefer a vault or encrypted-file provider)")
	setUsage(fs, stdout,
		"create a GitHub App via the GitHub App Manifest flow (one browser click)",
		"[--name N] [--org ORG] [--public] [--headless] [--provider P] [--key-ref R] [--plaintext]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	// Validate provider flag.
	if *flagVaultProvider != "" && !isKnownProvider(*flagVaultProvider) {
		return usageErr(stderr, fmt.Sprintf("bot create: unknown --provider %q; valid: %s",
			*flagVaultProvider, strings.Join(signing.VaultProviders, "|")))
	}
	if *flagPlaintext && *flagVaultProvider != "" {
		return usageErr(stderr, "bot create: --plaintext and --provider are mutually exclusive")
	}

	name := *flagName
	if name == "" {
		// Best-effort: derive from gh CLI.
		resolved, err := resolveDefaultBotName(context.Background(), *flagOrg, ghpkg.New().Bot())
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

	ctx := context.Background()
	cfg, err := bot.Create(ctx, bot.CreateOptions{
		Name:          name,
		Org:           *flagOrg,
		Public:        *flagPublic,
		Headless:      *flagHeadless,
		Out:           stdout,
		VaultProvider: *flagVaultProvider,
		KeyRef:        *flagKeyRef,
		Plaintext:     *flagPlaintext,
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
	if cfg.IsPointer() {
		fmt.Fprintf(stdout, "  key:    provider=%s key_ref=%s\n", cfg.Provider, cfg.KeyRef)
	}
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
// With --check it also does a live GET /app identity check per stored bot.
func cmdBotList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot list", stderr)
	liveCheck := fs.Bool("check", false, "perform a live GET /app identity check for each bot")
	setUsage(fs, stdout, "list provisioned bots in ~/.koryph/bots/", "[--check]")
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
		if *liveCheck {
			// Offline PEM check only — the full identity check is koryph bot check.
			if pemErr := bot.ValidatePEM(cfg); pemErr != nil {
				fmt.Fprintf(stdout, "    ! PEM invalid: %v\n", pemErr)
			} else {
				fmt.Fprintf(stdout, "    ✓ PEM valid (run `koryph bot check --name %s` for full identity verification)\n", n)
			}
		}
	}
	return 0
}

// cmdBotAttach implements 'koryph bot attach --name N --repo OWNER/REPO'.
//
// Idempotent: safe to re-run. Each step checks current state before mutating.
func cmdBotAttach(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot attach", stderr)
	flagName := fs.String("name", "", "bot name (required)")
	flagRepo := fs.String("repo", "", "GitHub repository as OWNER/REPO (required)")
	flagOrgSecrets := fs.Bool("org-secrets", false, "set secrets at org level with selected-repos visibility instead of per-repo")
	setUsage(fs, stdout,
		"wire a repo to a bot: add to installation, set secrets, enable Actions PR-approval toggle",
		"--name N --repo OWNER/REPO [--org-secrets]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *flagName == "" {
		return usageErr(stderr, "bot attach: --name is required")
	}
	if *flagRepo == "" {
		return usageErr(stderr, "bot attach: --repo is required")
	}

	cfg, err := bot.Load(*flagName)
	if err != nil {
		return fail(stderr, err)
	}

	fmt.Fprintf(stdout, "koryph bot attach: wiring %s to bot %s\n\n", *flagRepo, cfg.Name)

	ctx := context.Background()
	provider := ghpkg.New()
	result, err := bot.Attach(ctx, cfg, bot.AttachOptions{
		Name:       *flagName,
		Repo:       *flagRepo,
		OrgSecrets: *flagOrgSecrets,
		Out:        stdout,
		BotSvc:     provider.Bot(),
		SecretsSvc: provider.Secrets(),
		RepoSvc:    provider.Repo(),
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("bot attach: %w", err))
	}

	fmt.Fprintf(stdout, "\n✓ Done. %s is wired to bot %s (installation %d).\n\n",
		*flagRepo, cfg.Name, result.InstallationID)
	fmt.Fprintf(stdout, "Verify the full configuration:\n")
	fmt.Fprintf(stdout, "  koryph bot check --name %s --repo %s\n", cfg.Name, *flagRepo)
	return 0
}

// cmdBotCheck implements 'koryph bot check --name N [--repo OWNER/REPO]'.
//
// Runs the full validator chain: JWT validity, installation existence,
// installation covers repo, secrets present, Actions toggle on, caller
// workflow present.
func cmdBotCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot check", stderr)
	flagName := fs.String("name", "", "bot name (required)")
	flagRepo := fs.String("repo", "", "GitHub repository as OWNER/REPO (optional; adds repo-scoped validators)")
	setUsage(fs, stdout,
		"run the bot validator chain: JWT, installation, secrets, Actions toggle, caller workflow",
		"--name N [--repo OWNER/REPO]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *flagName == "" {
		return usageErr(stderr, "bot check: --name is required")
	}

	cfg, err := bot.Load(*flagName)
	if err != nil {
		return fail(stderr, err)
	}

	if *flagRepo != "" {
		fmt.Fprintf(stdout, "koryph bot check: validating bot %s against repo %s\n\n", cfg.Name, *flagRepo)
	} else {
		fmt.Fprintf(stdout, "koryph bot check: validating bot %s (credentials + identity only)\n\n", cfg.Name)
	}

	ctx := context.Background()
	provider := ghpkg.New()
	findings, err := bot.Check(ctx, cfg, bot.CheckOptions{
		Name:       *flagName,
		Repo:       *flagRepo,
		SecretsSvc: provider.Secrets(),
		RepoSvc:    provider.Repo(),
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("bot check: %w", err))
	}

	exitCode := bot.PrintCheckResults(stdout, findings)

	switch exitCode {
	case 0:
		fmt.Fprintln(stdout, "\nall checks passed.")
	case 1:
		fmt.Fprintln(stdout, "\nwarnings found (exit 1).")
	default:
		fmt.Fprintln(stdout, "\nfailures found (exit 2).")
	}
	return exitCode
}

// --- helpers ----------------------------------------------------------------

// resolveDefaultBotName derives a default bot name from provider identity.
// Org non-empty: <org>-release-bot. Personal: <gh-login>-release-bot.
func resolveDefaultBotName(ctx context.Context, org string, svc forge.BotService) (string, error) {
	if org != "" {
		return org + "-release-bot", nil
	}
	login, err := svc.CurrentUser(ctx)
	if err != nil {
		return "", err
	}
	return login + "-release-bot", nil
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

// cmdBotVaultMigrate implements 'koryph bot vault-migrate --name N'.
//
// Moves a plaintext private key from the bot credential JSON file into a
// secure provider (vault, macOS Keychain, or age-encrypted file) and rewrites
// the credential file as a pointer (Provider + KeyRef; no inline PEM).
//
// Destinations:
//
//	--provider P  use the named vault provider
//	(default)     apply the no-vault fallback ladder:
//	              keychain (darwin) > encrypted-file (other platforms)
//
// The migration is atomic from the user's perspective: the old PEM is not
// erased until the vault store succeeds.  On failure the original file is
// left untouched.
func cmdBotVaultMigrate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot vault-migrate", stderr)
	flagName := fs.String("name", "", "bot name (required)")
	flagVaultProvider := fs.String("provider", "", "destination vault provider (auto-selected when omitted)")
	fs.StringVar(flagVaultProvider, "vault-provider", "", "deprecated alias for --provider")
	hideFlag(fs, "vault-provider")
	flagKeyRef := fs.String("key-ref", "", "provider-specific key reference (auto-derived when omitted)")
	setUsage(fs, stdout,
		"move a plaintext bot private key into a vault or encrypted file",
		"--name N [--provider P] [--key-ref R]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *flagName == "" {
		return usageErr(stderr, "bot vault-migrate: --name is required")
	}
	if *flagVaultProvider != "" && !isKnownProvider(*flagVaultProvider) {
		return usageErr(stderr, fmt.Sprintf("bot vault-migrate: unknown --provider %q; valid: %s",
			*flagVaultProvider, strings.Join(signing.VaultProviders, "|")))
	}

	cfg, err := bot.Load(*flagName)
	if err != nil {
		return fail(stderr, err)
	}

	// Guard: already a pointer with no inline PEM.
	if cfg.IsPointer() && cfg.PEM == "" {
		fmt.Fprintf(stdout, "bot %q is already vault-backed (provider=%s key_ref=%s); nothing to migrate.\n",
			cfg.Name, cfg.Provider, cfg.KeyRef)
		return 0
	}

	// Must have inline PEM to migrate.
	if cfg.PEM == "" {
		return fail(stderr, fmt.Errorf("bot vault-migrate: bot %q has no inline PEM to migrate", cfg.Name))
	}

	// Resolve destination provider.
	provider := *flagVaultProvider
	if provider == "" {
		provider = signing.ResolveDefaultProvider()
	}

	keyRef := *flagKeyRef
	if keyRef == "" {
		keyRef = bot.DefaultKeyRef(provider, cfg.Name)
	}

	fmt.Fprintf(stdout, "Migrating bot %q: %s → %s (key_ref=%s)\n\n", cfg.Name, "inline PEM", provider, keyRef)

	ctx := context.Background()
	pemBytes := []byte(cfg.PEM)

	var storeErr error
	switch provider {
	case signing.ProviderEncryptedFile:
		passphrase, promptErr := signing.PromptPassphraseOnce(
			fmt.Sprintf("Passphrase for %s (new encrypted key): ", keyRef))
		if promptErr != nil {
			return fail(stderr, fmt.Errorf("bot vault-migrate: passphrase prompt: %w", promptErr))
		}
		storeErr = signing.StoreEncryptedFile(keyRef, pemBytes, passphrase)
		if storeErr == nil {
			fmt.Fprintf(stdout, "  ✓ private key encrypted and stored at %s\n", keyRef)
			fmt.Fprintf(stdout, "    (same posture as a passphrase-protected ~/.ssh key — keep it backed up, never commit it)\n")
		}

	case signing.ProviderKeychain:
		storeErr = signing.StoreKeychain(keyRef, pemBytes)
		if storeErr == nil {
			fmt.Fprintf(stdout, "  ✓ private key stored in macOS Keychain (%s)\n", keyRef)
			fmt.Fprintf(stdout, "    (same posture as a passphrase-protected ~/.ssh key — keep it backed up)\n")
		}

	default:
		// Generic CLI-backed vault.
		storeErr = signing.StoreSecret(ctx, provider, keyRef, pemBytes, "")
		if storeErr == nil {
			fmt.Fprintf(stdout, "  ✓ private key stored in %s (ref: %s)\n", provider, keyRef)
		}
	}

	if storeErr != nil {
		return fail(stderr, fmt.Errorf("bot vault-migrate: store via %s: %w", provider, storeErr))
	}

	// Rewrite cfg as pointer — clear inline PEM.
	cfg.Provider = provider
	cfg.KeyRef = keyRef
	cfg.PEM = ""

	if err := bot.Save(cfg); err != nil {
		return fail(stderr, fmt.Errorf("bot vault-migrate: save credential pointer: %w", err))
	}

	fmt.Fprintf(stdout, "\n✓ Migration complete.\n")
	fmt.Fprintf(stdout, "  %s now points to %s (%s)\n", bot.BotPath(cfg.Name), keyRef, provider)
	fmt.Fprintf(stdout, "  Verify: koryph bot check --name %s\n", cfg.Name)
	return 0
}

// isKnownProvider reports whether name is a recognized vault provider.
func isKnownProvider(name string) bool {
	for _, p := range signing.VaultProviders {
		if p == name {
			return true
		}
	}
	return false
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
