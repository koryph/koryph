// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/signing"
)

// keygenDuration is the timeout for ssh-keygen / ssh-add invocations.
const keygenDuration = 30 * time.Second

// cmdSigningKeygen generates a new SSH signing key and stores it with the
// chosen provider. This is the no-vault path: the operator does not need a
// password manager; key material is protected by a mandatory passphrase.
//
// Flow:
//  1. Resolve provider (explicit flag → project config → platform default)
//  2. Resolve key_ref (explicit flag → default path under ~/.koryph/signing/)
//  3. Prompt for passphrase twice (non-empty required for file + encrypted-file)
//  4. Generate an ed25519 key pair via ssh-keygen
//  5. Store the private key with the provider
//  6. On darwin with file/encrypted-file: load via ssh-add --apple-use-keychain
//     and print the UseKeychain hint
//  7. Print the public key and next steps (koryph signing setup + enable)
func cmdSigningKeygen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("signing keygen", stderr)
	projectID := fs.String("project", "", "project id (used to resolve existing config; optional)")
	provider := fs.String("provider", "", "vault provider: keychain|encrypted-file|file (default: platform best)")
	keyRef := fs.String("key-ref", "", "path to store the key (default: ~/.koryph/signing/<project>.key)")
	identity := fs.String("identity", "", "key comment / signer identity (default: <username>@<host>)")
	setUsage(fs, stdout, "generate a passphrase-protected SSH signing key (no-vault path)",
		"--project ID [--provider P] [--key-ref PATH] [--identity EMAIL]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}

	ctx := context.Background()

	// Resolve provider.
	chosenProvider := *provider
	if chosenProvider == "" {
		chosenProvider = signing.ResolveDefaultProvider()
	}

	// Validate provider for this platform.
	if chosenProvider == signing.ProviderKeychain && runtime.GOOS != "darwin" {
		return fail(stderr, fmt.Errorf("signing keygen: provider keychain is only supported on macOS; use encrypted-file on this platform"))
	}

	// Resolve key_ref path.
	chosenRef := *keyRef
	if chosenRef == "" {
		name := *projectID
		if name == "" {
			name = "default"
		}
		signingDir := filepath.Join(paths.KoryphHome(), "signing")
		if err := os.MkdirAll(signingDir, 0o700); err != nil {
			return fail(stderr, fmt.Errorf("signing keygen: create signing dir: %w", err))
		}
		chosenRef = filepath.Join(signingDir, name+".key")
	}

	// Resolve identity (comment in the generated key).
	chosenIdentity := *identity
	if chosenIdentity == "" {
		host, _ := os.Hostname()
		chosenIdentity = fmt.Sprintf("koryph-signing@%s", host)
	}

	// Prompt for passphrase (required for file and encrypted-file).
	var passphrase string
	if chosenProvider != signing.ProviderKeychain {
		pp, err := promptAndConfirmPassphrase(stderr)
		if err != nil {
			return fail(stderr, err)
		}
		passphrase = pp
	}

	// Generate the SSH key pair.
	pubKey, privKeyBytes, err := generateSSHKey(ctx, chosenRef, chosenIdentity, passphrase, chosenProvider)
	if err != nil {
		return fail(stderr, fmt.Errorf("signing keygen: %w", err))
	}

	// Store the private key with the chosen provider.
	if err := storeKeyForProvider(ctx, chosenProvider, chosenRef, privKeyBytes, passphrase); err != nil {
		return fail(stderr, fmt.Errorf("signing keygen: store: %w", err))
	}

	// On darwin with the file provider, load via ssh-add --apple-use-keychain
	// so the passphrase is cached in the macOS Keychain across reboots.
	// The encrypted-file provider is excluded: chosenRef is an age-encrypted
	// blob that ssh-add cannot parse; the key can be loaded after decryption
	// via `koryph signing enable --project <id>`.
	if runtime.GOOS == "darwin" && chosenProvider == signing.ProviderFile {
		if err := loadKeyAppleKeychain(ctx, chosenRef, stdout, stderr); err != nil {
			// Non-fatal: the key is stored; the user can load it manually.
			fmt.Fprintf(stderr, "note: ssh-add --apple-use-keychain failed (%v); load manually with: ssh-add %s\n", err, chosenRef)
		}
	}

	// Print results.
	fmt.Fprintf(stdout, "provider:      %s\n", chosenProvider)
	fmt.Fprintf(stdout, "key stored at: %s\n", chosenRef)
	fmt.Fprintf(stdout, "public key:    %s\n", pubKey)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "next steps:")
	nextProject := *projectID
	if nextProject == "" {
		nextProject = "<project-id>"
	}
	if *identity != "" {
		fmt.Fprintf(stdout, "  koryph signing setup --project %s --provider %s --key-ref %s --identity %s --public-key @%s.pub\n",
			nextProject, chosenProvider, chosenRef, *identity, chosenRef)
	} else {
		fmt.Fprintf(stdout, "  koryph signing setup --project %s --provider %s --key-ref %s --identity <your-email> --public-key @%s.pub\n",
			nextProject, chosenProvider, chosenRef, chosenRef)
	}
	fmt.Fprintf(stdout, "  koryph signing enable --project %s\n", nextProject)

	if runtime.GOOS == "darwin" && chosenProvider == signing.ProviderFile {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "macOS tip: add to ~/.ssh/config to persist across reboots:")
		fmt.Fprintln(stdout, "  Host *")
		fmt.Fprintln(stdout, "    UseKeychain yes")
		fmt.Fprintln(stdout, "    AddKeysToAgent yes")
	}

	return 0
}

// promptAndConfirmPassphrase prompts for a passphrase twice and returns it.
// An empty passphrase is rejected with an explanation pointing at vault docs.
func promptAndConfirmPassphrase(stderr io.Writer) (string, error) {
	pass1, err := signing.PromptPassphraseOnce("New signing key passphrase: ")
	if err != nil {
		return "", fmt.Errorf("passphrase input: %w", err)
	}
	if pass1 == "" {
		return "", fmt.Errorf(
			"signing keygen: passphrase must not be empty\n" +
				"A passphrase-protected key is standard SSH security practice\n" +
				"Without a passphrase, the key on disk is as insecure as a password in plaintext\n" +
				"If you prefer automated secret management, see docs/user-guide/signing.md for vault providers")
	}
	pass2, err := signing.PromptPassphraseOnce("Confirm passphrase: ")
	if err != nil {
		return "", fmt.Errorf("passphrase confirmation: %w", err)
	}
	if pass1 != pass2 {
		return "", fmt.Errorf("signing keygen: passphrases do not match")
	}
	return pass1, nil
}

// generateSSHKey generates an ed25519 SSH key pair. For encrypted-file provider
// the key is generated without an SSH passphrase (age wraps it instead). For
// file and keychain providers the key is generated with the given passphrase
// (or empty for keychain).
//
// Returns the public key line and the raw private key PEM bytes.
func generateSSHKey(ctx context.Context, keyRef, identity, passphrase string, provider string) (pubKeyLine string, privKey []byte, err error) {
	// For encrypted-file: generate WITHOUT SSH passphrase (age provides the
	// encryption layer). For file/keychain: embed the passphrase in the key.
	sshPassphrase := passphrase
	if provider == signing.ProviderEncryptedFile {
		sshPassphrase = "" // age encrypts; SSH passphrase on the key is redundant
	}

	// Write to a temp directory so we can read the private key regardless
	// of whether the final storage is a file or Keychain.
	tmpDir, err := os.MkdirTemp("", "koryph-keygen-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpKey := filepath.Join(tmpDir, "id_ed25519")
	res, err := execx.Run(ctx, execx.Cmd{
		Name: "ssh-keygen",
		Args: []string{
			"-t", "ed25519",
			"-f", tmpKey,
			"-N", sshPassphrase,
			"-C", identity,
		},
		Timeout: keygenDuration,
	})
	if err != nil {
		return "", nil, fmt.Errorf("ssh-keygen: %w", err)
	}
	if res.ExitCode != 0 {
		return "", nil, fmt.Errorf("ssh-keygen exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	// Read the generated keys.
	privBytes, err := os.ReadFile(tmpKey)
	if err != nil {
		return "", nil, fmt.Errorf("read private key: %w", err)
	}
	pubBytes, err := os.ReadFile(tmpKey + ".pub")
	if err != nil {
		return "", nil, fmt.Errorf("read public key: %w", err)
	}

	// Always copy the public key to <keyRef>.pub — it is not secret and
	// is needed for `koryph signing setup --public-key @<keyRef>.pub`
	// regardless of which provider stores the private key.
	if err := copyKey(tmpKey+".pub", keyRef+".pub"); err != nil {
		return "", nil, err
	}

	// For the file provider, also copy the private key to the final path
	// (ssh-keygen wrote it to tmpDir; other providers store it natively).
	if provider == signing.ProviderFile {
		if err := copyKey(tmpKey, keyRef); err != nil {
			return "", nil, err
		}
	}

	pubLine := strings.TrimSpace(string(pubBytes))
	return pubLine, privBytes, nil
}

// copyKey copies src to dst with mode 0600.
func copyKey(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// storeKeyForProvider stores the private key material with the appropriate
// provider. For file provider the key was already placed at keyRef by
// generateSSHKey.
func storeKeyForProvider(ctx context.Context, provider, keyRef string, privKey []byte, passphrase string) error {
	switch provider {
	case signing.ProviderFile:
		// Already written by generateSSHKey.
		return nil

	case signing.ProviderEncryptedFile:
		return signing.StoreEncryptedFile(keyRef, privKey, passphrase)

	case signing.ProviderKeychain:
		return signing.StoreKeychain(keyRef, privKey)

	default:
		return fmt.Errorf("keygen does not support provider %q (use koryph signing setup for vault-backed providers)", provider)
	}
}

// loadKeyAppleKeychain loads the key at keyRef into the system SSH agent using
// `ssh-add --apple-use-keychain`, which also caches the passphrase in the
// macOS Keychain for persistence across reboots.
func loadKeyAppleKeychain(ctx context.Context, keyRef string, stdout, _ io.Writer) error {
	res, err := execx.Run(ctx, execx.Cmd{
		Name:    "ssh-add",
		Args:    []string{"--apple-use-keychain", keyRef},
		Timeout: keygenDuration,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("ssh-add --apple-use-keychain exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	fmt.Fprintf(stdout, "key loaded into agent (passphrase cached in macOS Keychain)\n")
	return nil
}
