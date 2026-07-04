// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/signing"
)

// atFilePrefix marks a --public-key value as a path to read from disk.
const atFilePrefix = "@"

// cmdSigning dispatches the signing sub-verbs.
func cmdSigning(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "signing", "configure and operate vault-backed commit signing", []subVerb{
			{"setup --project ID --provider P --identity EMAIL [flags]", "write the signing policy into the project adapter"},
			{"enable --project ID", "load the key into the SSH agent + apply repo git config"},
			{"keygen --project ID [--provider P] [--key-ref PATH]", "generate + store a signing key (no-vault path; requires passphrase)"},
			{"status --project ID", "mode/provider/agent-ready/repo-config/posture summary"},
			{"verify --project ID --branch BR", "verify branch commit signatures (exit 1 on any bad)"},
		})
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "setup":
		return cmdSigningSetup(rest, stdout, stderr)
	case "enable":
		return cmdSigningEnable(rest, stdout, stderr)
	case "keygen":
		return cmdSigningKeygen(rest, stdout, stderr)
	case "status":
		return cmdSigningStatus(rest, stdout, stderr)
	case "verify":
		return cmdSigningVerify(rest, stdout, stderr)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown signing subcommand %q", sub))
	}
}

// cmdSign dispatches artifact-signing sub-verbs.
func cmdSign(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "sign", "sign artifacts with the project's vault key", []subVerb{
			{"blob --project ID <path>", "cosign sign-blob an artifact (writes <path>.sig)"},
		})
		return 0
	}
	if args[0] != "blob" {
		return usageErr(stderr, fmt.Sprintf("unknown sign subcommand %q (want blob)", args[0]))
	}
	return cmdSignBlob(args[1:], stdout, stderr)
}

// signingProject loads the registry record + project config for a signing
// command.
func signingProject(ctx context.Context, projectID string) (*registry.Store, *registry.Record, *project.Config, error) {
	store, err := openStore(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	rec, err := store.Get(projectID)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := project.Load(rec.Root)
	if err != nil {
		return nil, nil, nil, err
	}
	return store, rec, cfg, nil
}

// cmdSigningSetup writes the signing policy into the project's
// koryph.project.json (audited).
//
// The public key is resolved deterministically via exactly one of:
//
//	--public-key <literal>     SSH public key literal ("ssh-ed25519 AAAA...")
//	--public-key @<path>       read public key from file at path
//	--key-ref <URI>            view vault item by URI (JSON), extract key
//	--vault-name N --item-title T  view vault item by title (JSON), extract key
//
// Each project independently pins its own public_key; 'pass-cli ssh-agent
// load' may hold many keys — repo-level user.signingkey selects which one
// signs in each repo.
func cmdSigningSetup(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("signing setup", stderr)
	projectID := fs.String("project", "", "project id (required)")
	provider := fs.String("provider", "", "vault provider: protonpass|onepassword|file|command")
	keyRef := fs.String("key-ref", "", "vault item URI / file path for the signing key (also used for public-key resolution when no --public-key or --vault-name/--item-title is given)")
	vaultName := fs.String("vault-name", "", "vault name for public-key resolution via view_by_title template")
	itemTitle := fs.String("item-title", "", "item title for public-key resolution via view_by_title template (requires --vault-name)")
	identity := fs.String("identity", "", "signer email (required)")
	mode := fs.String("mode", signing.ModeSSH, "signing mode: ssh|gitsign")
	publicKey := fs.String("public-key", "", `SSH public key: literal ("ssh-ed25519 AAAA...") or "@<path>" to read from file`)
	artifacts := fs.Bool("artifacts", false, "enable cosign blob signing (`koryph sign blob`)")
	setUsage(fs, stdout, "write the vault-backed signing policy into the project adapter",
		"--project ID --provider P --identity EMAIL [flags]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *projectID == "" {
		return usageErr(stderr, "signing setup: --project is required")
	}
	if *identity == "" {
		return usageErr(stderr, "signing setup: --identity is required")
	}

	// Validate mutually exclusive public-key sources.
	hasPublicKey := *publicKey != ""
	hasVaultTitle := *vaultName != "" || *itemTitle != ""
	if hasPublicKey && hasVaultTitle {
		return usageErr(stderr, "signing setup: --public-key conflicts with --vault-name/--item-title (use exactly one public-key source)")
	}
	if (*vaultName != "") != (*itemTitle != "") {
		return usageErr(stderr, "signing setup: --vault-name and --item-title must be used together")
	}

	ctx := context.Background()
	store, rec, cfg, err := signingProject(ctx, *projectID)
	if err != nil {
		return fail(stderr, err)
	}

	// Resolve the SSH public key.
	resolvedPub, resolvedVaultName, resolvedItemTitle := "", "", ""
	switch {
	case hasPublicKey:
		// Literal or @file.
		pub, perr := resolvePublicKeyLiteral(*publicKey)
		if perr != nil {
			return fail(stderr, perr)
		}
		resolvedPub = pub

	case hasVaultTitle:
		// Resolve via view_by_title template.
		vault, verr := signing.LoadVault()
		if verr != nil {
			return fail(stderr, verr)
		}
		pub, verr := vault.ResolvePublicKey(ctx, *provider, "", *vaultName, *itemTitle)
		if verr != nil {
			return fail(stderr, fmt.Errorf("signing setup: public-key resolution failed: %w", verr))
		}
		resolvedPub = pub
		resolvedVaultName = *vaultName
		resolvedItemTitle = *itemTitle
		fmt.Fprintf(stdout, "resolved public key via --vault-name %q --item-title %q: %s\n",
			*vaultName, *itemTitle, resolvedPub)

	case *keyRef != "":
		// Resolve via view template (URI selector).
		vault, verr := signing.LoadVault()
		if verr != nil {
			return fail(stderr, verr)
		}
		pub, verr := vault.ResolvePublicKey(ctx, *provider, *keyRef, "", "")
		if verr != nil {
			return fail(stderr, fmt.Errorf("signing setup: public-key resolution failed: %w", verr))
		}
		resolvedPub = pub
		fmt.Fprintf(stdout, "resolved public key via --key-ref %q: %s\n", *keyRef, resolvedPub)
	}
	// For gitsign mode and non-SSH providers the public key is optional.

	sc := &signing.Config{
		Required:  true,
		Mode:      *mode,
		Provider:  *provider,
		KeyRef:    *keyRef,
		VaultName: resolvedVaultName,
		ItemTitle: resolvedItemTitle,
		Identity:  *identity,
		PublicKey: resolvedPub,
		Artifacts: *artifacts,
	}

	if err := sc.Validate(); err != nil {
		return fail(stderr, fmt.Errorf("signing setup: %w", err))
	}
	cfg.Signing = sc
	if err := cfg.Save(rec.Root); err != nil {
		return fail(stderr, err)
	}
	_ = store.Audit(registry.Event{
		Kind:      "update",
		ProjectID: *projectID,
		Actor:     cliActor(),
		Detail: map[string]string{
			"what":       "signing setup",
			"mode":       sc.EffectiveMode(),
			"provider":   sc.Provider,
			"identity":   sc.Identity,
			"key_source": signingKeySource(sc),
		},
	})
	fmt.Fprintf(stdout, "signing policy written to %s\n", filepath.Join(rec.Root, project.ConfigFileName))
	if err := printJSON(stdout, sc); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "next: koryph signing enable --project %s\n", *projectID)
	return 0
}

// resolvePublicKeyLiteral resolves a --public-key value: if it starts with
// "@" the rest is treated as a file path; otherwise the value is used as-is.
func resolvePublicKeyLiteral(value string) (string, error) {
	if strings.HasPrefix(value, atFilePrefix) {
		path := strings.TrimPrefix(value, atFilePrefix)
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("signing setup: --public-key @%s: %w", path, err)
		}
		pub := strings.TrimSpace(string(data))
		if pub == "" {
			return "", fmt.Errorf("signing setup: --public-key @%s: file is empty", path)
		}
		return pub, nil
	}
	return strings.TrimSpace(value), nil
}

// signingKeySource returns a human-readable string describing the public key
// provenance, for audit and status display.
func signingKeySource(sc *signing.Config) string {
	switch {
	case sc.VaultName != "" && sc.ItemTitle != "":
		return fmt.Sprintf("vault-name=%q item-title=%q", sc.VaultName, sc.ItemTitle)
	case sc.KeyRef != "":
		return "key-ref=" + sc.KeyRef
	default:
		return "literal"
	}
}

// cmdSigningEnable makes signing operational: agent loaded, key present,
// repo configured.
func cmdSigningEnable(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("signing enable", stderr)
	projectID := fs.String("project", "", "project id (required)")
	setUsage(fs, stdout, "load the key into the SSH agent + apply repo git config", "--project ID")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *projectID == "" {
		return usageErr(stderr, "signing enable: --project is required")
	}

	ctx := context.Background()
	_, rec, cfg, err := signingProject(ctx, *projectID)
	if err != nil {
		return fail(stderr, err)
	}
	if cfg.Signing == nil {
		return fail(stderr, fmt.Errorf("project %s has no signing policy (run `koryph signing setup` first)", *projectID))
	}
	vault, err := signing.LoadVault()
	if err != nil {
		return fail(stderr, err)
	}
	// Load the key into the operator's ambient agent (for the operator's own
	// signed commits) AND into the koryph scoped agent (which holds ONLY the
	// signing key; dispatched agents are pointed there so they never reach the
	// operator's other keys — see internal/signing/scoped.go).
	if err := signing.EnsureAgent(ctx, vault, cfg.Signing); err != nil {
		return fail(stderr, err)
	}
	if err := signing.EnsureScopedAgent(ctx, vault, cfg.Signing); err != nil {
		return fail(stderr, err)
	}
	if err := signing.ConfigureRepo(ctx, rec.Root, cfg.Signing); err != nil {
		return fail(stderr, err)
	}
	printSigningStatus(ctx, stdout, rec, cfg.Signing)
	if cfg.Signing.EffectiveMode() == signing.ModeSSH && !signing.AgentReady(ctx, cfg.Signing.PublicKey) {
		return fail(stderr, fmt.Errorf("agent load ran but the agent does not hold the configured public key — check key_ref / vault contents"))
	}
	fmt.Fprintln(stdout, "signing enabled")
	return 0
}

// signingStatusJSON is the --json shape for `signing status`. It carries
// every field that printSigningStatus renders to the human table, so scripts
// can branch on mode, agent_ready, allowed_signers_state, etc. without
// parsing free-form text.
type signingStatusJSON struct {
	ProjectID           string            `json:"project_id"`
	Required            bool              `json:"required"`
	Mode                string            `json:"mode"`
	Provider            string            `json:"provider"`
	KeySource           string            `json:"key_source"`
	Identity            string            `json:"identity"`
	Artifacts           bool              `json:"artifacts"`
	PostureSummary      string            `json:"posture_summary"`
	PostureNote         string            `json:"posture_note,omitempty"`
	PostureWarn         bool              `json:"posture_warn,omitempty"`
	PubkeyFP            string            `json:"pubkey_fp,omitempty"`
	AgentReady          *bool             `json:"agent_ready,omitempty"`
	Repo                signing.RepoState `json:"repo"`
	AllowedSignersPath  string            `json:"allowed_signers_path"`
	AllowedSignersState string            `json:"allowed_signers_state"`
}

// cmdSigningStatus prints the mode/provider/agent/repo summary.
func cmdSigningStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("signing status", stderr)
	projectID := fs.String("project", "", "project id (required)")
	asJSON := fs.Bool("json", false, "emit JSON")
	setUsage(fs, stdout, "mode/provider/agent-ready/repo-config/allowed_signers summary", "--project ID [--json]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *projectID == "" {
		return usageErr(stderr, "signing status: --project is required")
	}

	ctx := context.Background()
	_, rec, cfg, err := signingProject(ctx, *projectID)
	if err != nil {
		return fail(stderr, err)
	}
	if cfg.Signing == nil {
		if *asJSON {
			// Emit a minimal JSON object indicating signing is not configured.
			if err := printJSON(stdout, map[string]any{
				"project_id": *projectID,
				"configured": false,
			}); err != nil {
				return fail(stderr, err)
			}
			return 0
		}
		fmt.Fprintf(stdout, "project %s: signing not configured (run `koryph signing setup`)\n", *projectID)
		return 0
	}
	if *asJSON {
		if err := printJSON(stdout, buildSigningStatusJSON(ctx, rec, cfg.Signing)); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	printSigningStatus(ctx, stdout, rec, cfg.Signing)
	return 0
}

// buildSigningStatusJSON assembles the signingStatusJSON for --json output.
func buildSigningStatusJSON(ctx context.Context, rec *registry.Record, sc *signing.Config) signingStatusJSON {
	st := signing.InspectRepo(ctx, rec.Root)
	allowedPath := filepath.Join(rec.Root, signing.AllowedSignersFileName)
	allowedState := "missing"
	if data, err := os.ReadFile(allowedPath); err == nil {
		allowedState = "present"
		if sc.Identity != "" && !strings.Contains(string(data), sc.Identity) {
			allowedState = "present (identity NOT listed)"
		}
	}

	posture := signing.ClassifyPosture(sc)

	out := signingStatusJSON{
		ProjectID:           rec.ProjectID,
		Required:            sc.Required,
		Mode:                sc.EffectiveMode(),
		Provider:            sc.Provider,
		KeySource:           signingKeySource(sc),
		Identity:            sc.Identity,
		Artifacts:           sc.Artifacts,
		PostureSummary:      posture.Summary,
		PostureNote:         posture.Note,
		PostureWarn:         posture.Level == signing.PosturePlaintext && sc.Provider != "",
		Repo:                st,
		AllowedSignersPath:  allowedPath,
		AllowedSignersState: allowedState,
	}
	if sc.EffectiveMode() == signing.ModeSSH {
		fp := ""
		if sc.PublicKey != "" {
			fp = signing.KeyFingerprint(sc.PublicKey)
		}
		out.PubkeyFP = fp
		ready := signing.AgentReady(ctx, sc.PublicKey)
		out.AgentReady = &ready
	}
	return out
}

// printSigningStatus renders the policy, agent readiness, repo git config,
// and allowed-signers state.
func printSigningStatus(ctx context.Context, w io.Writer, rec *registry.Record, sc *signing.Config) {
	fmt.Fprintf(w, "project:         %s\n", rec.ProjectID)
	fmt.Fprintf(w, "required:        %v\n", sc.Required)
	fmt.Fprintf(w, "mode:            %s\n", sc.EffectiveMode())
	fmt.Fprintf(w, "provider:        %s\n", orDash(sc.Provider))
	fmt.Fprintf(w, "key source:      %s\n", signingKeySource(sc))
	fmt.Fprintf(w, "identity:        %s\n", orDash(sc.Identity))
	fmt.Fprintf(w, "artifacts:       %v\n", sc.Artifacts)

	// Posture ladder.
	posture := signing.ClassifyPosture(sc)
	postureLabel := "ok"
	if posture.Level == signing.PosturePlaintext && sc.Provider != "" {
		postureLabel = "WARN"
	}
	fmt.Fprintf(w, "posture:         %s (%s)\n", postureLabel, posture.Summary)
	if posture.Note != "" {
		fmt.Fprintf(w, "posture note:    %s\n", posture.Note)
	}

	if sc.EffectiveMode() == signing.ModeSSH {
		fp := "(no public key)"
		if sc.PublicKey != "" {
			fp = signing.KeyFingerprint(sc.PublicKey)
		}
		fmt.Fprintf(w, "pubkey fp:       %s\n", fp)
		fmt.Fprintf(w, "agent ready:     %s\n", yesno(signing.AgentReady(ctx, sc.PublicKey)))
	} else {
		fmt.Fprintf(w, "agent ready:     n/a (gitsign is keyless; first signature opens a browser for OIDC)\n")
	}
	st := signing.InspectRepo(ctx, rec.Root)
	fmt.Fprintf(w, "repo gpg.format: %s\n", orDash(st.GPGFormat))
	fmt.Fprintf(w, "repo signingkey: %s\n", orDash(st.SigningKey))
	fmt.Fprintf(w, "repo gpgsign:    %s\n", orDash(st.CommitGPGSign))
	if st.X509Program != "" {
		fmt.Fprintf(w, "repo x509 prog:  %s\n", st.X509Program)
	}
	signers := filepath.Join(rec.Root, signing.AllowedSignersFileName)
	state := "missing"
	if data, err := os.ReadFile(signers); err == nil {
		state = "present"
		if sc.Identity != "" && !strings.Contains(string(data), sc.Identity) {
			state = "present (identity NOT listed)"
		}
	}
	fmt.Fprintf(w, "allowed_signers: %s (%s)\n", signers, state)
	fmt.Fprintf(w, "repo allowedSignersFile: %s\n", orDash(st.AllowedSignersFile))
}

// cmdSigningVerify verifies branch signatures against the default branch and
// exits non-zero when any commit fails.
func cmdSigningVerify(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("signing verify", stderr)
	projectID := fs.String("project", "", "project id (required)")
	branch := fs.String("branch", "", "branch to verify against the default branch (required)")
	setUsage(fs, stdout, "verify branch commit signatures against the default branch (exit 1 on any bad)",
		"--project ID --branch BR")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *projectID == "" || *branch == "" {
		return usageErr(stderr, "signing verify: --project and --branch are required")
	}

	ctx := context.Background()
	_, rec, cfg, err := signingProject(ctx, *projectID)
	if err != nil {
		return fail(stderr, err)
	}
	bad, err := signing.Verify(ctx, rec.Root, rec.DefaultBranch, *branch)
	if err != nil {
		return fail(stderr, err)
	}
	if len(bad) > 0 {
		fmt.Fprintf(stdout, "%d unsigned/unverifiable commit(s) on %s..%s:\n", len(bad), rec.DefaultBranch, *branch)
		for _, b := range bad {
			fmt.Fprintln(stdout, "  "+b)
		}
		// Print provider-specific remediation hints to help the operator
		// diagnose and fix the root cause.
		if cfg.Signing != nil {
			printVerifyHints(ctx, stderr, rec.ProjectID, rec.Root, cfg.Signing)
		}
		return engine.ExitFatal
	}
	fmt.Fprintf(stdout, "all commits on %s..%s carry good signatures\n", rec.DefaultBranch, *branch)
	return 0
}

// printVerifyHints inspects the live signing state (agent + repo config) and
// emits actionable, provider-specific remediation advice when the root cause
// can be detected. Called only when verification found unsigned/bad commits.
func printVerifyHints(ctx context.Context, w io.Writer, projectID, repoRoot string, sc *signing.Config) {
	if sc.EffectiveMode() != signing.ModeSSH {
		// gitsign failures are OIDC-side; no vault provider is involved.
		fmt.Fprintf(w, "hint: gitsign commit verification failed — ensure the signer's Sigstore certificate is trusted and the identity matches %q\n", sc.Identity)
		return
	}

	st := signing.InspectRepo(ctx, repoRoot)
	agentOK := sc.PublicKey != "" && signing.AgentReady(ctx, sc.PublicKey)
	repoOK := st.AllowedSignersFile != ""

	if agentOK && repoOK {
		// Config looks healthy — signature is just bad or key not in signers.
		return
	}

	vault, _ := signing.LoadVault()

	fmt.Fprintln(w)
	if !agentOK {
		fmt.Fprintf(w, "hint: agent does not hold the configured key (provider=%s)\n", sc.Provider)
		if vault != nil {
			if pt, ok := vault.Providers[sc.Provider]; ok && pt.LoginHint != "" {
				fmt.Fprintf(w, "      1. log in:   %s\n", pt.LoginHint)
				fmt.Fprintf(w, "      2. load key: koryph signing enable --project %s\n", projectID)
			} else {
				fmt.Fprintf(w, "      load key: koryph signing enable --project %s\n", projectID)
			}
		} else {
			fmt.Fprintf(w, "      load key: koryph signing enable --project %s\n", projectID)
		}
	}
	if !repoOK {
		if !agentOK {
			fmt.Fprintln(w) // blank line between two hints
		}
		fmt.Fprintf(w, "hint: gpg.ssh.allowedSignersFile is not configured for this repo\n")
		fmt.Fprintf(w, "      configure: koryph signing enable --project %s\n", projectID)
	}
}

// cmdSignBlob cosign-signs an artifact (requires signing.artifacts).
func cmdSignBlob(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("sign blob", stderr)
	projectID := fs.String("project", "", "project id (required)")
	setUsage(fs, stdout, "cosign sign-blob an artifact via the vault key (writes <path>.sig)", "--project ID <path>")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if *projectID == "" {
		return usageErr(stderr, "sign blob: --project is required")
	}
	if len(pos) < 1 {
		return usageErr(stderr, "sign blob: <path> is required")
	}

	ctx := context.Background()
	_, _, cfg, err := signingProject(ctx, *projectID)
	if err != nil {
		return fail(stderr, err)
	}
	if cfg.Signing == nil || !cfg.Signing.Artifacts {
		return fail(stderr, fmt.Errorf("project %s does not enable artifact signing (signing.artifacts) — run `koryph signing setup ... --artifacts`", *projectID))
	}
	vault, err := signing.LoadVault()
	if err != nil {
		return fail(stderr, err)
	}
	sig, err := signing.SignBlob(ctx, vault, cfg.Signing, pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "signed: %s\n", sig)
	return 0
}

// cliActor identifies this CLI invocation for audit events.
func cliActor() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("koryph@%s:%d", host, os.Getpid())
}
