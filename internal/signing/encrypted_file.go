// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
)

// encryptedFileHeader is the age format magic. Presence confirms the file is an
// age-encrypted blob rather than a plaintext or PEM key.
const encryptedFileHeader = "age-encryption.org/v1"

// FetchEncryptedFile decrypts an age-encrypted file at the given path and
// returns the plaintext secret. The passphrase is read from KORYPH_PASSPHRASE
// if set, otherwise from the controlling TTY (/dev/tty).
//
// This is the Fetch implementation for the encrypted-file built-in provider.
// The returned bytes are held in memory only and never written to disk.
func FetchEncryptedFile(ref string) ([]byte, error) {
	if ref == "" {
		return nil, fmt.Errorf("signing: provider encrypted-file needs a key_ref path")
	}

	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, fmt.Errorf("signing: encrypted-file: read %s: %w", ref, err)
	}

	passphrase, err := readPassphraseForDecrypt(ref)
	if err != nil {
		return nil, err
	}

	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("signing: encrypted-file: invalid passphrase: %w", err)
	}

	r, err := age.Decrypt(bytes.NewReader(data), identity)
	if err != nil {
		return nil, fmt.Errorf("signing: encrypted-file: decrypt %s: %w (wrong passphrase?)", ref, err)
	}

	plaintext, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("signing: encrypted-file: read plaintext: %w", err)
	}
	return plaintext, nil
}

// StoreEncryptedFile encrypts secret with passphrase and writes it atomically
// to ref. The file is created with mode 0600. passphrase must be non-empty.
//
// This is the Store implementation for the encrypted-file built-in provider.
// The plaintext secret is never written to disk in unencrypted form.
func StoreEncryptedFile(ref string, secret []byte, passphrase string) error {
	if ref == "" {
		return fmt.Errorf("signing: provider encrypted-file needs a key_ref path")
	}
	if passphrase == "" {
		return fmt.Errorf("signing: encrypted-file: passphrase must not be empty — an unpassworded age file is as insecure as plaintext")
	}

	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return fmt.Errorf("signing: encrypted-file: create recipient: %w", err)
	}

	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return fmt.Errorf("signing: encrypted-file: encrypt: %w", err)
	}
	if _, err := w.Write(secret); err != nil {
		return fmt.Errorf("signing: encrypted-file: write plaintext: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("signing: encrypted-file: finalize: %w", err)
	}

	// Write atomically: temp file + rename so partial writes are invisible.
	tmp := ref + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("signing: encrypted-file: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, ref); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("signing: encrypted-file: rename to %s: %w", ref, err)
	}
	return nil
}

// IsAgeEncrypted reports whether data starts with the age header magic. Used
// by the posture classifier to distinguish age-encrypted files from plaintext.
func IsAgeEncrypted(data []byte) bool {
	return bytes.HasPrefix(data, []byte(encryptedFileHeader))
}

// readPassphraseForDecrypt returns the passphrase for decryption. It uses
// KORYPH_PASSPHRASE if set; otherwise it prompts on /dev/tty.
//
// KORYPH_PASSPHRASE is intended for automated / non-interactive use (e.g. CI).
// The trade-off: the env var is visible to child processes; prefer a vault
// provider for fully automated deployments. See the user guide for details.
func readPassphraseForDecrypt(ref string) (string, error) {
	if p := os.Getenv("KORYPH_PASSPHRASE"); p != "" {
		return p, nil
	}
	return PromptPassphraseOnce(fmt.Sprintf("Passphrase for %s: ", ref))
}

// PromptPassphraseOnce reads a passphrase from /dev/tty without echo. This
// always reads from the controlling TTY so it works even when stdin/stdout are
// redirected. Returns an error if no TTY is available.
//
// For non-interactive use, set the KORYPH_PASSPHRASE environment variable.
func PromptPassphraseOnce(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("signing: no controlling TTY for passphrase input (set KORYPH_PASSPHRASE env for non-interactive use): %w", err)
	}
	defer tty.Close()

	if _, err := fmt.Fprint(tty, prompt); err != nil {
		return "", fmt.Errorf("signing: write to TTY: %w", err)
	}

	pass, err := readLineNoEcho(tty)
	if err != nil {
		return "", fmt.Errorf("signing: read passphrase from TTY: %w", err)
	}
	fmt.Fprintln(tty) // newline after the hidden input
	return pass, nil
}
