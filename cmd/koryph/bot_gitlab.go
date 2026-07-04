// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/koryph/koryph/internal/bot"
)

// cmdBotCreateGitLab implements 'koryph bot create --forge gitlab'.
//
// Guided access-token flow:
//  1. Opens the GitLab project/personal access-token settings page.
//  2. Walks the user through token creation with the required scopes.
//  3. Prompts for the newly-created token (echo-suppressed).
//  4. Validates the token via GET /personal_access_tokens/self.
//  5. Stores the token via the no-vault fallback ladder (same as GitHub flow).
//  6. Persists ~/.koryph/bots/<name>.gitlab.json (mode 0600).
func cmdBotCreateGitLab(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot create --forge gitlab", stderr)
	flagName := fs.String("name", "", "bot name (required; used as the credential file key)")
	flagHost := fs.String("host", "gitlab.com", "GitLab host (default: gitlab.com; set for self-hosted instances)")
	flagProject := fs.String("project", "", "namespace/project path the token should cover (omit for a personal access token)")
	flagTokenName := fs.String("token-name", "", "display name to give the GitLab token (default: koryph-bot-<name>)")
	flagHeadless := fs.Bool("headless", false, "print the settings URL instead of opening the browser")
	flagVaultProvider := fs.String("vault-provider", "", "vault provider for the token (auto-selects when omitted)")
	flagKeyRef := fs.String("key-ref", "", "provider-specific key reference (auto-derived when omitted)")
	flagPlaintext := fs.Bool("plaintext", false, "store the token inline as plaintext (legacy; prefer a vault provider)")
	setUsage(fs, stdout,
		"guided GitLab access-token creation: opens settings URL, validates pasted token, stores via vault",
		"--name N [--host HOST] [--project NS/PROJ] [--token-name NAME] [--headless] [--vault-provider P] [--key-ref R] [--plaintext]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	if *flagName == "" {
		return usageErr(stderr, "bot create --forge gitlab: --name is required")
	}
	if err := bot.ValidateName(*flagName); err != nil {
		return usageErr(stderr, "bot create --forge gitlab: "+err.Error())
	}

	// Refuse to overwrite an existing credential file.
	if _, err := bot.LoadGitLab(*flagName); err == nil {
		return usageErr(stderr,
			fmt.Sprintf("bot create: gitlab bot %q already exists at %s\n  delete the file manually or choose a different --name",
				*flagName, bot.GitLabBotPath(*flagName)))
	}

	ctx := context.Background()
	cfg, err := bot.CreateGitLab(ctx, bot.GitLabCreateOptions{
		Name:          *flagName,
		Host:          *flagHost,
		Project:       *flagProject,
		TokenName:     *flagTokenName,
		Headless:      *flagHeadless,
		Out:           stdout,
		VaultProvider: *flagVaultProvider,
		KeyRef:        *flagKeyRef,
		Plaintext:     *flagPlaintext,
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("bot create --forge gitlab: %w", err))
	}

	fmt.Fprintf(stdout, "\nGitLab bot created successfully.\n")
	fmt.Fprintf(stdout, "  name:       %s\n", cfg.Name)
	fmt.Fprintf(stdout, "  host:       %s\n", cfg.Host)
	fmt.Fprintf(stdout, "  project:    %s\n", cfg.Project)
	fmt.Fprintf(stdout, "  token_name: %s\n", cfg.TokenName)
	if cfg.ExpiresAt != "" {
		fmt.Fprintf(stdout, "  expires_at: %s\n", cfg.ExpiresAt)
	} else {
		fmt.Fprintf(stdout, "  expires_at: (no expiry)\n")
	}
	if cfg.IsPointerGL() {
		fmt.Fprintf(stdout, "  key:        provider=%s key_ref=%s\n", cfg.Provider, cfg.KeyRef)
	}
	fmt.Fprintf(stdout, "  creds:      %s (0600)\n\n", bot.GitLabBotPath(cfg.Name))

	printGLBotNextSteps(stdout, cfg)
	return 0
}

// cmdBotAttachGitLab implements 'koryph bot attach --forge gitlab'.
func cmdBotAttachGitLab(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot attach --forge gitlab", stderr)
	flagName := fs.String("name", "", "bot name (required)")
	flagProject := fs.String("project", "", "namespace/project to attach (required)")
	setUsage(fs, stdout,
		"wire a GitLab project to a bot: set CI variables KORYPH_BOT_TOKEN and KORYPH_BOT_TOKEN_EXPIRY",
		"--name N --project NS/PROJ")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *flagName == "" {
		return usageErr(stderr, "bot attach --forge gitlab: --name is required")
	}
	if *flagProject == "" {
		return usageErr(stderr, "bot attach --forge gitlab: --project is required")
	}

	cfg, err := bot.LoadGitLab(*flagName)
	if err != nil {
		return fail(stderr, err)
	}

	fmt.Fprintf(stdout, "koryph bot attach --forge gitlab: wiring %s to bot %s\n\n", *flagProject, cfg.Name)

	ctx := context.Background()
	result, err := bot.AttachGitLab(ctx, cfg, bot.GitLabAttachOptions{
		Name:    *flagName,
		Project: *flagProject,
		Out:     stdout,
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("bot attach --forge gitlab: %w", err))
	}

	fmt.Fprintf(stdout, "\nDone. %s is wired to bot %s.\n", *flagProject, cfg.Name)
	fmt.Fprintf(stdout, "  Variables set: %v\n\n", result.VariablesSet)
	fmt.Fprintf(stdout, "Verify the configuration:\n")
	fmt.Fprintf(stdout, "  koryph bot check --forge gitlab --name %s --project %s\n", cfg.Name, *flagProject)
	return 0
}

// cmdBotCheckGitLab implements 'koryph bot check --forge gitlab'.
func cmdBotCheckGitLab(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bot check --forge gitlab", stderr)
	flagName := fs.String("name", "", "bot name (required)")
	flagProject := fs.String("project", "", "namespace/project (optional; adds CI variable validators)")
	setUsage(fs, stdout,
		"validate a GitLab bot: token active, scopes OK, expiry WARN, CI variables present",
		"--name N [--project NS/PROJ]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *flagName == "" {
		return usageErr(stderr, "bot check --forge gitlab: --name is required")
	}

	cfg, err := bot.LoadGitLab(*flagName)
	if err != nil {
		return fail(stderr, err)
	}

	if *flagProject != "" {
		fmt.Fprintf(stdout, "koryph bot check --forge gitlab: validating bot %s against project %s\n\n",
			cfg.Name, *flagProject)
	} else {
		fmt.Fprintf(stdout, "koryph bot check --forge gitlab: validating bot %s (token + identity only)\n\n",
			cfg.Name)
	}

	ctx := context.Background()
	findings, err := bot.CheckGitLab(ctx, cfg, bot.CheckGitLabOptions{
		Name:    *flagName,
		Project: *flagProject,
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("bot check --forge gitlab: %w", err))
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

// printGLBotNextSteps prints the next steps after creating a GitLab bot.
func printGLBotNextSteps(w io.Writer, cfg *bot.GitLabConfig) {
	ciURL := bot.GLBotInstallURL(cfg)
	fmt.Fprintln(w, "NEXT STEPS")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "1. Attach the bot to a project (sets CI variables):")
	fmt.Fprintf(w, "     koryph bot attach --forge gitlab --name %s --project %s\n", cfg.Name, cfg.Project)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "2. Verify the configuration:")
	fmt.Fprintf(w, "     koryph bot check --forge gitlab --name %s --project %s\n", cfg.Name, cfg.Project)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "3. View the CI/CD variables on GitLab:")
	fmt.Fprintf(w, "     %s\n", ciURL)
	fmt.Fprintln(w, "")
	if cfg.ExpiresAt != "" {
		fmt.Fprintf(w, "NOTE: This token expires on %s. Set a calendar reminder to rotate it\n", cfg.ExpiresAt)
		fmt.Fprintln(w, "before expiry using:")
		fmt.Fprintf(w, "     koryph bot create --forge gitlab --name %s   # create a new token\n", cfg.Name)
		fmt.Fprintln(w, "     # then re-attach the project to update CI variables")
	}
}
